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
// the grace period has passed, and no version row still
// names the blob. The version check is belt-and-braces against a refcount that
// drifted: a version is a live reference to content, and collecting bytes a
// historical version needs would turn "restore an old version" into a permanent
// error for that file.
//
// The clock being checked is COALESCE(collect_after, created_at + grace). A row that
// dropped its last reference carries collect_after, set at the moment the count reached
// zero. A row that was NEVER referenced has none — BlobIDFor creates it before the node
// that will reference it exists, and a publish that fails in between (quota 507, If-Match
// 412, a name fold 409) leaves refcount 0 and collect_after NULL forever, immune to the
// collector. Ageing those orphans by the row's own creation, with the same grace the
// caller passes for dropped references, is what makes them collectable at all; the grace
// is what keeps the sweep from racing the upload that is about to reference the row.
func (t *pgTx) CollectableBlobs(ctx context.Context, grace time.Duration) ([]BlobRef, error) {
	const q = `SELECT b.id, b.size FROM blobs b
		WHERE b.refcount = 0
		  AND COALESCE(b.collect_after, b.created_at + $1::interval) <= now()
		  AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.blob_id = b.id)
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.blob_id = b.id)
		LIMIT 1000`
	rows, err := t.tx.Query(ctx, q, intervalOf(grace))
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
	// PostgreSQL implementation.
	//
	// This reports the gap rather than returning an empty list. The comment said that for
	// several rounds while the code returned (nil, nil) — so SweepBlobs read it as "nothing to
	// collect" and returned successfully, which is precisely the silent no-op the comment
	// promised to avoid. A caller that cannot tell "no garbage" from "cannot tell" reports a
	// clean sweep on a backend that never looked.
	return nil, errNotImplemented
}

func (t *memTx) DeleteBlob(ctx context.Context, id uuid.UUID) error {
	// The memory store keeps no blob rows — only a content-address-to-id map, with no
	// refcount, size or creation time — so there is no row to delete and nothing to check
	// "still referenced" against, which is the guard the PostgreSQL version refuses to skip.
	// It used to return nil, making this the one call in the GC pair that lied on this
	// backend: a sweep would count bytes reclaimed that no backend ever dropped. Like
	// CollectableBlobs above, it reports the gap instead of pretending success.
	return errNotImplemented
}
