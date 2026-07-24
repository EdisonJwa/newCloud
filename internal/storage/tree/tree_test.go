package tree

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func node(owner, path string, isDir bool) layout.Node {
	return layout.Node{Owner: owner, Path: path, IsDir: isDir}
}

func put(t *testing.T, s *Store, n layout.Node, content string) layout.PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), n, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("put %s: %v", n.Path, err)
	}
	return res
}

func read(t *testing.T, s *Store, n layout.Node) string {
	t.Helper()
	rc, err := s.Open(context.Background(), n)
	if err != nil {
		t.Fatalf("open %s: %v", n.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDiskLayoutMatchesUserView is the whole point of this layout: an
// administrator can find a user's file with ls.
func TestDiskLayoutMatchesUserView(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	put(t, s, node("alice", "docs/report.txt", false), "hello")

	want := filepath.Join(dir, "alice", "docs", "report.txt")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("file is not where the tree layout promises: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content %q", got)
	}
}

func TestPutOpenRoundTrip(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "a/b/c.txt", false), "payload")
	if got := read(t, s, node("alice", "a/b/c.txt", false)); got != "payload" {
		t.Fatalf("content %q", got)
	}
}

func TestOpenIsSeekable(t *testing.T) {
	s := newStore(t)
	// 10000 bytes: enough that seeking to 5000 stays inside the file.
	put(t, s, node("alice", "big.txt", false), strings.Repeat("0123456789", 1000))

	rc, err := s.Open(context.Background(), node("alice", "big.txt", false))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if _, err := rc.Seek(5000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "0123" {
		t.Fatalf("seek landed wrong: %q", buf)
	}
}

// TestNoDeduplication documents the intended trade-off: identical content is
// stored twice, which is exactly what makes the layout inspectable and simple.
func TestNoDeduplication(t *testing.T) {
	s := newStore(t)
	r1 := put(t, s, node("alice", "one.txt", false), "same content")
	r2 := put(t, s, node("alice", "two.txt", false), "same content")

	if r1.Deduplicated || r2.Deduplicated {
		t.Fatal("tree layout must never report deduplication")
	}
	// The hash still matches: the content address is computed even though it is
	// not used for storage, because the metadata layer wants an etag.
	if r1.Hash != r2.Hash {
		t.Fatal("same content should hash identically")
	}
}

func TestReplacedExistingIsReported(t *testing.T) {
	s := newStore(t)
	r1 := put(t, s, node("alice", "f.txt", false), "v1")
	if r1.ReplacedExisting {
		t.Fatal("first write cannot replace anything")
	}
	r2 := put(t, s, node("alice", "f.txt", false), "v2 much longer")
	if !r2.ReplacedExisting {
		t.Fatal("second write must report replacement")
	}
	if got := read(t, s, node("alice", "f.txt", false)); got != "v2 much longer" {
		t.Fatalf("content after overwrite: %q", got)
	}
}

// TestInterruptedUploadKeepsOldContent is the reason writes are staged: a
// cancelled upload must leave the previous version intact, not a truncated file.
func TestInterruptedUploadKeepsOldContent(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "f.txt", false), "original")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Put(ctx, node("alice", "f.txt", false), strings.NewReader("replacement"), 11)
	if err == nil {
		t.Fatal("cancelled upload must fail")
	}

	if got := read(t, s, node("alice", "f.txt", false)); got != "original" {
		t.Fatalf("old content must survive a failed upload, got %q", got)
	}
}

func TestSizeMismatchRejected(t *testing.T) {
	s := newStore(t)
	_, err := s.Put(context.Background(), node("alice", "f.txt", false), strings.NewReader("abc"), 999)
	if !errors.Is(err, layout.ErrSizeMismatch) {
		t.Fatalf("want ErrSizeMismatch, got %v", err)
	}
	if _, err := s.Stat(context.Background(), node("alice", "f.txt", false)); !errors.Is(err, layout.ErrNotFound) {
		t.Fatal("a rejected upload must not be visible")
	}
}

// TestPathTraversalBlocked is the core security test for this layout. Both
// layers are exercised: segment validation rejects the input, and os.Root would
// refuse it even if validation were bypassed.
func TestPathTraversalBlocked(t *testing.T) {
	s := newStore(t)

	bad := []layout.Node{
		{Owner: "..", Path: "etc/passwd"},
		{Owner: ".", Path: "x"},
		{Owner: "alice", Path: "../etc/passwd"},
		{Owner: "alice", Path: "a/../../etc/passwd"},
		{Owner: "alice", Path: "/etc/passwd"},
		{Owner: "alice", Path: "a//b"},
		{Owner: "alice", Path: "a\x00b"},
		{Owner: "alice", Path: "a\\b"},
		{Owner: "", Path: "x"},
		{Owner: ".hidden", Path: "x"}, // would collide with internal dirs
	}
	for _, n := range bad {
		if _, err := s.Put(context.Background(), n, strings.NewReader("x"), 1); err == nil {
			t.Fatalf("owner=%q path=%q must be rejected", n.Owner, n.Path)
		}
	}
}

