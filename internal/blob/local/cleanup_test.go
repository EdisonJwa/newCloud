package local_test

// §9.3: partial uploads must be reclaimed, for the local layout too.
//
// This backend has no multipart API, but it has the same failure: Put writes to a temp file and
// renames it into place so a reader never sees a half-written object. The rename is atomic; the
// temp file is not. A crash between CreateTemp and Rename leaves a put-* file that nothing lists
// and nothing removes, because the deferred cleanup only runs if the process survives.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
)

// writeTemp drops a file that looks like an interrupted Put.
func writeTemp(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocalAbortsStaleTempFiles(t *testing.T) {
	// New puts the temp directory at <root>/tmp; the blobs themselves live in hash shards beside
	// it, so the sweep cannot mistake an object for a temp file.
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")

	s, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}

	// One stale temp file, one fresh one, and one unrelated file that must survive.
	stale := writeTemp(t, tmp, "put-crashed", 1000)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := writeTemp(t, tmp, "put-inflight", 2000)
	unrelated := writeTemp(t, tmp, "something-else", 10)
	if err := os.Chtimes(unrelated, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := s.AbortStaleUploads(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if res.Aborted != 1 {
		t.Fatalf("aborted %d temp files, want exactly the stale one", res.Aborted)
	}
	if res.Bytes != 1000 {
		t.Fatalf("reported %d bytes reclaimed, want 1000", res.Bytes)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale temp file survived; an interrupted write's temp file is never listed " +
			"as an object, so nothing else will ever remove it")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a temp file younger than the window was removed — that is a write in progress, " +
			"and deleting it corrupts a live upload")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("a file that is not one of our temp files was removed; the sweep must only touch " +
			"its own put-* files")
	}
}

func TestLocalRefusesAZeroWindow(t *testing.T) {
	root := t.TempDir()
	s, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}

	// A zero window would delete temp files belonging to writes happening right now. Refusing is
	// the safe answer, and a caller who forgot to configure a window should hear about it rather
	// than silently destroy live uploads.
	if _, err := s.AbortStaleUploads(context.Background(), 0); err == nil {
		t.Fatal("a zero age window was accepted; it would abort transfers that are in progress")
	}
}

// The interface check is compile-time: a backend that stops satisfying blob.UploadCleaner is a
// build failure rather than a silently disabled sweep.
var _ blob.UploadCleaner = (*local.Store)(nil)

var _ = blob.ErrNotFound
