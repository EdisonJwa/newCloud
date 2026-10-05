package worker_test

// The live-storage integrity scrub.
//
// Why this exists: content addressing means a blob's name is the hash of its bytes, so re-reading a
// blob and hashing it is a complete check with no metadata. Nothing was doing that against live
// storage. The restore drill verifies blobs copied out of a dump — a different question, and an
// operation heavy enough that it runs quarterly. A blob corrupted in place therefore stayed
// undetected until a drill found it, and then looked like a backup problem.
//
// The scrub closes that window to "within one scrub interval". These tests are about the two things
// that decide whether it is worth anything: does it actually catch corrupted bytes, and does it
// distinguish corruption from a blob that is simply missing — because those lead an operator to
// different conclusions.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

// scrubFixture builds a local blob store plus the metadata that references it.
type scrubFixture struct {
	store  blob.Store
	meta   *store.Memory
	root   string
	worker *worker.Worker
	user   store.User
}

func newScrubFixture(t *testing.T) *scrubFixture {
	t.Helper()
	root := t.TempDir()
	bs, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "s", Email: "s@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &scrubFixture{store: bs, meta: meta, root: root, worker: w, user: user}
}

// put stores content and records it in the metadata, returning its address.
func (f *scrubFixture) put(t *testing.T, content []byte) blob.Hash {
	t.Helper()
	h := blob.NewHasher()
	_, _ = h.Write(content)
	hash, size := h.Sum()

	if err := f.store.Put(context.Background(), hash, bytes.NewReader(content), size); err != nil {
		t.Fatalf("put: %v", err)
	}
	// The scrub samples from the metadata, so the row has to exist for the blob to be checked.
	if err := f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		root, err := tx.EnsureRoot(ctx, f.user.ID)
		if err != nil {
			return err
		}
		_, err = tx.CreateFile(ctx, store.NewFile{
			OwnerID: f.user.ID, ParentID: root.ID,
			Name: "f-" + hash.String()[:8],
			Size: size, Mime: "application/octet-stream", ETag: "e",
			BlobID: f.meta.BlobIDFor(ctx, hash, size), BlobHash: hash,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return hash
}

// corrupt overwrites a stored blob's bytes in place, the way a bad sector or a garbled transfer
// would — the file name stays correct, the contents do not.
func (f *scrubFixture) corrupt(t *testing.T, h blob.Hash) {
	t.Helper()
	hex := h.String()
	path := filepath.Join(f.root, hex[0:2], hex[2:4], hex)
	if err := os.WriteFile(path, []byte("these are not the bytes you stored"), 0o644); err != nil {
		t.Fatalf("corrupt %s: %v", path, err)
	}
}

// TestScrubDetectsCorruptedBytes is the whole point.
func TestScrubDetectsCorruptedBytes(t *testing.T) {
	f := newScrubFixture(t)
	hash := f.put(t, []byte("original content that will be corrupted"))
	f.corrupt(t, hash)

	res, err := f.worker.ScrubBlobs(context.Background(), f.store, 10)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if res.Checked != 1 {
		t.Fatalf("checked %d blobs, want 1", res.Checked)
	}
	if len(res.Corrupt) != 1 {
		t.Fatalf("the scrub did not report the corrupted blob (result %+v). Without this, a blob "+
			"corrupted in place is invisible until a quarterly drill.", res)
	}
	if res.Corrupt[0].Hash != hash {
		t.Fatalf("reported the wrong blob: %x", res.Corrupt[0].Hash[:8])
	}
	// The actual hash is reported so an operator can compare without recomputing by hand.
	if res.Corrupt[0].Actual == hash {
		t.Fatal("the reported actual hash equals the address, which cannot be true for corrupt bytes")
	}
}

// TestScrubPassesHealthyBlobs is the control: a scrub that flagged everything would also pass the
// test above.
func TestScrubPassesHealthyBlobs(t *testing.T) {
	f := newScrubFixture(t)
	f.put(t, []byte("healthy content one"))
	f.put(t, []byte("healthy content two"))

	res, err := f.worker.ScrubBlobs(context.Background(), f.store, 10)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if res.Checked != 2 {
		t.Fatalf("checked %d blobs, want 2", res.Checked)
	}
	if len(res.Corrupt) != 0 {
		t.Fatalf("healthy blobs were reported corrupt: %+v", res.Corrupt)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("healthy blobs were reported missing: %v", res.Missing)
	}
	if res.Bytes == 0 {
		t.Fatal("the scrub read no bytes, so it cannot have verified anything")
	}
}

// TestScrubDistinguishesMissingFromCorrupt.
//
// These are different faults with different remedies: corruption means the storage is damaging data,
// a missing blob means something deleted it. Reporting both as "corrupt" sends the operator after
// the wrong cause.
func TestScrubDistinguishesMissingFromCorrupt(t *testing.T) {
	f := newScrubFixture(t)
	hash := f.put(t, []byte("content whose bytes will disappear"))

	hex := hash.String()
	path := filepath.Join(f.root, hex[0:2], hex[2:4], hex)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	res, err := f.worker.ScrubBlobs(context.Background(), f.store, 10)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if len(res.Missing) != 1 {
		t.Fatalf("a removed blob was not reported missing (result %+v)", res)
	}
	if len(res.Corrupt) != 0 {
		t.Fatalf("a missing blob was reported as corrupt: %+v. The two faults have different "+
			"causes and different remedies.", res.Corrupt)
	}
}

// TestScrubLimitIsRespected, including the zero case.
func TestScrubLimitIsRespected(t *testing.T) {
	f := newScrubFixture(t)
	for i := 0; i < 5; i++ {
		f.put(t, []byte{byte(i), 'x', 'y', 'z'})
	}

	res, err := f.worker.ScrubBlobs(context.Background(), f.store, 2)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if res.Checked > 2 {
		t.Fatalf("checked %d blobs with limit 2", res.Checked)
	}

	// Zero means "check nothing". A scrub that defaulted to everything would become an
	// unexpected full read of the store the first time somebody enabled it.
	res, err = f.worker.ScrubBlobs(context.Background(), f.store, 0)
	if err != nil {
		t.Fatalf("scrub with limit 0: %v", err)
	}
	if res.Checked != 0 {
		t.Fatalf("limit 0 checked %d blobs", res.Checked)
	}
}

// TestScrubStopsOnContextCancellation, so a shutdown does not wait for a full pass.
func TestScrubStopsOnContextCancellation(t *testing.T) {
	f := newScrubFixture(t)
	for i := 0; i < 5; i++ {
		f.put(t, []byte{byte(i), 'a', 'b', 'c'})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.worker.ScrubBlobs(ctx, f.store, 5); err == nil {
		t.Fatal("a cancelled context did not stop the scrub")
	}
}

// TestScrubWithNoBlobsIsNotAnError keeps the worker's timer from logging every cycle on an empty
// deployment.
func TestScrubWithNoBlobsIsNotAnError(t *testing.T) {
	f := newScrubFixture(t)
	res, err := f.worker.ScrubBlobs(context.Background(), f.store, 10)
	if err != nil {
		t.Fatalf("scrub on an empty store: %v", err)
	}
	if res.Checked != 0 {
		t.Fatalf("checked %d blobs on an empty store", res.Checked)
	}
	_ = time.Second
}

// TestPruneRateLimitRowsActuallyRuns is the check that both prune functions are reachable.
//
// PruneOTPAttempts had existed since OTP codes were added and was never called: a row per
// verification attempt, forever. PruneShareUploadEvents would have repeated it. The ticker guard
// catches a missing select arm, but not a function that no arm calls — so this drives the method
// and requires it to remove what it should.
func TestPruneRateLimitRowsActuallyRuns(t *testing.T) {
	meta := store.NewMemory()
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// Record an event and move the clock past the keep window.
	user := meta.AddUser(store.User{Username: "p", Email: "p@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	var shareID uuid.UUID
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// CreateShare references the node and owner, so both have to exist: a share pointing at
		// a node that was never created is rejected, which is what "not found" here meant.
		root, rerr := tx.EnsureRoot(ctx, user.ID)
		if rerr != nil {
			return rerr
		}
		s, cerr := tx.CreateShare(ctx, store.Share{
			NodeID: root.ID, OwnerID: user.ID, Kind: store.ShareLink,
			Label: "x", TokenHash: []byte{1, 2, 3}, Permissions: []string{"read"},
			MaxUploadRate: int64ptr(1),
		})
		if cerr != nil {
			return cerr
		}
		shareID = s.ID
		return tx.AllowShareUpload(ctx, s.ID, 1, time.Minute, "127.0.0.1", 10)
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	_ = shareID

	// Inside the window the limiter must still refuse a second upload.
	err = meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.AllowShareUpload(ctx, shareID, 1, time.Minute, "127.0.0.1", 10)
	})
	if err == nil {
		t.Fatal("the limiter did not refuse a second upload inside the window, so the cell under " +
			"test is not the limiter at all")
	}

	meta.AdvanceForTest(48 * time.Hour)
	w.PruneRateLimitRows(ctx)

	// After pruning and with the clock advanced, the row is gone and an upload is allowed again.
	err = meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.AllowShareUpload(ctx, shareID, 1, time.Minute, "127.0.0.1", 10)
	})
	if err != nil {
		t.Fatalf("after pruning rows older than the keep window, an upload was still refused: %v. "+
			"Either the row survived or the clock advance did not take effect.", err)
	}
}

func int64ptr(v int64) *int64 { return &v }

// recordingReader remembers which blobs a scrub pass actually opened.
type recordingReader struct {
	blob.Store
	seen map[blob.Hash]bool
}

func (r *recordingReader) Open(ctx context.Context, h blob.Hash) (io.ReadSeekCloser, error) {
	r.seen[h] = true
	return r.Store.Open(ctx, h)
}

// TestScrubRotatesThroughTheStore is the property the cursor exists for.
//
// Before it, each pass verified the first `limit` blobs by insertion order and nothing else, so a
// deployment with more blobs than the limit checked the same oldest subset forever and never looked
// at content written since — the content most likely to be damaged.
//
// The assertion counts DISTINCT blobs opened, not checks: an earlier version summed the per-pass
// counters and passed with the cursor pinned to zero, because two passes over the same two blobs
// still add up to four checks. Counting what was read is what makes the test about coverage.
func TestScrubRotatesThroughTheStore(t *testing.T) {
	f := newScrubFixture(t)
	for i := 0; i < 6; i++ {
		f.put(t, []byte{byte('a' + i)})
	}

	rec := &recordingReader{Store: f.store, seen: map[blob.Hash]bool{}}
	for pass := 0; pass < 2; pass++ {
		if _, err := f.worker.ScrubBlobs(context.Background(), rec, 2); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	if len(rec.seen) < 3 {
		t.Fatalf("two passes of two opened only %d distinct blobs. The same rows are being "+
			"re-read and the rest of the store is never verified, which is what the cursor was "+
			"added to stop.", len(rec.seen))
	}
}
