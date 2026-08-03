package store

import (
	"context"

	"github.com/google/uuid"
)

// In-memory implementation of SharedTx.
//
// It mirrors the SQL semantics: every granted node, plus its descendants while the
// grant inherits, excluding the actor's own nodes, reduced to the roots of each
// covered subtree.

func (t *memTx) SharedWithMe(ctx context.Context, actorID uuid.UUID, limit int) ([]SharedNode, error) {
	groups := t.groupsOf(actorID)

	// Which nodes does any grant cover, for this actor?
	covered := map[uuid.UUID]bool{}
	for nodeID, entries := range t.m.acl {
		for _, e := range entries {
			matchesUser := e.UserID != nil && *e.UserID == actorID
			matchesGroup := e.GroupID != nil && groups[*e.GroupID]
			if !matchesUser && !matchesGroup {
				continue
			}
			covered[nodeID] = true
			if !e.Inherit {
				continue
			}
			// Walk down, marking descendants.
			stack := []uuid.UUID{nodeID}
			for len(stack) > 0 {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				for _, childID := range t.m.children[cur] {
					if covered[childID] {
						continue
					}
					covered[childID] = true
					stack = append(stack, childID)
				}
			}
		}
	}

	var out []SharedNode
	for id := range covered {
		rec, ok := t.m.nodes[id]
		if !ok || rec.node.TrashedAt != nil {
			continue
		}
		// Only the caller's *own* nodes are excluded, not everything in their
		// namespace: a grant on a folder inside somebody's tree is exactly the case
		// this exists for.
		if rec.node.OwnerID == actorID {
			continue
		}
		// Roots only: a covered node whose parent is also covered is not a root.
		if rec.node.ParentID != nil && covered[*rec.node.ParentID] {
			continue
		}

		perms, found, err := t.EffectivePermsFor(ctx, actorID, id)
		if err != nil {
			return nil, err
		}
		if !found || perms == 0 {
			continue
		}
		out = append(out, SharedNode{
			NodeID: id, OwnerID: rec.node.OwnerID, Name: rec.node.Name,
			Kind: rec.node.Kind, Size: rec.node.Size, Mime: rec.node.Mime,
			ModifiedAt: rec.node.ModifiedAt, Perms: perms,
		})
	}

	// Deterministic order: name, like the SQL's effective result for the UI.
	sortShared(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sortShared orders by name then id, so two runs produce the same page.
func sortShared(nodes []SharedNode) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0; j-- {
			if nodes[j].Name < nodes[j-1].Name ||
				(nodes[j].Name == nodes[j-1].Name && nodes[j].NodeID.String() < nodes[j-1].NodeID.String()) {
				nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
				continue
			}
			break
		}
	}
}

var _ SharedTx = (*memTx)(nil)
