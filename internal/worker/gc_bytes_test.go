package worker_test

// The blob collector's physical half.
//
// For its whole life the sweep deleted metadata rows and stopped there: the physical deletes
// in the storage backends had zero production callers, so every "collected N blobs" log line
// described rows while the bytes sat on the device forever. These tests pin the contract the
// other direction: every collected row is followed by a byte delete, a byte-delete failure
// after the row is committed does not fail the sweep (the row is gone; failing would report
// a collection that happened as if it had not), and an unwired deleter degrades to the row
// half instead of panicking.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

// gcFakeTx is the slice of store.Tx the sweep uses. The real in-memory store refuses
// CollectableBlobs (it does not model blob lifecycle), so a fake is the only way to drive
// the sweep end to end without PostgreSQL.
type gcFakeTx struct {
	store.Tx
	refs    []store.BlobRef
	hashes  map[uuid.UUID][32]byte
	deleted []uuid.UUID
	audits  []store.AuditEntry
}

func (t *gcFakeTx) CollectableBlobs(ctx context.Context, grace time.Duration) ([]store.BlobRef, error) {
	return t.refs, nil
}

func (t *gcFakeTx) BlobHash(ctx context.Context, id uuid.UUID) ([]byte, error) {
	h, ok := t.hashes[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return h[:], nil
}

func (t *gcFakeTx) DeleteBlob(ctx context.Context, id uuid.UUID) error {
	t.deleted = append(t.deleted, id)
	return nil
}

func (t *gcFakeTx) Audit(ctx context.Context, e store.AuditEntry) error {
	t.audits = append(t.audits, e)
	return nil
}

type gcFakeStore struct {
	store.Store
	tx *gcFakeTx
}

func (s *gcFakeStore) WithTx(ctx context.Context, fn func(ctx context.Context, tx store.Tx) error) error {
	return fn(ctx, s.tx)
}

// recordingDeleter remembers every byte delete and can be told to fail specific hashes, which
// is how a test exercises the post-commit failure path.
type recordingDeleter struct {
	deleted []blob.Hash
	failFor map[blob.Hash]bool
}

func (d *recordingDeleter) Delete(ctx context.Context, h blob.Hash) error {
	if d.failFor[h] {
		return fmt.Errorf("storage backend refused")
	}
	d.deleted = append(d.deleted, h)
	return nil
}

func gcFixture(t *testing.T, n int) (*worker.Worker, *gcFakeTx, []blob.Hash) {
	t.Helper()
	tx := &gcFakeTx{hashes: map[uuid.UUID][32]byte{}}
	var hashes []blob.Hash
	for i := 0; i < n; i++ {
		var h blob.Hash
		h[0], h[31] = byte(i+1), byte(i)
		hashes = append(hashes, h)
		id := uuid.New()
		tx.hashes[id] = h
		tx.refs = append(tx.refs, store.BlobRef{ID: id, Size: 10})
	}
	w := worker.New(&gcFakeStore{tx: tx}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w, tx, hashes
}

func TestSweepBlobsDeletesTheBytesOfEveryCollectedRow(t *testing.T) {
	w, tx, hashes := gcFixture(t, 3)
	del := &recordingDeleter{}
	w.WithByteDeletion(del)

	removed, err := w.SweepBlobs(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d rows, want 3", removed)
	}
	if len(tx.deleted) != 3 {
		t.Fatalf("%d rows deleted, want 3", len(tx.deleted))
	}
	if len(del.deleted) != 3 {
		t.Fatalf("%d byte deletes attempted, want 3. Without a physical delete per collected "+
			"row the sweep reclaims rows and calls it disk", len(del.deleted))
	}
	// The right bytes: the address comes from the metadata, read in the same transaction as
	// the delete, because after the row is committed nothing can name the object.
	want := map[blob.Hash]bool{}
	for _, h := range hashes {
		want[h] = true
	}
	for _, got := range del.deleted {
		if !want[got] {
			t.Fatalf("bytes deleted for %s, which no collected row named", got.String())
		}
		delete(want, got)
	}
	if len(want) != 0 {
		t.Fatalf("collected rows whose bytes were never deleted: %d missing", len(want))
	}
}

// TestSweepBlobsSurvivesAFailedByteDelete pins the honest failure mode: the row is already
// committed, so the sweep continues, reports what it collected, and the failure is left to
// the log (and eventually an orphan sweep) instead of being retried into a hole.
func TestSweepBlobsSurvivesAFailedByteDelete(t *testing.T) {
	w, _, hashes := gcFixture(t, 3)
	del := &recordingDeleter{failFor: map[blob.Hash]bool{hashes[1]: true}}
	w.WithByteDeletion(del)

	removed, err := w.SweepBlobs(context.Background())
	if err != nil {
		t.Fatalf("a failed byte delete must not fail the sweep once the row is committed: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d rows, want 3: the row deletes all succeeded", removed)
	}
	if len(del.deleted) != 2 {
		t.Fatalf("%d byte deletes succeeded, want 2", len(del.deleted))
	}
}

// TestSweepBlobsWithoutByteDeletionIsRowOnly: the unwired case (tests, and a layout with no
// content store) must degrade to the historical behaviour, not panic.
func TestSweepBlobsWithoutByteDeletionIsRowOnly(t *testing.T) {
	w, tx, _ := gcFixture(t, 2)

	removed, err := w.SweepBlobs(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 || len(tx.deleted) != 2 {
		t.Fatalf("row collection broke without a deleter: removed=%d deleted=%d", removed, len(tx.deleted))
	}
}

// TestSweepBlobsWithGenuinelyVanishedRows: a ref whose hash cannot be read (a concurrent
// collector already took the row) is skipped, as before.
func TestSweepBlobsWithGenuinelyVanishedRows(t *testing.T) {
	w, tx, _ := gcFixture(t, 2)
	del := &recordingDeleter{}
	w.WithByteDeletion(del)

	// One row loses its hash between collect and delete — a concurrent sweep got there first.
	delete(tx.hashes, tx.refs[0].ID)

	removed, err := w.SweepBlobs(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d rows, want 1", removed)
	}
	if len(del.deleted) != 1 {
		t.Fatalf("%d byte deletes, want 1: a vanished row cannot have bytes to delete", len(del.deleted))
	}
}
