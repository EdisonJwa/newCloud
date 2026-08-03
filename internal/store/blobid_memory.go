package store

import (
	"context"

	"github.com/google/uuid"
)

// BlobIDFor is the metadata layer's dedup index entry point.
//
// The in-memory store keeps its own map so tests exercise the same contract
// without a database.
func (m *Memory) BlobIDFor(ctx context.Context, hash [32]byte, size int64) uuid.UUID {
	// size is accepted and ignored: the in-memory store keeps no blob rows, so there is nothing
	// to record it on. The signature is shared with PostgreSQL so callers cannot forget it.
	// If this is called from inside a transaction, the write must land on the transaction's
	// snapshot, not on the store.
	//
	// It previously always locked the receiver. WithTx hands the callback a *clone*, so a write
	// to the original was either lost when the clone's map was committed over it, or (worse)
	// kept a value the transaction would later discard. The observable effect was that two files
	// with identical content received *different* blob ids — deduplication silently off — and
	// BlobCount() stayed zero, which is the assertion that caught it.
	if tx, ok := ctx.Value(txMemKey{}).(*memTx); ok {
		return tx.blobID(hash)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.blobIDLocked(hash)
}

// blobID resolves the id inside the transaction's snapshot.
//
// No lock needed: WithTx serialises transactions with txMu and the snapshot is private to this
// callback.
func (t *memTx) blobID(hash [32]byte) uuid.UUID {
	if id, ok := t.m.blobIDs[hash]; ok {
		return id
	}
	id := newID()
	t.m.blobIDs[hash] = id
	return id
}

// blobIDLocked is the shared body; the caller holds m.mu.
func (m *Memory) blobIDLocked(hash [32]byte) uuid.UUID {
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

// BlobHash returns the content address recorded for a blob.
//
// The in-memory store keeps only the hash-to-id direction, so the reverse is a scan. That is
// fine at the sizes a memory store is used for, and the alternative — a second map — would be
// another pair of structures to keep in step.
func (t *memTx) BlobHash(ctx context.Context, id uuid.UUID) ([]byte, error) {
	for hash, bid := range t.m.blobIDs {
		if bid == id {
			out := make([]byte, len(hash))
			copy(out, hash[:])
			return out, nil
		}
	}
	return nil, ErrNotFound
}
