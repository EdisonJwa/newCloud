package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PostgreSQL implementation of ShareRateTx.

// AllowShareUpload enforces a share's upload rate as one statement.
//
// The shape is deliberate:
//
//	WITH recent AS (SELECT count(*) ... FOR UPDATE-ish window)
//	INSERT ... SELECT ... WHERE count < rate
//
// The INSERT can only produce a row when the window has room, so "count then insert" cannot
// interleave with another request between the two. Doing it in Go would be two statements with a
// gap between them, and the gap is exactly where a limit of 10 becomes 20.
//
// Locking: the count reads share_upload_events for this share. Rather than serialise all uploads to
// a share behind an explicit lock — which would make a busy drop box slower than the rate it is
// enforcing — the statement takes a transaction-scoped advisory lock on the share id. That
// serialises only the decision, not the upload, and only for the same share.
func (t *pgTx) AllowShareUpload(ctx context.Context, id uuid.UUID, rate int64, window time.Duration,
	ip string, bytes int64) error {

	if rate <= 0 {
		// No rate limit configured. Still record the event? No: an unconfigured share should not
		// grow a table for a limit nobody asked for.
		return nil
	}

	// The advisory lock is what makes count-then-insert safe across concurrent requests without
	// holding a row lock for the duration of an upload. hashtext gives a stable int from the uuid.
	if _, err := t.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text)::bigint)`,
		id.String()); err != nil {
		return translate(err)
	}

	const q = `
		INSERT INTO share_upload_events (share_id, ip, bytes)
		SELECT $1, $2, $4
		WHERE (SELECT count(*) FROM share_upload_events
		        WHERE share_id = $1 AND occurred_at > now() - $3::interval) < $5`
	tag, err := t.tx.Exec(ctx, q, id, nullableString(ip), intervalOf(window), bytes, rate)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrShareRateLimited
	}
	return nil
}

// PruneShareUploadEvents deletes rows older than the longest window in use.
func (t *pgTx) PruneShareUploadEvents(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM share_upload_events
		WHERE occurred_at <= now() - $1::interval`, intervalOf(olderThan))
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}
