// Package dircommit creates a directory in the two places a deployment stores it.
//
// A directory exists twice: as a metadata node (what listings, authorization and the trash
// use) and, on the tree layout, as a real directory on disk (what an operator sees, what a
// backup copies, and what the layout's "what you see is on disk" contract promises). The
// blob layout records a directory marker in the resolver instead of a filesystem entry, but
// the shape of the problem is the same: a request succeeds in one store and not the other.
//
// Creating the metadata node first and then ignoring the content store's error is what the
// 2026-10-23 sixth review reproduced (NC-R6-001): WebDAV COPY, the JSON batch COPY, MKCOL
// and REST mkdir all answered 2xx while the tree layout had no directory on disk, and the
// metadata and the success audit row said otherwise. The failure is not a traversal bug —
// the source tree was fine — it is an output write that was never checked.
//
// The order here is the one the upload path already uses: write content first, then commit
// metadata. If the physical create fails, the metadata transaction never runs, so there is
// no node and no "ok" audit row claiming a directory that does not exist.
//
// # A failed metadata commit leaves an orphan, deliberately
//
// When the metadata transaction fails after the directory was created, this package does NOT
// remove it. That is a decision, not an omission. The seventh review reproduced two ways a
// compensating delete loses data (P0-R7-001/002):
//
//   - checking that the directory is empty and then calling the store's recursive Delete
//     leaves a gap in which another writer's file appears; the Delete removes it. The tree
//     store's Delete is RemoveAll, not an atomic "remove only if still empty".
//   - the "it did not exist when I started" observation is not proof of ownership. A second
//     request can commit a live metadata node for the same path between that observation and
//     the cleanup, and the loser then deletes the winner's directory.
//
// No observation taken before the metadata transaction survives the whole window, and adding
// one more metadata lookup still leaves a check-then-delete race. The safe outcome when
// ownership cannot be proven is to retain the directory: it may be unreferenced, or a
// concurrent request may have adopted the same name and made it live, and the create path
// cannot tell which after the fact. Either way, no destructive cleanup is attempted. Deleting
// somebody else's bytes is not recoverable. A startup or operator reconciliation pass may
// remove proven-empty directories under a lock that also covers name creation; that is future
// work, not claimed here.
package dircommit

import (
	"context"
	"errors"
	"fmt"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// Create makes dir exist in the content store first, then runs commit, which must create
// the corresponding metadata node (and its success audit row) in one transaction.
//
// commit runs only after the content store accepted the directory, so an error from it
// cannot leave a live node that names a directory the content store does not have. If
// commit fails, the directory is retained unchanged: see the package comment for why
// removing it is not safe. It may have been adopted by a concurrent create by then, so
// neither "unreferenced" nor "empty" is guaranteed.
//
// This is not distributed atomicity between a filesystem and a database. It is the
// ordering that makes the failure the client receives true. The remaining inconsistency is
// a retention decision, not a claim that the path is unreferenced or empty.
func Create(
	ctx context.Context,
	content layout.ContentStore,
	dir layout.Node,
	commit func(context.Context) error,
) error {
	if !dir.IsDir {
		return fmt.Errorf("%w: Create requires a directory node", layout.ErrInvalidNode)
	}
	if commit == nil {
		return errors.New("dircommit: no metadata commit function")
	}

	if err := content.Mkdir(ctx, dir); err != nil {
		return fmt.Errorf("create directory %q in the content store: %w", dir.Path, err)
	}

	if err := commit(ctx); err != nil {
		// Keep the error the client can act on (503 for an audit outage, 409 for a conflict),
		// and say what state was left behind. Nothing here touches the directory again.
		return fmt.Errorf("%w (the content-store path %q was created before the metadata "+
			"transaction and is retained; its ownership may have changed while the transaction "+
			"ran, so no destructive cleanup was attempted)", err, dir.Path)
	}
	return nil
}
