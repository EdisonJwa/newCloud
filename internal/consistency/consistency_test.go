package consistency

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestJournalHappyPathAndNonTerminal(t *testing.T) {
	j := NewMemoryJournal()
	ctx := context.Background()
	id := uuid.New()
	if _, err := j.Create(ctx, Operation{ID: id, OwnerID: uuid.New(), Kind: "copy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Transition(ctx, id, StateIntent, StateStaging); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Transition(ctx, id, StateStaging, StatePublishing); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Transition(ctx, id, StatePublishing, StateCommitted); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Transition(ctx, id, StateCommitted, StateAborted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("committed -> aborted = %v, want invalid transition", err)
	}
	open, err := j.NonTerminal(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("NonTerminal returned %d after commit", len(open))
	}
}

func TestJournalRejectsSkipsAndDuplicates(t *testing.T) {
	j := NewMemoryJournal()
	ctx := context.Background()
	id := uuid.New()
	op := Operation{ID: id, OwnerID: uuid.New(), Kind: "copy"}
	if _, err := j.Create(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Create(ctx, op); !errors.Is(err, ErrOperationExists) {
		t.Fatalf("duplicate create = %v", err)
	}
	if _, err := j.Transition(ctx, id, StateIntent, StatePublishing); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("INTENT -> PUBLISHING = %v, want invalid transition", err)
	}
	if _, err := j.Transition(ctx, uuid.New(), StateIntent, StateStaging); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("unknown op = %v", err)
	}
}

func TestJournalConcurrentTransitionsHaveOneWinner(t *testing.T) {
	j := NewMemoryJournal()
	ctx := context.Background()
	id := uuid.New()
	if _, err := j.Create(ctx, Operation{ID: id, OwnerID: uuid.New(), Kind: "copy"}); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	var wins int
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := j.Transition(ctx, id, StateIntent, StateStaging); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d transitions from INTENT won, want exactly 1", wins)
	}
}

type visibilityMap map[uuid.UUID]VisibilityNode

func (m visibilityMap) VisibilityNode(ctx context.Context, id uuid.UUID) (VisibilityNode, bool, error) {
	n, ok := m[id]
	return n, ok, nil
}

func TestEffectiveVisibilityWalksTheWholeAncestorChain(t *testing.T) {
	ctx := context.Background()
	root, dir, file := uuid.New(), uuid.New(), uuid.New()
	staged := uuid.New()
	m := visibilityMap{
		root: {ID: root},
		dir:  {ID: dir, ParentID: &root},
		file: {ID: file, ParentID: &dir},
	}
	ok, err := EffectiveExternalVisibility(ctx, m, file)
	if err != nil || !ok {
		t.Fatalf("live chain visible = %v, %v", ok, err)
	}

	// Stage the top-most ancestor, not the target: checking only the target row's
	// staged_by would leak the live-looking child by ID.
	rootNode := m[root]
	rootNode.StagedBy = &staged
	m[root] = rootNode
	ok, err = EffectiveExternalVisibility(ctx, m, file)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a live child under a staged root was reported externally visible (G2)")
	}
}

func TestEffectiveVisibilityFailsClosed(t *testing.T) {
	ctx := context.Background()
	root, child := uuid.New(), uuid.New()
	trashed := visibilityMap{root: {ID: root, Trashed: true}, child: {ID: child, ParentID: &root}}
	if ok, err := EffectiveExternalVisibility(ctx, trashed, child); err != nil || ok {
		t.Fatalf("trashed ancestor: visible=%v err=%v", ok, err)
	}
	missing := visibilityMap{child: {ID: child, ParentID: &root}}
	if ok, err := EffectiveExternalVisibility(ctx, missing, child); err != nil || ok {
		t.Fatalf("missing ancestor: visible=%v err=%v", ok, err)
	}
	cycle := visibilityMap{root: {ID: root, ParentID: &child}, child: {ID: child, ParentID: &root}}
	if ok, err := EffectiveExternalVisibility(ctx, cycle, child); err == nil || ok {
		t.Fatalf("cycle: visible=%v err=%v, want false and an error", ok, err)
	}
}

func TestRevisionCAS(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRevisions()
	node := uuid.New()
	if _, err := r.Current(ctx, node); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("missing node Current = %v", err)
	}
	if _, err := r.Seed(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := r.CAS(ctx, node, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.CAS(ctx, node, 1, 2); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale CAS = %v, want conflict", err)
	}
	if got, _ := r.Current(ctx, node); got != 2 {
		t.Fatalf("current revision = %d, want 2", got)
	}
	if err := r.CAS(ctx, node, 2, 4); err == nil {
		t.Fatal("CAS accepted a non-unit advance")
	}
}
