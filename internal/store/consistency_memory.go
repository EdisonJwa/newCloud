package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/consistency"
)

// Phase 1 metadata foundations on the in-memory store. They are deliberately
// separate from the Store/Tx interfaces: no handler uses them until Phase 1R, and
// the Phase 1 conformance tests pin the contract first.

var _ consistency.Journal = (*Memory)(nil)
var _ consistency.RevisionStore = (*Memory)(nil)
var _ consistency.VisibilityReader = (*Memory)(nil)

func (m *Memory) Create(ctx context.Context, op consistency.Operation) (consistency.Operation, error) {
	return m.journal.Create(ctx, op)
}

func (m *Memory) Get(ctx context.Context, id uuid.UUID) (consistency.Operation, error) {
	return m.journal.Get(ctx, id)
}

func (m *Memory) Transition(ctx context.Context, id uuid.UUID, from, to consistency.State) (consistency.Operation, error) {
	return m.journal.Transition(ctx, id, from, to)
}

func (m *Memory) NonTerminal(ctx context.Context, limit int) ([]consistency.Operation, error) {
	return m.journal.NonTerminal(ctx, limit)
}

func (m *Memory) Unresolved(ctx context.Context, limit int) ([]consistency.Operation, error) {
	return m.journal.Unresolved(ctx, limit)
}

// CompleteOperation is the transactional half of publish: the memory snapshot
// carries a cloned journal, so a rollback discards this transition too.
func (t *memTx) CompleteOperation(ctx context.Context, opID, ownerID uuid.UUID, kind string) error {
	if opID == uuid.Nil {
		return consistency.ErrPublishOperation
	}
	if t.m.journal == nil {
		return consistency.ErrOperationNotFound
	}
	op, err := t.m.journal.Get(ctx, opID)
	if err != nil {
		return err
	}
	if op.State != consistency.StatePublishing {
		return fmt.Errorf("%w: %s", consistency.ErrStateConflict, op.State)
	}
	if op.OwnerID != ownerID || op.Kind != kind {
		return fmt.Errorf("%w: journal owner/kind does not match the publish", consistency.ErrStateConflict)
	}
	_, err = t.m.journal.Transition(ctx, opID, consistency.StatePublishing, consistency.StateCommitted)
	return err
}

// Current returns the node's revision. A node without a seeded revision is a
// conflict, not revision zero.
func (m *Memory) Current(ctx context.Context, nodeID uuid.UUID) (consistency.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.nodes[nodeID]; !ok {
		return 0, consistency.ErrRevisionConflict
	}
	rev, ok := m.revisions[nodeID]
	if !ok {
		return 0, consistency.ErrRevisionConflict
	}
	return rev, nil
}

// CAS advances a node revision by exactly one.
func (m *Memory) CAS(ctx context.Context, nodeID uuid.UUID, expected, next consistency.Revision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[nodeID]; !ok {
		return consistency.ErrRevisionConflict
	}
	if next != expected+1 {
		return consistency.ErrRevisionConflict
	}
	if m.revisions[nodeID] != expected {
		return consistency.ErrRevisionConflict
	}
	m.revisions[nodeID] = next
	return nil
}

// SeedRevision installs the first revision for an existing node. Tests call it;
// the eventual CreateFile/CreateDir wiring will seed in the same transaction.
func (m *Memory) SeedRevision(ctx context.Context, nodeID uuid.UUID) (consistency.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[nodeID]; !ok {
		return 0, errors.New("store: cannot seed a missing node")
	}
	if _, exists := m.revisions[nodeID]; exists {
		return 0, consistency.ErrRevisionConflict
	}
	const first consistency.Revision = 1
	m.revisions[nodeID] = first
	return first, nil
}

// VisibilityNode implements the ancestor-chain visibility reader.
func (m *Memory) VisibilityNode(ctx context.Context, id uuid.UUID) (consistency.VisibilityNode, bool, error) {
	if err := ctx.Err(); err != nil {
		return consistency.VisibilityNode{}, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[id]
	if !ok {
		return consistency.VisibilityNode{}, false, nil
	}
	out := consistency.VisibilityNode{ID: n.node.ID, ParentID: n.node.ParentID}
	out.Trashed = n.node.TrashedAt != nil
	out.StagedBy = m.stagedBy[id]
	return out, true, nil
}

// SetStagedByForTest records operation ownership for the G2 tests. Production
// staging will write the same column in the same transaction as node creation;
// this method is test-only until Phase 1R wires it into Tx.
func (m *Memory) SetStagedByForTest(nodeID, opID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if opID == uuid.Nil {
		delete(m.stagedBy, nodeID)
		return
	}
	op := opID
	m.stagedBy[nodeID] = &op
}
