package lease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryLeaseOneOwnerAndEpoch(t *testing.T) {
	l := NewMemory()
	owner := uuid.New()
	first, err := l.Acquire(owner)
	if err != nil {
		t.Fatal(err)
	}
	if first.Epoch != 1 {
		t.Fatalf("first epoch = %d, want 1", first.Epoch)
	}
	if _, err := l.Acquire(uuid.New()); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire = %v, want ErrHeld", err)
	}
	first.Close()

	second, err := l.Acquire(owner)
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != 2 {
		t.Fatalf("epoch after takeover = %d, want 2", second.Epoch)
	}
}

func TestMemoryLeaseLossFires(t *testing.T) {
	l := NewMemory()
	got, err := l.Acquire(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	l.Lose()
	select {
	case <-got.Lost:
	case <-time.After(time.Second):
		t.Fatal("Lost did not fire after the lease was lost")
	}
}

func TestMemoryLeaseCloseIsIdempotent(t *testing.T) {
	l := NewMemory()
	got, err := l.Acquire(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	got.Close()
	got.Close()
}

func TestFailStopRunsOnceOnLoss(t *testing.T) {
	l := NewMemory()
	got, err := l.Acquire(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	called := make(chan struct{})
	stopWatch := FailStop(context.Background(), got, func() {
		calls.Add(1)
		close(called)
	})
	defer stopWatch()
	l.Lose()
	select {
	case <-got.Lost:
	case <-time.After(time.Second):
		t.Fatal("Lost did not fire")
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("FailStop callback did not run")
	}
	if calls.Load() != 1 {
		t.Fatalf("FailStop callback ran %d times, want 1", calls.Load())
	}
}
