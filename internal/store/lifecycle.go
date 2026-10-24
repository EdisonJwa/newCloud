package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PurgeResult reports what a lifecycle sweep removed.
type PurgeResult struct {
	// Nodes is how many node rows were permanently deleted (a subtree counts once
	// per row, not once per root).
	Nodes int
	// Roots is how many top-level trash entries went away, which is what a user
	// would see disappear from their trash list.
	Roots int
	// Bytes is the storage released from the quota tally.
	Bytes int64
	// More reports that the batch bound was reached, so the caller knows the bin is
	// not necessarily empty.
	//
	// User-initiated purge (core-scope §3) runs in bounded batches and may be called again; it is
	// idempotent, so a client that sees More still knows exactly what state it is in — "call me
	// again", never "unknown". The worker's sweep ignores it: the next pass picks up where this one
	// stopped, and reporting it there would only be noise.
	More bool
}

// LifecycleTx is the retention surface.
//
// Before this existed there was no code path anywhere that deleted a node: the
// trash grew without bound, and "trash_retention_days" was a configuration value
// the product described but could not honour.
type LifecycleTx interface {
	// PurgeTrash permanently deletes trashed subtrees older than retention,
	// releasing their quota and dropping the versions they own.
	//
	// A zero retention means "never purge", which is why the caller must pass an
	// explicit duration rather than a default: deleting a user's recoverable data
	// because a field was omitted is not a failure mode worth having.
	PurgeTrash(ctx context.Context, retention time.Duration) (PurgeResult, error)

	// PurgeTrashFor permanently deletes ONE account's entire trash, ignoring retention
	// (core-scope §3).
	//
	// This is "Empty trash": the user has decided, so the retention window that protects them
	// from an accidental delete is exactly the thing they are overriding. It runs through the same
	// accounting and the same recursive delete as PurgeTrash — there is no second purge
	// implementation, because a second one is a second set of refcount, quota and retention bugs.
	//
	// maxRoots bounds one call. The handler runs it in batches and may be asked again; a bin with
	// a million entries must not hold one HTTP request for minutes.
	//
	// ownerID scopes it absolutely: a caller can only ever purge what they own, and the ownership
	// test is in the statement that selects the roots, not in the handler that asked.
	PurgeTrashFor(ctx context.Context, ownerID uuid.UUID, maxRoots int) (PurgeResult, error)

	// ExpireVersions deletes version rows older than retention, keeping refcounts
	// correct. Zero means never.
	ExpireVersions(ctx context.Context, retention time.Duration) (int, error)
}

// PurgeableTrashRoot reports one trashed subtree root that is old enough to purge.
//
// The type exists so a caller can preview a purge (which is what a dry run needs)
// without performing it.
type PurgeableTrashRoot struct {
	NodeID    uuid.UUID
	OwnerID   uuid.UUID
	Name      string
	TrashedAt time.Time
}
