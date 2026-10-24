package worker

// Liveness of the maintenance loop (2026-10-24 review, M12).
//
// The worker shares the process with the HTTP listener, so a defect here is an outage for
// every user rather than a missed maintenance cycle. The old shape had three: all duties
// shared one select loop (so a slow blob sweep delayed lock expiry, despite a comment
// claiming otherwise), nothing recovered a panic (so one corrupt row ended the process), and
// nothing bounded a pass (so a query blocked on a lock stalled every other duty silently).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// buildFullWorker returns a worker with every interval set, using the production constructor so
// a duty added there without an interval shows up here rather than being skipped at runtime.
func buildFullWorker() *Worker {
	w := New(nil, slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError + 1})))
	return w.WithBlobScrub(nil, 10)
}

func quietWorker() *Worker {
	return &Worker{log: slog.New(slog.NewTextHandler(io.Discard,
		&slog.HandlerOptions{Level: slog.LevelError + 1}))}
}

// A panic in one pass must not take the worker — or the process — down.
func TestAPanickingPassDoesNotStopTheWorker(t *testing.T) {
	w := quietWorker()
	var runs atomic.Int32
	d := duty{name: "boom", interval: 5 * time.Millisecond, run: func(ctx context.Context) error {
		if runs.Add(1) == 1 {
			panic("a corrupt row")
		}
		return nil
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	w.loop(ctx, d)

	if got := runs.Load(); got < 2 {
		t.Fatalf("the duty ran %d time(s) after panicking; the worker must survive and keep "+
			"ticking, or one corrupt row ends the process", got)
	}
}

// A slow duty must not delay another. This is the property the old single select loop only
// claimed in its comment.
func TestASlowDutyDoesNotDelayAnother(t *testing.T) {
	w := quietWorker()
	slow := duty{name: "slow", interval: 2 * time.Millisecond, run: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	}}
	var fastRuns atomic.Int32
	fast := duty{name: "fast", interval: 2 * time.Millisecond, run: func(ctx context.Context) error {
		fastRuns.Add(1)
		return nil
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.loop(ctx, slow) }()
	go func() { defer wg.Done(); w.loop(ctx, fast) }()
	wg.Wait()

	if got := fastRuns.Load(); got < 10 {
		t.Fatalf("the fast duty ran %d times while a slow one held its goroutine; with one "+
			"shared loop it would be starved, which is what lock expiry was", got)
	}
}

// Shutdown is joined: runDuties must not return while a pass is still working.
func TestShutdownWaitsForAnInFlightPass(t *testing.T) {
	w := quietWorker()
	started := make(chan struct{})
	finished := make(chan struct{})
	d := duty{name: "stubborn", interval: 5 * time.Millisecond, run: func(ctx context.Context) error {
		close(started)
		// Deliberately ignores cancellation: this pass must be WAITED FOR, not abandoned.
		time.Sleep(150 * time.Millisecond)
		close(finished)
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.runDuties(ctx, []duty{d})
	}()

	<-started
	cancel()
	<-done

	select {
	case <-finished:
	default:
		t.Fatal("runDuties returned while a pass was still running; a purge would be killed " +
			"half-way through a transaction")
	}
}

// A hung pass is cut off at the timeout instead of stalling that duty forever.
func TestAHungPassIsCutOffAtItsTimeout(t *testing.T) {
	w := quietWorker()
	w.passCap = 20 * time.Millisecond

	var sawDeadline atomic.Bool
	d := duty{name: "hung", run: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			sawDeadline.Store(errors.Is(ctx.Err(), context.DeadlineExceeded))
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}}

	began := time.Now()
	w.pass(context.Background(), d)
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("pass took %v; it was not cut off at its timeout", elapsed)
	}
	if !sawDeadline.Load() {
		t.Fatal("the duty never saw its pass context expire; the timeout is not reaching it")
	}
}

// Every duty must be runnable: a name, a positive interval, and something to run. A duty with
// no run would crash the goroutine; one with no interval is silently skipped and looks enabled.
func TestEveryDutyIsRunnable(t *testing.T) {
	w := buildFullWorker()
	duties := w.duties()
	if len(duties) < 8 {
		t.Fatalf("only %d duties; the list lost entries, and a lost duty is maintenance that "+
			"silently stops happening", len(duties))
	}
	seen := map[string]bool{}
	for _, d := range duties {
		if d.name == "" {
			t.Fatal("a duty has no name; its failures would be unattributable in the log")
		}
		if seen[d.name] {
			t.Fatalf("two duties are both named %q; the log cannot tell them apart", d.name)
		}
		seen[d.name] = true
		if d.run == nil {
			t.Fatalf("duty %q has no run function", d.name)
		}
		if d.interval <= 0 {
			t.Fatalf("duty %q has interval %v, so runDuties skips it: it would look configured "+
				"and never run", d.name, d.interval)
		}
	}
}