// TestSymlinkEscapeBlocked proves the syscall-level containment: even a symlink
// planted inside the store cannot be followed outside it.
func TestSymlinkEscapeBlocked(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	// A file the attacker wants to read.
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := os.MkdirAll(filepath.Join(dir, "alice"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alice", "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	rc, err := s.Open(context.Background(), node("alice", "link.txt", false))
	if err == nil {
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		if string(b) == "classified" {
			t.Fatal("symlink escaped the store root")
		}
	}
}

func TestDeleteRootRefused(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "keep.txt", false), "x")

	if err := s.Delete(context.Background(), node("alice", "", true)); err == nil {
		t.Fatal("deleting a store root must be refused")
	}
	if got := read(t, s, node("alice", "keep.txt", false)); got != "x" {
		t.Fatal("file should still exist")
	}
}

func TestDeleteFileAndDirectory(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "d/a.txt", false), "a")
	put(t, s, node("alice", "d/sub/b.txt", false), "b")

	if err := s.Delete(context.Background(), node("alice", "d", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(context.Background(), node("alice", "d/a.txt", false)); !errors.Is(err, layout.ErrNotFound) {
		t.Fatal("directory contents should be gone")
	}
}

func TestListAndLimit(t *testing.T) {
	s := newStore(t)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		put(t, s, node("alice", "d/"+name, false), "x")
	}
	if err := s.Mkdir(context.Background(), node("alice", "d/sub", true)); err != nil {
		t.Fatal(err)
	}

	all, err := s.List(context.Background(), node("alice", "d", true), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 entries, got %d", len(all))
	}
	// Sorted by path, stable across filesystems.
	for i := 1; i < len(all); i++ {
		if all[i-1].Path >= all[i].Path {
			t.Fatalf("list not sorted: %v", all)
		}
	}

	limited, err := s.List(context.Background(), node("alice", "d", true), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit ignored: got %d entries", len(limited))
	}
}

func TestMoveFile(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "a.txt", false), "content")

	if err := s.Move(context.Background(), node("alice", "a.txt", false), node("alice", "dir/b.txt", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(context.Background(), node("alice", "a.txt", false)); !errors.Is(err, layout.ErrNotFound) {
		t.Fatal("source should be gone")
	}
	if got := read(t, s, node("alice", "dir/b.txt", false)); got != "content" {
		t.Fatalf("content after move: %q", got)
	}
}

func TestMoveDirectoryIntoItselfRefused(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "d/x.txt", false), "x")

	err := s.Move(context.Background(), node("alice", "d", true), node("alice", "d/sub", true))
	if err == nil {
		t.Fatal("moving a directory into its own subtree must be refused")
	}
}

func TestCrossOwnerMoveRefused(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "a.txt", false), "x")

	err := s.Move(context.Background(), node("alice", "a.txt", false), node("bob", "a.txt", false))
	if err == nil {
		t.Fatal("cross-owner move must be a copy+delete, not a rename")
	}
}

// TestOwnerIsolation: one user's files must never be reachable via another
// user's namespace.
func TestOwnerIsolation(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "secret.txt", false), "alice only")

	if _, err := s.Open(context.Background(), node("bob", "secret.txt", false)); !errors.Is(err, layout.ErrNotFound) {
		t.Fatal("bob must not see alice's file at the same path")
	}
}

func TestWalk(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "d/a.txt", false), "1")
	put(t, s, node("alice", "d/sub/b.txt", false), "22")

	var paths []string
	err := s.Walk(context.Background(), node("alice", "d", true), func(n layout.Node) error {
		paths = append(paths, n.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 { // a.txt, sub, sub/b.txt
		t.Fatalf("want 3 entries, got %v", paths)
	}
}

func TestUsageMatchesContent(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "d/a.txt", false), "12345")
	put(t, s, node("alice", "d/sub/b.txt", false), "1234567890")

	total, err := s.Usage(context.Background(), node("alice", "d", true))
	if err != nil {
		t.Fatal(err)
	}
	if total != 15 {
		t.Fatalf("usage: want 15, got %d", total)
	}
}

func TestCapabilitiesReflectLayout(t *testing.T) {
	s := newStore(t)
	caps := s.Capabilities()
	if caps.Deduplicated {
		t.Fatal("tree layout does not deduplicate")
	}
	if !caps.DirectFSInspect {
		t.Fatal("tree layout is inspectable on disk")
	}
	if caps.EfficientRename {
		t.Fatal("tree layout rename is proportional to subtree size")
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "ok.txt", false), "ok")

	// Force a failure.
	_, _ = s.Put(context.Background(), node("alice", "bad.txt", false), strings.NewReader("x"), 99)

	entries, err := os.ReadDir(filepath.Join(s.dir, tmpDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp area should be empty, found %d entries", len(entries))
	}
}

func TestEmptyFile(t *testing.T) {
	s := newStore(t)
	put(t, s, node("alice", "empty.txt", false), "")
	info, err := s.Stat(context.Background(), node("alice", "empty.txt", false))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 0 {
		t.Fatalf("size %d", info.Size)
	}
}

var _ = bytes.MinRead
