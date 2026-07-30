package store

// Search in the in-memory store.

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Search implements Searcher.
func (m *Memory) Search(ctx context.Context, ownerID uuid.UUID, query string, limit int) ([]RecentFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	needle := strings.ToLower(query)
	if needle == "" {
		return []RecentFile{}, nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	hits := make([]*memNode, 0, 8)
	for _, n := range m.nodes {
		if n.node.OwnerID != ownerID || n.node.TrashedAt != nil {
			continue
		}
		if strings.Contains(strings.ToLower(n.node.Name), needle) {
			hits = append(hits, n)
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i].node, hits[j].node
		if !a.ModifiedAt.Equal(b.ModifiedAt) {
			return a.ModifiedAt.After(b.ModifiedAt)
		}
		return a.ID.String() < b.ID.String()
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}

	out := make([]RecentFile, 0, len(hits))
	for _, n := range hits {
		out = append(out, RecentFile{Node: n.node, Path: m.pathLocked(n.node)})
	}
	return out, nil
}
