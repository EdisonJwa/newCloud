// Package swap installs a staged replacement over an existing node.
//
// This is one operation with a lot of sharp edges, and it exists because getting it wrong destroys
// the thing it was asked to replace. It was first written for WebDAV COPY (NC-RR-005), where the
// original implementation trashed the destination BEFORE the copy began, so a copy that then failed
// left the user's path empty while reporting an error. The JSON batch API then grew its own, simpler
// version — and reproduced the same class of bug on the tree layout one round later, because the
// ordering that makes it work is not obvious from the code that fails. Both callers now use this.
//
// The ordering is the whole content of the function, and it is not interchangeable:
//
//  1. The replacement is built BESIDE the destination, under a reserved sibling name. Nothing has
//     touched the destination yet, so a failure here is clean: the original is untouched.
//
//  2. For a PATH-BACKED layout (tree), the physical moves happen BEFORE the metadata switch. The old
//     destination still occupies its path on disk — trashing is a metadata operation and leaves the
//     bytes where they are — so renaming the staged tree onto it fails with ENOTEMPTY for a
//     directory. Doing the switch first leaves the metadata naming a node whose bytes were never
//     installed: the destination path then reads 404 while the operation reports 500.
//
//  3. The namespace switch is ONE metadata transaction: original renamed aside, staged renamed into
//     place, displaced original trashed, both audit rows written. A completed swap must never be
//     missing from the log, and an interrupted one must not claim a success it did not reach.
//
// A layout with EfficientRename (blob) skips step 2 entirely: a rename is a metadata update there,
// so there are no bytes to move and no window in which the destination path is briefly absent.
//
// # Two kinds of stage, and why the difference is load-bearing
//
// The staged node is not always something this function may destroy. A COPY overwrite builds a new
// duplicate beside the destination: it is disposable, nobody can name it, and a failed install must
// remove it. A MOVE overwrite has nothing to duplicate — the "stage" is the caller's EXISTING source
// node, relocated to a reserved sibling name. It is borrowed: on failure the only correct outcome is
// that it goes back where it came from, metadata and bytes.
//
// Treating the second as the first is how a full trash turned a MOVE into data loss: the metadata
// transaction rolled back with ErrTrashFull, the rollback deleted the staged bytes, and the source
// path answered 404 because its node's content had been removed (2026-10-23 review, P0). A boolean
// parameter would let that mistake be made again by omission, so a stage cannot be named without
// saying which kind it is: see Replacement, Disposable and Borrowed.
package swap

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// Deps are the collaborators an install needs.
type Deps struct {
	Meta store.Store
	// Content is the content store the owner's bytes live in.
	Content layout.ContentStore
	Log     *slog.Logger
	// TrashCap is the retained-usage cap for the trash, resolved by the caller because it is a
	// runtime setting rather than a constant.
	TrashCap int64
}

// Request describes one staged replacement.
type Request struct {
	// Owner is the content-store namespace, i.e. the actor's account id.
	Owner string
	// Parent is the destination's parent node, and ParentRel that parent's path.
	Parent    store.Node
	ParentRel string
	DstName   string
	// Existing is the node being replaced; its bytes are at ParentRel/DstName.
	Existing store.Node
	// StagedIsDir says whether the replacement is a directory, which cleanup needs in order to
	// remove its bytes on a path-backed layout.
	StagedIsDir bool
	// Replacement builds the staged node and states who owns it, which is what decides how a
	// failed install compensates. It must not touch the destination: everything it writes is
	// beside it, so a failure here leaves the destination untouched.
	Replacement Replacement
	// AuditRows runs inside the switching transaction. Both callers write two rows — the install and
	// the deletion of the displaced original — and their metadata differs (a COPY names its source,
	// a MOVE names both paths), so the shape is a callback rather than a fixed struct.
	AuditRows func(ctx context.Context, tx store.Tx, installed, aside store.Node) error
	// The actor fields below are used only by the rollback audit row a disposable stage writes.
	ActorID    *uuid.UUID
	ActorEmail string
	ActorIP    string
	UserAgent  string
}

// Replacement is the node a swap installs, together with the ownership rule that decides what a
// failure may do to it.
//
// The zero value is invalid and Install refuses it. That is the point: a caller cannot name a
// staged node without saying whether it is a copy this operation made or the caller's own file
// temporarily relocated, and the two need opposite compensation. Getting that wrong in the
// direction of "clean it up like a copy" deletes user data (see the package comment).
type Replacement struct {
	build    func(ctx context.Context, stagedName string) (store.Node, error)
	restore  func(ctx context.Context, staged store.Node) error
	borrowed bool
}

