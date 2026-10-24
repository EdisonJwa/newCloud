package swap

// Relocation: moving one existing node to a new parent and name, with its bytes.
//
// This is the other half of the swap package's job, and it lives here because three call sites need
// the SAME sequence and the same compensation: the JSON API's plain MOVE, a MOVE overwrite's staging
// step, and the compensating step that undoes it. The WebDAV handler had its own copy of it, and that
// copy is where the fourth review (2026-10-23) found a MOVE overwriting its own destination and a
// failed MOVE that had already trashed a folder.
//
// The ordering is not interchangeable, and it is the mirror of Install's:
//
//  1. The metadata rename/reparent commits with its audit row in ONE transaction, so a relocation
//     that is in the log happened, and one that happened is in the log.
//
//  2. On a path-backed layout the bytes follow in a separate operation, because a metadata row and a
//     filesystem rename cannot commit together. If they cannot follow, the metadata is put back —
//     best effort and loud — and the caller is told the relocation failed. That is the honest
//     contract: a node whose metadata says it moved while its bytes did not is a file the user can
//     see and cannot open.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ErrContentMove reports that the metadata relocation was rolled back because the node's bytes could
// not follow it. The metadata is back where it started when this is returned, so a caller can map it
// to "nothing was changed" rather than to a generic server fault.
var ErrContentMove = errors.New("swap: the content could not follow the relocation")

// Relocation describes moving one existing node.
type Relocation struct {
	// Node is the node as it is now; its ID is the identity that survives the move.
	Node store.Node
	// FromRel is the content path the node's bytes are at, and Node.Name/ParentID are where its
	// metadata says they should be.
	FromRel string
	// ParentID is the destination parent and Name the destination name.
	ParentID uuid.UUID
	Name     string
	// ToRel is the content path the bytes have to end up at.
	ToRel string
	// Audit, when set, runs inside the transaction that performs the relocation, so the record and
	// the change commit together. It receives the node as it will be after the move.
	Audit func(ctx context.Context, tx store.Tx, moved store.Node) error
}

// Relocate moves one node's metadata and, on a path-backed layout, its bytes.
//
// A no-op relocation (same parent, same name) still succeeds and returns the node: the caller may be
// replaying an idempotent operation, and refusing it would make retries fail.
func Relocate(ctx context.Context, d Deps, in Relocation) (store.Node, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if in.Node.ID == (uuid.UUID{}) {
		return store.Node{}, errors.New("swap: relocation of a zero node")
	}
	// Validate the address before anything moves. A caller that passed the REST layer's "/a/b"
	// spelling would otherwise fail after the metadata had already changed.
	if err := layout.ValidateSegment(in.Name); err != nil {
		return store.Node{}, fmt.Errorf("swap: destination name %q: %w", in.Name, err)
	}
	if _, err := layout.ValidatePath(in.FromRel); err != nil {
		return store.Node{}, fmt.Errorf("swap: source path %q: %w", in.FromRel, err)
	}
	if _, err := layout.ValidatePath(in.ToRel); err != nil {
		return store.Node{}, fmt.Errorf("swap: destination path %q: %w", in.ToRel, err)
	}

	var moved store.Node
	err := d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n := in.Node
		if n.Name != in.Name {
			renamed, rerr := tx.Rename(ctx, in.Node.ID, in.Name)
			if rerr != nil {
				return rerr
			}
			n = renamed
		}
		if in.Node.ParentID == nil || *in.Node.ParentID != in.ParentID {
			reparented, merr := tx.Move(ctx, in.Node.ID, in.ParentID)
			if merr != nil {
				return merr
			}
			n = reparented
		}
		moved = n
		if in.Audit == nil {
			return nil
		}
		return in.Audit(ctx, tx, moved)
	})
	if err != nil {
		return store.Node{}, err
	}

	// A layout whose rename IS a metadata update has nothing left to do: its path-to-content
	// resolver reads the metadata that just changed. Calling Move for such a layout always failed,
	// and the discarded error hid it.
	if d.Content.Capabilities().EfficientRename {
		return moved, nil
	}

	from := cleanRel(in.FromRel)
	to := cleanRel(in.ToRel)
	if from == to {
		// The metadata moved but the path did not (only possible for a same-parent rename that the
		// caller addressed through a different path): nothing to rename on disk.
		return moved, nil
	}
	if merr := d.Content.Move(ctx,
		layout.Node{Owner: in.Node.OwnerID.String(), Path: from, IsDir: in.Node.IsDir()},
		layout.Node{Owner: in.Node.OwnerID.String(), Path: to, IsDir: in.Node.IsDir()}); merr != nil {
		d.Log.Error("swap: relocation could not move the content; restoring the metadata",
			"node", in.Node.ID.String(), "from", from, "to", to, "error", merr)
		if cerr := d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			if in.Node.Name != in.Name {
				if _, rerr := tx.Rename(ctx, in.Node.ID, in.Node.Name); rerr != nil {
					return rerr
				}
			}
			if in.Node.ParentID != nil && *in.Node.ParentID != in.ParentID {
				if _, mverr := tx.Move(ctx, in.Node.ID, *in.Node.ParentID); mverr != nil {
					return mverr
				}
			}
			return nil
		}); cerr != nil {
			d.Log.Error("swap: COMPENSATION FAILED — the metadata says the node moved and its "+
				"content did not; an operator has to reconcile it",
				"node", in.Node.ID.String(), "error", cerr)
		}
		return store.Node{}, fmt.Errorf("%w: %v", ErrContentMove, merr)
	}
	return moved, nil
}

// cleanRel renders a content path in the relative form a store is addressed by.
//
// The REST and WebDAV layers both produce relative paths; this is the belt under that brace, so a
// future caller cannot reintroduce an absolute path on one layout only (the tree layout refuses an
// absolute path outright, while the blob layout would accept one and hide the mistake).
func cleanRel(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}
