package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of LoginFlowTx (ADR-0015).

// loginFlowColumns is shared by every scan so the column order cannot drift.
const loginFlowColumns = `id, user_id, client_name, created_at, expires_at, approved_at`

func (t *pgTx) CreateLoginFlow(ctx context.Context, f LoginFlow, pollHash, loginHash [32]byte) error {
	const q = `INSERT INTO login_flows (id, poll_token_hash, login_token_hash, client_name, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := t.tx.Exec(ctx, q, f.ID, pollHash[:], loginHash[:], f.ClientName, f.CreatedAt, f.ExpiresAt)
	return translate(err)
}

func (t *pgTx) LoginFlowByLoginHash(ctx context.Context, loginHash [32]byte) (LoginFlow, error) {
	// Expiry is evaluated by the database's clock, not the caller's: the row's window is
	// what the server promised the browser, and a skewed process clock must not extend it.
	const q = `SELECT ` + loginFlowColumns + ` FROM login_flows
		WHERE login_token_hash = $1 AND expires_at > now()`
	return scanLoginFlow(t.tx.QueryRow(ctx, q, loginHash[:]))
}

func (t *pgTx) ApproveLoginFlow(ctx context.Context, loginHash [32]byte, userID uuid.UUID,
	at time.Time) (LoginFlow, error) {

	// The predicate is the whole concurrency story: only an unapproved flow (or one
	// already claimed by this same account, which is a harmless double-submit) can be
	// approved, and the row must still be inside its window.
	const q = `UPDATE login_flows SET user_id = $2, approved_at = $3
		WHERE login_token_hash = $1 AND expires_at > $3
		  AND (user_id IS NULL OR user_id = $2)
		RETURNING ` + loginFlowColumns
	flow, err := scanLoginFlow(t.tx.QueryRow(ctx, q, loginHash[:], userID, at))
	if err == nil {
		return flow, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return LoginFlow{}, err
	}

	// No row was updated. Distinguish "gone/expired" from "somebody else approved it
	// first", because the browser page should say the truth about which happened.
	existing, serr := scanLoginFlow(t.tx.QueryRow(ctx,
		`SELECT `+loginFlowColumns+` FROM login_flows WHERE login_token_hash = $1`, loginHash[:]))
	if serr != nil {
		return LoginFlow{}, serr
	}
	if existing.UserID != nil && *existing.UserID != userID {
		return LoginFlow{}, ErrLoginFlowConflict
	}
	// Same account but expired: the caller's request was well-formed and the flow is no
	// longer usable, which the handler answers the same way it answers "unknown token".
	return LoginFlow{}, ErrNotFound
}

func (t *pgTx) ConsumeLoginFlow(ctx context.Context, pollHash [32]byte, at time.Time) (LoginFlow, error) {
	// DELETE ... RETURNING rather than UPDATE: the row is single-use, and deleting it
	// makes a replay impossible even for a caller holding both hashes. The approval and
	// expiry predicates mean an unapproved flow is never consumed and stays available to
	// the browser step.
	const q = `DELETE FROM login_flows
		WHERE poll_token_hash = $1 AND user_id IS NOT NULL AND approved_at IS NOT NULL
		  AND expires_at > $2
		RETURNING ` + loginFlowColumns
	return scanLoginFlow(t.tx.QueryRow(ctx, q, pollHash[:], at))
}

func (t *pgTx) DeleteExpiredLoginFlows(ctx context.Context, now time.Time) (int, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM login_flows WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

func scanLoginFlow(row pgx.Row) (LoginFlow, error) {
	var f LoginFlow
	if err := row.Scan(&f.ID, &f.UserID, &f.ClientName, &f.CreatedAt, &f.ExpiresAt,
		&f.ApprovedAt); err != nil {
		return LoginFlow{}, translate(err)
	}
	return f, nil
}
