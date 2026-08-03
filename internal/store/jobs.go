package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Background job queue (INV-06).
//
// The table has existed since the first migration with everything a durable queue
// needs: a run-after time, attempt counting, a max, a lease (`locked_until` /
// `locked_by`) and a dedupe key. Nothing ever read it. `Enqueue` was called only from a
// test, so any operation that queued follow-up work queued it into a void — the schema
// described a capability the process did not have.
//
// The semantics here are chosen so the queue is safe to run from several processes,
// which the project's "the code does not assume it is the only process" rule requires:
//
//   - Claiming is `FOR UPDATE SKIP LOCKED` inside a short transaction, so two workers
//     never take the same row and neither blocks the other.
//   - A claimed job carries a lease. If the worker dies, the lease expires and the job
//     becomes claimable again — that is the recovery path, and it is why the lease is
//     stored rather than held in memory.
//   - Retrying is the default on error, up to max_attempts; after that the job stays
//     in the table with its error and is skipped. Nothing is deleted, so a failure can
//     be inspected later.
//   - Completion is a timestamp rather than a delete, so "did this run" is answerable.

// JobStatus is the queue state of one job, for the admin view and for tests.
type JobStatus string

const (
	// JobPending is claimable now (or scheduled for later).
	JobPending JobStatus = "pending"
	// JobRunning is leased by a worker.
	JobRunning JobStatus = "running"
	// JobFailed has exhausted its attempts.
	JobFailed JobStatus = "failed"
	// JobDone completed.
	JobDone JobStatus = "done"
)

// QueuedJob is a claimed job handed to the worker.
type QueuedJob struct {
	ID          int64
	Kind        string
	Payload     map[string]any
	Attempts    int
	MaxAttempts int
}

// JobQueueTx is the consumer side of the queue.
//
// It is separate from Enqueue (which lives on Tx) because claiming and finishing are
// whole-transaction operations rather than steps inside a caller's business change.
type JobQueueTx interface {
	// ClaimJobs leases up to limit due jobs to this worker.
	//
	// Jobs whose lease has expired are claimable again: that is how a crashed worker's
	// work is recovered without operator intervention.
	ClaimJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]QueuedJob, error)
	// CompleteJob marks a job done.
	CompleteJob(ctx context.Context, id int64) error
	// FailJob records an error and either reschedules with backoff or gives up.
	//
	// permanent stops retrying regardless of the remaining budget: an error that cannot
	// succeed must not consume five attempts over an hour to reach the same conclusion.
	// It returns true when the job will be retried, so the caller can log the difference
	// rather than guessing.
	FailJob(ctx context.Context, id int64, cause error, permanent bool) (retrying bool, err error)
	// ReapExpiredLeases releases leases held past their expiry and counts them.
	//
	// Releasing is implicit in claiming (an expired lease is claimable), so this is
	// for observability: a rising count means workers are dying mid-job.
	ReapExpiredLeases(ctx context.Context) (int, error)
	// JobCounts reports the queue by state, for /readyz and the admin view.
	JobCounts(ctx context.Context) (pending, running, failed int, err error)
}

// jobBackoff is how long a failed job waits before its next attempt.
//
// Exponential but capped: a transient database blip should retry in seconds, while a
// permanently broken job must not turn into a hot loop. The cap is deliberately short
// enough that a fix deployed after noticing the failure is picked up quickly.
func jobBackoff(attempts int) time.Duration {
	base := 30 * time.Second
	for i := 1; i < attempts && base < 30*time.Minute; i++ {
		base *= 2
	}
	if base > 30*time.Minute {
		return 30 * time.Minute
	}
	return base
}

// ErrNoJob is returned when a completion targets a job that is not leased by the caller.
var ErrNoJob = errors.New("store: job not found or not held")

var _ = uuid.Nil
