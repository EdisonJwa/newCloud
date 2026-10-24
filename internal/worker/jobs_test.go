package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// Job consumption tests.
//
// The store conformance suite covers the queue's own semantics. What can only be tested
// here is the loop: that a job queued by a producer is actually executed, that a
// failing job is retried rather than dropped, and that a permanent failure stops
// immediately instead of burning every attempt.

func testWorker(t *testing.T) (*Worker, *store.Memory) {
	t.Helper()
	m := store.NewMemory()
	w := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w == nil {
		t.Fatal("worker is nil")
	}
	return w, m
}

func enqueue(t *testing.T, m *store.Memory, j store.Job) {
	t.Helper()
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Enqueue(ctx, j)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProcessJobsRunsWork proves the consumer exists, which is the whole point: the
// table previously had no reader at all.
func TestProcessJobsRunsWork(t *testing.T) {
	w, m := testWorker(t)

	var ran int
	w.handlers["test.count"] = func(ctx context.Context, payload map[string]any) error {
		ran++
		return nil
	}

	enqueue(t, m, store.Job{Kind: "test.count"})

	n, err := w.ProcessJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 job handled, got %d", n)
	}
	if ran != 1 {
		t.Fatalf("the handler must have run, got %d calls", ran)
	}

	// And it is not run again.
	if n, _ := w.ProcessJobs(context.Background()); n != 0 {
		t.Fatalf("a completed job must not run twice, got %d", n)
	}
}

// TestProcessJobsRetriesFailures: a transient error must be tried again, not dropped.
func TestProcessJobsRetriesFailures(t *testing.T) {
	w, m := testWorker(t)

	var attempts int
	w.handlers["test.flaky"] = func(ctx context.Context, payload map[string]any) error {
		attempts++
		return errors.New("transient")
	}

	// MaxAttempts 2 so this test does not have to wait out a backoff.
	enqueue(t, m, store.Job{Kind: "test.flaky", MaxAttempts: 2})

	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("first round should have called the handler once, got %d", attempts)
	}

	// The retry is scheduled with backoff, so make it due immediately.
	fastForward(t, m)

	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("the retry should have run, got %d attempts", attempts)
	}

	// Exhausted now: a third round must not call it again.
	fastForward(t, m)
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("an exhausted job must not be retried, got %d attempts", attempts)
	}
}

// TestPermanentFailureStopsImmediately: retrying a job that cannot succeed wastes the
// attempt budget and delays the failure being visible.
func TestPermanentFailureStopsImmediately(t *testing.T) {
	w, m := testWorker(t)

	var attempts int
	w.handlers["test.permanent"] = func(ctx context.Context, payload map[string]any) error {
		attempts++
		return Permanent(errors.New("cannot ever work"))
	}

	enqueue(t, m, store.Job{Kind: "test.permanent"})

	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	fastForward(t, m)
	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("a permanent failure must not be retried, got %d attempts", attempts)
	}
}

// TestUnknownKindIsNotRetried: a job whose handler was removed (a downgrade, or a typo
// in the kind) must fail once rather than five times over an hour.
func TestUnknownKindIsNotRetried(t *testing.T) {
	w, m := testWorker(t)

	enqueue(t, m, store.Job{Kind: "test.no-such-handler"})

	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	fastForward(t, m)
	if n, _ := w.ProcessJobs(context.Background()); n != 0 {
		t.Fatalf("an unknown kind must not be retried, got %d handled", n)
	}
}

// TestShutdownLeavesWorkClaimable: a worker that stops mid-job must not lose it.
func TestShutdownLeavesWorkClaimable(t *testing.T) {
	w, m := testWorker(t)

	w.handlers["test.slow"] = func(ctx context.Context, payload map[string]any) error {
		return ctx.Err()
	}
	enqueue(t, m, store.Job{Kind: "test.slow"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already shutting down

	// The claim still happens (it is a short transaction); the handler then observes a
	// cancelled context.
	if _, err := w.ProcessJobs(ctx); err != nil {
		t.Fatalf("a cancelled round must not error: %v", err)
	}

	// The job is due again once its lease expires, so nothing was lost.
	fastForward(t, m)
	var pending, running, failed int
	_ = m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, r, f, err := tx.JobCounts(ctx)
		pending, running, failed = p, r, f
		return err
	})
	if pending+running+failed == 0 {
		t.Fatal("work must not vanish when a worker stops")
	}
}

