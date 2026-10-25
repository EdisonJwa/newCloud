package store

// Directory ETag propagation (M1, competitiveness-analysis §5).
//
// A walking sync client — every generic WebDAV client, including Nextcloud's own desktop
// client — detects change by comparing the ETag each PROPFIND Depth:1 returns for a
// collection. An ETag that never changes (which is what a directory whose etag is derived
// from its own mtime effectively was: nothing updated a directory's mtime when a child
// changed) forces the client to walk every subtree on every pass. That walk is the single
// most-complained-about property of the category's sync clients, and the server half of
// fixing it is here: every mutation stamps a fresh change token on the containing directory
// and every directory above it, in the same transaction.
//
// Files are never stamped by this machinery. A file's ETag is its content hash and the
// precondition vocabulary (If-Match, If-None-Match) is written in terms of it; a random
// token on a file would break conditional writes for no sync benefit. Only collections get
// change tokens.

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// newChangeToken mints one quoted token, in the same `"hex"` shape etagFor gives files so a
// client never sees two etag grammars from one server.
func newChangeToken() string {
	return `"` + strings.ReplaceAll(uuid.New().String(), "-", "")[:16] + `"`
}

// bumpDirAndUp stamps dirID and every LIVE ancestor of it with a fresh change token.
//
// The token is per call, not per row: one mutation is one change, and every directory it
// reaches carrying the same token says exactly that — "this subtree changed at this event".
// The token is random rather than clock-derived because the in-memory backend's clock can
// return the same value twice in a row (measured on this platform), and an etag that can
// repeat after a mutation is an etag a client will trust once too often.
//
// Trashed ancestors are skipped: they are invisible to every listing, and churning their
// tokens would give the trash view an etag churn nothing reads.
func (t *pgTx) bumpDirAndUp(ctx context.Context, dirID uuid.UUID) error {
	if dirID == uuid.Nil {
		return nil
	}
	const q = `
		WITH RECURSIVE up AS (
		    SELECT id, parent_id FROM nodes WHERE id = $1 AND trashed_at IS NULL
		    UNION
		    SELECT n.id, n.parent_id FROM nodes n JOIN up u ON n.id = u.parent_id
		)
		UPDATE nodes SET etag = $2, modified_at = now()
		WHERE id IN (SELECT id FROM up) AND trashed_at IS NULL`
	_, err := t.tx.Exec(ctx, q, dirID, newChangeToken())
	return translate(err)
}

// bumpDirAndUp is the memory twin of the PostgreSQL statement above: same semantics, same
// skips (trashed rows), same per-call token. The workspace is transaction-private, so the
// walk cannot race anything.
func (t *memTx) bumpDirAndUp(ctx context.Context, dirID uuid.UUID) error {
	if dirID == uuid.Nil {
		return nil
	}
	token := newChangeToken()
	now := t.m.now()
	id := dirID
	for {
		rec, ok := t.m.nodes[id]
		if !ok || rec.node.TrashedAt != nil {
			return nil
		}
		rec.node.ETag = token
		rec.node.ModifiedAt = now
		if rec.node.ParentID == nil {
			return nil
		}
		id = *rec.node.ParentID
	}
}
