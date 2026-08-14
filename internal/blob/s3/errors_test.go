package s3_test

// S3 error injection: the "S3 错误注入无数据破坏" clause of §G3.
//
// The in-process fake has supported FailNextOn since it was written, and nothing used it. That
// left the backend's error paths entirely untested on the S3 side — the local backend has crash
// tests, the S3 one had only the happy path. The properties below are the ones that decide whether
// a transient S3 failure corrupts data or merely fails a request.
//
// The design that makes these pass is worth stating, because a refactor could remove it: Put hashes
// the bytes and compares against the expected address BEFORE handing the reader to the client. A
// transfer that is truncated or altered by a failing endpoint therefore cannot be committed under
// the requested key — the mismatch is detected locally, before anything is written.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/s3"
	"github.com/EdisonJwa/newCloud/internal/storage/s3fake"
)

// Note on FailAlwaysOn rather than FailNextOn.
//
// The first version of these tests used FailNextOn, and four of the five passed against a store
// that ignored errors entirely. The cause is retry: minio-go retries 5xx, so "fail the next
// request" is consumed by the first attempt and the retry succeeds. A test written that way
// asserts the happy path while looking like it tests failure — the same shape of mistake as a
// mutation that does not apply.

// failingStore returns a store wired to a fake that can be made to fail specific requests.
func failingStore(t *testing.T) (*s3.Store, *s3fake.Server) {
	t.Helper()
	fake := s3fake.New("errbucket")
	t.Cleanup(fake.Close)
	store, err := s3.New(context.Background(), s3.Config{
		Endpoint:  fake.Endpoint(),
		Bucket:    "errbucket",
		AccessKey: "fake-access",
		SecretKey: "fake-secret",
		Secure:    false,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	return store, fake
}

// TestPutFailsCleanlyWhenTheBackendErrors.
//
// A failed PUT must return an error and must not leave an object behind. Leaving one would be worse
// than the error: the blob would exist with content nobody verified, and a later Put of the same
// address would short-circuit on the idempotency check and consider the upload done.
func TestPutFailsCleanlyWhenTheBackendErrors(t *testing.T) {
	store, fake := failingStore(t)
	ctx := context.Background()

	content := []byte("bytes that will fail to upload")
	hash := hashOf(content)

	fake.FailAlwaysOn("PUT")

	err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content)))
	if err == nil {
		t.Fatal("Put reported success while the endpoint was failing; the caller would record a " +
			"file whose bytes were never stored")
	}

	// The decisive check: nothing was committed under that key.
	if _, statErr := store.Stat(ctx, hash); !errors.Is(statErr, blob.ErrNotFound) {
		t.Fatalf("a failed Put left an object behind (Stat returned %v). A blob that exists but "+
			"was never verified is worse than a missing one: the next Put of the same address "+
			"short-circuits on the idempotency check and believes the upload succeeded.", statErr)
	}
}

// TestPutRejectsCorruptTransferBeforeCommitting.
//
// Put hashes what it read and compares to the expected address before uploading. This is what makes
// a corrupted transfer a local rejection rather than a poisoned object: the mismatch is found before
// the client is asked to store anything.
func TestPutRejectsCorruptTransferBeforeCommitting(t *testing.T) {
	store, _ := failingStore(t)
	ctx := context.Background()

	wanted := []byte("the content the address was computed from")
	hash := hashOf(wanted)

	// The same declared size and address, different bytes — what a truncated or mangled transfer
	// looks like.
	actual := []byte("the content a broken transfer delivered!!!!")

	err := store.Put(ctx, hash, bytes.NewReader(actual), int64(len(actual)))
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v. Without the local hash check a corrupt transfer "+
			"is stored under an address it does not match, and every later read returns bytes "+
			"whose hash disagrees with the name — the failure surfaces much later, somewhere else.",
			err)
	}

	if _, statErr := store.Stat(ctx, hash); !errors.Is(statErr, blob.ErrNotFound) {
		t.Fatal("a hash-mismatched Put stored an object anyway")
	}
}

