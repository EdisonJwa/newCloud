package store

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

// In-memory dead-property implementation.

func (t *memTx) SetDeadProp(ctx context.Context, nodeID uuid.UUID, space, name, value string) error {
	if _, ok := t.m.nodes[nodeID]; !ok {
		return ErrNotFound
	}
	if t.m.deadProps[nodeID] == nil {
		t.m.deadProps[nodeID] = map[[2]string]string{}
	}
	t.m.deadProps[nodeID][[2]string{space, name}] = value
	return nil
}

func (t *memTx) DeadProps(ctx context.Context, nodeID uuid.UUID) ([]DeadProp, error) {
	set := t.m.deadProps[nodeID]
	out := make([]DeadProp, 0, len(set))
	for k, v := range set {
		out = append(out, DeadProp{NodeID: nodeID, Space: k[0], Name: k[1], Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Space != out[j].Space {
			return out[i].Space < out[j].Space
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (t *memTx) DeleteDeadProp(ctx context.Context, nodeID uuid.UUID, space, name string) error {
	delete(t.m.deadProps[nodeID], [2]string{space, name})
	return nil
}
