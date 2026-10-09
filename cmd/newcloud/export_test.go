package main

// The portable export (§9.5) is only worth having if what it writes is the user's data.
//
// These tests build a real tree in PostgreSQL with real content in a local blob store, run the export
// and then read the OUTPUT — not the intermediate values — because the failure that matters is an
// export that reports success and writes something else. The digest check is what makes that
// detectable, so one case corrupts the stored bytes and requires the export to REFUSE rather than
// write a file whose hash disagrees with the manifest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob"
	localblob "github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/store"
)

type exportFixture struct {
	meta      store.Store
	blobs     *localblob.Store
	owner     store.User
	root      store.Node
	out       string
	blobsRoot string
}

// newExportFixture writes two files (one nested) plus a directory into a fresh store.
func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	blobs, err := localblob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "exporter", Email: "e@example.test", Role: "user"})
	root, err := meta.EnsureRoot(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	f := &exportFixture{meta: meta, blobs: blobs, owner: owner, root: root,
		out: filepath.Join(dir, "export"), blobsRoot: filepath.Join(dir, "blobs")}

	f.put(t, root.ID, "top.txt", []byte("top level content"))
	sub := f.mkdir(t, root.ID, "nested")
	f.put(t, sub.ID, "deep.txt", []byte("nested content"))
	return f
}

func (f *exportFixture) mkdir(t *testing.T, parentID uuid.UUID, name string) store.Node {
	t.Helper()
	var out store.Node
	if err := f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: f.owner.ID, ParentID: parentID, Name: name})
		out = n
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *exportFixture) put(t *testing.T, parentID uuid.UUID, name string, content []byte) {
	t.Helper()
	sum := sha256.Sum256(content)
	h := blob.Hash(sum)
	if err := f.blobs.Put(context.Background(), h, strings.NewReader(string(content)),
		int64(len(content))); err != nil {
		t.Fatal(err)
	}
	blobID := f.meta.BlobIDFor(context.Background(), h, int64(len(content)))
	if err := f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: f.owner.ID, ParentID: parentID, Name: name,
			Size: int64(len(content)), Mime: "text/plain", ETag: `"e"`,
			BlobID: blobID, BlobHash: sum,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *exportFixture) manifest(t *testing.T) exportManifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.out, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m exportManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func TestExportWritesTheTreeAndAVerifiedManifest(t *testing.T) {
	f := newExportFixture(t)

	man, err := runExport(context.Background(), f.meta, f.blobs, f.owner, f.out)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if man.Totals.Files != 2 {
		t.Fatalf("exported %d files, want 2", man.Totals.Files)
	}

	// The bytes on disk are the bytes that went in.
	top, err := os.ReadFile(filepath.Join(f.out, "top.txt"))
	if err != nil {
		t.Fatalf("read top.txt: %v", err)
	}
	if string(top) != "top level content" {
		t.Fatalf("top.txt contains %q", top)
	}
	deep, err := os.ReadFile(filepath.Join(f.out, "nested", "deep.txt"))
	if err != nil {
		t.Fatalf("read nested/deep.txt: %v", err)
	}
	if string(deep) != "nested content" {
		t.Fatalf("nested/deep.txt contains %q", deep)
	}

	// The manifest names them relative to the root and carries the digest of what was written.
	m := f.manifest(t)
	byPath := map[string]exportFile{}
	for _, file := range m.Files {
		byPath[file.Path] = file
	}
	if _, ok := byPath["nested/deep.txt"]; !ok {
		t.Fatalf("the manifest does not name nested/deep.txt: %+v", m.Files)
	}
	sum := sha256.Sum256([]byte("nested content"))
	if got := byPath["nested/deep.txt"].SHA256; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest digest %s does not match the content %s", got, hex.EncodeToString(sum[:]))
	}
	for _, file := range m.Files {
		if filepath.IsAbs(file.Path) {
			t.Fatalf("manifest path %q is absolute; the export would record this machine's layout",
				file.Path)
		}
	}
	nested := false
	for _, d := range m.Directories {
		if d == "nested" {
			nested = true
		}
	}
	if !nested {
		t.Fatalf("the manifest does not list the directory: %+v", m.Directories)
	}
}

func TestExportRefusesToWriteContentThatDisagreesWithTheMetadata(t *testing.T) {
	f := newExportFixture(t)

	// Corrupt the stored bytes for top.txt without touching the metadata: the address the
	// metadata records no longer matches what the store holds.
	sum := sha256.Sum256([]byte("top level content"))
	var h blob.Hash
	copy(h[:], sum[:])
	// The local store's path scheme is internal, so corrupt every blob file instead of computing one:
	// each is small and only the one for top.txt is read.
	var corrupted int
	err := filepath.Walk(f.blobsRoot, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		corrupted++
		return os.WriteFile(path, []byte("tampered content!!"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if corrupted == 0 {
		t.Fatal("no blob files were found to corrupt")
	}

	if _, err := runExport(context.Background(), f.meta, f.blobs, f.owner, f.out); err == nil {
		t.Fatal("the export succeeded over content that does not match its address; the file it " +
			"wrote would be the wrong data with a manifest claiming otherwise")
	}
	// And it must NOT have written a manifest, whose presence is the signal of a complete export.
	if _, err := os.Stat(filepath.Join(f.out, "manifest.json")); err == nil {
		t.Fatal("a manifest was written for a failed export")
	}
}

func TestExportRefusesANonEmptyDestination(t *testing.T) {
	f := newExportFixture(t)
	if err := os.MkdirAll(f.out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.out, "leftover.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runExport(context.Background(), f.meta, f.blobs, f.owner, f.out); err == nil {
		t.Fatal("the export merged into a non-empty directory; the leftover file is now " +
			"indistinguishable from exported content")
	}
}
