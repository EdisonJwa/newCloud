package store_test

// The orphan-at-birth case for the blob collector.
//
// BlobIDFor creates the blobs row before the node that will reference it exists; a publish
// that fails in between (quota 507, If-Match 412, a name fold 409, an audit 503) abandons a
// row with refcount 0 and collect_after NULL. The dropped-reference clock never starts for a
// reference that never existed, so a collector that required collect_after to be set kept
// every failed upload forever. These cases drive the real query against the real database and
// age the row the only way the grace can be exercised deterministically: by moving creation
// into the past.
//
// The shared-suite twin (AbandonedUploadBecomesCollectable in internal/storetest) covers the
// same contract through the Store API alone; this file pins the grace-parameter semantics that
// need SQL access to the row.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestPGCollectorPicksUpOrphanAtBirth(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	var hash [32]byte
	hash[0] = 0x3c
	orphan := s.BlobIDFor(ctx, hash, 4242)
	if orphan == (uuid.UUID{}) {
		t.Fatal("BlobIDFor returned no id, so the orphan was never created")
	}

	collectable := func(grace time.Duration) []store.BlobRef {
		t.Helper()
		var refs []store.BlobRef
		if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, err := tx.CollectableBlobs(ctx, grace)
			refs = got
			return err
		}); err != nil {
			t.Fatalf("CollectableBlobs: %v", err)
		}
		return refs
	}
	contains := func(refs []store.BlobRef, id uuid.UUID) bool {
		for _, r := range refs {
			if r.ID == id {
				return true
			}
		}
		return false
	}

	// A row created moments ago is inside any real grace period: the sweep must leave it
	// alone, because the upload that references it may simply be slow.
	if refs := collectable(7 * 24 * time.Hour); contains(refs, orphan) {
		t.Fatal("an unreferenced blob created moments ago was collectable with a week of grace; " +
			"the grace exists so the sweep cannot race the upload that is about to reference the row")
	}

	// Age the row past the grace, exactly as time would.
	if _, err := pool.Exec(ctx,
		`UPDATE blobs SET created_at = now() - interval '8 days' WHERE id = $1`, orphan); err != nil {
		t.Fatal(err)
	}

	refs := collectable(7 * 24 * time.Hour)
	if !contains(refs, orphan) {
		t.Fatal("an abandoned blob (never referenced, created 8 days ago) was not collectable " +
			"with a 7-day grace. It has no collect_after to expire — the reference never existed " +
			"— so a collector that required collect_after to be set keeps every failed publish " +
			"as garbage forever")
	}
	for _, r := range refs {
		if r.ID == orphan && r.Size != 4242 {
			t.Fatalf("candidate reported size %d, want 4242: the sweep's reclaim accounting reads this", r.Size)
		}
	}

	// The sweep can finish the job: the guarded delete takes the row, and only once.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.DeleteBlob(ctx, orphan)
	}); err != nil {
		t.Fatalf("DeleteBlob on the orphan: %v", err)
	}
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.DeleteBlob(ctx, orphan)
	}); err == nil {
		t.Fatal("a second DeleteBlob on the same row succeeded; a guarded delete that reports " +
			"success twice is not reporting on the row at all")
	}
}
