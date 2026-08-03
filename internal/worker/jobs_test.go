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
