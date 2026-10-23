package s3

// Regression tests for the two object-store integrity defects an independent re-review reproduced
// on 2026-10-23 (NC-RR-003 and NC-RR-004).
//
// Both are about the difference between "something at this address" and "these exact bytes at this
// address". verify_test.go proves that a corrupted PUT is caught and removed, which is one failure
// class; it never asked what happens when the READBACK ITSELF fails, and it never asked whether a
// deduplication hit is verified at all.
//
// Every test here asserts the END STATE OF THE BUCKET, not a returned error alone. An assertion on
// the error would have passed against NC-RR-003 while the valid object had already been deleted.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

// revokedStore builds a verifying store over a fresh fake and hands back the fake and bucket too,
// so a test can inspect what the store actually holds rather than only what it returned.
func revokedStore(t *testing.T) (*Store, *s3fake.Server, string) {
	t.Helper()
	const bucket = "recheck-bucket"
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)

	s, err := New(context.Background(), Config{
		Endpoint:       fake.Endpoint(),
		Bucket:         bucket,
		AccessKey:      "fake-access",
		SecretKey:      "fake-secret",
		VerifyAfterPut: true,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return s, fake, bucket
}

func verifyHashOf(b []byte) blob.Hash { return sha256.Sum256(b) }

// TestAnInconclusiveReadbackDoesNotDeleteAValidObject is NC-RR-003, the reproduced defect.
//
// The sequence the review executed: a successful PUT (or a deduplication hit), then a persistent GET
// failure. Verification could not decide anything, and the old code reported that as corruption and
// removed the object, so the bucket ended up with zero objects where it should have had one.
//
// A transient readback failure is an absence of evidence. It must be reported — the caller cannot be
// told the content is verified — and it must not destroy anything.
func TestAnInconclusiveReadbackDoesNotDeleteAValidObject(t *testing.T) {
	s, fake, bucket := revokedStore(t)
	ctx := context.Background()

	payload := []byte("bytes that were written successfully")
	h := verifyHashOf(payload)

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("the first put must succeed: %v", err)
	}
	if got := fake.ObjectCount(bucket); got != 1 {
		t.Fatalf("object count after put = %d, want 1", got)
	}

	// Every subsequent GET fails with a 500, persistently: minio-go retries 5xx internally, so the
	// one-shot injection would be satisfied by the first attempt and then the retry would succeed.
	fake.FailAlwaysOn("GET")
	t.Cleanup(fake.ClearFailures)

	// Re-put the same bytes: the object exists, so this is the deduplication path and its readback
	// is the one that fails.
	err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload)))
	if err == nil {
		t.Fatal("an inconclusive readback must be reported; the caller must not be told the " +
			"content is verified")
	}

	// The decisive assertion: the valid object survives.
	if got := fake.ObjectCount(bucket); got != 1 {
		t.Fatalf("object count = %d after an inconclusive readback, want 1. A transient GET "+
			"failure deleted a successfully written valid object: an unverifiable object is not "+
			"a corrupt one.", got)
	}
	stored, ok := fake.GetDirect(bucket, s.key(h))
	if !ok {
		t.Fatal("the object is gone from the bucket")
	}
	if !bytes.Equal(stored, payload) {
		t.Fatal("the surviving object's bytes changed")
	}
}

// TestAPostPutReadbackFailureLeavesTheObjectInPlace is the same property on the write path.
//
// The deduplication path above is one way to reach verification; a fresh upload that then fails its
// readback is the other, and it is the one the review's probe actually exercised. Both must behave
// the same way, which is why both are pinned.
func TestAPostPutReadbackFailureLeavesTheObjectInPlace(t *testing.T) {
	s, fake, bucket := revokedStore(t)
	ctx := context.Background()

	payload := []byte("fresh upload, unreadable afterwards")
	h := verifyHashOf(payload)

	// Let the PUT land, then make only the readback fail.
	fake.FailAlwaysOn("GET")
	t.Cleanup(fake.ClearFailures)

	err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload)))
	if err == nil {
		t.Fatal("a put whose readback failed must not report success")
	}
	// The failure must be classified as inconclusive, which is what stops the destructive branch.
	if !errors.Is(err, ErrVerifyUnavailable) {
		t.Fatalf("a failed readback must report ErrVerifyUnavailable, got %v", err)
	}

	if got := fake.ObjectCount(bucket); got != 1 {
		t.Fatalf("object count = %d, want 1: the freshly written object was deleted because "+
			"its readback could not be performed", got)
	}
}

