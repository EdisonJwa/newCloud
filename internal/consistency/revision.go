package consistency

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// ErrRevisionConflict means a compare-and-swap lost to another mutation.
var ErrRevisionConflict = errors.New("consistency: node revision conflict")

// Revision is the resource-state token from ADR-0013 §6.4. It is not the content
// ETag: side-table mutations bump it, content ETag stays content-only.
type Revision uint64

// RevisionStore is the optimistic-concurrency surface. PostgreSQL implements it
// with `UPDATE ... WHERE revision = $expected` so the check and the write are one
// statement; the memory implementation is the executable contract.
type RevisionStore interface {
	Current(ctx context.Context, nodeID uuid.UUID) (Revision, error)
	// CAS sets next only when the current value is expected. A missing node is an
	// error, not revision 0: creating a revision for a node that does not exist
	// would let an uncommitted staging node leak into the live contract.
	CAS(ctx context.Context, nodeID uuid.UUID, expected, next Revision) error
}

// MemoryRevisions is a concurrency-safe in-memory RevisionStore.
type MemoryRevisions struct {
	mu   sync.Mutex
	revs map[uuid.UUID]Revision
}

// NewMemoryRevisions returns an empty revision store.
func NewMemoryRevisions() *MemoryRevisions {
	return &MemoryRevisions{revs: make(map[uuid.UUID]Revision)}
}

var _ RevisionStore = (*MemoryRevisions)(nil)

func (m *MemoryRevisions) Current(ctx context.Context, nodeID uuid.UUID) (Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rev, ok := m.revs[nodeID]
	if !ok {
		return 0, ErrRevisionConflict
	}
	return rev, nil
}

func (m *MemoryRevisions) CAS(ctx context.Context, nodeID uuid.UUID, expected, next Revision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if next != expected+1 {
		return fmt.Errorf("consistency: revision must advance by one: %d -> %d", expected, next)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rev, ok := m.revs[nodeID]
	if !ok {
		return ErrRevisionConflict
	}
	if rev != expected {
		return fmt.Errorf("%w: current %d, expected %d", ErrRevisionConflict, rev, expected)
	}
	m.revs[nodeID] = next
	return nil
}

// Seed installs the first revision for a newly created node. It is distinct from
// CAS so creation is explicit and cannot silently overwrite a live revision.
func (m *MemoryRevisions) Seed(ctx context.Context, nodeID uuid.UUID) (Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.revs[nodeID]; exists {
		return 0, ErrRevisionConflict
	}
	const first Revision = 1
	m.revs[nodeID] = first
	return first, nil
}