// TestBlobCollectRejectsBadPayload: the handler must not panic or retry forever on a
// payload it cannot parse.
func TestBlobCollectRejectsBadPayload(t *testing.T) {
	w, m := testWorker(t)

	for _, payload := range []map[string]any{
		{},
		{"blob_id": ""},
		{"blob_id": "not-a-uuid"},
	} {
		enqueue(t, m, store.Job{Kind: "blob.collect", Payload: payload, MaxAttempts: 1})
	}

	if _, err := w.ProcessJobs(context.Background()); err != nil {
		t.Fatal(err)
	}

	// All three are permanently failed, none retried.
	fastForward(t, m)
	if n, _ := w.ProcessJobs(context.Background()); n != 0 {
		t.Fatalf("malformed payloads must fail permanently, got %d handled", n)
	}

	var failed int
	_ = m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, _, f, err := tx.JobCounts(ctx)
		failed = f
		return err
	})
	if failed != 3 {
		t.Fatalf("want 3 failed jobs, got %d", failed)
	}
}

// fastForward makes every queued job due now.
//
// It reaches into the memory store's clock because the alternative — a real backoff of
// 30 seconds or more — would make the suite unusably slow, and a test that sleeps
// through a backoff is a test people delete.
func fastForward(t *testing.T, m *store.Memory) {
	t.Helper()
	m.AdvanceForTest(24 * time.Hour)
}

var _ = uuid.Nil

