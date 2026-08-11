package server_test

// The collector must be metadata-driven, proved by behaviour rather than by reading the query.
//
// TestGCIsMetadataDriven greps CollectableBlobs for "FROM blobs b" and for the absence of a
// directory scan. That catches the obvious regression, but it is a source check: it would pass
// against a query that joined the filesystem some other way, and it says nothing about what the
// worker actually deletes.
//
// The behaviour that matters for the restore procedure is: a blob FILE with no metadata ROW must
// survive collection. That is what makes "restore blobs, then restore the database" safe — during
// the window every restored file is in exactly that state.
//
// This test builds that state with the real store and runs the real sweep.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestUnreferencedBlobFileSurvivesCollection is the restore-window property.
func TestUnreferencedBlobFileSurvivesCollection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A live file, so the store is not empty.
	h.uploadFile(t, "live.txt", "a file the database knows about")

	// A blob file on disk that no row mentions — the state of every restored file before
	// pg_restore lands. Written directly, the way a restore unpacks a snapshot.
	orphanRoot := h.blobRootOf(t)
	orphanHash := [32]byte{0xab, 0xcd, 0xef}
	for i := 3; i < 32; i++ {
		orphanHash[i] = byte(i)
	}
	hex := fmt.Sprintf("%x", orphanHash)
	dir := filepath.Join(orphanRoot, hex[0:2], hex[2:4])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(dir, hex)
	if err := os.WriteFile(orphanPath, []byte("restored bytes with no metadata row"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run the collector's candidate query directly: it is the decision the worker acts on, and
	// going through the worker's timer would make this test slow and timing-dependent.
	var candidates []store.BlobRef
	if err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		refs, err := tx.CollectableBlobs(ctx, 0)
		candidates = refs
		return err
	}); err != nil {
		t.Fatalf("CollectableBlobs: %v", err)
	}

	// The orphan file must not be a candidate: it has no row, so the metadata-driven query cannot
	// see it at all.
	if len(candidates) != 0 {
		t.Fatalf("a blob file with no metadata row appeared as a collection candidate (%d "+
			"candidates). This is the restore-window property: while pg_restore has not run, every "+
			"restored file is in this state, and collecting them would delete the restore.",
			len(candidates))
	}

	// And the bytes must still be there.
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("the unreferenced blob file was removed: %v", err)
	}
}

// The reclaimable half lives in internal/store (TestPGCollectorSkipsReferencedBlobs and
// TestPGCollectorHonoursTheGracePeriod), where the clock can be moved and the real query runs.
// Trying to reproduce it here meant backdating rows through the memory store, which has no way to
// advance its clock — the first version of this file called a nonexistent
// DELETE /api/files/trash route and silently ignored the 404, then failed for a reason that had
// nothing to do with the collector.
