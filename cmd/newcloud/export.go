package main

// Portable export (§9.5).
//
// The requirement is not "add a download button": it is that a user's files and the metadata needed to
// read them can leave this system in a form that does not depend on this system's database. A dump
// satisfies a backup and not an export — the point is that somebody with a tar of files, a manifest
// and no knowledge of the schema can read what they own.
//
// So the command writes a directory tree with real filenames, verifies every file against the content
// address the metadata records BEFORE reporting success, and writes a manifest that stands alone. The
// verification is the part that makes the output evidence: without it, a file that arrived truncated
// looks exactly like a file that arrived whole.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob"
	localblob "github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// exportManifest is the metadata half of the export.
//
// It is deliberately self-describing JSON with every path stored RELATIVE to the export root: an
// absolute path records the machine the export ran on, which is not a property of the user's data and
// would make two exports of the same content differ.
type exportManifest struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Account     exportAccount `json:"account"`
	Root        string        `json:"root"`
	Files       []exportFile  `json:"files"`
	Directories []string      `json:"directories"`
	// Skipped names anything that could not be exported and why. An export that silently drops a
	// file is worse than one that fails: the operator copies it somewhere and deletes the source.
	Skipped []exportSkip `json:"skipped,omitempty"`
	Totals  exportTotals `json:"totals"`
}

type exportAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

type exportFile struct {
	// Path is relative to the export root, using forward slashes on every platform.
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Mime     string    `json:"mime"`
	ETag     string    `json:"etag,omitempty"`
	SHA256   string    `json:"sha256"`
	Modified time.Time `json:"modified_at"`
	Created  time.Time `json:"created_at"`
}

type exportSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type exportTotals struct {
	Files       int   `json:"files"`
	Directories int   `json:"directories"`
	Bytes       int64 `json:"bytes"`
	Skipped     int   `json:"skipped"`
}

func exportCommand(args []string) error {
	log := newLogger()

	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	user := flags.String("user", "", "username or id of the account to export (required)")
	out := flags.String("out", "", "destination directory; must not exist or be empty")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *user == "" || *out == "" {
		return errors.New("export: --user and --out are required")
	}

	cfg, err := config.Load(configPath())
	if err != nil {
		return err
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("export: DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// The schema check is not needed here: export reads through the store interface, and
	// a read-only command that refuses because a LATER migration is missing would be
	// unable to export the very data an operator is trying to save before an upgrade.
	meta, closeMeta, _, err := openMetadata(ctx, log)
	if err != nil {
		return err
	}
	defer closeMeta()

	blobs, err := openExportBlobs(cfg, meta, log)
	if err != nil {
		return err
	}

	// Resolve the account first: exporting the wrong user because a username was ambiguous is the
	// mistake this command cannot take back, since the output leaves the machine.
	acct, err := findAccount(ctx, meta, *user)
	if err != nil {
		return err
	}

	man, err := runExport(ctx, meta, blobs, acct, *out)
	if err != nil {
		return err
	}
	log.Info("export complete",
		"user", acct.Username, "out", *out,
		"files", man.Totals.Files, "directories", man.Totals.Directories,
		"bytes", man.Totals.Bytes, "skipped", man.Totals.Skipped)
	if man.Totals.Skipped > 0 {
		log.Warn("some nodes were not exported", "count", man.Totals.Skipped,
			"manifest", filepath.Join(*out, "manifest.json"))
	}
	return nil
}

// runExport writes the tree and the manifest, and verifies every byte it writes.
func runExport(ctx context.Context, meta store.Store, blobs blob.Store, acct store.User, out string) (exportManifest, error) {
	man := exportManifest{
		GeneratedAt: time.Now().UTC(),
		Account:     exportAccount{ID: acct.ID.String(), Username: acct.Username, Email: acct.Email},
	}

	// Refuse a non-empty destination. Merging into an existing directory leaves files from a
	// previous export interleaved with this one, and neither the manifest nor the operator can tell
	// which is which.
	entries, err := os.ReadDir(out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return man, fmt.Errorf("export: read destination: %w", err)
	}
	if len(entries) != 0 {
		return man, fmt.Errorf("export: %s is not empty; refusing to mix this export with what is "+
			"already there", out)
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return man, fmt.Errorf("export: create destination: %w", err)
	}

	var nodes []store.Node
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, lerr := tx.ListOwnerNodes(ctx, acct.ID)
		nodes = n
		return lerr
	}); err != nil {
		return man, fmt.Errorf("export: list nodes: %w", err)
	}

	paths, err := exportPaths(nodes)
	if err != nil {
		return man, err
	}

	// Directories first, so a file write never has to create its parent.
	for _, n := range nodes {
		if !n.IsDir() {
			continue
		}
		rel := paths[n.ID]
		if rel == "" {
			continue // the account root is the destination itself
		}
		if err := os.MkdirAll(filepath.Join(out, filepath.FromSlash(rel)), 0o700); err != nil {
			return man, fmt.Errorf("export: create directory %s: %w", rel, err)
		}
		man.Directories = append(man.Directories, rel)
	}

	for _, n := range nodes {
		if n.IsDir() {
			continue
		}
		rel := paths[n.ID]
		if rel == "" {
			man.Skipped = append(man.Skipped, exportSkip{Path: n.Name,
				Reason: "the node has no path from the account root; it is unreachable in the tree"})
			continue
		}
		written, hash, werr := writeExportedFile(ctx, meta, blobs, out, rel, n)
		if werr != nil {
			// A content-store failure is not a reason to produce a manifest that omits the file:
			// the operator asked for an export, and half an export silently labelled complete is
			// the outcome that loses data.
			return man, fmt.Errorf("export: %s: %w", rel, werr)
		}
		man.Files = append(man.Files, exportFile{
			Path: rel, Size: written, Mime: n.Mime, ETag: n.ETag,
			SHA256: hash, Modified: n.ModifiedAt.UTC(), Created: n.CreatedAt.UTC(),
		})
		man.Totals.Bytes += written
	}
	sort.Slice(man.Files, func(i, j int) bool { return man.Files[i].Path < man.Files[j].Path })
	sort.Strings(man.Directories)

	man.Totals.Files = len(man.Files)
	man.Totals.Directories = len(man.Directories)
	man.Totals.Skipped = len(man.Skipped)

	// The manifest is written last, and only after every file verified: its presence is the
	// signal that the export ran to completion.
	body, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return man, err
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), append(body, '\n'), 0o600); err != nil {
		return man, fmt.Errorf("export: write manifest: %w", err)
	}
	return man, nil
}