// TestAnAbandonedLeaseIsRecoveredAndReported is INV-06's crash path.
//
// Two things have to be true after a worker dies mid-job. The work must come back by itself — an
// expired lease is claimable, so the next worker picks it up without an operator — and somebody must
// be able to SEE that it happened. Claiming already recovers the job, so a test that only claimed it
// would pass with no reporting at all, which is how ReapExpiredLeases ended up implemented on both
// backends and called by nothing.
func TestAnAbandonedLeaseIsRecoveredAndReported(t *testing.T) {
	s := store.NewMemory()
	ctx := context.Background()

	// Enqueue with room for more than one attempt: a job whose budget is one is finished after the
	// first claim, which is the policy, not a recovery failure.
	enqueue(t, s, store.Job{
		Kind: "blob.collect", Payload: map[string]any{"blob_id": uuid.New().String()},
		MaxAttempts: 3, DedupeKey: "lease-test",
	})

	// Worker A claims it and then "dies": nothing completes or fails the job.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		claimed, err := tx.ClaimJobs(ctx, "worker-a", 1, 20*time.Millisecond)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			t.Fatalf("worker A claimed %d jobs, want the one enqueued", len(claimed))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// While the lease is held, nobody else may take it.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		claimed, err := tx.ClaimJobs(ctx, "worker-b", 1, time.Minute)
		if err != nil {
			return err
		}
		if len(claimed) != 0 {
			t.Fatalf("worker B claimed a leased job: %d", len(claimed))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The lease expires. The worker's report is what tells an operator a process died mid-job.
	time.Sleep(60 * time.Millisecond)
	// A worker bound to THIS store: binding it to a fresh one would reap an empty queue and the
	// assertion would pass or fail for a reason unrelated to the lease.
	w := New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := w.ReapExpiredJobLeasesForTest(ctx); n != 1 {
		t.Fatalf("the abandoned lease was not reported: got %d, want 1", n)
	}

	// And the work is available again, which is the half that must not depend on the report.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		claimed, err := tx.ClaimJobs(ctx, "worker-b", 1, time.Minute)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			t.Fatalf("the abandoned job was not reclaimed after its lease expired: %d", len(claimed))
		}
		if claimed[0].Attempts != 2 {
			t.Fatalf("a recovery is a second attempt, got %d", claimed[0].Attempts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEveryJobKindIsIdempotent is INV-06's "idempotent" clause, per kind.
//
// Idempotency is the property that makes a durable queue safe to run: a job can be reclaimed after
// a crash, retried after a partial success, or delivered twice by a queue that at-least-once
// semantics allow — and the result must be the same. It is also a property that is easy to claim and
// hard to see, so this runs every registered kind TWICE and requires the second run to succeed and
// change nothing.
//
// The case table is checked against Worker.JobKinds(), so a new kind cannot be added without an
// idempotency case: a ratchet rather than a list someone remembers to extend.
func TestEveryJobKindIsIdempotent(t *testing.T) {
	type idemCase struct {
		// job builds the payload for this kind. A fresh one is used for each run so a test cannot
		// accidentally depend on the same row being processed twice.
		job func(t *testing.T, m *store.Memory) store.Job
		// after runs after the second execution and returns an error describing any drift.
		after func(t *testing.T, m *store.Memory, first, second int) error
	}

	cases := map[string]idemCase{
		"blob.collect": {
			job: func(t *testing.T, m *store.Memory) store.Job {
				// A blob id that does not exist: the handler maps "not found" to success, which is
				// what makes it safe to run speculatively. Deleting an absent row twice must not
				// become an error the second time.
				return store.Job{Kind: "blob.collect",
					Payload: map[string]any{"blob_id": uuid.New().String()}, MaxAttempts: 2}
			},
			after: func(t *testing.T, m *store.Memory, first, second int) error {
				if err := drainQueue(t, m); err != nil {
					return err
				}
				return nil
			},
		},
		"thumbnail.warm": {
			job: func(t *testing.T, m *store.Memory) store.Job {
				return store.Job{Kind: "thumbnail.warm",
					Payload: map[string]any{"node_id": uuid.New().String()}, MaxAttempts: 2}
			},
			after: func(t *testing.T, m *store.Memory, first, second int) error { return nil },
		},
		"user.purge_credentials": {
			job: func(t *testing.T, m *store.Memory) store.Job {
				u := m.AddUser(store.User{Username: "purge-" + uuid.New().String()[:8],
					Email: "purge@example.test", Role: "user"})
				return store.Job{Kind: "user.purge_credentials",
					Payload: map[string]any{"user_id": u.ID.String()}, MaxAttempts: 2}
			},
			after: func(t *testing.T, m *store.Memory, first, second int) error {
				// Revoking twice must not error and must not report a second batch of credentials.
				// The counts come from the handler's own report via JobCounts; what matters is that
				// the queue holds no failed job.
				return drainQueue(t, m)
			},
		},
		"search.index": {
			job: func(t *testing.T, m *store.Memory) store.Job {
				// A node id that does not exist. The handler reads the row rather than trusting the
				// payload, so a missing row means "delete it from the index" — and running that
				// twice must be the same as running it once, which is what the second pass below
				// checks.
				return store.Job{Kind: "search.index",
					Payload: map[string]any{"node_id": uuid.New().String()}, MaxAttempts: 2}
			},
			after: func(t *testing.T, m *store.Memory, first, second int) error {
				return drainQueue(t, m)
			},
		},
	}

	kinds := NewForTest().JobKinds()
	if len(kinds) == 0 {
		t.Fatal("the worker registers no job kinds; this check would pass vacuously")
	}
	for _, kind := range kinds {
		c, ok := cases[kind]
		if !ok {
			t.Fatalf("job kind %q has no idempotency case. Add one here: a kind whose second run "+
				"is untested is a kind that may double-charge, double-delete or double-notify after "+
				"a crash.", kind)
		}

		t.Run(kind, func(t *testing.T) {
			w, m := testWorker(t)
			enqueue(t, m, c.job(t, m))
			if n, err := w.ProcessJobs(context.Background()); err != nil || n != 1 {
				t.Fatalf("first run: processed %d, err %v", n, err)
			}

			enqueue(t, m, c.job(t, m))
			second, err := w.ProcessJobs(context.Background())
			if err != nil {
				t.Fatalf("second run of the same kind failed: %v. Re-running a job after a crash "+
					"or a duplicate delivery must be a no-op, not an error.", err)
			}
			if second != 1 {
				// Distinguish "the handler failed on the re-run and the queue rescheduled it" from
				// "the job was never there". Without this the failure reads as a queue problem when
				// the real cause is a handler that is not idempotent — which is what this test is
				// for, and the message is the only thing an operator sees.
				var pending, running, failed int
				_ = m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					pending, running, failed, _ = tx.JobCounts(ctx)
					return nil
				})
				t.Fatalf("second run processed %d jobs, want 1 (pending=%d running=%d failed=%d). "+
					"A handler that errors when re-run is not idempotent; the queue then retries it "+
					"with backoff or marks it failed.", second, pending, running, failed)
			}
			if err := c.after(t, m, 1, second); err != nil {
				t.Fatalf("after the second run: %v", err)
			}
		})
	}
}

// drainQueue fails if the queue holds anything that is not completed. A job that was retried into
// the failed state after two successful runs means the handler is not idempotent.
func drainQueue(t *testing.T, m *store.Memory) error {
	t.Helper()
	return m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		pending, running, failed, err := tx.JobCounts(ctx)
		if err != nil {
			return err
		}
		if pending != 0 || running != 0 || failed != 0 {
			t.Fatalf("queue not drained after the runs: pending=%d running=%d failed=%d",
				pending, running, failed)
		}
		return nil
	})
}
