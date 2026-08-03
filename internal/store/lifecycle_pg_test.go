package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// Lifecycle tests: the retention surface (§8.1, §8.2).
//
// These run against PostgreSQL only, on purpose: retention is enforced by an
// age predicate in SQL, and the in-memory store has no clock injection to age a row
// with. Testing the interesting case means actually making a row old, not simulating
// the decision.

// trashAndAge creates a file, deletes it, and backdates its trashed_at so retention
// considers it expired.
func trashAndAge(t *testing.T, pool *pgxpool.Pool, s store.Store, owner uuid.UUID,
	root uuid.UUID, name string, size int64, age time.Duration) store.Node {

	t.Helper()
	var node store.Node
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root, Name: name, Size: size,
			Mime: "application/octet-stream", ETag: "e", BlobID: newBlob(t, pool),
		})
		node = n
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Trash(ctx, node.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE nodes SET trashed_at = now() - $2::interval WHERE id = $1`,
		node.ID, age.String()); err != nil {
		t.Fatal(err)
	}
	return node
}

// TestPGPurgeTrashIsOptIn: a zero retention must delete nothing. Getting this
// wrong deletes every user's recoverable data on the first sweep.
func TestPGPurgeTrashIsOptIn(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	trashAndAge(t, pool, s, owner, root.ID, "old.bin", 10, 400*24*time.Hour)

	var res store.PurgeResult
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, perr := tx.PurgeTrash(ctx, 0)
		res = got
		return perr
	}); err != nil {
		t.Fatal(err)
	}
	if res.Nodes != 0 {
		t.Fatalf("retention 0 means forever; purged %d nodes", res.Nodes)
	}
}

// TestPGPurgeTrashOnlyExpired: the sweep must not take a deletion the user could
// still undo.
func TestPGPurgeTrashOnlyExpired(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	expired := trashAndAge(t, pool, s, owner, root.ID, "expired.bin", 10, 40*24*time.Hour)
	fresh := trashAndAge(t, pool, s, owner, root.ID, "fresh.bin", 10, time.Minute)

	var res store.PurgeResult
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, perr := tx.PurgeTrash(ctx, 30*24*time.Hour)
		res = got
		return perr
	}); err != nil {
		t.Fatal(err)
	}
	if res.Nodes != 1 || res.Roots != 1 {
		t.Fatalf("want one expired root purged, got nodes=%d roots=%d", res.Nodes, res.Roots)
	}

	// The expired one is gone for good...
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, rerr := tx.NodeIncludingTrashed(ctx, expired.ID)
		return rerr
	}); err == nil {
		t.Fatal("the expired node should have been deleted")
	}
	// ...and the fresh one is still restorable.
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, rerr := tx.Restore(ctx, fresh.ID)
		return rerr
	}); err != nil {
		t.Fatalf("a node inside the window must still restore: %v", err)
	}
}

// TestPGPurgeTrashReleasesQuota: deleted bytes that still count against the quota
// leave a user unable to upload after cleaning up.
func TestPGPurgeTrashReleasesQuota(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	trashAndAge(t, pool, s, owner, root.ID, "big.bin", 4096, 40*24*time.Hour)

	before, err := s.Usage(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if before != 4096 {
		t.Fatalf("the write should have consumed quota, used=%d", before)
	}

	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, perr := tx.PurgeTrash(ctx, 30*24*time.Hour)
		return perr
	}); err != nil {
		t.Fatal(err)
	}

	after, err := s.Usage(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("purging must release the quota, used=%d", after)
	}
}

// TestPGPurgeTrashTakesTheWholeSubtree: purging a directory must not leave its
// children behind as unreachable rows.
func TestPGPurgeTrashTakesTheWholeSubtree(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	var dir, sub, file store.Node
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		d, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: root.ID, Name: "d"})
		if err != nil {
			return err
		}
		dir = d
		sc, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: d.ID, Name: "sub"})
		if err != nil {
			return err
		}
		sub = sc
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: sc.ID, Name: "deep.bin", Size: 8,
			BlobID: newBlob(t, pool),
		})
		if err != nil {
			return err
		}
		file = f
		return tx.Trash(ctx, d.ID)
	}); err != nil {
		t.Fatal(err)
	}
	_ = sub
	if _, err := pool.Exec(context.Background(),
		`UPDATE nodes SET trashed_at = now() - interval '40 days' WHERE id = $1`, dir.ID); err != nil {
		t.Fatal(err)
	}

	var res store.PurgeResult
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, perr := tx.PurgeTrash(ctx, 30*24*time.Hour)
		res = got
		return perr
	}); err != nil {
		t.Fatal(err)
	}
	if res.Nodes != 3 {
		t.Fatalf("want the directory and both descendants deleted, got %d", res.Nodes)
	}
	if res.Roots != 1 {
		t.Fatalf("a subtree is one trash entry, got %d roots", res.Roots)
	}

	var remaining int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM nodes WHERE id = ANY($1)`,
		[]uuid.UUID{dir.ID, sub.ID, file.ID}).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d rows of the purged subtree survived", remaining)
	}
}

// TestPGExpireVersionsKeepsBlobReferencesConsistent guards the INV-07 path: expiring
// a version must not leave a blob refcount that keeps its bytes alive forever.
func TestPGExpireVersionsKeepsBlobReferencesConsistent(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	root, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	var file store.Node
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "v.txt", Size: 5,
			ETag: "e1", BlobID: newBlob(t, pool),
		})
		file = n
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Three overwrites, each keeping a version.
	for i := 0; i < 3; i++ {
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.UpdateFile(ctx, store.UpdateFile{
				NodeID: file.ID, Size: 6, ETag: fmt.Sprintf("e%d", i+2),
				BlobID: newBlob(t, pool), KeepVersion: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	var before int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM versions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("the versions were not recorded")
	}

	// Age every version past the window, then expire.
	if _, err := pool.Exec(context.Background(),
		`UPDATE versions SET created_at = now() - interval '400 days'`); err != nil {
		t.Fatal(err)
	}
	var removed int
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, eerr := tx.ExpireVersions(ctx, 365*24*time.Hour)
		removed = n
		return eerr
	}); err != nil {
		t.Fatal(err)
	}
	if removed != before {
		t.Fatalf("want %d versions expired, got %d", before, removed)
	}

	// Every blob a version used to point at must now be collectable: refcount zero.
	var stillReferenced int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&stillReferenced); err != nil {
		t.Fatal(err)
	}
	if stillReferenced != 1 {
		// Exactly one blob is still the live file's content; the rest are history.
		t.Fatalf("only the current file's blob should remain referenced, got %d", stillReferenced)
	}
}
