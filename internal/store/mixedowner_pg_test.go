package store_test

// PostgreSQL-gated regressions for mixed-owner subtree accounting.
//
// A directory is not one owner's bytes: group spaces and ACL-shared folders hold files whose
// owner_id is the writer's. Every case here builds that shape and checks that BOTH owners'
// counters move, which is the property the single-owner tests cannot see.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/storetest"
)

// TestPGDeleteUserTrashesForeignChildren runs the account-deletion suite against PostgreSQL.
//
// The in-memory DeleteUser still trashes only the account's own rows (userdelete_memory.go),
// so the suite is wired here only; when that implementation catches up, the call belongs in
// storetest.Run's AccountDeletion block so both backends prove the same behaviour.
func TestPGDeleteUserTrashesForeignChildren(t *testing.T) {
	pool := testPool(t)
	s := store.NewPG(pool)
	s.EnableAudit()

	storetest.MixedOwnerDeletionSuite(t,
		func(t *testing.T) (store.Store, uuid.UUID) {
			return s, newUser(t, pool)
		},
		func(t *testing.T) uuid.UUID { return newBlob(t, pool) })
}

// TestPGExpireRetainedMovesEachOwnersBytes is the retention-expiry twin of the mixed-owner
// trash accounting: expiring a lapsed node moves it into the trash, and the bytes of EVERYONE
// under it must move with it.
//
// It lives here rather than in the shared suite because the in-memory ExpireRetained marks only
// the lapsed node itself, not its subtree (lifecycle_memory.go); wiring the case into the shared
// runner waits on that implementation matching the SQL one.
func TestPGExpireRetainedMovesEachOwnersBytes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	ownerA := newUser(t, pool)
	ownerB := newUser(t, pool)

	var dirID, fileBID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		root, err := tx.EnsureRoot(ctx, ownerA)
		if err != nil {
			return err
		}
		dir, err := tx.CreateDir(ctx, store.NewNode{OwnerID: ownerA, ParentID: root.ID, Name: "delivery"})
		if err != nil {
			return err
		}
		dirID = dir.ID
		// Both owners carry bytes under the lapsed directory. A dir itself is always
		// size 0, so A needs a file of their own here: with only B's bytes in the
		// subtree, a query that consumed just the first owner's row would pass or fail
		// depending on the order GROUP BY happened to return.
		if _, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: ownerA, ParentID: dirID, Name: "own.bin", Size: 512,
			Mime: "application/octet-stream", ETag: "etag-a",
			BlobID: newBlob(t, pool), BlobHash: [32]byte{9},
		}); err != nil {
			return err
		}
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: ownerB, ParentID: dirID, Name: "guest.bin", Size: 2048,
			Mime: "application/octet-stream", ETag: "etag-b",
			BlobID: newBlob(t, pool), BlobHash: [32]byte{4},
		})
		fileBID = f.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Stamp the retention the way a delivered share does, and lapse it. NewNode has no
	// RetainUntil because retention arrives with SHARE-05 deliveries, not with mkdir; the
	// column is what ExpireRetained selects on.
	if _, err := pool.Exec(ctx,
		`UPDATE nodes SET retain_until = now() - interval '1 hour' WHERE id = $1`, dirID); err != nil {
		t.Fatal(err)
	}

	readInt := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	usedB0 := readInt(`SELECT used_bytes FROM users WHERE id = $1`, ownerB)
	trashB0 := readInt(`SELECT trash_bytes FROM users WHERE id = $1`, ownerB)
	usedA0 := readInt(`SELECT used_bytes FROM users WHERE id = $1`, ownerA)
	trashA0 := readInt(`SELECT trash_bytes FROM users WHERE id = $1`, ownerA)

	var expired int
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.ExpireRetained(ctx)
		expired = n
		return err
	}); err != nil {
		t.Fatalf("ExpireRetained: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired %d nodes, want 1", expired)
	}

	// B's file went to the trash with the lapsed directory: it must not be live under a
	// trashed ancestor, where no listing reaches it and no purge follows it.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.Node(ctx, fileBID)
		return err
	}); err == nil {
		t.Fatal("B's file stayed live after its directory's retention expired; a trashed " +
			"ancestor with live children below it is unreachable from every listing")
	}

	if got := readInt(`SELECT used_bytes FROM users WHERE id = $1`, ownerB); got != usedB0-2048 {
		t.Fatalf("B's used_bytes %d, want %d: the expiry moved B's bytes out of the logical "+
			"quota, whoever stamped the retention on the folder", got, usedB0-2048)
	}
	if got := readInt(`SELECT trash_bytes FROM users WHERE id = $1`, ownerB); got != trashB0+2048 {
		t.Fatalf("B's trash_bytes %d, want %d: the bytes must land in B's retained counter, or "+
			"the later purge subtracts from a counter that was never credited", got, trashB0+2048)
	}
	if got := readInt(`SELECT used_bytes FROM users WHERE id = $1`, ownerA); got != usedA0-512 {
		t.Fatalf("A's used_bytes %d, want %d: A's own bytes under the lapsed directory must move too — "+
			"a subtree sum that answered for one owner only leaves the other behind", got, usedA0-512)
	}
	if got := readInt(`SELECT trash_bytes FROM users WHERE id = $1`, ownerA); got != trashA0+512 {
		t.Fatalf("A's trash_bytes %d, want %d", got, trashA0+512)
	}
}
