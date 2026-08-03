package store

// In-memory implementation of subtree enumeration.
//
// It mirrors the SQL's semantics exactly, including the "one extra row to detect
// truncation" trick: `complete` must mean the same thing in both backends, or a handler
// that refuses on an incomplete walk would behave differently in tests than in production.

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

// Descendants returns every node below rootID in breadth-first order.
func (t *memTx) Descendants(ctx context.Context, rootID uuid.UUID, limit int) ([]Node, bool, error) {
	if limit <= 0 {
		limit = 10_000
	}

	type item struct {
		id    uuid.UUID
		depth int
		name  string
	}

	// Breadth-first, matching the SQL's ORDER BY depth, name.
	var (
		out      []Node
		seen     = map[uuid.UUID]bool{rootID: true}
		frontier = []uuid.UUID{rootID}
		depth    int
	)
	for len(frontier) > 0 && depth < 64 {
		depth++
		var level []item
		for _, parent := range frontier {
			for _, childID := range t.m.children[parent] {
				if seen[childID] {
					// A cycle could only arise from a bug; skipping terminates the walk
					// rather than looping forever.
					continue
				}
				rec, ok := t.m.nodes[childID]
				if !ok {
					continue
				}
				level = append(level, item{id: childID, depth: depth, name: rec.node.Name})
			}
		}
		sort.Slice(level, func(i, j int) bool {
			if level[i].name != level[j].name {
				return level[i].name < level[j].name
			}
			return level[i].id.String() < level[j].id.String()
		})

		frontier = frontier[:0]
		for _, it := range level {
			seen[it.id] = true
			frontier = append(frontier, it.id)

			rec := t.m.nodes[it.id]
			// Trashed nodes are still enumerated: a subtree being trashed must be walkable
			// so a restore can re-check it, and hiding them here would make "trash a folder
			// containing a protected file" succeed while "restore it" silently dropped that
			// file.
			out = append(out, rec.node)
			if len(out) > limit {
				return out[:limit], false, nil
			}
		}
	}

	if len(out) > limit {
		return out[:limit], false, nil
	}
	return out, true, nil
}

// AllRoots returns every user's namespace root.
func (t *memTx) AllRoots(ctx context.Context) ([]Node, error) {
	var out []Node
	for _, rec := range t.m.nodes {
		if rec.node.ParentID == nil && rec.node.TrashedAt == nil {
			out = append(out, rec.node)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// AllRoots implements Store.AllRoots.
func (m *Memory) AllRoots(ctx context.Context) ([]Node, error) {
	var out []Node
	err := m.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		got, aerr := tx.AllRoots(ctx)
		out = got
		return aerr
	})
	return out, err
}
