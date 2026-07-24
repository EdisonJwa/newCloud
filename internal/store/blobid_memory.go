package store

import (
	"context"

	"github.com/google/uuid"
)

// BlobIDFor is the metadata layer's dedup index entry point.
//
// The in-memory store keeps its own map so tests exercise the same contract
// without a database.
func (m *Memory) BlobIDFor(ctx context.Context, hash [32]byte) uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id, ok := m.blobIDs[hash]; ok {
		return id
	}
	id := newID()
	m.blobIDs[hash] = id
	return id
}

// BlobCount reports how many distinct content addresses the store has seen. Used
// by tests to assert deduplication actually happens.
func (m *Memory) BlobCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.blobIDs)
}
