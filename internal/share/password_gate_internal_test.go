package share

// Tests for the share-password gate's two concurrency properties (2026-10-25 review,
// fix 9): the attempt cap must hold when guesses arrive in parallel, and the KDF must
// not be something a burst of guesses can multiply into a memory event.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestSharePasswordCapHoldsUnderConcurrency is the parallel-guess race.
//
// The old order — read the count, compare, then record the outcome — let every
// concurrent guess observe "under the limit" before any of them had recorded, so
// twenty simultaneous guesses all ran and all counted only afterwards. The cap now
// counts and records in one transaction before the KDF runs, so the number of
// attempts that actually executes is bounded by the allowance.
func TestSharePasswordCapHoldsUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink, Password: "correct-horse"})
	sh, err := f.svc.Resolve(context.Background(), c.Token)
	if err != nil {
		t.Fatal(err)
	}

	const guesses = 20
	var wg sync.WaitGroup
	for i := 0; i < guesses; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.svc.VerifyPassword(context.Background(), sh, "wrong")
		}()
	}
	wg.Wait()

	var recorded int
	if err := f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.PasswordFailureCount(ctx, sh.ID, time.Hour)
		recorded = n
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The in-memory backend executes the count-and-record transaction atomically, so
	// exactly PasswordMaxAttempts guesses proceed. The small slack exists for a real
	// database, where the pair spans two statements in one transaction and a
	// concurrent pair can interleave once; twenty recorded failures — what the old
	// order produced — is the regression this fails on.
	if recorded > store.PasswordMaxAttempts+2 {
		t.Fatalf("%d of %d parallel guesses were recorded; the cap is %d. Attempts past "+
			"the allowance must be refused before the comparison runs, not counted "+
			"afterwards.", recorded, guesses, store.PasswordMaxAttempts)
	}
}

// TestArgonGateBoundsConcurrentVerifications pins the semaphore half: however many
// callers Verify concurrently, no more than the gate's capacity reach the KDF at once.
// compareHash is substituted with a fake that parks until released, so the measured
// concurrency is exact rather than timing-dependent.
func TestArgonGateBoundsConcurrentVerifications(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink, Password: "pw"})
	sh, err := f.svc.Resolve(context.Background(), c.Token)
	if err != nil {
		t.Fatal(err)
	}

	const cap = 2
	f.svc.argonGate = make(chan struct{}, cap)

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	release := make(chan struct{})
	f.svc.compareHash = func(string, []byte, ArgonParams) bool {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return false
	}

	const callers = 6
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.svc.VerifyPassword(context.Background(), sh, "guess")
		}()
	}

	// Wait until the gate is actually full, so the assertion cannot pass merely
	// because the goroutines had not started yet.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		cur := maxInFlight
		mu.Unlock()
		if cur >= cap || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if maxInFlight > cap {
		t.Fatalf("%d KDF evaluations ran concurrently; the gate allows %d. A burst of "+
			"parallel verifications must queue for the hash, not each allocate its own "+
			"64 MiB of hash memory.", maxInFlight, cap)
	}
}
