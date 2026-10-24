package worker

// Run's completion contract.
//
// Run used to return nothing and expose nothing: the serving process started it with `go
// wk.Run(ctx)` and never joined it, so shutdown killed the process while a purge could be
// mid-transaction — despite this file's comment claiming shutdown was joined. These tests pin
// the two halves of the fix: Wait reports completion after cancellation, and a second Run in
// the same process cannot start a second set of duties.

import (
	"context"
	"testing"
	"time"
)

// waitForRegistration blocks until Run has recorded itself, so the tests cannot pass through
// Wait's "never started" path by racing the goroutine.
func waitForRegistration(t *testing.T, w *Worker) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.runMu.Lock()
		started := w.done != nil
		w.runMu.Unlock()
		if started {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never registered itself")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWaitReturnsAfterRunFinishes(t *testing.T) {
	w := quietWorker()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w.Run(ctx)
	}()
	waitForRegistration(t, w)

	// Before cancellation, a bounded Wait times out rather than claiming success.
	if w.Wait(0) {
		t.Fatal("Wait reported completion while the worker was still running")
	}

	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !w.Wait(time.Second) {
		t.Fatal("Wait reported the worker still running after Run returned")
	}
	// And it stays cheap: a finished Run must satisfy even a zero timeout.
	if !w.Wait(0) {
		t.Fatal("Wait failed on an already-finished Run")
	}
}

func TestWaitOnANeverStartedWorkerDoesNotBlock(t *testing.T) {
	w := quietWorker()
	if !w.Wait(0) {
		t.Fatal("a worker that was never started must need no join")
	}
}

// A second Run must not start a second set of duty goroutines on the same store.
func TestRunCannotStartTwice(t *testing.T) {
	w := quietWorker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w.Run(ctx)
	}()
	waitForRegistration(t, w)

	second := make(chan struct{})
	go func() {
		defer close(second)
		w.Run(ctx)
	}()

	// The second Run blocks (it must not spin up duties), then completes when the first
	// one does. A small sleep plus a non-blocking probe keeps this from being a timing
	// assertion on the happy path.
	time.Sleep(20 * time.Millisecond)
	select {
	case <-second:
		t.Fatal("the second Run returned while the first was still running; it did not join")
	default:
	}

	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the first Run did not return after cancellation")
	}
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the second Run never returned after the first finished")
	}
}

// TestSessionPruneDutyIsRegistered: the duty must be in the ticker set, or PruneSessions is
// another correct function that nothing calls — which is exactly the shape the rate-limit
// prunes were found in.
func TestSessionPruneDutyIsRegistered(t *testing.T) {
	w := buildFullWorker()
	for _, d := range w.duties() {
		if d.name == "session-prune" {
			if d.interval <= 0 {
				t.Fatal("the session-prune duty has a non-positive interval; runDuties skips it, " +
					"so it would look configured and never run")
			}
			if d.run == nil {
				t.Fatal("the session-prune duty has no run function")
			}
			return
		}
	}
	t.Fatal("no session-prune duty in the ticker set; the sessions table grows without bound again")
}
