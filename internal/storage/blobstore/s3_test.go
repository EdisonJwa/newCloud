package blobstore

// The layout conformance suite, run against the object-store backend.
//
// This is the point of the conformance suite existing at all: the same tests that prove local
// disk behaves correctly now prove the S3 backend is interchangeable from the content store's
// point of view. Anything the rest of the server relies on — overwrite semantics, versioning,
// delete, listing — is exercised without a single storage-specific assertion.
//
// The test used to be gated behind TEST_S3_ENDPOINT and therefore *skipped* wherever no MinIO
// happened to be running, which in this repository was everywhere. A skipped test is not
// evidence: "the S3 backend passes the same suite as local disk" was a claim nothing checked.
//
// It now runs against an in-process fake by default. Setting TEST_S3_ENDPOINT still runs it
// against a real endpoint, which is what a release check should do — the fake proves the
// backend's *own* logic; a real server additionally proves protocol compatibility.

import (
	"context"
	"os"
	"testing"

	s3blob "github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/layouttest"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

func TestS3Conformance(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "newcloud-test"
	}

	var (
		accessKey, secretKey string
		secure               bool
	)

	if endpoint == "" {
		// No live endpoint configured: run against the in-process fake.
		fake := s3fake.New(bucket)
		t.Cleanup(fake.Close)
		endpoint = fake.Endpoint()
		accessKey = "fake-access"
		secretKey = "fake-secret"
	} else {
		accessKey = os.Getenv("TEST_S3_ACCESS_KEY")
		secretKey = os.Getenv("TEST_S3_SECRET_KEY")
		secure = os.Getenv("TEST_S3_SECURE") == "1"
	}

	layouttest.Run(t, func(t *testing.T) layout.ContentStore {
		blobs, err := s3blob.New(context.Background(), s3blob.Config{
			Endpoint:  endpoint,
			Bucket:    bucket,
			AccessKey: accessKey,
			SecretKey: secretKey,
			Secure:    secure,
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