// Disposable stages a NEW node that this operation creates and owns.
//
// The bytes are a duplicate nobody else can name, and the row exists only for this swap, so every
// failure path removes both: leaving them behind would hold quota and put a name in the user's
// listing that they never created.
func Disposable(build func(ctx context.Context, stagedName string) (store.Node, error)) Replacement {
	return Replacement{build: build}
}

// Borrowed stages an EXISTING node that the caller has temporarily relocated to the reserved name
// — the MOVE overwrite case, where there is no duplicate to build.
//
// restore must undo that relocation completely: the node's name, its parent and, on a path-backed
// layout, its bytes back at the original path. It is called instead of any cleanup on every failure
// after the stage succeeded. Nothing in this package may delete a borrowed node's content or trash
// its row, because both are the user's file, not a by-product of the swap.
//
// A restore that fails leaves the node at the intermediate name with its bytes intact — loud,
// recoverable, and never destroyed. That is the only acceptable last resort.
func Borrowed(
	build func(ctx context.Context, stagedName string) (store.Node, error),
	restore func(ctx context.Context, staged store.Node) error,
) Replacement {
	return Replacement{build: build, restore: restore, borrowed: true}
}

// Install builds the replacement beside the destination and switches it into place.
//
// On success the destination name holds the staged node and the previous node is in the trash with
// its bytes intact at the backup path. On failure nothing is destroyed: a disposable stage is
// removed, a borrowed stage is returned to its owner, and the original's bytes are put back at the
// destination path.
func Install(ctx context.Context, d Deps, in Request) (store.Node, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if in.Replacement.build == nil {
		return store.Node{}, errors.New(
			"swap: the replacement was not declared as swap.Disposable or swap.Borrowed")
	}
	if in.Replacement.borrowed && in.Replacement.restore == nil {
		return store.Node{}, errors.New("swap: a borrowed replacement has no Restore function")
	}
	// Refuse an unusable address before anything is created or moved. A caller that handed over an
	// absolute parent path (the REST layer's "/a/b" spelling instead of its internal "a/b") would
	// otherwise fail half-way through — after a borrowed source had already been relocated.
	if _, err := layout.ValidatePath(in.ParentRel); err != nil {
		return store.Node{}, fmt.Errorf("swap: parent path %q is not a relative path: %w", in.ParentRel, err)
	}
	if err := layout.ValidateSegment(in.DstName); err != nil {
		return store.Node{}, fmt.Errorf("swap: destination name %q: %w", in.DstName, err)
	}

	pathsFollowContent := !d.Content.Capabilities().EfficientRename
	stagedName := StagedName(in.DstName)
	stagedRel := path.Join(in.ParentRel, stagedName)
	dstRel := path.Join(in.ParentRel, in.DstName)
	backupName := BackupName(in.DstName)
	backupRel := path.Join(in.ParentRel, backupName)

	undo := func(staged store.Node, reason string) {
		undoStaged(ctx, d, in, staged, stagedName, stagedRel, reason)
	}

	// Step 1: the replacement, beside the destination.
	staged, err := in.Replacement.build(ctx, stagedName)
	if err != nil {
		// Nothing has touched the destination. A disposable stage may have left bytes behind and
		// they are ours to remove. A borrowed stage is the caller's own node: its builder must
		// reverse any partial work itself, because only it knows where the node came from — and
		// nothing here may call Restore, since a stage that failed did not return a node to restore.
		if !in.Replacement.borrowed {
			discardStaged(ctx, d, in, stagedName, stagedRel, "replacement that could not be staged")
		} else {
			d.Log.Warn("swap: the borrowed replacement could not be staged; "+
				"its own builder is responsible for putting the source back",
				"name", in.DstName, "error", err)
		}
		return store.Node{}, err
	}

	if pathsFollowContent {
		// Step 2: the original's bytes have to leave the destination path before anything can be
		// installed there. A trashed node keeps its bytes, so trashing the metadata does NOT free
		// the path.
		if err := d.Content.Move(ctx,
			layout.Node{Owner: in.Owner, Path: dstRel, IsDir: in.Existing.IsDir()},
			layout.Node{Owner: in.Owner, Path: backupRel, IsDir: in.Existing.IsDir()}); err != nil {
			undo(staged, "staged replacement whose original could not be moved aside")
			return store.Node{}, fmt.Errorf("swap: move the original aside: %w", err)
		}
		if err := d.Content.Move(ctx,
			layout.Node{Owner: in.Owner, Path: stagedRel, IsDir: in.StagedIsDir},
			layout.Node{Owner: in.Owner, Path: dstRel, IsDir: in.StagedIsDir}); err != nil {
			// The destination path is empty at this point, so the original is put back before the
			// failure is reported — otherwise the caller sees an error and an unreadable path.
			restoreOriginal(ctx, d, in, dstRel, backupRel)
			undo(staged, "staged replacement that could not be installed")
			return store.Node{}, fmt.Errorf("swap: install the staged bytes: %w", err)
		}
	}

	// Step 3: the namespace switch, in one transaction.
	var installed store.Node
	err = d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Rename the original OUT of the destination name first: the destination has to be free
		// before the staged node can take it, and a rename to a free sibling name cannot fail for a
		// name conflict.
		aside, rerr := tx.Rename(ctx, in.Existing.ID, backupName)
		if rerr != nil {
			return rerr
		}
		moved, rerr := tx.Rename(ctx, staged.ID, in.DstName)
		if rerr != nil {
			return rerr
		}
		installed = moved

		// The displaced original goes to the trash in the same transaction that makes the
		// replacement live. Trashing it later would leave a window in which both copies are visible
		// and neither is in the bin, which is a state a sync client can act on.
		if terr := tx.TrashCapped(ctx, aside.ID, d.TrashCap); terr != nil {
			return terr
		}
		if in.AuditRows != nil {
			return in.AuditRows(ctx, tx, installed, aside)
		}
		return nil
	})
	if err != nil {
		d.Log.Error("swap: the metadata switch failed; restoring the original",
			"name", in.DstName, "error", err)
		if pathsFollowContent {
			// Put the staged bytes back out of the destination path, then the original back into it.
			if rerr := d.Content.Move(ctx,
				layout.Node{Owner: in.Owner, Path: dstRel, IsDir: in.StagedIsDir},
				layout.Node{Owner: in.Owner, Path: stagedRel, IsDir: in.StagedIsDir}); rerr != nil {
				d.Log.Error("swap: could not move the staged bytes back out of the destination path",
					"path", dstRel, "error", rerr)
			}
			restoreOriginal(ctx, d, in, dstRel, backupRel)
		}
		undo(staged, "staged replacement whose switch did not commit")
		return store.Node{}, err
	}

	return installed, nil
}

