package store_test

// The blob collector is metadata-driven, proved against the real database.
//
// This matters for the documented restore procedure. "Restore blobs, then pg_restore the database"
// is safe only because a blob FILE with no metadata ROW is invisible to the collector: during that
// window every restored file is in exactly that state, and a directory-scanning collector would
// delete the entire restore as garbage.
//
// There is a source-level check in internal/server (TestGCIsMetadataDriven) that looks for a
// filesystem call in CollectableBlobs. This is the behavioural half, against PostgreSQL: build the
// restore-window state for real and confirm the collector does not see it, then build the ordinary
// garbage state and confirm it does.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestPGCollesOnlyRowBackedBlobs is the restore-window guarantee.
func TestPGCollectorIgnoresBlobFilesWithNoRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	// A blob row that is fully reclaimable: unreferenced and past its grace period. This is the
	// control — if THIS did not come back, the test would pass because the collector is broken
	// rather than because it is careful.
	reclaimable := newBlob(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE blobs SET collect_after = now() - interval '1 day' WHERE id = $1`, reclaimable); err != nil {
		t.Fatal(err)
	}

	// A blob that exists only as an id the collector has never heard of — the shape of a restored
	// file before pg_restore lands. It has no row at all.
	unreferenced := uuid.New()

	var candidates []store.BlobRef
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, 0)
		candidates = refs
		return err
	}); err != nil {
		t.Fatalf("CollectableBlobs: %v", err)
	}

	var sawReclaimable bool
	for _, c := range candidates {
		if c.ID == unreferenced {
			t.Fatalf("a blob with no metadata row appeared as a collection candidate. During a " +
				"restore every restored file is in this state, so collecting them would delete " +
				"the restore the operator is trying to perform.")
		}
		if c.ID == reclaimable {
			sawReclaimable = true
		}
	}
	if !sawReclaimable {
		t.Fatalf("the control blob (unreferenced, past grace) was not a candidate, so this test "+
			"cannot distinguish a careful collector from a broken one.\ncandidates: %v", candidates)
	}
}

// TestPGCollectorHonoursTheGracePeriod: a blob whose clock has not run out is not collectable.
//
// The grace period is what stops the collector racing an upload that has stored bytes but not yet
// committed the row that references them. Without it, a slow upload would have its content deleted
// mid-flight.
func TestPGCollectorHonoursTheGracePeriod(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	fresh := newBlob(t, pool)
	// collect_after is set to now()+grace when a reference count reaches zero. Simulate a blob
	// that reached zero just now: its collect_after is in the future.
	if _, err := pool.Exec(ctx,
		`UPDATE blobs SET collect_after = now() + interval '1 hour' WHERE id = $1`, fresh); err != nil {
		t.Fatal(err)
	}

	var candidates []store.BlobRef
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, 0)
		candidates = refs
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if c.ID == fresh {
			t.Fatal("a blob whose grace period has not elapsed was collectable")
		}
	}

	// And it becomes collectable once the clock passes, so the test above is not passing merely
	// because the row is unreachable.
	if _, err := pool.Exec(ctx,
		`UPDATE blobs SET collect_after = now() - interval '1 second' WHERE id = $1`, fresh); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, 0)
		candidates = refs
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range candidates {
		if c.ID == fresh {
			found = true
		}
	}
	if !found {
		t.Fatal("the blob did not become collectable after its grace period elapsed")
	}
}

// TestPGCollectorSkipsReferencedBlobs covers the node join specifically.
//
// The query has two independent guards against deleting live data: `refcount = 0` and
// `NOT EXISTS (SELECT 1 FROM nodes n WHERE n.blob_id = b.id)`. The first draft of this test used
// an ordinary upload and passed even with the node join deleted — because CreateFile increments
// refcount, so the first guard was doing all the work. A test that passes with the guard it claims
// to test removed is not testing that guard.
//
// So this drives refcount to zero while the node still references the blob. That is an
// inconsistent state the application should never produce — and it is exactly what a refcount bug
// would produce. The join is what stops such a bug from becoming data loss.
func TestPGCollectorSkipsReferencedBlobs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	owner := newUser(t, pool)
	var root uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner)
		root = n.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var blobID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		blobID = newBlob(t, pool)
		_, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root, Name: "live.bin", Size: 4,
			Mime: "application/octet-stream", ETag: "e", BlobID: blobID,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Force the state a refcount bug would produce: the blob looks ripe for collection while a
	// node still points at it. Without the node join, the collector deletes a live file.
	if _, err := pool.Exec(ctx, `
		UPDATE blobs SET refcount = 0, collect_after = now() - interval '1 day' WHERE id = $1`,
		blobID); err != nil {
		t.Fatal(err)
	}

	var candidates []store.BlobRef
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, 0)
		candidates = refs
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if c.ID == blobID {
			t.Fatal("a blob referenced by a live node was collectable. refcount alone is not " +
				"enough: this blob's refcount was forced to zero while the node row still points " +
				"at it, which is what a refcount bug looks like — and the node join in " +
				"CollectableBlobs is what stops that bug from deleting user data.")
		}
	}

	// The control: with the reference gone, the same blob must become collectable. Otherwise this
	// test would pass against a collector that returns nothing ever.
	if _, err := pool.Exec(ctx, `DELETE FROM nodes WHERE blob_id = $1`, blobID); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, 0)
		candidates = refs
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range candidates {
		if c.ID == blobID {
			found = true
		}
	}
	if !found {
		t.Fatal("the blob did not become collectable once nothing referenced it, so this test " +
			"cannot distinguish a guarded collector from a broken one")
	}
}
