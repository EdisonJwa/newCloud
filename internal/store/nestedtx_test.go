package store_test

// A nested WithTx must fail, not hang.
//
// The memory store serialises transactions on a mutex it holds for the whole callback. The re-entry
// guard catches the common case — a nested call carrying the transaction context — but it cannot see
// a nested call made with a DIFFERENT context, which is what a handler does when it passes
// r.Context() instead of the ctx it was handed. That case used to block on the mutex until the outer
// callback returned, which is never: the test suite hung for fifteen minutes and the cause was a
// single line in an unrelated package.
//
// A named error after a bounded wait is worth much more than a hang, so the wait has a deadline.
// These two cases pin both halves: the ordinary re-entry guard still answers immediately, and the
// context-less one answers at all.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestNestedTransactionWithTheSameContextIsRefusedImmediately(t *testing.T) {
	m := store.NewMemory()

	err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		inner := m.WithTx(ctx, func(ctx context.Context, tx store.Tx) error { return nil })
		if !errors.Is(inner, store.ErrNestedTx) {
			return errors.New("the re-entry guard did not refuse a nested transaction: " +
				errString(inner))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNestedTransactionWithAFreshContextFailsInsteadOfHanging(t *testing.T) {
	m := store.NewMemory()
	// Short enough that the test is fast, long enough that a scheduling hiccup is not a failure.
	m.SetTxLockTimeoutForTest(50 * time.Millisecond)

	start := time.Now()
	err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		// Deliberately NOT ctx: this is the mistake the guard cannot detect.
		return m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return nil
		})
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a nested transaction with a fresh context succeeded; it should not be able to " +
			"start a second transaction while the first is open")
	}
	if !errors.Is(err, store.ErrNestedTx) {
		t.Fatalf("want ErrNestedTx, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the failure took %s; the wait must be bounded or a mistake becomes a hang",
			elapsed)
	}
}

// errString renders an error for a message when it may be nil.
func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
