package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func hashOf(b []byte) blob.Hash {
	sum := sha256.Sum256(b)
	var h blob.Hash
	copy(h[:], sum[:])
	return h
}

func TestPutOpenRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	payload := []byte("hello newcloud")
	h := hashOf(payload)

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}

	rc, err := s.Open(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: %q", got)
	}
}

// TestOpenIsSeekable backs the HTTP Range requirement: serving a byte range
// must not require re-reading a 100 GiB object.
func TestOpenIsSeekable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789"), 1000)
	h := hashOf(payload)

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(ctx, h)
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

// TestPutRejectsHashMismatch is the guard against a poisoned dedup index: a
// truncated or corrupted upload must never be stored under a hash it does not
// have, because other users would then read the wrong bytes.
func TestPutRejectsHashMismatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	payload := []byte("original")
	wrong := hashOf([]byte("something else"))

	err := s.Put(ctx, wrong, bytes.NewReader(payload), int64(len(payload)))
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
	if _, err := s.Stat(ctx, wrong); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("a rejected upload must not be visible")
	}
}

func TestPutRejectsSizeMismatch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	payload := []byte("abc")
	h := hashOf(payload)

	if err := s.Put(ctx, h, bytes.NewReader(payload), 999); err == nil {
		t.Fatal("declared size mismatch must fail")
	}
}

func TestPutIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	payload := []byte("dedup me")
	h := hashOf(payload)

	for i := 0; i < 3; i++ {
		if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	info, err := s.Stat(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("size %d", info.Size)
	}
}

func TestStatAndDelete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	payload := []byte("delete me")
	h := hashOf(payload)

	if _, err := s.Stat(ctx, h); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("stat on missing blob must be ErrNotFound")
	}
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, h); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("second delete must be ErrNotFound")
	}
}

// TestNoTempFilesLeftBehind matters for a 40 TiB volume: leaked temp files
// silently consume capacity until the disk fills.
func TestNoTempFilesLeftBehind(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Successful put.
	ok := []byte("ok")
	if err := s.Put(ctx, hashOf(ok), bytes.NewReader(ok), int64(len(ok))); err != nil {
		t.Fatal(err)
	}
	// Failed put (hash mismatch).
	bad := []byte("bad")
	_ = s.Put(ctx, hashOf([]byte("other")), bytes.NewReader(bad), int64(len(bad)))

	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir should be empty, found %d entries", len(entries))
	}
}

func TestShardingLayout(t *testing.T) {
	s := newStore(t)
	h := hashOf([]byte("shard me"))
	hex := h.String()

	want := filepath.Join(s.root, hex[0:2], hex[2:4], hex)
	if got := s.path(h); got != want {
		t.Fatalf("shard path\n got %s\nwant %s", got, want)
	}
}

func TestCheckSameFilesystemPasses(t *testing.T) {
	s := newStore(t)
	if err := s.CheckSameFilesystem(); err != nil {
		t.Fatalf("root and tmp must share a filesystem in the test dir: %v", err)
	}
}

func TestCancelledContextAborts(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	payload := bytes.Repeat([]byte("x"), 1<<20)
	err := s.Put(ctx, hashOf(payload), bytes.NewReader(payload), int64(len(payload)))
	if err == nil {
		t.Fatal("cancelled request must not complete a write")
	}
}
