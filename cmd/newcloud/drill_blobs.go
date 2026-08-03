package main

// Blob content verification for the restore drill.
//
// The database drill proves the *metadata* survives. It says nothing about the bytes, and the
// bytes are the user's actual data: a restore that brings back a perfectly consistent node
// tree pointing at blobs that were never copied, or were copied truncated, produces a
// deployment that lists every file and opens none of them.
//
// The check is possible because blob storage is content-addressed — the metadata names a hash
// and the store must produce bytes that hash to it — and it is done end-to-end rather than by
// stat()ing files, so a restore that copied the blobs but lost the resolver rows fails here
// instead of failing for a user.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// blobCheck is the outcome of verifying blob content.
type blobCheck struct {
	Files      int
	Bytes      int64
	Verified   int
	Problems   []string
	Skipped    bool
	SkipReason string
}

// verifyBlobs copies the blob root and reads every file back through a content store over
// the copy.
//
// liveContent serves the live bytes and restoredMeta is a store opened over the *restored*
// database. Taking the file list from the restored metadata is what makes this a check of the
// restored pair rather than a comparison of the live deployment with itself.
func verifyBlobs(ctx context.Context, cfg config.Config, liveContent layout.ContentStore,
	restoredMeta store.Store, scratchRoot string) blobCheck {
	var res blobCheck

	if cfg.Storage.Backend != "local" || cfg.Storage.Layout != config.LayoutBlob {
		// This needs a local content-addressed root it can copy. An S3 backend needs a
		// bucket-level copy, and the tree layout is a filesystem mirror restored as ordinary
		// files — neither is what this function checks.
		res.Skipped = true
		res.SkipReason = fmt.Sprintf(
			"blob verification supports the local/blob layout, not %s/%s",
			cfg.Storage.Backend, cfg.Storage.Layout)
		return res
	}
	if liveContent == nil {
		res.Skipped = true
		res.SkipReason = "no live content store to compare against"
		return res
	}

	src := cfg.Storage.Local.Root
	if _, err := os.Stat(src); err != nil {
		res.Skipped = true
		res.SkipReason = "blob root does not exist: " + err.Error()
		return res
	}

	// 1. Copy, the way a file-level backup would.
	if err := copyTree(src, scratchRoot); err != nil {
		res.Problems = append(res.Problems, "copy blob root: "+err.Error())
		return res
	}

	// 2. Open a content store over the copy.
	localStore, err := local.New(scratchRoot)
	if err != nil {
		res.Problems = append(res.Problems, "open the restored blob store: "+err.Error())
		return res
	}
	restoredContent, err := blobstore.New(localStore, resolverFor(restoredMeta))
	if err != nil {
		res.Problems = append(res.Problems, "build the restored content store: "+err.Error())
		return res
	}

	// 3. Read every file node back through both stores and compare.
	files, complete, ferr := allFileNodes(ctx, restoredMeta)
	if ferr != nil {
		res.Problems = append(res.Problems, "list restored file nodes: "+ferr.Error())
		return res
	}
	if !complete {
		// A truncated walk means some files were never compared, and calling the check a pass
		// would overstate what was verified.
		res.Problems = append(res.Problems,
			"the restored tree is larger than the walk limit; not every file's bytes were compared")
	}
	res.Files = len(files)

	for _, f := range files {
		rel, perr := pathOf(ctx, restoredMeta, f)
		if perr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"node %q: cannot rebuild its path: %v", f.Name, perr))
			continue
		}
		owner := f.OwnerID.String()

		// The expected digest comes from the *metadata*, not from the live copy.
		//
		// Comparing the restored bytes against the live bytes proves only that the copy is
		// faithful — a corrupted byte in both sides passes. What matters is whether the
		// restored bytes still hash to what the metadata recorded when the file was written,
		// which is the same check a downloader's client performs.
		wantSum, hasHash := blobHashFor(ctx, restoredMeta, f.BlobID)
		var wantSize int64 = f.Size

		got, gerr := restoredContent.Open(ctx, layout.Node{Owner: owner, Path: rel})
		if gerr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s: the restored copy cannot serve it: %v", rel, gerr))
			continue
		}
		gotSum, gotSize, gerr2 := digestOf(got)
		got.Close()
		if gerr2 != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("%s: read restored: %v", rel, gerr2))
			continue
		}

		if gotSize != wantSize {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s: restored size is %d B, metadata says %d B", rel, gotSize, wantSize))
			continue
		}
		if hasHash && gotSum != wantSum {
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s: restored bytes hash to %s, metadata recorded %s — the backup is corrupt",
				rel, shortHash(gotSum), shortHash(wantSum)))
			continue
		}
		if !hasHash {
			// No recorded hash means the content address is unavailable, so the strongest
			// available check is the size comparison above. Saying so is better than reporting
			// a byte-level verification that did not happen.
			res.Problems = append(res.Problems, fmt.Sprintf(
				"%s: no recorded content hash; only the size could be checked", rel))
			continue
		}
		res.Verified++
		res.Bytes += wantSize
	}

	return res
}

