package store

// MarkersFor on the in-memory backend.
//
// The point of this backend is behavioural parity with PostgreSQL, so the semantics must
// match exactly: only unrevoked shares count, and only nodes in the requested set appear.

import (
	"context"

	"github.com/google/uuid"
)

// MarkersFor returns share counts and version presence for the given nodes.
func (m *Memory) MarkersFor(ctx context.Context, ownerID uuid.UUID, nodeIDs []uuid.UUID) (map[uuid.UUID]Marker, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	want := make(map[uuid.UUID]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		want[id] = struct{}{}
	}

	out := make(map[uuid.UUID]Marker, len(nodeIDs))

	for _, sh := range m.shares {
		if sh.OwnerID != ownerID || sh.RevokedAt != nil {
			continue
		}
		if _, ok := want[sh.NodeID]; !ok {
			continue
		}
		marker := out[sh.NodeID]
		marker.ShareCount++
		out[sh.NodeID] = marker
	}

	// Versions live on the node record in this backend. A stored version always implies a
	// newer current one exists, so presence is the whole answer.
	for id := range want {
		rec, ok := m.nodes[id]
		if !ok || len(rec.versions) == 0 {
			continue
		}
		marker := out[id]
		marker.HasVersions = true
		out[id] = marker
	}

	return out, nil
}