// restoreOriginal puts the displaced original's bytes back at the destination path.
//
// Best effort and loud: the caller is already returning an error, and a compensation failure must
// not replace the real cause — but it must be visible, because the state it leaves needs an operator.
func restoreOriginal(ctx context.Context, d Deps, in Request, dstRel, backupRel string) {
	if rerr := d.Content.Move(ctx,
		layout.Node{Owner: in.Owner, Path: backupRel, IsDir: in.Existing.IsDir()},
		layout.Node{Owner: in.Owner, Path: dstRel, IsDir: in.Existing.IsDir()}); rerr != nil {
		d.Log.Error("swap: COMPENSATION FAILED — the original's bytes are not at the destination path",
			"dst", dstRel, "backup", backupRel, "error", rerr)
	}
}

// undoStaged is the single decision point for "the install did not commit, what happens to the
// staged node?".
//
// Disposable: remove it. Borrowed: put it back. Keeping the choice in one function is what stops a
// failure path added later from inheriting the cleanup that was only ever right for a copy.
func undoStaged(ctx context.Context, d Deps, in Request, staged store.Node, stagedName, stagedRel, reason string) {
	if !in.Replacement.borrowed {
		discardStaged(ctx, d, in, stagedName, stagedRel, reason)
		return
	}
	if err := in.Replacement.restore(ctx, staged); err != nil {
		d.Log.Error("swap: COMPENSATION FAILED — the borrowed node was left at the intermediate "+
			"name with its bytes intact; it was NOT deleted. An operator can recover it by hand",
			"name", stagedName, "node", staged.ID.String(), "reason", reason, "error", err)
		return
	}
	d.Log.Warn("swap: returned a borrowed replacement to its owner after a failed install",
		"name", stagedName, "node", staged.ID.String(), "reason", reason)
}

