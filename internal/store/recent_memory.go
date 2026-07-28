package store

// RecentFiles in the in-memory store.
//
// Sorted explicitly rather than relying on insertion order: a test that depends on the order
// files happened to be created in would pass for the wrong reason and break the moment the
// implementation changed.

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

// RecentFiles implements RecentReader.
func (m *Memory) RecentFiles(ctx context.Context, ownerID uuid.UUID, limit int) ([]RecentFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 12
	}
	if limit > 100 {
		limit = 100
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	candidates := make([]*memNode, 0, len(m.nodes))
	for _, n := range m.nodes {
		if n.node.OwnerID != ownerID || n.node.Kind != KindFile || n.node.TrashedAt != nil {
			continue
		}
		candidates = append(candidates, n)
	}

	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].node, candidates[j].node
		if !a.ModifiedAt.Equal(b.ModifiedAt) {
			return a.ModifiedAt.After(b.ModifiedAt)
		}
		// A tie-break keeps the result stable between calls; without it the home screen
		// could reorder itself on every refresh.
		return a.ID.String() < b.ID.String()
	})

	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	out := make([]RecentFile, 0, len(candidates))
	for _, n := range candidates {
		out = append(out, RecentFile{Node: n.node, Path: m.pathLocked(n.node)})
	}
	return out, nil
}

// pathLocked reconstructs a node's slash-separated path from its parents. The caller must
// hold at least a read lock.
func (m *Memory) pathLocked(n Node) string {
	parts := []string{}
	cur := n
	// Bounded for the same reason as the SQL depth cap: a corrupt parent chain must not hang
	// the server.
	for depth := 0; cur.ParentID != nil && depth < 64; depth++ {
		parent, ok := m.nodes[*cur.ParentID]
		if !ok {
			break
		}
		// The owner's root has an empty name; including it would produce a leading slash and
		// disagree with the SQL implementation.
		if cur.Name != "" {
			parts = append(parts, cur.Name)
		}
		cur = parent.node
	}

	path := ""
	for i := len(parts) - 1; i >= 0; i-- {
		path += parts[i]
		if i > 0 {
			path += "/"
		}
	}
	return path
}