// TestAConfirmedMismatchStillRemovesTheObject holds the line in the other direction.
//
// The fix must not turn "never delete" into the new rule. A readback that PROVES the bytes are wrong
// has to remove the object, because leaving it there poisons every later read of that address and no
// re-upload can repair it: the idempotent path would find an object and keep accepting it. This is
// the WC-GAP-003 behaviour, and it must survive the NC-RR-003 fix.
func TestAConfirmedMismatchStillRemovesTheObject(t *testing.T) {
	s, fake, bucket := revokedStore(t)
	ctx := context.Background()

	payload := []byte("the bytes the client sent")
	h := verifyHashOf(payload)

	// Store different bytes under the same address, so the readback can compare and find them
	// wrong. CorruptNextPut makes the fake do this while still answering 200, which is exactly the
	// "acknowledged but wrong" case a content-addressed store cannot detect on its own.
	fake.CorruptNextPut()

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Fatal("a put whose stored bytes do not match the address must fail")
	}

	if got := fake.ObjectCount(bucket); got != 0 {
		t.Fatalf("object count = %d after a CONFIRMED mismatch, want 0. A proven-corrupt object "+
			"must not be left at its address: every later read would serve it and no re-upload "+
			"could repair it.", got)
	}

	// And the address is reusable: the retry uploads correct bytes and verifies.
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("the address was permanently poisoned: %v", err)
	}
	stored, ok := fake.GetDirect(bucket, s.key(h))
	if !ok || !bytes.Equal(stored, payload) {
		t.Fatal("the corrected re-upload did not land the right bytes")
	}
}

// TestACleanupDoesNotDeleteAnotherWritersObject is the audit's PRG-006 requirement: establish
// ownership before destructive cleanup.
//
// A hash key can be rewritten by another deduplicating writer at any moment. The dangerous ordering
// is: this call verifies and finds its own object corrupt, then another writer replaces the object
// with a CORRECT copy, then this call deletes unconditionally — destroying valid content in the
// middle of somebody else's upload.
//
// The guard is the ETag the store returned from PutObject. If the stored ETag no longer matches,
// this call does not own the key and must leave it alone.
func TestACleanupDoesNotDeleteAnotherWritersObject(t *testing.T) {
	ctx := context.Background()
	const bucket = "ownership-test"
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)

	s, err := New(ctx, Config{
		Endpoint: fake.Endpoint(), Bucket: bucket,
		AccessKey: "fake-access", SecretKey: "fake-secret",
		VerifyAfterPut: false, // verification is out of scope: this is about the removal decision
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("the bytes the address was computed from")
	h := verifyHashOf(payload)

	// This writer stores a corrupt object and records the ETag the store reported.
	fake.CorruptNextPut()
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("put: %v", err)
	}
	ours, ok := fake.Object(bucket, s.key(h))
	if !ok {
		t.Fatal("the corrupt object was not stored, so the fixture proves nothing")
	}

	// Another writer replaces it with the CORRECT bytes for the same address.
	fake.PutDirect(bucket, s.key(h), payload)
	replaced, _ := fake.Object(bucket, s.key(h))
	if replaced.ETag == ours.ETag {
		t.Fatal("the fixture did not actually change the object; the ETags are identical")
	}

	// The guarded removal must refuse: this call no longer owns the key.
	if err := s.removeIfStillOurs(ctx, h, ours.ETag); !errors.Is(err, errObjectReplaced) {
		t.Fatalf("removeIfStillOurs = %v, want errObjectReplaced", err)
	}
	stored, still := fake.GetDirect(bucket, s.key(h))
	if !still {
		t.Fatal("another writer's object was deleted by a cleanup that did not own it (PRG-006)")
	}
	if !bytes.Equal(stored, payload) {
		t.Fatal("the surviving object's bytes changed")
	}

	// And when the ETag still matches, cleanup does happen — otherwise a proven-corrupt object would
	// be left at its address forever.
	if err := s.removeIfStillOurs(ctx, h, replaced.ETag); err != nil {
		t.Fatalf("removeIfStillOurs with a matching ETag: %v", err)
	}
	if got := fake.ObjectCount(bucket); got != 0 {
		t.Fatalf("object count = %d, want 0 after a removal this call did own", got)
	}
}

