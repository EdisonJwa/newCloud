package blobstore

// The layout conformance suite, run against the object-store backend.
//
// This is the point of the conformance suite existing at all: the same tests that prove local
// disk behaves correctly now prove the S3 backend is interchangeable from the content store's
// point of view. Anything the rest of the server relies on — overwrite semantics, versioning,
// delete, listing — is exercised without a single storage-specific assertion.

import (
	"context"
	"os"
	"testing"

	s3blob "github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/layouttest"
)

func TestS3Conformance(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set; skipping the S3 conformance run")
	}

	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "newcloud-test"
	}

	layouttest.Run(t, func(t *testing.T) layout.ContentStore {
		blobs, err := s3blob.New(context.Background(), s3blob.Config{
			Endpoint:  endpoint,
			Bucket:    bucket,
			AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("TEST_S3_SECRET_KEY"),
			Secure:    os.Getenv("TEST_S3_SECURE") == "1",
			// A per-test prefix keeps concurrent runs and leftovers apart without needing a
			// bucket per test.
			Prefix: "conformance/" + t.Name(),
		})
		if err != nil {
			t.Fatalf("s3: %v", err)
		}

		s, err := New(blobs, newResolver())
		if err != nil {
			t.Fatalf("blobstore: %v", err)
		}
		return s.WithSpoolDir(t.TempDir())
	})
}