// discardStaged removes a staged replacement completely.
//
// Trashing the metadata node is not enough on a path-backed layout: a trashed node keeps its bytes
// until the retention sweep purges it, so a failed overwrite would leave a full copy of the source
// occupying the account's quota, and the audit trail would name a folder the user never created.
// The bytes are therefore removed explicitly; the row is trashed rather than purged because the
// store exposes no single-node purge and a trashed row is harmless.
//
// It refuses a borrowed stage outright, so a future failure path that calls it directly cannot
// delete the caller's own node: destroying user data has to be impossible, not merely undocumented.
func discardStaged(ctx context.Context, d Deps, in Request, stagedName, stagedRel, reason string) {
	if in.Replacement.borrowed {
		d.Log.Error("swap: refusing to discard a borrowed replacement — it is the caller's own node, "+
			"and deleting it is the data-loss defect this ownership rule exists to prevent",
			"name", stagedName, "reason", reason)
		return
	}
	if !d.Content.Capabilities().EfficientRename && stagedRel != "" {
		err := d.Content.Delete(ctx, layout.Node{
			Owner: in.Owner, Path: stagedRel, IsDir: in.StagedIsDir,
		})
		if err != nil && !errors.Is(err, layout.ErrNotFound) {
			d.Log.Error("swap: could not remove the staged bytes",
				"path", stagedRel, "error", err)
		}
	}
	trashSibling(ctx, d, in, stagedName, reason)
}

// trashSibling trashes a node by name under the destination's parent.
//
// Used only on cleanup paths, where the operation has already been reported as failed and a second
// error would replace the cause with the cleanup's own.
func trashSibling(ctx context.Context, d Deps, in Request, name, reason string) {
	var victim store.Node
	err := d.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, in.Parent.ID, name)
		if cerr != nil {
			return cerr
		}
		victim = n
		if terr := tx.Trash(ctx, n.ID); terr != nil {
			return terr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", ActorUserID: in.ActorID, ActorEmail: in.ActorEmail,
			ActorIP: in.ActorIP, UserAgent: in.UserAgent,
			Action: "file.write", TargetKind: "node", TargetID: n.ID.String(),
			Result:   "denied",
			Metadata: map[string]any{"name": name, "rollback": reason},
		})
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		d.Log.Error("swap: could not trash the intermediate node; remove it by hand",
			"name", name, "reason", reason, "error", err)
		return
	}
	if victim.ID != (uuid.UUID{}) {
		d.Log.Warn("swap: trashed an intermediate node",
			"name", name, "node", victim.ID.String(), "reason", reason)
	}
}

// StagedName is the reserved sibling name a replacement is built under.
func StagedName(dstName string) string { return reservedName("staged", dstName) }

// BackupName is the reserved sibling name the displaced original is renamed to.
func BackupName(dstName string) string { return reservedName("replaced", dstName) }

// reservedName builds "<rand>.newcloud-<kind>-<dst>".
//
// These must be legal node names, because the store validates every name with the same
// layout.ValidateSegment rule a client's name gets — a control-character prefix (the obvious way to
// make a name unclaimable) is rejected and the whole operation would fail with "invalid name". They
// are instead made unreservable by convention and by shape:
//
//   - the ".newcloud-" infix is reserved by this product and documented as such;
//   - the destination name is embedded, so the intermediate is attributable in a log line and an
//     audit row rather than being an anonymous hex string an operator cannot place;
//   - eight random bytes make a collision with a real file effectively impossible AND make two
//     concurrent overwrites of the same destination independent — a deterministic suffix would have
//     both stage to the same name and one would fail with a conflict.
//
// The destination name is truncated when long: a 255-byte legal name plus the prefix would exceed
// the limit the validator enforces. The truncation walks back to a rune boundary, because slicing at
// a byte offset can split a multi-byte character and layout.ValidateSegment rejects invalid UTF-8 —
// which would fail the whole operation for a file whose only crime was a long non-ASCII name.
func reservedName(kind, dstName string) string {
	var buf [8]byte
	_, _ = crand.Read(buf[:])
	marker := hex.EncodeToString(buf[:])

	const maxDst = 160
	if len(dstName) > maxDst {
		dstName = dstName[:maxDst]
		for len(dstName) > 0 && !utf8.ValidString(dstName) {
			dstName = dstName[:len(dstName)-1]
		}
	}
	return marker + ".newcloud-" + kind + "-" + dstName
}
