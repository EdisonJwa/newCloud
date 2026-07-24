// Package layouttest provides a conformance suite that every ContentStore
// implementation must pass.
//
// The point is that the WebDAV and API layers must not care which layout is
// active. If a behaviour differs between blob and tree, it belongs in Caps, not
// in a type switch scattered through the handlers. Running the same suite
// against both keeps that honest.
package layouttest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// Factory builds a fresh, empty store for a test. Tests that need to know the
// layout read it back from Store.Mode().
type Factory func(t *testing.T) layout.ContentStore

func node(owner, p string, isDir bool) layout.Node {
	return layout.Node{Owner: owner, Path: p, IsDir: isDir}
}

func put(t *testing.T, s layout.ContentStore, n layout.Node, content string) layout.PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), n, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("put %s/%s: %v", n.Owner, n.Path, err)
	}
	return res
}

func readAll(t *testing.T, s layout.ContentStore, n layout.Node) string {
	t.Helper()
	rc, err := s.Open(context.Background(), n)
	if err != nil {
		t.Fatalf("open %s/%s: %v", n.Owner, n.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Run executes the conformance suite.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("WriteReadRoundTrip", func(t *testing.T) {
		s := factory(t)
		put(t, s, node("alice", "a/b/c.txt", false), "hello")
		if got := readAll(t, s, node("alice", "a/b/c.txt", false)); got != "hello" {
			t.Fatalf("content %q", got)
		}
	})

	t.Run("SeekableOpen", func(t *testing.T) {
		s := factory(t)
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
	})

	t.Run("OpenMissingIsNotFound", func(t *testing.T) {
		s := factory(t)
		_, err := s.Open(context.Background(), node("alice", "nope.txt", false))
		if !errors.Is(err, layout.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("OverwriteReplacesContent", func(t *testing.T) {
		s := factory(t)
		first := put(t, s, node("alice", "f.txt", false), "v1")
		if first.ReplacedExisting {
			t.Fatal("first write cannot replace")
		}
		second := put(t, s, node("alice", "f.txt", false), "v2 longer")
		if !second.ReplacedExisting {
			t.Fatal("overwrite must be reported")
		}
		if got := readAll(t, s, node("alice", "f.txt", false)); got != "v2 longer" {
			t.Fatalf("content after overwrite: %q", got)
		}
	})

	t.Run("IdenticalContentHashesIdentically", func(t *testing.T) {
		s := factory(t)
		a := put(t, s, node("alice", "a.txt", false), "same")
		b := put(t, s, node("alice", "b.txt", false), "same")
		if a.Hash != b.Hash {
			t.Fatal("content address must depend only on content")
		}
	})

	t.Run("EmptyFile", func(t *testing.T) {
		s := factory(t)
		put(t, s, node("alice", "empty.txt", false), "")
		st, err := s.Stat(context.Background(), node("alice", "empty.txt", false))
		if err != nil {
			t.Fatal(err)
		}
		if st.Size != 0 {
			t.Fatalf("size %d", st.Size)
		}
	})

	t.Run("SizeMismatchRejected", func(t *testing.T) {
		s := factory(t)
		_, err := s.Put(context.Background(), node("alice", "f.txt", false), strings.NewReader("abc"), 99)
		if err == nil {
			t.Fatal("size mismatch must fail")
		}
		if _, err := s.Stat(context.Background(), node("alice", "f.txt", false)); !errors.Is(err, layout.ErrNotFound) {
			t.Fatal("a rejected upload must not be visible")
		}
	})

	t.Run("UnsafePathsRejected", func(t *testing.T) {
		s := factory(t)
		for _, p := range []string{"../etc/passwd", "/abs/path", "a\x00b", "a\\b"} {
			if _, err := s.Put(context.Background(), node("alice", p, false), strings.NewReader("x"), 1); err == nil {
				t.Fatalf("path %q must be rejected", p)
			}
		}
	})

	t.Run("EmptyOwnerRejected", func(t *testing.T) {
		s := factory(t)
		if _, err := s.Put(context.Background(), node("", "a.txt", false), strings.NewReader("x"), 1); err == nil {
			t.Fatal("empty owner must be rejected")
		}
	})

	t.Run("PutDirectoryRejected", func(t *testing.T) {
		s := factory(t)
		if _, err := s.Put(context.Background(), node("alice", "d", true), strings.NewReader("x"), 1); err == nil {
			t.Fatal("Put on a directory must be rejected")
		}
	})

	t.Run("MkdirAndListDirectChildren", func(t *testing.T) {
		s := factory(t)
		if err := s.Mkdir(context.Background(), node("alice", "d", true)); err != nil {
			t.Fatal(err)
		}
		put(t, s, node("alice", "d/a.txt", false), "a")
		put(t, s, node("alice", "d/b.txt", false), "b")
		if err := s.Mkdir(context.Background(), node("alice", "d/sub", true)); err != nil {
			t.Fatal(err)
		}
		put(t, s, node("alice", "d/sub/deep.txt", false), "deep")

		children, err := s.List(context.Background(), node("alice", "d", true), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 3 {
			t.Fatalf("want 3 direct children, got %d: %v", len(children), children)
		}
		for _, c := range children {
			if strings.HasSuffix(c.Path, "deep.txt") {
				t.Fatal("List must not recurse")
			}
		}
	})

	t.Run("ListLimit", func(t *testing.T) {
		s := factory(t)
		if err := s.Mkdir(context.Background(), node("alice", "d", true)); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"a", "b", "c", "d"} {
			put(t, s, node("alice", "d/"+n+".txt", false), n)
		}
		children, err := s.List(context.Background(), node("alice", "d", true), 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 2 {
			t.Fatalf("limit ignored: got %d", len(children))
		}
	})

	t.Run("MovePreservesContent", func(t *testing.T) {
		s := factory(t)
		put(t, s, node("alice", "a.txt", false), "content")
		if err := s.Move(context.Background(), node("alice", "a.txt", false), node("alice", "dir/b.txt", false)); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, s, node("alice", "dir/b.txt", false)); got != "content" {
			t.Fatalf("content after move: %q", got)
		}
		// Open and discard immediately: on Windows an unreleased handle makes
		// the temp directory un-removable and turns a pass into a confusing
		// cleanup failure.
		rc, err := s.Open(context.Background(), node("alice", "a.txt", false))
		if err == nil {
			rc.Close()
			t.Fatal("source should be gone")
		}
		if !errors.Is(err, layout.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("CrossOwnerMoveRefused", func(t *testing.T) {
		s := factory(t)
		put(t, s, node("alice", "a.txt", false), "x")
		err := s.Move(context.Background(), node("alice", "a.txt", false), node("bob", "a.txt", false))
		if err == nil {
			t.Fatal("cross-owner move must be refused")
		}
	})

	// The two layouts legitimately differ here, so assert the documented
	// behaviour rather than a shared one.
	t.Run("DeleteSemantics", func(t *testing.T) {
		s := factory(t)
		put(t, s, node("alice", "a.txt", false), "x")
		if err := s.Delete(context.Background(), node("alice", "a.txt", false)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.Open(context.Background(), node("alice", "a.txt", false)); !errors.Is(err, layout.ErrNotFound) {
			t.Fatal("deleted file must be gone")
		}
	})

	t.Run("OwnerIsolation", func(t *testing.T) {
		s := factory(t)
		put(t, s, node("alice", "secret.txt", false), "alice only")
		if _, err := s.Open(context.Background(), node("bob", "secret.txt", false)); !errors.Is(err, layout.ErrNotFound) {
			t.Fatal("one user must not see another's file at the same path")
		}
	})

	t.Run("CapsMatchMode", func(t *testing.T) {
		s := factory(t)
		caps := s.Capabilities()
		switch s.Mode() {
		case layout.ModeBlob:
			if !caps.Deduplicated || caps.DirectFSInspect {
				t.Fatalf("blob caps wrong: %+v", caps)
			}
		case layout.ModeTree:
			if caps.Deduplicated || !caps.DirectFSInspect {
				t.Fatalf("tree caps wrong: %+v", caps)
			}
		default:
			t.Fatalf("unknown mode %q", s.Mode())
		}
	})
}
