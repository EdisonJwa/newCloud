package store

import (
	"context"
	"time"
)

// In-memory implementation of the job queue.
//
// It follows the same rules as the SQL: a lease expires, attempts are counted at claim
// time, and an exhausted job stays in the queue as failed rather than disappearing.
// The shared suite runs both, because a queue that behaves differently in tests is a
// queue nobody has actually tested.

func (t *memTx) ClaimJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]QueuedJob, error) {
	if limit <= 0 {
		limit = 10
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	now := t.m.now()

	var out []QueuedJob
	for i := range t.m.jobs {
		if len(out) >= limit {
			break
		}
		j := &t.m.jobs[i]
		if j.CompletedAt != nil {
			continue
		}
		if j.Attempts >= j.MaxAttempts {
			continue
		}
		if j.RunAfter.After(now) {
			continue
		}
		if j.LockedUntil != nil && j.LockedUntil.After(now) {
			continue
		}

		// Claim: count the attempt now, not on completion, so a worker that dies
		// before reporting anything still uses up an attempt.
		j.Attempts++
		until := now.Add(lease)
		j.LockedUntil = &until
		j.LockedBy = workerID

		maxAttempts := j.MaxAttempts
		if maxAttempts == 0 {
			maxAttempts = 1
		}
		out = append(out, QueuedJob{
			ID: j.ID, Kind: j.Kind, Payload: copyPayload(j.Payload),
			Attempts: j.Attempts, MaxAttempts: maxAttempts,
		})
	}
	return out, nil
}

func (t *memTx) CompleteJob(ctx context.Context, id int64) error {
	for i := range t.m.jobs {
		j := &t.m.jobs[i]
		if j.ID != id || j.CompletedAt != nil {
			continue
		}
		now := t.m.now()
		j.CompletedAt = &now
		j.LockedUntil = nil
		j.LockedBy = ""
		j.LastError = ""
		return nil
	}
	return ErrNoJob
}

func (t *memTx) FailJob(ctx context.Context, id int64, cause error, permanent bool) (bool, error) {
	for i := range t.m.jobs {
		j := &t.m.jobs[i]
		if j.ID != id {
			continue
		}
		if cause != nil {
			j.LastError = cause.Error()
		}
		j.LockedUntil = nil
		j.LockedBy = ""

		maxAttempts := j.MaxAttempts
		if maxAttempts == 0 {
			maxAttempts = 1
		}
		if permanent && j.Attempts < maxAttempts {
			// Make it terminal: leaving budget on the row would keep it claimable and
			// the "permanent" failure would be retried anyway.
			j.MaxAttempts = j.Attempts
		}
		if permanent || j.Attempts >= maxAttempts {
			return false, nil
		}
		j.RunAfter = t.m.now().Add(jobBackoff(j.Attempts))
		return true, nil
	}
	return false, ErrNoJob
}

func (t *memTx) ReapExpiredLeases(ctx context.Context) (int, error) {
	now := t.m.now()
	n := 0
	for i := range t.m.jobs {
		j := &t.m.jobs[i]
		if j.CompletedAt != nil || j.LockedUntil == nil {
			continue
		}
		if j.LockedUntil.Before(now) {
			j.LockedUntil = nil
			j.LockedBy = ""
			n++
		}
	}
	return n, nil
}

func (t *memTx) JobCounts(ctx context.Context) (pending, running, failed int, err error) {
	now := t.m.now()
	for i := range t.m.jobs {
		j := &t.m.jobs[i]
		if j.CompletedAt != nil {
			continue
		}
		maxAttempts := j.MaxAttempts
		if maxAttempts == 0 {
			maxAttempts = 1
		}
		switch {
		case j.Attempts >= maxAttempts:
			failed++
		case j.LockedUntil != nil && j.LockedUntil.After(now):
			running++
		default:
			pending++
		}
	}
	return pending, running, failed, nil
}

// copyPayload detaches a job's payload from the caller's map.
//
// Without it, a worker that mutated the payload it was handed would change the stored
// row, which the SQL implementation cannot do — the two would diverge exactly in the
// case where a handler is careless.
func copyPayload(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

var _ JobQueueTx = (*memTx)(nil)
