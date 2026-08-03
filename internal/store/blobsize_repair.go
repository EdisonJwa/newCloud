package store

// RepairBlobSizes backfills blobs.size from the content store (INV-07).
//
// `BlobIDFor` used to hardcode size = 0 in its INSERT, and had no parameter to pass the real
// length. Nothing user-visible broke, because node rows carry their own size — but two consumers
// read blobs.size: the GC's reclaimed-bytes accounting, and an operator inspecting the table to
// understand disk usage. Both were told every blob was empty.
//
// The obvious conclusion is that the historical rows are unrecoverable, and last round's notes
// said so. That was wrong. The objects are content-addressed and still on disk, so the real
// length is one `Stat` away: the hash names the object, and the object knows its size. This
// function is the correction.
//
// It is deliberately not a migration. A migration runs inside one database transaction and
// cannot call `os.Stat`, and the repair must tolerate objects that are genuinely missing (a blob
// row whose bytes never landed is a separate, reportable condition rather than a reason to abort
// the whole backfill).

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

// BlobSizeRepair reports what a backfill changed.
type BlobSizeRepair struct {
	// Scanned is how many blob rows were examined.
	Scanned int
	// Fixed is how many had a size that disagreed with the stored object.
	Fixed int
	// TotalFixedBytes is the sum of the sizes that were recovered, for the operator log.
	TotalFixedBytes int64
	// Missing lists blobs whose bytes could not be found. These are reported rather than
	// repaired: a blob row pointing at content that is not there is a real integrity problem,
	// and overwriting its size would hide it.
	Missing []string
	// BackendError is set when the content store could not be reached at all, which is a
	// different condition from an individual object being absent.
	BackendError error
}

// RepairBlobSizes reads every blob row and corrects its size from the stored object.
//
// stat is passed in rather than the store being opened here: the metadata layer must not depend
// on which storage backend is configured, and this keeps the check identical for local disk and
// S3 (both implement blob.Store.Stat).
//
// Missing objects do not abort the run. A single absent blob should be reported by name so an
// operator can investigate it; failing the whole backfill because one object is gone would leave
// the other thousand rows wrong.
func RepairBlobSizes(ctx context.Context, pool *pgxpool.Pool, stat func(context.Context, blob.Hash) (blob.Info, error)) (BlobSizeRepair, error) {
	var rep BlobSizeRepair

	rows, err := pool.Query(ctx, `SELECT id, sha256, size FROM blobs ORDER BY created_at, id`)
	if err != nil {
		return rep, fmt.Errorf("store: read blobs for size repair: %w", err)
	}

	type blobRow struct {
		id   string
		hash []byte
		size int64
	}
	var all []blobRow
	for rows.Next() {
		var b blobRow
		if err := rows.Scan(&b.id, &b.hash, &b.size); err != nil {
			rows.Close()
			return rep, fmt.Errorf("store: scan blob for size repair: %w", err)
		}
		all = append(all, b)
		rep.Scanned++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	for _, b := range all {
		if len(b.hash) != 32 {
			rep.Missing = append(rep.Missing,
				fmt.Sprintf("%s: malformed content address (%d bytes)", b.id, len(b.hash)))
			continue
		}
		var h blob.Hash
		copy(h[:], b.hash)

		info, serr := stat(ctx, h)
		if serr != nil {
			// A missing object is not a backend failure. Conflating the two made the operator log
			// say "the content store reported errors" when the store was perfectly healthy and a
			// single row was dangling — the exact misdiagnosis this function exists to avoid.
			//
			// An unreachable store shows up as *every* object failing; that is distinguishable
			// from a dangling row by counting, which is why the classification happens below
			// rather than here.
			rep.Missing = append(rep.Missing,
				fmt.Sprintf("%s (%s): %v", b.id, h.String()[:12], serr))
			continue
		}

		if info.Size == b.size {
			continue
		}

		if _, uerr := pool.Exec(ctx, `UPDATE blobs SET size = $2 WHERE id = $1::uuid`, b.id, info.Size); uerr != nil {
			return rep, fmt.Errorf("store: update blob size for %s: %w", b.id, uerr)
		}
		rep.Fixed++
		rep.TotalFixedBytes += info.Size
	}

	// Classify after the loop: if nothing could be stat'd at all, the store is unreachable and
	// that is a different condition from a handful of dangling rows. Reporting a healthy store as
	// broken sends an operator to check credentials and endpoints that are fine.
	if rep.Scanned > 0 && len(rep.Missing) == rep.Scanned {
		rep.BackendError = fmt.Errorf("no blob could be stat'd (%d attempted); the content store "+
			"is likely unreachable or pointed at the wrong root", rep.Scanned)
	}

	return rep, nil
}
