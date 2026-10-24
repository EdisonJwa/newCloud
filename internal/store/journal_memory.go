package store

// The in-memory journal, held to the same contract as the SQL one by the shared conformance suite.

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

func (m *Memory) opMap() map[uuid.UUID]Op {
	if m.ops == nil {
		m.ops = map[uuid.UUID]Op{}
	}
	return m.ops
}

// StartOp writes the intent.
func (t *memTx) StartOp(ctx context.Context, op Op) (Op, error) {
	if op.ID == uuid.Nil {
		op.ID = uuid.New()
	}
	if op.State == "" {
		op.State = OpIntent
	}
	if !validOpState(op.State) {
		return Op{}, ErrInvalidName
	}
	now := t.m.now()
	op.CreatedAt, op.UpdatedAt = now, now
	t.m.opMap()[op.ID] = op
	return op, nil
}

// AdvanceOp moves an operation, refusing if it is not in the expected state.
func (t *memTx) AdvanceOp(ctx context.Context, id uuid.UUID, from, to string) error {
	op, ok := t.m.opMap()[id]
	if !ok || op.State != from {
		return ErrPrecondition
	}
	if !validOpState(to) {
		return ErrInvalidName
	}
	op.State, op.UpdatedAt = to, t.m.now()
	t.m.opMap()[id] = op
	return nil
}

// OpByID reads one operation.
func (t *memTx) OpByID(ctx context.Context, id uuid.UUID) (Op, error) {
	op, ok := t.m.opMap()[id]
	if !ok {
		return Op{}, ErrNotFound
	}
	return op, nil
}

// OpenOps lists unfinished operations, oldest first, matching the SQL ordering.
func (t *memTx) OpenOps(ctx context.Context, limit int) ([]Op, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []Op
	for _, op := range t.m.opMap() {
		if !OpTerminal(op.State) {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// validOpState mirrors the CHECK constraint in migration 0016.
func validOpState(state string) bool {
	for _, s := range OpStates() {
		if s == state {
			return true
		}
	}
	return false
}
