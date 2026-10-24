package consistency

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// VisibilityNode is the metadata needed to decide external visibility. It is
// deliberately separate from store.Node so the rule can be shared by path and
// direct-ID routes without importing a storage layout.
type VisibilityNode struct {
	ID          uuid.UUID
	ParentID    *uuid.UUID
	Trashed     bool
	StagedBy    *uuid.UUID
	Quarantined bool
}

// VisibilityReader loads one node. found=false means the node does not exist;
// err carries a backend failure and must never be treated as "not visible".
type VisibilityReader interface {
	VisibilityNode(ctx context.Context, id uuid.UUID) (node VisibilityNode, found bool, err error)
}

// MaxVisibilityDepth bounds an ancestor walk. A cycle is a metadata defect, not a
// reason to spin, so the walk fails closed.
const MaxVisibilityDepth = 256

// EffectiveExternalVisibility implements ADR-0013 G2: a node is externally
// visible only when it and every ancestor are live, reachable, and not staged,
// trashed, quarantined or owned by an unpublished operation. Checking the target
// row's StagedBy alone is insufficient, because a live child of a staged root
// would otherwise be addressable by its ID.
func EffectiveExternalVisibility(ctx context.Context, r VisibilityReader, id uuid.UUID) (bool, error) {
	if id == uuid.Nil {
		return false, nil
	}
	seen := make(map[uuid.UUID]struct{}, 8)
	for depth := 0; depth < MaxVisibilityDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, dup := seen[id]; dup {
			return false, fmt.Errorf("consistency: visibility cycle at %s", id)
		}
		seen[id] = struct{}{}

		n, found, err := r.VisibilityNode(ctx, id)
		if err != nil {
			return false, err
		}
		if !found || n.Trashed || n.StagedBy != nil || n.Quarantined {
			return false, nil
		}
		if n.ParentID == nil {
			return true, nil
		}
		id = *n.ParentID
	}
	return false, fmt.Errorf("consistency: visibility chain deeper than %d", MaxVisibilityDepth)
}
