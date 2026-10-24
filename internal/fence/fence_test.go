package fence

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ids() (owner, dir, child, other uuid.UUID) {
	return uuid.New(), uuid.New(), uuid.New(), uuid.New()
}

func TestSharedReadersCoexistAndExclusiveWaits(t *testing.T) {
	l := New()
	owner, node, _, _ := ids()
	ctx := context.Background()

	r1, err := l.Acquire(ctx, Read(NodeKey(owner, node)))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Acquire(ctx, Read(NodeKey(owner, node)))
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan struct{})
	go func() {
		rel, aerr := l.Acquire(ctx, Write(NodeKey(owner, node)))
		if aerr != nil {
			t.Errorf("exclusive acquire: %v", aerr)
			return
		}
		close(acquired)
		rel()
	}()

	select {
	case <-acquired:
		t.Fatal("exclusive acquired while two readers held the node")
	case <-time.After(50 * time.Millisecond):
	}

	r1()
	select {
	case <-acquired:
		t.Fatal("exclusive acquired while one reader still held the node")
	case <-time.After(50 * time.Millisecond):
	}

	r2()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("exclusive never acquired after readers released")
	}
}

func TestDirectoryExclusiveConflictsWithChildRead(t *testing.T) {
	l := New()
	owner, dir, child, _ := ids()
	ctx := context.Background()

	dirRelease, err := l.Acquire(ctx, Write(NodeKey(owner, dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer dirRelease()

	// A child operation takes the directory as a shared ancestor.
	childCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(childCtx, Read(NodeKey(owner, dir)), Write(NodeKey(owner, child))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("child operation under an exclusive directory = %v, want context deadline", err)
	}
}

func TestDifferentChildrenUnderSharedParentCoexist(t *testing.T) {
	l := New()
	owner, dir, childA, childB := ids()
	ctx := context.Background()

	a, err := l.Acquire(ctx, Read(NodeKey(owner, dir)), Write(NodeKey(owner, childA)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Acquire(ctx, Read(NodeKey(owner, dir)), Write(NodeKey(owner, childB)))
	if err != nil {
		t.Fatal(err)
	}
	a()
	b()
}

func TestNameAndNodeKeysAreDistinct(t *testing.T) {
	l := New()
	owner, _, child, _ := ids()
	ctx := context.Background()

	nodeRel, err := l.Acquire(ctx, Write(NodeKey(owner, child)))
	if err != nil {
		t.Fatal(err)
	}
	nameRel, err := l.Acquire(ctx, Write(NameKey(owner, child, "file.txt")))
	if err != nil {
		t.Fatalf("a name key must not be the same lock as the node key: %v", err)
	}
	nodeRel()
	nameRel()
}

func TestDuplicateIntentIsRejected(t *testing.T) {
	l := New()
	owner, node, _, _ := ids()
	_, err := l.Acquire(context.Background(),
		Read(NodeKey(owner, node)), Write(NodeKey(owner, node)))
	if !errors.Is(err, ErrDuplicateIntent) {
		t.Fatalf("duplicate shared+exclusive intent = %v, want ErrDuplicateIntent", err)
	}
}

func TestOppositeOrdersDoNotDeadlock(t *testing.T) {
	l := New()
	owner, a, b, _ := ids()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	for _, intents := range [][]Intent{
		{Write(NodeKey(owner, a)), Read(NodeKey(owner, b))},
		{Write(NodeKey(owner, b)), Read(NodeKey(owner, a))},
	} {
		go func(intents []Intent) {
			defer wg.Done()
			rel, err := l.Acquire(ctx, intents...)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			time.Sleep(5 * time.Millisecond)
			rel()
		}(intents)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		t.Fatalf("opposite acquisition order deadlocked: %v", err)
	}
}

func TestCancellationDoesNotLeavePartialHold(t *testing.T) {
	l := New()
	owner, node, _, _ := ids()
	rel, err := l.Acquire(context.Background(), Write(NodeKey(owner, node)))
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, Read(NodeKey(owner, node))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
	}
	if got := l.Held(); got != 1 {
		t.Fatalf("Held() = %d, want only the original holder", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	l := New()
	owner, node, _, _ := ids()
	rel, err := l.Acquire(context.Background(), Write(NodeKey(owner, node)))
	if err != nil {
		t.Fatal(err)
	}
	rel()
	rel()
	if got := l.Held(); got != 0 {
		t.Fatalf("Held() = %d after release, want 0", got)
	}
}

// R12-001: the caller's variadic slice is ordinary mutable memory. Acquire must
// copy it, otherwise a caller reusing the slice makes Release release the wrong
// keys and leaves the original lock held forever.
func TestReleaseDoesNotDependOnCallerSliceMutation(t *testing.T) {
	l := New()
	owner, a, b, _ := ids()
	ctx := context.Background()

	intents := []Intent{Write(NodeKey(owner, a))}
	rel, err := l.Acquire(ctx, intents...)
	if err != nil {
		t.Fatal(err)
	}
	intents[0] = Write(NodeKey(owner, b))
	rel()

	if got := l.Held(); got != 0 {
		t.Fatalf("Held() = %d after Release; caller slice mutation changed the release keys (R12-001)", got)
	}
	relA, err := l.Acquire(ctx, Write(NodeKey(owner, a)))
	if err != nil {
		t.Fatalf("original key remained locked: %v", err)
	}
	relA()
	relB, err := l.Acquire(ctx, Write(NodeKey(owner, b)))
	if err != nil {
		t.Fatalf("unrelated key was locked by the release: %v", err)
	}
	relB()
}

func TestAcquireDoesNotReorderCallerSlice(t *testing.T) {
	l := New()
	owner, a, b, _ := ids()
	intents := []Intent{Write(NodeKey(owner, b)), Write(NodeKey(owner, a))}
	before := append([]Intent(nil), intents...)
	rel, err := l.Acquire(context.Background(), intents...)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	for i := range intents {
		if intents[i] != before[i] {
			t.Fatalf("Acquire mutated caller slice at %d: %+v -> %+v", i, before[i], intents[i])
		}
	}
}
