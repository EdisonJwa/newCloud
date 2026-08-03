package store_test

// blobs.size must record the real content length.
//
// It was hardcoded to 0 in both BlobIDFor and BlobID. Nothing user-visible broke, because the
// node rows carry their own size — which is exactly why it survived: the only consumers are the
// GC's reclaimed-bytes accounting and an operator reading the table to understand disk usage.
// Both were being told every blob was empty.
//
// Found by running the restore drill and noticing `blobs` rows with size 0 next to nodes claiming
// 512 bytes.

import (
	"context"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestBlobRowRecordsContentSize.
func TestBlobRowRecordsContentSize(t *testing.T) {
	pool := repairTestPool(t)
	s := store.NewPG(pool)
	ctx := context.Background()

	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i)
	}
	const size = 4096

	// Clean up before asserting, not after: a failing assertion would otherwise leave the row
	// behind and the next run would see a stale size. That exact problem made a failure look
	// like flakiness in an earlier round.
	id := s.BlobIDFor(ctx, hash, size)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM blobs WHERE sha256 = $1`, hash[:])
	})
	if id.String() == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("BlobIDFor must return an id")
	}

	var got int64
	if err := pool.QueryRow(ctx, `SELECT size FROM blobs WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read size: %v", err)
	}
	if got != size {
		t.Fatalf("blobs.size = %d, want %d: the row must record the real content length, "+
			"or GC accounting reports every blob as empty", got, size)
	}

	// A second registration of the same content must not corrupt the recorded size.
	if again := s.BlobIDFor(ctx, hash, size); again != id {
		t.Fatalf("the same content address must map to one row, got %v then %v", id, again)
	}
	if err := pool.QueryRow(ctx, `SELECT size FROM blobs WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != size {
		t.Fatalf("re-registering changed the size to %d", got)
	}

}