// exportPaths maps node id to a path relative to the account root.
//
// A node whose ancestry does not reach the root gets no path rather than a guessed one: inventing a
// location would put a file in the export where the user never had it.
func exportPaths(nodes []store.Node) (map[uuid.UUID]string, error) {
	byID := make(map[uuid.UUID]store.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}

	paths := make(map[uuid.UUID]string, len(nodes))
	var resolve func(n store.Node, depth int) string
	resolve = func(n store.Node, depth int) string {
		if depth > 256 {
			return "" // a cycle, which the store should not permit and must not hang here
		}
		if p, ok := paths[n.ID]; ok {
			return p
		}
		if n.ParentID == nil {
			paths[n.ID] = "" // the account root becomes the destination directory
			return ""
		}
		parent, ok := byID[*n.ParentID]
		if !ok {
			return ""
		}
		pp := resolve(parent, depth+1)
		if pp == "" && parent.ParentID != nil {
			return "" // the parent is unreachable, so this node is too
		}
		joined := n.Name
		if pp != "" {
			joined = pp + "/" + n.Name
		}
		paths[n.ID] = joined
		return joined
	}
	for _, n := range nodes {
		resolve(n, 0)
	}
	return paths, nil
}

// writeExportedFile copies one file out and returns what it wrote and its digest.
//
// The expected digest comes from the METADATA, not from the bytes that happen to be in the store:
// comparing the copy against itself would prove only that the copy is self-consistent. The recorded
// address is what the user was promised when they uploaded the file.
func writeExportedFile(ctx context.Context, meta store.Store, blobs blob.Store, out, rel string, n store.Node) (int64, string, error) {
	// The destination is built from names that came out of the database. The store forbids
	// separators and ".." inside a name; this check is what keeps the command safe if that ever
	// changes, because the output leaves the machine.
	clean := filepath.Clean(filepath.Join(out, filepath.FromSlash(rel)))
	root, err := filepath.Abs(out)
	if err != nil {
		return 0, "", err
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return 0, "", err
	}
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return 0, "", fmt.Errorf("path %q escapes the export root", rel)
	}
	if n.BlobID == nil {
		return 0, "", errors.New("the node has no content address")
	}

	var recorded []byte
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		h, herr := tx.BlobHash(ctx, *n.BlobID)
		recorded = h
		return herr
	}); err != nil {
		return 0, "", fmt.Errorf("read recorded address: %w", err)
	}
	var want blob.Hash
	if len(recorded) != len(want) {
		return 0, "", fmt.Errorf("the recorded address is %d bytes; the metadata is inconsistent",
			len(recorded))
	}
	copy(want[:], recorded)

	src, err := blobs.Open(ctx, want)
	if err != nil {
		return 0, "", fmt.Errorf("open content %s: %w", want, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(clean, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hasher), src)
	if err != nil {
		dst.Close()
		return 0, "", err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return 0, "", err
	}
	if err := dst.Close(); err != nil {
		return 0, "", err
	}

	var got blob.Hash
	copy(got[:], hasher.Sum(nil))
	if got != want {
		// The bytes in the store do not match the address the metadata records. Keep going would
		// produce an export that looks complete and contains something else.
		return 0, "", fmt.Errorf("content does not match the recorded address: metadata says %s, "+
			"the bytes hash to %s", want, got)
	}
	return written, got.String(), nil
}

// findAccount resolves a username or uuid to an account.
func findAccount(ctx context.Context, meta store.Store, who string) (store.User, error) {
	if id, err := uuid.Parse(who); err == nil {
		var u store.User
		if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, gerr := tx.UserByID(ctx, id)
			u = got
			return gerr
		}); err != nil {
			return store.User{}, fmt.Errorf("export: no account with id %s: %w", who, err)
		}
		return u, nil
	}
	var u store.User
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, gerr := tx.UserByUsername(ctx, who)
		u = got
		return gerr
	}); err != nil {
		return store.User{}, fmt.Errorf("export: no account named %q: %w", who, err)
	}
	return u, nil
}

// openExportBlobs opens the content store described by the configuration.
func openExportBlobs(cfg config.Config, meta store.Store, log *slog.Logger) (blob.Store, error) {
	if cfg.Storage.Backend == "s3" {
		_, bs, err := openS3Storage(cfg, meta, log)
		return bs, err
	}
	return localblob.New(cfg.Storage.Local.Root)
}
