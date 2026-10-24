// Package treewalk resolves a whole directory subtree, or proves that it cannot.
//
// It exists because two surfaces transfer whole trees — WebDAV COPY and the JSON batch/archive API —
// and both had their own recursion over Tx.Children. That call truncates at the configured page size
// (limits.max_profind_children) and reports nothing about it, so a folder with one row more than the
// limit was copied "successfully" with a file missing, and archived into a valid ZIP with an entry
// omitted. Neither result looks wrong to the client, which is the worst property a data operation can
// have.
//
// Tx.Descendants answers the completeness question directly (`complete`). This package turns that
// answer into a resolved tree, and refuses instead of returning a tree it could not prove. The rules
// are:
//
//   - the node count is bounded by the caller's budget; a subtree larger than the budget yields
//     ErrTooLarge rather than a prefix of the tree;
//   - the depth is bounded by the store's own walk limit; a tree at that bound yields ErrTooDeep,
//     because the store would stop there and report completeness it did not achieve;
//   - every live node must hang off the root or off another returned node. A node whose parent the
//     walk did not return yields ErrInconsistent: the walk was truncated without saying so, or the
//     parent chain is corrupt;
//   - trashed nodes are dropped. A live folder can contain a trashed child, and copying or archiving
//     it would resurrect something the user deleted.
//
// The package deliberately knows nothing about permissions, HTTP status codes or audit records:
// those differ per surface, and folding them in would make the two protocols disagree about what a
// complete walk means.
package treewalk

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// DefaultBudget is the node budget used when a caller does not choose one.
//
// It matches the recursive-permission walk both surfaces already use, so an operation that passed
// the permission check is not then refused by the transfer walk for being one node smaller.
const DefaultBudget = 10_000

// MaxDepth is the deepest chain this walk will prove complete.
//
// It is the same bound Tx.Descendants enforces (and transferMaxDepth in the JSON API already used).
// A tree that reaches it is refused rather than described as complete, because the store's walk
// would have stopped there regardless of how much tree remained.
const MaxDepth = 64

var (
	// ErrTooLarge means the subtree has more nodes than the caller's budget.
	ErrTooLarge = errors.New("treewalk: subtree exceeds the node budget")
	// ErrTooDeep means the subtree is nested to the depth where the store's walk stops.
	ErrTooDeep = errors.New("treewalk: subtree is deeper than the walk can prove complete")
	// ErrInconsistent means a live node's parent was not returned by the walk.
	ErrInconsistent = errors.New("treewalk: subtree parent links are inconsistent")
)

// Tree is one resolved subtree: a node, its path inside the caller's namespace, and its children.
//
// Rel is filled from the root down, so a content store can be addressed without a query per node.
// Only a complete tree is ever returned; a *Tree therefore always represents everything below its
// root.
type Tree struct {
	Node     store.Node
	Rel      string
	Children []*Tree
}

// Resolve returns the complete subtree under root, or an error.
//
// rootRel is the root's path inside the caller's namespace. budget bounds the whole subtree, not one
// directory; a budget of zero or less means DefaultBudget.
func Resolve(
	ctx context.Context, meta store.Store, root store.Node, rootRel string, budget int,
) (*Tree, error) {

	out := &Tree{Node: root, Rel: rootRel}
	if !root.IsDir() {
		return out, nil
	}
	if budget <= 0 {
		budget = DefaultBudget
	}

	var (
		flat     []store.Node
		complete bool
	)
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, c, derr := tx.Descendants(ctx, root.ID, budget)
		flat, complete = got, c
		return derr
	})
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, fmt.Errorf("%w: more than %d descendants", ErrTooLarge, budget)
	}

	byID := make(map[uuid.UUID]*Tree, len(flat)+1)
	byID[root.ID] = out
	live := make([]store.Node, 0, len(flat))
	for _, n := range flat {
		if n.TrashedAt != nil {
			continue
		}
		live = append(live, n)
		byID[n.ID] = &Tree{Node: n}
	}

	// Every live node must hang off the root or off another returned node.
	for _, n := range live {
		if n.ParentID == nil {
			return nil, fmt.Errorf("%w: %q has no parent", ErrInconsistent, n.Name)
		}
		if _, ok := byID[*n.ParentID]; !ok {
			return nil, fmt.Errorf("%w: the parent of %q was not returned", ErrInconsistent, n.Name)
		}
	}

	// Attach in a second pass. Paths are computed by a walk from the root afterwards, so this does
	// not depend on the store's ordering (breadth-first today, but that is an implementation detail).
	for _, n := range live {
		parent := byID[*n.ParentID]
		parent.Children = append(parent.Children, byID[n.ID])
	}

	var assign func(t *Tree, depth int) int
	assign = func(t *Tree, depth int) int {
		deepest := depth
		for _, c := range t.Children {
			c.Rel = path.Join(t.Rel, c.Node.Name)
			if d := assign(c, depth+1); d > deepest {
				deepest = d
			}
		}
		return deepest
	}
	if deepest := assign(out, 0); deepest >= MaxDepth-1 {
		return nil, fmt.Errorf("%w: nested %d levels deep", ErrTooDeep, deepest)
	}

	return out, nil
}
