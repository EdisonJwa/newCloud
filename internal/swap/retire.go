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
	"fmt"
	"log/slog"
	"path"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
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

	// Step 1: the content leaves the live path.
	if err := d.Content.Move(ctx,
		layout.Node{Owner: n.OwnerID.String(), Path: fromRel, IsDir: n.IsDir()},
		layout.Node{Owner: n.OwnerID.String(), Path: toRel, IsDir: n.IsDir()}); err != nil {
		// Nothing has changed in the namespace, so this is a clean failure.
		return fmt.Errorf("swap: move the content out of the live path: %w", err)
	}

	// Step 2: the namespace switch, in one transaction, under whatever precondition the caller's
	// trash step applies.
	//
	// This order is deliberate and it is not the obvious one. Switching the namespace first would
	// free the name before the bytes moved, and the content move is keyed on the PATH — so a writer
	// that created a new file at that path in the window would have ITS bytes moved to the reserved
	// name instead. Measured: that ordering turned a successful writer's file into a 500 on the next
	// read. Content first means the worst case is a superseded copy left behind, never a live
	// writer's data taking a walk.
	if terr := d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		moved, rerr := tx.Rename(ctx, n.ID, reserved)
		if rerr != nil {
			return rerr
		}
		return trash(ctx, tx, moved)
	}); terr != nil {
		// The content has already moved but the namespace did not change, so the question that
		// decides the repair is: is the live path occupied right now?
		//
		//   - Empty: the bytes we moved were the only copy, so they go back. This is the ordinary
		//     failure (a refused trash cap, an audit write that could not commit).
		//   - Occupied: a writer committed after our move, so what sits there is its content and
		//     the copy we moved is superseded. Putting ours back would overwrite exactly the write
		//     that a concurrent request returned success for.
		//
		// This is a cheap check and the right one: the alternative was comparing content hashes,
		// which costs a full read of the file to answer a question the filesystem already knows.
		live := layout.Node{Owner: n.OwnerID.String(), Path: fromRel, IsDir: n.IsDir()}
		if _, statErr := d.Content.Stat(ctx, live); errors.Is(statErr, layout.ErrNotFound) {
			if mvErr := d.Content.Move(ctx,
				layout.Node{Owner: n.OwnerID.String(), Path: toRel, IsDir: n.IsDir()},
				live); mvErr != nil {
				d.Log.Error("swap: the delete failed and its content could not be put back; the "+
					"bytes are at the reserved name and the row still names the original",
					"node", n.ID.String(), "reserved", reserved, "error", mvErr)
			}
		} else {
			d.Log.Warn("swap: delete refused while a writer held the live path; the superseded "+
				"content is left at the reserved name and the caller must retry",
				"node", n.ID.String(), "reserved", reserved)
		}
		return terr
	}
	return nil
}
