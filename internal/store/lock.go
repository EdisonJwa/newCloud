package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Lock is a WebDAV write lock.
//
// It is durable on purpose. The stdlib's in-memory lock system silently breaks
// under restart or a second process, and a broken lock is how Office co-authoring
// overwrites someone else's edits without any error.
type Lock struct {
	Token      string
	NodeID     uuid.UUID
	UserID     *uuid.UUID
	OwnerXML   string
	Depth      string // "0" or "infinity"
	TimeoutSec int
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// LockTx is the lock surface.
type LockTx interface {
	// CreateLock fails with ErrConflict when an unexpired lock already exists on
	// the node. The check and the insert are one statement, so two clients racing
	// for the same file cannot both win.
	CreateLock(ctx context.Context, l Lock) error
	// ActiveLock returns the node's live lock, if any.
	ActiveLock(ctx context.Context, nodeID uuid.UUID) (Lock, error)
	// RefreshLock extends a lock the caller owns.
	RefreshLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID, expires time.Time) (Lock, error)
	// DeleteLock removes a lock owned by the caller. It reports whether one was
	// actually removed, so an ownership mismatch is distinguishable from success.
	DeleteLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID) (bool, error)
	// ExpireLocks deletes expired locks and reports how many went.
	ExpireLocks(ctx context.Context) (int, error)
	// LocksForUser lists a user's live locks, for the cleanup worker and the UI.
	LocksForUser(ctx context.Context, userID uuid.UUID) ([]Lock, error)
	// LocksInSubtree lists every live lock on the node and its descendants.
	//
	// DELETE and MOVE of a collection destroy or relocate the whole subtree, so a lock
	// anywhere inside it is a lock on something the request is about to affect. Asking per
	// node would be one query per descendant; this is one query per request.
	LocksInSubtree(ctx context.Context, rootID uuid.UUID) ([]Lock, error)
}
