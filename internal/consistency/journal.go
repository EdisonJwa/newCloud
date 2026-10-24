package consistency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// State is one durable operation-journal state (ADR-0013 §4.1/§4.4).
type State string

const (
	StateIntent        State = "INTENT"
	StateStaging       State = "STAGING"
	StatePublishing    State = "PUBLISHING"
	StateCommitted     State = "COMMITTED"
	StateAborted       State = "ABORTED"
	StateDiscarded     State = "DISCARDED"
	StateNeedsOperator State = "NEEDS_OPERATOR"
)

// Terminal reports whether no normal transition leaves this state.
func (s State) Terminal() bool {
	switch s {
	case StateCommitted, StateAborted, StateDiscarded, StateNeedsOperator:
		return true
	default:
		return false
	}
}

// Operation is the durable record of one cross-store operation.
type Operation struct {
	ID         uuid.UUID
	OwnerID    uuid.UUID
	Kind       string
	State      State
	StagedRoot *uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Journal errors.
var (
	ErrOperationNotFound = errors.New("consistency: operation not found")
	ErrOperationExists   = errors.New("consistency: operation already exists")
	ErrStateConflict     = errors.New("consistency: operation state conflict")
	ErrInvalidTransition = errors.New("consistency: invalid operation transition")
	// ErrLeaseStale means the process's lease epoch is no longer current. A
	// publish must abort before metadata becomes visible.
	ErrLeaseStale = errors.New("consistency: singleton lease epoch is stale")
)

// Journal is the durable operation store. The Phase 1 memory implementation is
// the executable contract; the PostgreSQL implementation must provide the same
// compare-and-swap transitions in one database transaction.
type Journal interface {
	Create(ctx context.Context, op Operation) (Operation, error)
	Get(ctx context.Context, id uuid.UUID) (Operation, error)
	Transition(ctx context.Context, id uuid.UUID, from, to State) (Operation, error)
	NonTerminal(ctx context.Context, limit int) ([]Operation, error)
	// Unresolved returns operations quarantined as NEEDS_OPERATOR. They are
	// terminal for normal recovery, but must keep the process unready across
	// restarts until an operator resolves them.
	Unresolved(ctx context.Context, limit int) ([]Operation, error)
}

var allowed = map[State]map[State]bool{
	StateIntent:        {StateStaging: true, StateAborted: true},
	StateStaging:       {StatePublishing: true, StateDiscarded: true},
	StatePublishing:    {StateCommitted: true, StateAborted: true, StateNeedsOperator: true},
	StateCommitted:     {},
	StateAborted:       {},
	StateDiscarded:     {},
	StateNeedsOperator: {},
}

// ValidTransition reports whether the journal state machine permits from -> to.
func ValidTransition(from, to State) bool { return allowed[from][to] }

// MemoryJournal is an in-memory, concurrency-safe Journal.
type MemoryJournal struct {
	mu  sync.Mutex
	ops map[uuid.UUID]Operation
	now func() time.Time
}

// NewMemoryJournal returns an empty journal.
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{ops: make(map[uuid.UUID]Operation), now: time.Now}
}

var _ Journal = (*MemoryJournal)(nil)

func (j *MemoryJournal) Create(ctx context.Context, op Operation) (Operation, error) {
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	if op.ID == uuid.Nil {
		return Operation{}, fmt.Errorf("consistency: operation id is required")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, exists := j.ops[op.ID]; exists {
		return Operation{}, ErrOperationExists
	}
	if op.State == "" {
		op.State = StateIntent
	}
	if op.State != StateIntent {
		return Operation{}, fmt.Errorf("%w: a new operation must start in INTENT", ErrInvalidTransition)
	}
	now := j.now().UTC()
	op.CreatedAt = now
	op.UpdatedAt = now
	j.ops[op.ID] = op
	return op, nil
}

func (j *MemoryJournal) Get(ctx context.Context, id uuid.UUID) (Operation, error) {
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[id]
	if !ok {
		return Operation{}, ErrOperationNotFound
	}
	return op, nil
}

func (j *MemoryJournal) Transition(ctx context.Context, id uuid.UUID, from, to State) (Operation, error) {
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[id]
	if !ok {
		return Operation{}, ErrOperationNotFound
	}
	if op.State != from {
		return Operation{}, fmt.Errorf("%w: current %s, expected %s", ErrStateConflict, op.State, from)
	}
	if !allowed[from][to] {
		return Operation{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	op.State = to
	op.UpdatedAt = j.now().UTC()
	j.ops[id] = op
	return op, nil
}

// Clone returns an independent copy for a metadata transaction's private
// workspace. The real PostgreSQL implementation uses one database transaction;
// the memory backend needs this so a rollback discards journal transitions too.
func (j *MemoryJournal) Clone() *MemoryJournal {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := &MemoryJournal{ops: make(map[uuid.UUID]Operation, len(j.ops)), now: j.now}
	for k, v := range j.ops {
		out.ops[k] = v
	}
	return out
}

// Unresolved returns operations awaiting an operator. They are intentionally not
// part of NonTerminal, so a restart cannot silently forget them.
func (j *MemoryJournal) Unresolved(ctx context.Context, limit int) ([]Operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Operation, 0)
	for _, op := range j.ops {
		if op.State != StateNeedsOperator {
			continue
		}
		out = append(out, op)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (j *MemoryJournal) NonTerminal(ctx context.Context, limit int) ([]Operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Operation, 0)
	for _, op := range j.ops {
		if op.State.Terminal() {
			continue
		}
		out = append(out, op)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
