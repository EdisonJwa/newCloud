package s3_test

// §9.3: 清理未完成 Multipart Upload；AWS 明确建议对残留分块设置生命周期规则，否则可能持续产生费用.
//
// An interrupted multipart upload leaves parts that are not an object: ListObjects cannot see them,
// a HEAD cannot see them, and the usage the server accounts for does not include them. That is
// precisely why they persist — nothing the server already does will ever notice them.

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

// newCleanupStore wires a store to a fresh fake.
func newCleanupStore(t *testing.T, bucket string) (*s3.Store, *s3fake.Server) {
	t.Helper()
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)
	store, err := s3.New(context.Background(), s3.Config{
		Endpoint:  fake.Endpoint(),
		Bucket:    bucket,
		AccessKey: "fake-access",
		SecretKey: "fake-secret",
		Secure:    false,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return store, fake
}

func TestS3AbortsStaleMultipartUploads(t *testing.T) {
	store, fake := newCleanupStore(t, "stale")
	ctx := context.Background()
	// An upload that was started and abandoned. StartMultipartForTest stages it the way an
	// interrupted Put would, without completing it.
	const key = "ab/cd/the-object-left-behind"
	id := fake.StartMultipartForTest("stale", key)
	if fake.PendingMultipart() != 1 {
		t.Fatalf("setup: %d pending uploads, want 1", fake.PendingMultipart())
	}

	// A zero-window pass would abort a transfer in progress, so the window is enforced and the
	// caller must supply one.
	if _, err := store.AbortStaleUploads(ctx, 0); err == nil {
		t.Fatal("a zero age window was accepted; it would abort transfers that are in progress")
	}

	// A JUST-STARTED upload must survive a one-hour window. The age is set explicitly rather than
	// left to elapsed wall time: the first version of this case used a one-nanosecond window and
	// was a coin flip on Windows, where the clock granularity is about a millisecond — the upload's
	// timestamp and the cutoff can land on the same tick, so a genuinely fresh upload reads as
	// fresh and the assertion about a STALE one never gets a fair run.
	fake.AgeMultipartForTest(id, 0)
	res, err := store.AbortStaleUploads(ctx, time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if res.Aborted != 0 {
		t.Fatalf("aborted %d just-started uploads; a transfer in progress must not be touched",
			res.Aborted)
	}
	if fake.PendingMultipart() != 1 {
		t.Fatal("a fresh upload was removed by a sweep with a one-hour window")
	}

	// The same pass, one backdated upload: it must go.
	fake.AgeMultipartForTest(id, 2*time.Hour)
	res, err = store.AbortStaleUploads(ctx, time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if res.Aborted != 1 {
		t.Fatalf("aborted %d uploads, want 1 — the abandoned parts are billable and invisible to "+
			"every listing, so this pass is the only thing that can remove them", res.Aborted)
	}
	if fake.PendingMultipart() != 0 {
		t.Fatal("the stale upload survived the sweep")
	}
}

func TestS3MultipartCleanupLeavesCompletedObjectsAlone(t *testing.T) {
	store, fake := newCleanupStore(t, "objects")
	ctx := context.Background()

	// A real, completed object. The hash must be the actual SHA-256 of the bytes: Put verifies it,
	// which is why the first version of this test failed at Put rather than at the assertion.
	const payload = "live bytes"
	var h blob.Hash
	h = sha256.Sum256([]byte(payload))
	if err := store.Put(ctx, h, strings.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := store.AbortStaleUploads(ctx, time.Nanosecond); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// The sweep must not have touched the object: it reconciles PARTIAL uploads, and a pass that
	// also removed objects would be a data-loss bug wearing a maintenance label.
	if _, err := store.Stat(ctx, h); err != nil {
		t.Fatalf("the completed object is gone after a multipart sweep: %v", err)
	}
	if fake.PendingMultipart() != 0 {
		t.Fatal("a completed object was somehow left as a pending upload")
	}
}
