package worker

// Job consumer (INV-06).
//
// Until now nothing read the jobs table: Enqueue was called only from a test, so any
// operation that queued follow-up work queued it into a void. The schema had leases,
// attempt counting and dead-letter columns that described a capability the process did
// not have.
//
// The design points that matter:
//
//   - Handlers are registered by kind, so adding work is a registration rather than a
//     switch statement in the loop.
//   - An unknown kind is a permanent error, not a retry: retrying it burns five
//     attempts over an hour to arrive at the same answer.
//   - Every handler must be idempotent, because a lease can expire while a slow handler
//     is still running and the job will be claimed again. That is stated here because
//     it is the property the queue is built around, not a caveat.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// JobHandler runs one job's work.
//
// Returning an error retries the job (up to its attempt limit) with backoff. Returning
// a PermanentError stops retrying immediately.
type JobHandler func(ctx context.Context, payload map[string]any) error

// PermanentError marks a job that cannot succeed by trying again.
type PermanentError struct {
	Reason string
}

func (e PermanentError) Error() string { return "permanent: " + e.Reason }

// Permanent wraps an error so the queue records it and stops.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return PermanentError{Reason: err.Error()}
}

// jobWorkerID identifies this process in a job's lease.
//
// It includes a random component so two processes that happen to start in the same
// millisecond are still distinguishable: a lease stamped with the wrong worker id makes
// "which process left this job stuck" unanswerable.
func jobWorkerID() string {
	return fmt.Sprintf("%s-%s", hostnameOrUnknown(), uuid.New().String()[:8])
}

// registerJobs wires the job kinds this build knows how to run.
func (w *Worker) registerJobs() {
	w.handlers = map[string]JobHandler{
		// A blob that was written but whose metadata transaction rolled back leaves
		// bytes nothing references. The periodic sweep handles the common case; this
		// job exists so a caller can ask for one specific blob to be considered for
		// collection promptly instead of waiting up to six hours.
		"blob.collect": func(ctx context.Context, payload map[string]any) error {
			id, ok := payload["blob_id"].(string)
			if !ok || id == "" {
				return Permanent(fmt.Errorf("blob.collect: missing blob_id"))
			}
			blobID, err := uuid.Parse(id)
			if err != nil {
				return Permanent(fmt.Errorf("blob.collect: bad blob_id %q: %w", id, err))
			}
			return w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				// DeleteBlob refuses when the row is still referenced, which is the
				// check that makes this safe to run speculatively.
				derr := tx.DeleteBlob(ctx, blobID)
				if derr == store.ErrNotFound {
					return nil // already gone, or still referenced: nothing to do
				}
				return derr
			})
		},

		// Thumbnails are generated on demand today. This kind exists so a warmer can be
		// queued for a directory without blocking the upload that triggered it.
		"thumbnail.warm": func(ctx context.Context, payload map[string]any) error {
			// Nothing to do yet: the generator is content-addressed and cheap to warm
			// lazily. Registered rather than omitted so a producer that queues it does
			// not create a dead-letter row.
			return nil
		},

		// A user who is disabled must have their sessions and app passwords revoked.
		// DisableUser does this synchronously; the job exists for the case where the
		// disable is decided by a sync process that should not hold a transaction open
		// across every credential store.
		"user.purge_credentials": func(ctx context.Context, payload map[string]any) error {
			id, ok := payload["user_id"].(string)
			if !ok || id == "" {
				return Permanent(fmt.Errorf("user.purge_credentials: missing user_id"))
			}
			userID, err := uuid.Parse(id)
			if err != nil {
				return Permanent(fmt.Errorf("user.purge_credentials: bad user_id %q: %w", id, err))
			}
			return w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				if err := tx.RevokeUserSessions(ctx, userID.String()); err != nil {
					return err
				}
				_, rerr := tx.RevokeUserAppPasswords(ctx, userID)
				return rerr
			})
		},
	}
}

// ProcessJobs runs one claim/execute/finish round.
//
// It returns the number of jobs handled, so the loop and the tests can both observe
// progress without inspecting the queue.
func (w *Worker) ProcessJobs(ctx context.Context) (int, error) {
	// Claim and finish are separate transactions on purpose. Holding one transaction
	// across a handler would keep row locks for the handler's whole duration, and a
	// slow handler would then block every other worker's claims.
	var claimed []store.QueuedJob
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		jobs, cerr := tx.ClaimJobs(ctx, w.workerID, w.jobBatch, w.jobLease)
		claimed = jobs
		return cerr
	})
	if err != nil {
		return 0, fmt.Errorf("worker: claim jobs: %w", err)
	}
	// Publish the queue depth even when nothing was claimed: the interesting signal is
	// often a *deep* queue, and a worker that only reported on activity would go quiet
	// exactly when the backlog builds.
	if w.metrics != nil {
		var pending, running, failed int
		_ = w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			p, r, f, cerr := tx.JobCounts(ctx)
			pending, running, failed = p, r, f
			return cerr
		})
		w.metrics.SetJobs(pending, running, failed)
	}

	if len(claimed) == 0 {
		return 0, nil
	}

	handled := 0
	for _, job := range claimed {
		if err := ctx.Err(); err != nil {
			// Shutting down: leave the rest leased. Their leases expire and another
			// worker (or this one after a restart) picks them up. Returning the
			// already-handled count keeps the caller's accounting honest.
			return handled, nil
		}

		handler, known := w.handlers[job.Kind]
		var runErr error
		switch {
		case !known:
			runErr = Permanent(fmt.Errorf("no handler for job kind %q", job.Kind))
		default:
			runErr = handler(ctx, job.Payload)
		}

		if runErr != nil {
			var permanent PermanentError
			isPermanent := errors.As(runErr, &permanent)
			if ferr := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				retrying, ferr := tx.FailJob(ctx, job.ID, runErr, isPermanent)
				if ferr != nil {
					return ferr
				}
				if retrying {
					w.log.Warn("job failed, will retry",
						"job", job.ID, "kind", job.Kind,
						"attempt", job.Attempts, "of", job.MaxAttempts, "error", runErr)
				} else {
					w.log.Error("job failed permanently",
						"job", job.ID, "kind", job.Kind,
						"attempts", job.Attempts, "error", runErr)
				}
				return nil
			}); ferr != nil {
				return handled, fmt.Errorf("worker: record failure of job %d: %w", job.ID, ferr)
			}
			continue
		}

		if cerr := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.CompleteJob(ctx, job.ID)
		}); cerr != nil {
			return handled, fmt.Errorf("worker: complete job %d: %w", job.ID, cerr)
		}
		handled++
		if w.log != nil {
			w.log.Debug("job completed", "job", job.ID, "kind", job.Kind)
		}
	}
	return handled, nil
}

// hostnameOrUnknown is used in the worker id; a bare uuid would be harder to attribute
// when reading the locked_by column during an incident.
func hostnameOrUnknown() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "worker"
	}
	return h
}
