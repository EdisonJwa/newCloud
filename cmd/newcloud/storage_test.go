package main

// openStorage is where the configuration becomes an actual content store, so it is the one
// place that can prove the S3 wiring rather than just the S3 client. The unit tests around it
// use the same code path the binary takes.

import (
	"context"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	storepkg "github.com/EdisonJwa/newCloud/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestOpenStorageRejectsTreeOnS3: the tree layout is a filesystem mirror by definition, so
// pairing it with an object store has to fail at startup rather than on the first write.
func TestOpenStorageRejectsTreeOnS3(t *testing.T) {
	var cfg config.Config
	cfg.Storage.Backend = "s3"
	cfg.Storage.Layout = config.LayoutTree
	cfg.Storage.Local.Root = t.TempDir()
	cfg.Storage.S3 = config.S3Storage{Endpoint: "minio.example.com", Bucket: "b"}

	if _, err := openStorage(cfg, storepkg.NewMemory(), testLogger()); err == nil {
		t.Fatal("tree layout on the s3 backend must be rejected")
	}
}

// TestOpenStorageBuildsS3 runs the real constructor against an S3-compatible endpoint.
func TestOpenStorageBuildsS3(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "newcloud-test"
	}

	var accessKey, secretKey string
	if endpoint == "" {
		// No live endpoint: use the in-process fake so the wiring is actually exercised.
		fake := s3fake.New(bucket)
		t.Cleanup(fake.Close)
		endpoint = fake.Endpoint()
		accessKey, secretKey = "fake-access", "fake-secret"
	} else {
		accessKey = os.Getenv("TEST_S3_ACCESS_KEY")
		secretKey = os.Getenv("TEST_S3_SECRET_KEY")
	}

	var cfg config.Config
	cfg.Storage.Backend = "s3"
	cfg.Storage.Layout = config.LayoutBlob
	// Uploads stage on local disk even when the bytes go to a bucket, so this is still needed.
	cfg.Storage.Local.Root = t.TempDir()
	// TLS is the default, and the zero value of a *bool leaves it unset, which must mean true.
	// The test server speaks plaintext, so this has to be explicit.
	insecure := false
	cfg.Storage.S3 = config.S3Storage{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Secure:    &insecure,
		Prefix:    "wiring",
	}

	cs, err := openStorage(cfg, storepkg.NewMemory(), testLogger())
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	if cs == nil {
		t.Fatal("openStorage returned no store")
	}

	// A put through the assembled stack proves the adapter, the resolver and the object store
	// are wired to each other and not merely constructed.
	n := layout.Node{Owner: "6f1c1a2e-0000-4000-8000-000000000001", Path: "hello.txt"}
	res, err := cs.Put(context.Background(), n, strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatalf("put through the s3 stack: %v", err)
	}
	if res.Size != 5 {
		t.Fatalf("put reported %d bytes, want 5", res.Size)
	}
	if cs.Mode() != layout.ModeBlob {
		t.Fatalf("mode = %q, want blob", cs.Mode())
	}
	// Reading back goes through the metadata resolver to find the content address, which is
	// the piece this test deliberately does not build: the conformance suite covers it with a
	// resolver that knows the node. Here the claim under test is narrower and explicit — the
	// configured backend produces a working blob-mode store that accepts bytes.
}

// TestOpenStorageRejectsUnknownBackend keeps the error message useful.
func TestOpenStorageRejectsUnknownBackend(t *testing.T) {
	var cfg config.Config
	cfg.Storage.Backend = "smb"
	cfg.Storage.Layout = config.LayoutBlob
	cfg.Storage.Local.Root = t.TempDir()

	if _, err := openStorage(cfg, storepkg.NewMemory(), testLogger()); err == nil {
		t.Fatal("an unknown backend must be rejected")
	}
}
