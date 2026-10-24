package swap

// Retiring a node: moving its content out of the live path before the row leaves the namespace.
//
// Trashing is a metadata operation. On a path-backed layout the bytes stay where they are, at the
// path the file used to occupy, until the retention sweep removes them. That has two consequences,
// and the 2026-10-24 review's phase-0 reproductions are about the first of them:
//
//   - The live path is occupied by a deleted file. Anything that wants to publish a NEW file there
//     without replacing — the safe way to install content — cannot, because the name is not free
//     until the purge runs. That is why no-replace publish is not enabled on this layout.
//   - The retained copy is a trash entry whose bytes are addressable at the old path, which is the
//     same artefact problem the staging names have.
//
// Moving the bytes to a reserved sibling name and renaming the metadata to match fixes both: the
// live path is free the moment the row is trashed, and the retained copy is a normal trash entry at
// a path derived from its own name, so the purge and the restore route already handle it.

import (
	"context"
	"errors"
	"log/slog"
	"path"

	"github.com/EdisonJwa/newCloud/internal/store"
)

var (
	errNoParent    = errors.New("swap: cannot retire a node with no parent")
	errNoTrashStep = errors.New("swap: retire needs a trash step")
)

// TrashStep performs the metadata half: it must trash the node it is given, inside a transaction,
// together with whatever audit row the operation owes.
//
// A callback rather than a fixed statement because the two protocols audit differently (WebDAV has
// no session id to record, the API has one), and because the trash cap is resolved by the caller.
type TrashStep func(ctx context.Context, tx store.Tx, n store.Node) error

// Retire moves a node's content out of the live path and then trashes it.
//
// On a layout where paths follow content this is two steps with a compensation between them: the
// content and metadata move to a reserved sibling name, and only then does the row go to the trash.
// If the trash fails the move is reversed, because a failed DELETE must not leave the user's file
// renamed to an internal artefact.
//
// On a layout where they do not (the blob layout) there is nothing to move: the path is a metadata
// mapping and the bytes are content-addressed, so trashing is already atomic with respect to the
// namespace.
func Retire(ctx context.Context, d Deps, n store.Node, parentRel string, trash TrashStep) error {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if n.ParentID == nil {
		// The namespace root has no parent and is never retired; the callers refuse this earlier.
		return errNoParent
	}
	if trash == nil {
		return errNoTrashStep
	}

	// Where the content path does not follow the name, the row is the only thing to move.
	if d.Content.Capabilities().EfficientRename {
		return d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return trash(ctx, tx, n)
		})
	}

	reserved := BackupName(n.Name)
	fromRel := path.Join(parentRel, n.Name)
	toRel := path.Join(parentRel, reserved)

	moved, err := Relocate(ctx, d, Relocation{
		Node: n, FromRel: fromRel, ParentID: *n.ParentID, Name: reserved, ToRel: toRel,
	})
	if err != nil {
		return err
	}

	if terr := d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return trash(ctx, tx, moved)
	}); terr != nil {
		// Reverse the move: the delete did not happen, so the file must be exactly where it was,
		// under the name the user knows.
		if _, rerr := Relocate(ctx, d, Relocation{
			Node: moved, FromRel: toRel, ParentID: *moved.ParentID, Name: n.Name, ToRel: fromRel,
		}); rerr != nil {
			d.Log.Error("swap: COMPENSATION FAILED — a failed delete left the node under a "+
				"reserved name with its bytes intact; it was NOT deleted. An operator can recover "+
				"it by hand",
				"name", reserved, "node", moved.ID.String(), "error", rerr)
		}
		return terr
	}
	return nil
}
