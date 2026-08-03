package s3

// Contract tests for the object-store backend.
//
// They run against an in-process fake by default and against a real endpoint when
// TEST_S3_ENDPOINT is set. The earlier note here claimed a fake "would only test this package
// against my own assumptions about HTTP" — true of a careless fake, but the alternative was a
// test that skipped everywhere it mattered, which is worse than an imperfect test that runs.
// A real endpoint still catches protocol drift the fake cannot.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "newcloud-test"
	}

	var accessKey, secretKey string
	secure := false

	if endpoint == "" {
		// No live endpoint: run against the in-process fake, so these tests are evidence
		// rather than a skip.
		fake := s3fake.New(bucket)
		t.Cleanup(fake.Close)
		endpoint = fake.Endpoint()
		accessKey, secretKey = "fake-access", "fake-secret"
	} else {
		accessKey = os.Getenv("TEST_S3_ACCESS_KEY")
		secretKey = os.Getenv("TEST_S3_SECRET_KEY")
		secure = os.Getenv("TEST_S3_SECURE") == "1"
	}

	// A per-test prefix keeps parallel runs and leftovers from colliding without having to
	// create a bucket per test.
	prefix := "t/" + randHex(8)

	s, err := New(context.Background(), Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Secure:    secure,
		Prefix:    prefix,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return s
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	const hexes = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, b := range buf {
		out[i*2] = hexes[b>>4]
		out[i*2+1] = hexes[b&0x0f]
	}
	return string(out)
}

func hashOf(b []byte) blob.Hash {
	return sha256.Sum256(b)
}

func TestRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	payload := []byte("the quick brown fox")
	h := hashOf(payload)

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := s.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", info.Size, len(payload))
	}

	rc, err := s.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content = %q, want %q", got, payload)
	}
}

// TestOpenSeeks is the property the HTTP layer depends on: serving a Range must not stream the
// whole object, and must return the right bytes.
func TestOpenSeeks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	h := hashOf(payload)
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if _, err := rc.Seek(5000, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if !bytes.Equal(buf, payload[5000:5032]) {
		t.Fatal("seeking returned the wrong bytes")
	}

	// Seeking backwards must also work: http.ServeContent does it when it re-reads a header.
	if _, err := rc.Seek(10, io.SeekStart); err != nil {
		t.Fatalf("backwards Seek: %v", err)
	}
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("read after backwards seek: %v", err)
	}
	if !bytes.Equal(buf, payload[10:42]) {
		t.Fatal("backwards seek returned the wrong bytes")
	}
}

// TestPutRejectsMismatchedHash is the integrity test: a payload that does not hash to its
// claimed address must be refused, and must leave nothing behind.
func TestPutRejectsMismatchedHash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	payload := []byte("honest bytes")
	wrong := hashOf([]byte("something else entirely"))

	err := s.Put(ctx, wrong, bytes.NewReader(payload), int64(len(payload)))
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}

	if _, err := s.Stat(ctx, wrong); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a rejected put must not store anything; Stat err = %v", err)
	}
}

// TestPutIsIdempotent: the content address is the identity, so storing it twice is a no-op
// rather than corruption.
func TestPutIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	payload := []byte("same content")
	h := hashOf(payload)

	for i := 0; i < 2; i++ {
		if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatalf("Put #%d: %v", i+1, err)
		}
	}

	rc, err := s.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content changed after a repeated put: %q", got)
	}
}

// TestPutRejectsSizeMismatch: a truncated upload must not be published under the full hash.
func TestPutRejectsSizeMismatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	payload := []byte("0123456789")
	h := hashOf(payload)

	err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload)+5))
	if err == nil {
		t.Fatal("a size mismatch must fail")
	}
}

// TestDeleteAndMissing covers the not-found semantics the interface distinguishes.
func TestDeleteAndMissing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Stat(ctx, hashOf([]byte("never stored"))); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Stat on a missing blob = %v, want ErrNotFound", err)
	}
	if _, err := s.Open(ctx, hashOf([]byte("never stored"))); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open on a missing blob = %v, want ErrNotFound", err)
	}

	payload := []byte("temporary")
	h := hashOf(payload)
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, h); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Stat after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, h); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

// TestLargeBlobUploads: anything above 16 MiB makes minio-go switch to a managed multipart
// upload, which is a different code path inside the library and worth exercising once.
func TestLargeBlobUploads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the large-object test in -short mode")
	}
	s := testStore(t)
	ctx := context.Background()

	payload := make([]byte, 17<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	h := hashOf(payload)
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if _, err := rc.Seek(int64(len(payload))-1024, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail := make([]byte, 1024)
	if _, err := io.ReadFull(rc, tail); err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !bytes.Equal(tail, payload[len(payload)-1024:]) {
		t.Fatal("tail of a multipart object does not match")
	}
}
