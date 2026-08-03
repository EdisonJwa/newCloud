package store_test

// blobs.size backfill (INV-07).
//
// `BlobIDFor` wrote size = 0 for as long as it existed. The earlier note said those rows could
// not be recovered; that was wrong, and these tests pin the correction: the objects are
// content-addressed, so the hash names the object and the object knows its length.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// fakeBlobs answers Stat from a map of hash → size, and can be told to fail.
type fakeBlobs struct {
	sizes map[string]int64
	// failAll simulates an unreachable store.
	failAll bool
}

func (f *fakeBlobs) stat(ctx context.Context, h blob.Hash) (blob.Info, error) {
	if f.failAll {
		return blob.Info{}, errors.New("dial tcp: connection refused")
	}
	size, ok := f.sizes[h.String()]
	if !ok {
		return blob.Info{}, blob.ErrNotFound
	}
	return blob.Info{Hash: h, Size: size}, nil
}

// seedBlobRow inserts a blob row with an arbitrary size and a deterministic hash.
func seedBlobRow(t *testing.T, pool *pgxpool.Pool, seed byte, size int64) (uuid.UUID, blob.Hash) {
	t.Helper()
	var h blob.Hash
	for i := range h {
		h[i] = seed
	}
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO blobs (id, sha256, size, backend) VALUES ($1, $2, $3, 'local')`,
		id, h[:], size); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM blobs WHERE id = $1`, id)
	})
	return id, h
}

// TestRepairBlobSizesRecoversFromDisk is the case the note claimed was impossible.
func TestRepairBlobSizesRecoversFromDisk(t *testing.T) {
	pool := repairTestPool(t)

	// Three rows written by the old code: all claiming to be empty.
	idA, hA := seedBlobRow(t, pool, 0x11, 0)
	idB, hB := seedBlobRow(t, pool, 0x22, 0)
	idC, hC := seedBlobRow(t, pool, 0x33, 0)

	blobs := &fakeBlobs{sizes: map[string]int64{
		hA.String(): 512,
		hB.String(): 700,
		hC.String(): 1024,
	}}

	rep, err := store.RepairBlobSizes(context.Background(), pool, blobs.stat)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fixed != 3 {
		t.Fatalf("want 3 rows fixed, got %d (scanned %d)", rep.Fixed, rep.Scanned)
	}
	if rep.TotalFixedBytes != 512+700+1024 {
		t.Fatalf("recovered bytes = %d, want %d", rep.TotalFixedBytes, 512+700+1024)
	}

	for id, want := range map[uuid.UUID]int64{idA: 512, idB: 700, idC: 1024} {
		var got int64
		if err := pool.QueryRow(context.Background(),
			`SELECT size FROM blobs WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("blob %v size = %d, want %d", id, got, want)
		}
	}
}

// TestRepairBlobSizesIsIdempotent: a second pass must do nothing, or `migrate` rewrites every
// row on every run.
func TestRepairBlobSizesIsIdempotent(t *testing.T) {
	pool := repairTestPool(t)
	_, h := seedBlobRow(t, pool, 0x44, 0)
	blobs := &fakeBlobs{sizes: map[string]int64{h.String(): 100}}

	if _, err := store.RepairBlobSizes(context.Background(), pool, blobs.stat); err != nil {
		t.Fatal(err)
	}
	second, err := store.RepairBlobSizes(context.Background(), pool, blobs.stat)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fixed != 0 {
		t.Fatalf("a second pass must fix nothing, fixed %d", second.Fixed)
	}
}

// TestRepairBlobSizesReportsMissingObjectsWithoutInventingASize.
//
// A blob row whose bytes are gone is an integrity problem. The repair must say so and leave the
// row alone: writing a guessed size would hide the one thing the operator needs to see.
func TestRepairBlobSizesReportsMissingObjectsWithoutInventingASize(t *testing.T) {
	pool := repairTestPool(t)
	id, h := seedBlobRow(t, pool, 0x55, 7) // a size that does not match anything on disk

	// The store returns NotFound for this hash.
	blobs := &fakeBlobs{sizes: map[string]int64{}}

	rep, err := store.RepairBlobSizes(context.Background(), pool, blobs.stat)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) == 0 {
		t.Fatal("a dangling blob row must be reported")
	}
	if rep.Fixed != 0 {
		t.Fatalf("a missing object must not be 'fixed', fixed %d", rep.Fixed)
	}

	// The row keeps its old size rather than being zeroed or guessed.
	var got int64
	if err := pool.QueryRow(context.Background(),
		`SELECT size FROM blobs WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("a missing object must leave the size untouched, got %d", got)
	}
	_ = h
}

// TestRepairBlobSizesDistinguishesUnreachableStore.
//
// A single dangling row is not a backend failure. Conflating them made the operator log claim
// "the content store reported errors" when the store was healthy — sending someone to check
// credentials and endpoints that were fine.
func TestRepairBlobSizesDistinguishesUnreachableStore(t *testing.T) {
	pool := repairTestPool(t)

	// Every object resolves: no backend error may be reported, even though one row is missing.
	_, h := seedBlobRow(t, pool, 0x66, 0)
	healthy := &fakeBlobs{sizes: map[string]int64{h.String(): 64}}
	rep, err := store.RepairBlobSizes(context.Background(), pool, healthy.stat)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackendError != nil {
		t.Fatalf("a healthy store must not be reported as failing: %v", rep.BackendError)
	}

	// Now make every stat fail: that IS a backend error.
	broken := &fakeBlobs{failAll: true}
	rep, err = store.RepairBlobSizes(context.Background(), pool, broken.stat)
	if err != nil {
		t.Fatal(err)
	}
	if rep.BackendError == nil {
		t.Fatal("when every object fails, the store itself is the problem and must be reported")
	}
}
