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
	// A conditional insert: the WHERE clause makes "is there a conflicting lock"
	// and "take the lock" one atomic step. Reading then inserting would let two
	// clients both observe "no lock" and both proceed.
	//
	// Expired locks do not block, so a client that crashed mid-edit cannot lock a
	// file out forever.
	const q = `INSERT INTO lock_tokens (token, node_id, user_id, owner_xml, depth, timeout_sec, expires_at)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (
		    SELECT 1 FROM lock_tokens
		    WHERE node_id = $2 AND expires_at > now()
		)`
	tag, err := t.tx.Exec(ctx, q, l.Token, l.NodeID, l.UserID, l.OwnerXML, l.Depth, l.TimeoutSec, l.ExpiresAt)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
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