// TestAMissingETagStillCleansUp pins the degraded branch: some S3-compatible servers omit the ETag
// from a PUT response. Refusing to clean up would leave a proven-corrupt object at its address for
// every future reader, which is worse than the narrower ownership guarantee.
func TestAMissingETagStillCleansUp(t *testing.T) {
	ctx := context.Background()
	const bucket = "noetag-test"
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)

	s, err := New(ctx, Config{
		Endpoint: fake.Endpoint(), Bucket: bucket,
		AccessKey: "fake-access", SecretKey: "fake-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("no etag from the store")
	h := verifyHashOf(payload)
	// Length-preserving corruption, so the fixture does not accidentally test a size check.
	corrupt := append([]byte(nil), payload...)
	corrupt[len(corrupt)/2] ^= 0xFF
	fake.PutDirect(bucket, s.key(h), corrupt)

	if err := s.removeIfStillOurs(ctx, h, ""); err != nil {
		t.Fatalf("removeIfStillOurs with no ETag: %v", err)
	}
	if got := fake.ObjectCount(bucket); got != 0 {
		t.Fatalf("a corrupt object survived a cleanup with no ETag to compare: %d objects", got)
	}
}

// TestAVerifyingPutRejectsAnExistingCorruptKey is NC-RR-004, the reproduced defect.
//
// The old Put took a Stat hit as "already stored" and returned success BEFORE verifying, so a
// deployment with verify_after_put: true silently accepted a corrupt object whenever the address
// already existed — which is precisely the state worth checking. The review's probe corrupted an
// object with verification off, then re-uploaded the correct bytes with verification on and got a
// success for a bucket that still held the bad bytes.
func TestAVerifyingPutRejectsAnExistingCorruptKey(t *testing.T) {
	ctx := context.Background()
	payload := []byte("correct content whose address is already occupied by garbage")
	h := verifyHashOf(payload)

	const bucket = "dedupe-recheck"
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)

	// First store: verification OFF, and the write is corrupted on the way in. This is the "older
	// or concurrently corrupted blob" the review describes.
	unverified, err := New(ctx, Config{
		Endpoint: fake.Endpoint(), Bucket: bucket,
		AccessKey: "fake-access", SecretKey: "fake-secret",
		VerifyAfterPut: false,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	fake.CorruptNextPut()
	// With verification off nothing notices, which is the documented trade.
	if err := unverified.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("the unverified put should succeed: %v", err)
	}
	stored, ok := fake.GetDirect(bucket, unverified.key(h))
	if !ok || bytes.Equal(stored, payload) {
		t.Fatal("the fixture did not actually corrupt the stored object, so the test would " +
			"prove nothing")
	}

	// Second store over the same bucket and key: verification ON, correct bytes. The address
	// exists, so the old code returned nil here without looking.
	verified, err := New(ctx, Config{
		Endpoint: fake.Endpoint(), Bucket: bucket,
		AccessKey: "fake-access", SecretKey: "fake-secret",
		VerifyAfterPut: true,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if err := verified.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err == nil {
		t.Fatalf("a verifying Put accepted an existing corrupt object at the same address. The "+
			"bucket still holds %d bytes that do not hash to the key, and a deduplication hit is "+
			"not evidence of integrity.", len(stored))
	}
	// The corrupt object must be left in place, not deleted by a writer that did not create it:
	// this Put cannot know who else references that key.
	if got := fake.ObjectCount(bucket); got != 1 {
		t.Fatalf("object count = %d, want 1: the pre-existing object was removed by a caller "+
			"that did not write it", got)
	}
}

// TestAVerifyingPutAcceptsAGenuineDuplicate: the check must not break ordinary deduplication.
//
// A guard that rejected every duplicate would also "fix" NC-RR-004, by making the idempotent path
// useless. The property that matters is that a verified duplicate still succeeds.
func TestAVerifyingPutAcceptsAGenuineDuplicate(t *testing.T) {
	s, fake, bucket := revokedStore(t)
	ctx := context.Background()

	payload := []byte("a perfectly ordinary duplicate")
	h := verifyHashOf(payload)

	for i := 0; i < 3; i++ {
		if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatalf("put #%d: %v", i+1, err)
		}
	}
	if got := fake.ObjectCount(bucket); got != 1 {
		t.Fatalf("object count = %d for three identical puts, want 1 (deduplicated)", got)
	}
}

// TestAnUnverifiedStoreStillTakesTheCheapPath pins the documented trade.
//
// verify_after_put: false means "do not spend an extra read per upload". The fix for NC-RR-004 must
// not quietly turn readback on for everyone: the default is off because re-reading every object
// doubles egress, and a deployment that accepted that cost trade must not be silently charged it.
func TestAnUnverifiedStoreStillTakesTheCheapPath(t *testing.T) {
	ctx := context.Background()
	const bucket = "cheap-path-recheck"
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)

	s, err := New(ctx, Config{
		Endpoint: fake.Endpoint(), Bucket: bucket,
		AccessKey: "fake-access", SecretKey: "fake-secret",
		VerifyAfterPut: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("deduplicated without a readback")
	h := verifyHashOf(payload)
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("first put: %v", err)
	}

	// With verification off, a duplicate must succeed even though every GET now fails: no readback
	// happens at all, so the failing GET must never be reached.
	fake.FailAlwaysOn("GET")
	t.Cleanup(fake.ClearFailures)

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("a duplicate put on an unverified store made a readback: %v", err)
	}
}

// TestVerifyMaxBytesSkipsTheDedupePathToo: the size bypass applies to a deduplication hit, or a
// deployment that configured it for cost reasons would get the readback it disabled on exactly the
// path it most wanted to avoid.
func TestVerifyMaxBytesSkipsTheDedupePathToo(t *testing.T) {
	ctx := context.Background()
	const bucket = "maxbytes-recheck"
	fake := s3fake.New(bucket)
	t.Cleanup(fake.Close)

	s, err := New(ctx, Config{
		Endpoint: fake.Endpoint(), Bucket: bucket,
		AccessKey: "fake-access", SecretKey: "fake-secret",
		VerifyAfterPut: true,
		VerifyMaxBytes: 4, // everything used here is larger
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("larger than the bypass threshold")
	h := verifyHashOf(payload)
	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("first put: %v", err)
	}

	fake.FailAlwaysOn("GET")
	t.Cleanup(fake.ClearFailures)

	if err := s.Put(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("an object above verify_max_bytes was read back anyway: %v", err)
	}
}

// TestAMissingObjectIsInconclusiveNotCorruption pins the classification that is easy to get wrong:
// the object not being there is a readback failure, not proof of corrupt bytes, and must not take
// the destructive branch.
func TestAMissingObjectIsInconclusiveNotCorruption(t *testing.T) {
	s, _, _ := revokedStore(t)
	ctx := context.Background()

	missing := verifyHashOf([]byte("never uploaded anywhere"))
	err := s.verifyStored(ctx, missing, 0)
	if err == nil {
		t.Fatal("verifying a missing object must fail")
	}
	if isMismatch(err) {
		t.Fatal("a missing object was classified as corrupt content; a readback that could not " +
			"happen proves nothing about bytes and must not trigger destructive cleanup")
	}
	if !errors.Is(err, ErrVerifyUnavailable) {
		t.Fatalf("a missing object should classify as unavailable, got %v", err)
	}
}
