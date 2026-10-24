package consistency

import (
	"context"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

type recoveryStaging struct {
	discarded []uuid.UUID
}

func (s *recoveryStaging) StagePut(context.Context, uuid.UUID, string, io.Reader, int64) (layout.PutResult, error) {
	return layout.PutResult{}, nil
}
func (s *recoveryStaging) StageMkdir(context.Context, uuid.UUID, string) error { return nil }
func (s *recoveryStaging) PublishNoReplace(context.Context, uuid.UUID, string, layout.Node) error {
	return nil
}
func (s *recoveryStaging) PublishReplace(context.Context, uuid.UUID, string, layout.Node, layout.Node) error {
	return nil
}
func (s *recoveryStaging) Discard(_ context.Context, op uuid.UUID) error {
	s.discarded = append(s.discarded, op)
	return nil
}

func TestRecoverAbortsIntentDiscardsStagingAndQuarantinesPublishing(t *testing.T) {
	j := NewMemoryJournal()
	ctx := context.Background()
	owner := uuid.New()
	mk := func(state State) uuid.UUID {
		id := uuid.New()
		if _, err := j.Create(ctx, Operation{ID: id, OwnerID: owner, Kind: "copy"}); err != nil {
			t.Fatal(err)
		}
		switch state {
		case StateStaging:
			if _, err := j.Transition(ctx, id, StateIntent, StateStaging); err != nil {
				t.Fatal(err)
			}
		case StatePublishing:
			if _, err := j.Transition(ctx, id, StateIntent, StateStaging); err != nil {
				t.Fatal(err)
			}
			if _, err := j.Transition(ctx, id, StateStaging, StatePublishing); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	intentID := mk(StateIntent)
	stagingID := mk(StateStaging)
	publishingID := mk(StatePublishing)

	st := &recoveryStaging{}
	results, err := Recover(ctx, j, st, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("recovered %d operations, want 3", len(results))
	}
	if got := mustState(t, j, intentID); got != StateAborted {
		t.Fatalf("INTENT -> %s, want ABORTED", got)
	}
	if got := mustState(t, j, stagingID); got != StateDiscarded {
		t.Fatalf("STAGING -> %s, want DISCARDED", got)
	}
	if got := mustState(t, j, publishingID); got != StateNeedsOperator {
		t.Fatalf("PUBLISHING -> %s, want NEEDS_OPERATOR", got)
	}
	if len(st.discarded) != 1 || st.discarded[0] != stagingID {
		t.Fatalf("discarded stages = %v, want only %s", st.discarded, stagingID)
	}

	// A restart sees no non-terminal operations, but the quarantined operation
	// must still keep readiness false.
	again, err := Recover(ctx, j, st, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second recovery returned %d operations, want none", len(again))
	}
	unresolved, err := HasUnresolved(ctx, j, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !unresolved {
		t.Fatal("NEEDS_OPERATOR was forgotten after restart (R12-003)")
	}
}

func mustState(t *testing.T, j Journal, id uuid.UUID) State {
	t.Helper()
	op, err := j.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return op.State
}
