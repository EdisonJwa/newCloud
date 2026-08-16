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
