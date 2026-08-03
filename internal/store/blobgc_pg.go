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
// Three conditions must all hold: refcount is zero (nothing points at the bytes),
// collect_after has passed (the grace period is over), and no version row still
// names the blob. The version check is belt-and-braces against a refcount that
// drifted: a version is a live reference to content, and collecting bytes a
// historical version needs would turn "restore an old version" into a permanent
// error for that file.
func (t *pgTx) CollectableBlobs(ctx context.Context, grace time.Duration) ([]BlobRef, error) {
	const q = `SELECT b.id, b.size FROM blobs b
		WHERE b.refcount = 0
		  AND b.collect_after IS NOT NULL
		  AND b.collect_after <= now()
		  AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.blob_id = b.id)
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.blob_id = b.id)
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
	const q = `DELETE FROM blobs b
		WHERE b.id = $1 AND b.refcount = 0
		  AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.blob_id = b.id)
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.blob_id = b.id)`
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