// allFileNodes returns every live file node across all users' roots.
func allFileNodes(ctx context.Context, meta store.Store) ([]store.Node, bool, error) {
	roots, err := meta.AllRoots(ctx)
	if err != nil {
		return nil, false, err
	}

	complete := true
	var out []store.Node
	for _, root := range roots {
		var ()
		nodes, ok, derr := descendantsOf(ctx, meta, root.ID)
		if derr != nil {
			return nil, false, derr
		}
		if !ok {
			complete = false
		}
		for _, n := range nodes {
			if n.IsDir() || n.TrashedAt != nil {
				continue
			}
			out = append(out, n)
		}
	}
	return out, complete, nil
}

// descendantsOf runs one subtree walk inside a transaction.
func descendantsOf(ctx context.Context, meta store.Store, rootID uuid.UUID) ([]store.Node, bool, error) {
	var (
		nodes []store.Node
		ok    bool
	)
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, complete, werr := tx.Descendants(ctx, rootID, 100_000)
		nodes, ok = got, complete
		return werr
	})
	return nodes, ok, err
}

// pathOf rebuilds a node's slash-separated path from its owner's root.
func pathOf(ctx context.Context, meta store.Store, node store.Node) (string, error) {
	var parts []string
	cur := node
	for i := 0; i < 64; i++ {
		if cur.ParentID == nil {
			break
		}
		parts = append(parts, cur.Name)
		var parent store.Node
		if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			p, perr := tx.Node(ctx, *cur.ParentID)
			parent = p
			return perr
		}); err != nil {
			return "", err
		}
		cur = parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	out := ""
	for _, p := range parts {
		if out != "" {
			out += "/"
		}
		out += p
	}
	return out, nil
}

// digestOf reads a stream to the end and returns its digest and length.
func digestOf(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// shortHash truncates a hex digest for messages.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// resolverFor adapts the store's resolver to the blobstore's, reusing the adapter main.go
// already defines so the two cannot drift.
func resolverFor(meta store.Store) blobstore.NodeResolver {
	return &resolverAdapter{r: meta.Resolver()}
}

// copyTree copies a directory tree.
//
// <root>/tmp is skipped: it is the spool directory for in-progress uploads, the real backup
// scripts exclude it for the same reason, and copying a partial upload would put bytes in the
// "backup" that no metadata can reference.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if rel == "tmp" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

// copyFile copies one file's bytes.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, cerr := io.Copy(out, in); cerr != nil {
		out.Close()
		return cerr
	}
	return out.Close()
}

// blobHashFor returns the sha256 recorded for a blob, hex encoded.
//
// It reads the content address from the metadata rather than hashing the live file: the
// question the drill answers is "do the restored bytes still match what was written", and
// hashing the live copy would only prove the two copies agree — including when both are wrong.
//
// The second return is false when the blob row carries no usable hash, so the caller can say
// so instead of claiming a byte-level check it did not perform.
func blobHashFor(ctx context.Context, meta store.Store, blobID *uuid.UUID) (string, bool) {
	if blobID == nil {
		return "", false
	}
	var sum []byte
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, herr := tx.BlobHash(ctx, *blobID)
		sum = got
		return herr
	})
	if err != nil || len(sum) != sha256.Size {
		return "", false
	}
	return hex.EncodeToString(sum), true
}
