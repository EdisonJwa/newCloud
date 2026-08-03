package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of the job queue.

// ClaimJobs leases due jobs to this worker.
//
// The `FOR UPDATE SKIP LOCKED` is the whole concurrency design: two workers asking at
// the same moment walk past each other's locked rows instead of queueing behind them,
// so adding a replica increases throughput rather than contention.
//
// Note what is *not* done here: the lease is not renewed by a background goroutine.
// A job holds its lease for `lease`, and if it takes longer the row becomes claimable
// again while the first worker is still running it. That is deliberate — a stuck job
// must not block the queue forever — and it is why handlers must be idempotent, which
// is exactly the property INV-06 asks for.
func (t *pgTx) ClaimJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]QueuedJob, error) {
	if limit <= 0 {
		limit = 10
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}

	const q = `
		WITH due AS (
		    SELECT id FROM jobs
		    WHERE completed_at IS NULL
		      AND run_after <= now()
		      AND attempts < max_attempts
		      AND (locked_until IS NULL OR locked_until < now())
		    ORDER BY run_after
		    LIMIT $1
		    FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs j
		SET locked_until = now() + $2::interval,
		    locked_by = $3,
		    attempts = j.attempts + 1
		FROM due
		WHERE j.id = due.id
		RETURNING j.id, j.kind, j.payload, j.attempts, j.max_attempts`

	rows, err := t.tx.Query(ctx, q, limit, intervalOf(lease), workerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []QueuedJob
	for rows.Next() {
		var j QueuedJob
		if err := rows.Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts, &j.MaxAttempts); err != nil {
			return nil, translate(err)
		}
		out = append(out, j)
	}
	return out, translate(rows.Err())
}

// CompleteJob marks a job done and clears its lease.
func (t *pgTx) CompleteJob(ctx context.Context, id int64) error {
	const q = `UPDATE jobs
		SET completed_at = now(), locked_until = NULL, locked_by = NULL, last_error = NULL
		WHERE id = $1 AND completed_at IS NULL`
	tag, err := t.tx.Exec(ctx, q, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoJob
	}
	return nil
}

// FailJob records the error and decides whether to retry.
//
// Attempts were already incremented at claim time, so the comparison here is against
// the value stored on the row: `attempts >= max_attempts` means this was the last try.
func (t *pgTx) FailJob(ctx context.Context, id int64, cause error, permanent bool) (bool, error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	// Read the attempt count to compute the backoff, then update. Both happen in the
	// caller's transaction, so the row cannot change between them.
	var attempts, maxAttempts int
	err := t.tx.QueryRow(ctx,
		`SELECT attempts, max_attempts FROM jobs WHERE id = $1`, id).Scan(&attempts, &maxAttempts)
	if err != nil {
		return false, translate(err)
	}

	if permanent || attempts >= maxAttempts {
		// Exhausted: keep the row and its error so the failure is inspectable, but
		// release the lease so it stops being claimed.
		//
		// A *permanent* failure is made terminal by raising attempts to max_attempts.
		// Merely clearing the lease is not enough: the row would stay claimable because
		// it still had budget left, and the job would be retried after all — the
		// opposite of what calling it permanent means.
		if permanent && attempts < maxAttempts {
			_, uerr := t.tx.Exec(ctx,
				`UPDATE jobs SET locked_until = NULL, locked_by = NULL, last_error = $2,
				     max_attempts = $3 WHERE id = $1`,
				id, msg, attempts)
			if uerr != nil {
				return false, translate(uerr)
			}
			return false, nil
		}

		_, uerr := t.tx.Exec(ctx,
			`UPDATE jobs SET locked_until = NULL, locked_by = NULL, last_error = $2 WHERE id = $1`,
			id, msg)
		if uerr != nil {
			return false, translate(uerr)
		}
		return false, nil
	}

	_, uerr := t.tx.Exec(ctx, `UPDATE jobs
		SET locked_until = NULL, locked_by = NULL, last_error = $2,
		    run_after = now() + $3::interval
		WHERE id = $1`, id, msg, intervalOf(jobBackoff(attempts)))
	if uerr != nil {
		return false, translate(uerr)
	}
	return true, nil
}

// ReapExpiredLeases clears expired leases.
//
// Claiming already ignores an expired lease, so this is mostly observability: a count
// that keeps rising means jobs are dying mid-flight, which the queue can recover from
// but an operator should know about.
func (t *pgTx) ReapExpiredLeases(ctx context.Context) (int, error) {
	tag, err := t.tx.Exec(ctx,
		`UPDATE jobs SET locked_until = NULL, locked_by = NULL
		 WHERE completed_at IS NULL AND locked_until IS NOT NULL AND locked_until < now()`)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// JobCounts reports the queue by state.
func (t *pgTx) JobCounts(ctx context.Context) (pending, running, failed int, err error) {
	const q = `
		SELECT
		  count(*) FILTER (WHERE completed_at IS NULL AND attempts < max_attempts AND (locked_until IS NULL OR locked_until < now())) AS pending,
		  count(*) FILTER (WHERE completed_at IS NULL AND attempts < max_attempts AND locked_until IS NOT NULL AND locked_until >= now()) AS running,
		  count(*) FILTER (WHERE completed_at IS NULL AND attempts >= max_attempts) AS failed
		FROM jobs`
	if scanErr := t.tx.QueryRow(ctx, q).Scan(&pending, &running, &failed); scanErr != nil {
		if scanErr == pgx.ErrNoRows {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, translate(scanErr)
	}
	return pending, running, failed, nil
}

var _ JobQueueTx = (*pgTx)(nil)
var _ = fmt.Sprintf