// TestStatFailureIsNotMistakenForAbsence.
//
// Put's first act is an existence check. If a failing Stat were read as "not found" rather than
// "could not tell", Put would proceed to upload over something it never inspected — and on a
// content-addressed store that is normally harmless but hides a credential or endpoint problem
// behind a slow, silent re-upload.
func TestStatFailureIsNotMistakenForAbsence(t *testing.T) {
	store, fake := failingStore(t)
	ctx := context.Background()

	content := []byte("content that exists")
	hash := hashOf(content)
	if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A HEAD that fails must surface as an error, not as ErrNotFound.
	fake.FailAlwaysOn("HEAD")

	_, err := store.Stat(ctx, hash)
	if err == nil {
		t.Fatal("a failing Stat reported success")
	}
	if errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a failing Stat was translated to ErrNotFound. The caller then believes the " +
			"object is absent rather than unreachable — and Put would upload over a key it never " +
			"managed to inspect.")
	}
}

// TestOpenFailureIsReported is the read half.
//
// GetObject is lazy, so a missing or unreachable object has to be detected by the Stat inside Open.
// A failure there must not surface as a reader that errors mid-stream: the HTTP handler would have
// already written a 200 and part of a body by then.
func TestOpenFailureIsReported(t *testing.T) {
	store, fake := failingStore(t)
	ctx := context.Background()

	content := []byte("readable content")
	hash := hashOf(content)
	if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Open detects a missing object with a Stat, which issues a HEAD — so HEAD is what must fail
	// for this path. Failing GET instead leaves Open succeeding and the error surfacing on the
	// first read, which is a different (also worth knowing) property: the handler would already
	// have written a 200 and part of a body by then.
	fake.FailAlwaysOn("HEAD")

	rc, err := store.Open(ctx, hash)
	if err == nil {
		rc.Close()
		t.Fatal("Open succeeded while the endpoint was failing; the error would surface during " +
			"the first read, after the handler had already sent headers and a 200")
	}
}

// TestDeleteFailureIsReported.
//
// Deleting is what the collector relies on to reclaim space. A Delete that reports success while
// the endpoint refused it makes reclaimed-bytes accounting wrong and leaks storage quietly.
func TestDeleteFailureIsReported(t *testing.T) {
	store, fake := failingStore(t)
	ctx := context.Background()

	content := []byte("content to delete")
	hash := hashOf(content)
	if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("setup: %v", err)
	}

	fake.FailAlwaysOn("DELETE")

	if err := store.Delete(ctx, hash); err == nil {
		t.Fatal("Delete reported success while the endpoint was failing; the collector would " +
			"record reclaimed space that was never reclaimed")
	}

	// And the object is still there, which is the honest outcome: nothing was lost.
	if _, err := store.Stat(ctx, hash); err != nil {
		t.Fatalf("a failed Delete must leave the object intact, but Stat says %v", err)
	}
}

// hashOf computes a blob address.
func hashOf(b []byte) blob.Hash {
	h := blob.NewHasher()
	_, _ = h.Write(b)
	sum, _ := h.Sum()
	return sum
}

// TestOpenSucceedsAndFailsOnFirstReadDocumentsTheGap is the property the test above does NOT cover,
// stated so nobody assumes it is handled.
//
// Open's existence check is a HEAD. If the HEAD succeeds and the subsequent GET fails, Open returns
// a reader and the error appears on the first Read — after the HTTP handler has already sent
// headers and a 200. The client sees a truncated body rather than an error status.
//
// This is not fixable inside the store without buffering the whole object, which is the wrong trade
// for a streaming file server. It is documented here because it is a real limitation of the shape:
// a storage backend that fails mid-stream produces a short response, and the only defences are a
// Content-Length the client can check and blob hashes the reader can verify afterwards.
func TestOpenSucceedsAndFailsOnFirstRead(t *testing.T) {
	store, fake := failingStore(t)
	ctx := context.Background()

	content := []byte("content whose read will fail")
	hash := hashOf(content)
	if err := store.Put(ctx, hash, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// HEAD succeeds (so Open's existence check passes), GET fails (so the body cannot be read).
	fake.FailAlwaysOn("GET")

	rc, err := store.Open(ctx, hash)
	if err != nil {
		// Also an acceptable outcome, and better: the failure is reported before any bytes are
		// promised. Either behaviour is fine; silently returning short content is not.
		t.Logf("Open reported the failure immediately: %v", err)
		return
	}
	defer rc.Close()

	buf := make([]byte, len(content)+16)
	n, readErr := rc.Read(buf)
	if readErr == nil && n == len(content) {
		t.Fatal("the read succeeded despite the endpoint failing, which means the fake is not " +
			"actually failing GET requests and this test proves nothing")
	}
	// The documented limitation: the error arrives here, on the read, not at Open.
	t.Logf("as documented: Open succeeded and the failure surfaced on the first read (%d bytes, %v)",
		n, readErr)
}
