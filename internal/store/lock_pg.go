package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL lock implementation.

func (t *pgTx) CreateLock(ctx context.Context, l Lock) error {
	// One live lock per node, decided by the unique index (migrations/0017).
	//
	// The previous statement was `INSERT ... WHERE NOT EXISTS (SELECT ... FROM
	// lock_tokens WHERE node_id = $2 ...)`, which reads as atomic and is not: under
	// READ COMMITTED two concurrent transactions each see no committed live row and
	// both insert, so PostgreSQL could grant two exclusive locks on one node while the
	// in-memory backend — which serialises transactions — could not. A unique index is
	// the only construct that makes the answer the same for both.
	//
	// Expired rows are cleared for this node first, because the index is on node_id
	// alone (`expires_at > now()` is not immutable and cannot be a partial predicate).
	// The sweeper still removes every other node's expired locks on its own cadence.
	if _, err := t.tx.Exec(ctx,
		`DELETE FROM lock_tokens WHERE node_id = $1 AND expires_at <= now()`, l.NodeID); err != nil {
		return translate(err)
	}

	const q = `INSERT INTO lock_tokens (token, node_id, user_id, owner_xml, depth, timeout_sec, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (node_id) DO NOTHING`
	tag, err := t.tx.Exec(ctx, q, l.Token, l.NodeID, l.UserID, l.OwnerXML, l.Depth, l.TimeoutSec, l.ExpiresAt)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		// Somebody else holds the node. A conflict is the honest answer, not a lock
		// the database quietly refused to record.
		return ErrConflict
	}
	return nil
}

func (t *pgTx) ActiveLock(ctx context.Context, nodeID uuid.UUID) (Lock, error) {
	const q = `SELECT token, node_id, user_id, owner_xml, depth, timeout_sec, created_at, expires_at
		FROM lock_tokens WHERE node_id = $1 AND expires_at > now() ORDER BY created_at LIMIT 1`
	return scanLock(t.tx.QueryRow(ctx, q, nodeID))
}

func (t *pgTx) RefreshLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID, expires time.Time) (Lock, error) {
	// Ownership is part of the predicate: only the lock holder can extend it.
	// Without that, any authenticated user could keep someone else's lock alive.
	const q = `UPDATE lock_tokens SET expires_at = $4
		WHERE node_id = $1 AND token = $2 AND user_id = $3 AND expires_at > now()
		RETURNING token, node_id, user_id, owner_xml, depth, timeout_sec, created_at, expires_at`
	return scanLock(t.tx.QueryRow(ctx, q, nodeID, token, userID, expires))
}

func (t *pgTx) DeleteLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID) (bool, error) {
	const q = `DELETE FROM lock_tokens
		WHERE node_id = $1 AND token = $2 AND (user_id = $3 OR user_id IS NULL)`
	tag, err := t.tx.Exec(ctx, q, nodeID, token, userID)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() > 0, nil
}

func (t *pgTx) ExpireLocks(ctx context.Context) (int, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM lock_tokens WHERE expires_at <= now()`)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

func (t *pgTx) LocksForUser(ctx context.Context, userID uuid.UUID) ([]Lock, error) {
	const q = `SELECT token, node_id, user_id, owner_xml, depth, timeout_sec, created_at, expires_at
		FROM lock_tokens WHERE user_id = $1 AND expires_at > now() ORDER BY created_at DESC`
	rows, err := t.tx.Query(ctx, q, userID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Lock
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, translate(rows.Err())
}

func (t *pgTx) scanLockRows(ctx context.Context, q string, args ...any) ([]Lock, error) {
	rows, err := t.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []Lock
	for rows.Next() {
		l, serr := scanLock(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, l)
	}
	return out, translate(rows.Err())
}

// LocksInSubtree lists every live lock on the node or below it.
//
// One recursive query rather than one per descendant: DELETE of a collection with a
// thousand files must not become a thousand round trips, and the answer is needed before
// anything is destroyed, so the cost lands on the refusal path rather than the happy one.
func (t *pgTx) LocksInSubtree(ctx context.Context, rootID uuid.UUID) ([]Lock, error) {
	const q = `
		WITH RECURSIVE subtree AS (
		    SELECT id FROM nodes WHERE id = $1
		    UNION ALL
		    SELECT n.id FROM nodes n JOIN subtree s ON n.parent_id = s.id
		)
		SELECT l.token, l.node_id, l.user_id, l.owner_xml, l.depth, l.timeout_sec,
		       l.created_at, l.expires_at
		FROM lock_tokens l
		JOIN subtree s ON s.id = l.node_id
		WHERE l.expires_at > now()
		ORDER BY l.created_at`
	return t.scanLockRows(ctx, q, rootID)
}

func scanLock(row pgx.Row) (Lock, error) {
	var l Lock
	err := row.Scan(&l.Token, &l.NodeID, &l.UserID, &l.OwnerXML, &l.Depth,
		&l.TimeoutSec, &l.CreatedAt, &l.ExpiresAt)
	if err != nil {
		return Lock{}, translate(err)
	}
	return l, nil
}

var _ LockTx = (*pgTx)(nil)
var _ = errors.Is
