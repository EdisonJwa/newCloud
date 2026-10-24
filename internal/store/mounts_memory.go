package store

// In-memory implementation of the mount surface (ADR-0014).
//
// Mirrors mounts_pg.go exactly, including DeleteMount's pairing guard: the shared
// conformance suite runs both against the same cases, and a divergence here is how
// "works in tests, 500s in production" happens.

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

func (t *memTx) CreateMount(ctx context.Context, m NewMount) (Mount, error) {
	if m.ID == uuid.Nil || m.NodeID == uuid.Nil {
		return Mount{}, errors.New("mount needs an id and a node id")
	}
	if _, exists := t.m.mounts[m.ID]; exists {
		return Mount{}, ErrConflict
	}
	if _, exists := t.m.mountByNode[m.NodeID]; exists {
		return Mount{}, ErrConflict
	}
	out := Mount{NewMount: m, CreatedAt: t.m.now(), HealthStatus: "unknown"}
	t.m.mounts[m.ID] = &out
	t.m.mountByNode[m.NodeID] = m.ID
	return out, nil
}

func (t *memTx) MountByNodeID(ctx context.Context, nodeID uuid.UUID) (Mount, error) {
	id, ok := t.m.mountByNode[nodeID]
	if !ok {
		return Mount{}, ErrNotFound
	}
	return *t.m.mounts[id], nil
}

func (t *memTx) MountByID(ctx context.Context, id uuid.UUID) (Mount, error) {
	m, ok := t.m.mounts[id]
	if !ok {
		return Mount{}, ErrNotFound
	}
	return *m, nil
}

func (t *memTx) MountsForOwner(ctx context.Context, ownerID uuid.UUID) ([]Mount, error) {
	var out []Mount
	for _, m := range t.m.mounts {
		if m.OwnerID == ownerID {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (t *memTx) AllMounts(ctx context.Context) ([]Mount, error) {
	out := make([]Mount, 0, len(t.m.mounts))
	for _, m := range t.m.mounts {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OwnerID != out[j].OwnerID {
			return out[i].OwnerID.String() < out[j].OwnerID.String()
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// DeleteMount mirrors the PostgreSQL pairing guard: both rows, one transaction, and a
// refusal when the mount point somehow grew live children.
func (t *memTx) DeleteMount(ctx context.Context, id uuid.UUID) error {
	m, ok := t.m.mounts[id]
	if !ok {
		return ErrNotFound
	}
	for _, childID := range t.m.children[m.NodeID] {
		if n, exists := t.m.nodes[childID]; exists && n.node.TrashedAt == nil {
			return errors.New("mount node has live children; refusing to delete")
		}
	}
	// The children index is keyed parent -> nameKey -> child id, so the mount node's
	// own entry in its parent's index has to be removed by scanning for its id: the
	// folded key it was created under is not remembered here.
	delete(t.m.nodes, m.NodeID)
	for _, kids := range t.m.children {
		for key, childID := range kids {
			if childID == m.NodeID {
				delete(kids, key)
			}
		}
	}
	delete(t.m.mounts, id)
	delete(t.m.mountByNode, m.NodeID)
	return nil
}

func (t *memTx) SetMountHealth(ctx context.Context, id uuid.UUID, status, detail string, at time.Time) error {
	m, ok := t.m.mounts[id]
	if !ok {
		return ErrNotFound
	}
	m.HealthStatus = status
	m.HealthDetail = detail
	m.HealthAt = &at
	return nil
}
