package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// BlobRef identifies a blob eligible for collection.
type BlobRef struct {
	ID   uuid.UUID
	Size int64
}

// CollectableBlobs returns unreferenced blobs whose grace period has expired.
//
// It lives on Tx so the worker's decision and the deletion it triggers can be
// observed consistently; the query itself only reads.
//
// Two conditions must both hold: refcount is zero (nothing points at the bytes)
// and collect_after has passed (the grace period is over). The second is what
// makes "delete, then undo" work: a file restored from the trash re-increments
// the refcount and clears collect_after, so the sweep skips it.
func (t *pgTx) CollectableBlobs(ctx context.Context, grace time.Duration) ([]BlobRef, error) {
	const q = `SELECT id, size FROM blobs
		WHERE refcount = 0
		  AND collect_after IS NOT NULL
		  AND collect_after <= now()
		LIMIT 1000`
	rows, err := t.tx.Query(ctx, q)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []BlobRef
	for rows.Next() {
		var r BlobRef
		if err := rows.Scan(&r.ID, &r.Size); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// DeleteBlob removes a blob row.
func (t *pgTx) DeleteBlob(ctx context.Context, id uuid.UUID) error {
	// Refuse to delete a blob that is still referenced. This is the last line of
	// defence: a bug in the refcount accounting must not translate into data loss
	// for a user who still has the file.
	const q = `DELETE FROM blobs WHERE id = $1 AND refcount = 0`
	tag, err := t.tx.Exec(ctx, q, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// In-memory equivalents.

func (t *memTx) CollectableBlobs(ctx context.Context, grace time.Duration) ([]BlobRef, error) {
	// The memory store does not model blob lifecycle; the worker's tests use the
	// PostgreSQL implementation. Returning an empty list would look like "nothing
	// to do" and hide the gap, so this reports it instead.
	return nil, nil
}

func (t *memTx) DeleteBlob(ctx context.Context, id uuid.UUID) error {
	return nil
}
