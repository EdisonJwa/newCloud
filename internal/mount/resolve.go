package mount

// Path resolution across mount points (ADR-0014).
//
// The store resolves a path by walking node rows. A mount point is a node row; everything
// BELOW it is not. This walk produces one answer for "where does this path live": either
// entirely in the store, or in a remote with a mount root and a remaining relative path.
//
// Both HTTP surfaces (the JSON API and WebDAV) resolve through this walk, because the
// alternative — each surface inventing its own mount detection — is exactly how one surface
// ends up able to see into a mount the other refuses.

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// Target is the outcome of resolving a caller's path.
type Target struct {
	// Chain is the store-resolved node chain, root first. When External is true it ends
	// at the mount point; the remaining path never had rows.
	Chain []store.Node
	// External is true when the path is at or below a mount point. A path that stops
	// exactly AT the root is external with Rel "" — listing the mount point lists the
	// remote root, not an empty folder.
	External bool
	// Mount is the mount record, valid iff External.
	Mount store.Mount
	// Rel is the path below the mount point, clean and slash-separated, "" at the root.
	Rel string
}

// Node is the node the path resolved to: the mount point when External, otherwise the
// chain's leaf. Authorization is always answered against THIS node — the ACL system
// authorizes nodes, and a mount point is one.
func (t Target) Node() store.Node { return t.Chain[len(t.Chain)-1] }

// ErrNoMount is returned by MountAt when the node is not a mount point. It is a control
// flow signal for callers that already hold a resolved node, not an error to surface.
var ErrNoMount = errors.New("mount: node is not a mount point")

// ResolveTarget resolves owner's path against both stores of truth: the metadata store
// down to the deepest existing row, and the mount table for the fork.
//
// The walk is one ChildByName per segment. Path depths are single digits; a join would
// save round trips the profile does not need yet, and per-segment is what makes the
// fork point exact — a bulk Resolve answers "all-or-nothing", which cannot say WHERE a
// path left the database.
func ResolveTarget(ctx context.Context, meta store.Store, owner uuid.UUID, p string) (Target, error) {
	segs, err := splitRel(p)
	if err != nil {
		return Target{}, err
	}
	root, err := meta.Root(ctx, owner)
	if err != nil {
		return Target{}, err
	}
	chain := []store.Node{root}
	cur := root

	for i, seg := range segs {
		if m, merr := MountAt(ctx, meta, cur.ID); merr == nil {
			// cur is a mount point: the remaining segments, including this one, are
			// the remote's path.
			return Target{Chain: chain, External: true, Mount: m,
				Rel: path.Join(append([]string{""}, segs[i:]...)...)}, nil
		} else if !errors.Is(merr, ErrNoMount) {
			return Target{}, merr
		}
		child, cerr := childByName(ctx, meta, cur.ID, seg)
		if cerr != nil {
			if errors.Is(cerr, store.ErrNotFound) {
				return Target{}, store.ErrNotFound
			}
			return Target{}, cerr
		}
		chain = append(chain, child)
		cur = child
	}

	// The whole path walked inside the store. The leaf itself may still BE a mount
	// point (the path stopped at the root): its contents are remote too.
	if m, merr := MountAt(ctx, meta, cur.ID); merr == nil {
		return Target{Chain: chain, External: true, Mount: m, Rel: ""}, nil
	} else if !errors.Is(merr, ErrNoMount) {
		return Target{}, merr
	}
	return Target{Chain: chain, External: false}, nil
}

// MountAt answers whether node id is a mount point. ErrNoMount says it is not.
func MountAt(ctx context.Context, meta store.Store, nodeID uuid.UUID) (store.Mount, error) {
	var m store.Mount
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		m, err = tx.MountByNodeID(ctx, nodeID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.Mount{}, ErrNoMount
	}
	return m, err
}

func childByName(ctx context.Context, meta store.Store, parent uuid.UUID, name string) (store.Node, error) {
	var n store.Node
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		n, err = tx.ChildByName(ctx, parent, name)
		return err
	})
	return n, err
}

// splitRel cleans a caller path into segments, refusing the degenerate names the store
// refuses: "", ".", ".." and separators-in-a-segment never reach a remote as path pieces.
func splitRel(p string) ([]string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, nil
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return nil, store.ErrInvalidName
		}
		if strings.ContainsAny(part, "\x00") {
			return nil, store.ErrInvalidName
		}
		out = append(out, part)
	}
	return out, nil
}
