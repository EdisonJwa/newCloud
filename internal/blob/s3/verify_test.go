package s3_test

// WC-GAP-003: post-upload verification of the stored bytes.
//
// The property is subtle enough to state twice. `Put` hashes the INPUT and refuses a payload whose
// bytes do not match the address, which proves the file was correct when it left this process. It
// says nothing about what the object store now holds. A retried multipart part assembled from a
// buffer being rewritten concurrently — the documented minio-go v7.4.1 race — produces an object
// with the right LENGTH at the right KEY holding the wrong BYTES, and the upload is acknowledged.
//
// So these tests corrupt the store's copy AFTER a successful send and require Put to notice. The
// fake is told to acknowledge and store different bytes; nothing in the request path can see that
// except a read-back.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

// verifyingStore builds a store with the read-back enabled.
func verifyingStore(t *testing.T) (*s3.Store, *s3fake.Server) {
	t.Helper()
	fake := s3fake.New("verifybucket")
	t.Cleanup(fake.Close)
	store, err := s3.New(context.Background(), s3.Config{
		Endpoint:       fake.Endpoint(),
		Bucket:         "verifybucket",
		AccessKey:      "fake-access",
		SecretKey:      "fake-secret",
		Secure:         false,
		VerifyAfterPut: true,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return store, fake
}

// TestPutRefusesAnObjectThatDoesNotMatchItsAddress is the gap.
//
// Without the read-back this call SUCCEEDS: the input hashed correctly, the store acknowledged, and
// the corrupted bytes are now the object at a content address they do not match. Every later read
// serves corruption, and because Put is idempotent on an existing key, re-uploading the correct
// bytes would take the shortcut and never repair it.
func TestPutRefusesAnObjectThatDoesNotMatchItsAddress(t *testing.T) {
	store, fake := verifyingStore(t)
	ctx := context.Background()

	content := []byte("the bytes the address was computed from")
	hash := hashOf(content)

	fake.CorruptNextPut()

	err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content)))
	if err == nil {
		t.Fatal("Put reported success while the store held bytes that do not match the address. " +
			"Nothing is left to detect it: the address looks valid, and a re-upload would take " +
			"the idempotent shortcut instead of repairing it.")
	}
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch from the read-back, got %v", err)
	}
}

// TestACorruptedObjectIsRemovedSoTheAddressIsNotPoisoned.
//
// Reporting the failure is half the fix. If the bad object stays, every later Put of the CORRECT
// bytes returns early on the existence check, so the address can never be repaired without an
// operator deleting the object by hand.
func TestACorruptedObjectIsRemovedSoTheAddressIsNotPoisoned(t *testing.T) {
	store, fake := verifyingStore(t)
	ctx := context.Background()

	content := []byte("content that will be corrupted once")
	hash := hashOf(content)

	fake.CorruptNextPut()
	if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err == nil {
		t.Fatal("the corrupted write was accepted")
	}

	// The object must be gone, which is what makes the next attempt a real upload.
	if n := fake.ObjectCount("verifybucket"); n != 0 {
		t.Fatalf("the corrupt object was left behind (%d objects). A later Put of the right "+
			"bytes would see the key and return success without writing them.", n)
	}

	// And the retry succeeds and stores the right bytes.
	if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("the retry after a rejected corruption failed: %v", err)
	}
	rc, err := store.Open(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var got bytes.Buffer
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("the stored bytes after repair are %q, want %q", got.Bytes(), content)
	}
}

// TestVerificationIsOffByDefaultAndCanBeDisabled keeps the cost visible.
//
// The read-back doubles the bytes moved per upload. A deployment with a trusted local object store
// may reasonably refuse that, so the behaviour must be a setting rather than a surprise — and a
// test asserting the off-by-default state is what stops a later refactor from quietly turning a
// bandwidth cost on for everyone.
func TestVerificationIsOffByDefaultAndCanBeDisabled(t *testing.T) {
	fake := s3fake.New("noverifybucket")
	t.Cleanup(fake.Close)

	store, err := s3.New(context.Background(), s3.Config{
		Endpoint:  fake.Endpoint(),
		Bucket:    "noverifybucket",
		AccessKey: "fake-access",
		SecretKey: "fake-secret",
		Secure:    false,
		// VerifyAfterPut deliberately unset.
	})
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("unverified but acknowledged")
	hash := hashOf(content)

	fake.CorruptNextPut()
	// Without verification this succeeds — which is the documented trade, not a bug. Asserting it
	// makes the trade explicit: changing the default would fail this test and force the author to
	// think about the cost.
	if err := store.Put(context.Background(), hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("with verification off, Put should not read the object back: %v", err)
	}
}

// TestVerificationHonoursTheSizeCeiling: a deployment can cap the read-back cost.
func TestVerificationHonoursTheSizeCeiling(t *testing.T) {
	fake := s3fake.New("capbucket")
	t.Cleanup(fake.Close)
	store, err := s3.New(context.Background(), s3.Config{
		Endpoint:       fake.Endpoint(),
		Bucket:         "capbucket",
		AccessKey:      "fake-access",
		SecretKey:      "fake-secret",
		Secure:         false,
		VerifyAfterPut: true,
		VerifyMaxBytes: 8, // everything in this test is larger
	})
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("larger than the ceiling")
	hash := hashOf(content)
	fake.CorruptNextPut()

	if err := store.Put(context.Background(), hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("an object above verify_max_bytes must skip the read-back, got %v", err)
	}
}

// TestAHealthyObjectPassesVerification: the guard must not refuse correct writes.
//
// Without this, a verification step that always failed would look like a working detector.
func TestAHealthyObjectPassesVerification(t *testing.T) {
	store, _ := verifyingStore(t)
	ctx := context.Background()

	for _, content := range [][]byte{
		[]byte("small"),
		bytes.Repeat([]byte("x"), 32<<10),
	} {
		hash := hashOf(content)
		if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("a correct upload was rejected by verification: %v", err)
		}
	}
}
