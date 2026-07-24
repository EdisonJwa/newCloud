package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementations of AppPasswordTx.

func (t *pgTx) CreateAppPassword(ctx context.Context, p AppPassword) error {
	const q = `INSERT INTO app_passwords (id, user_id, label, prefix, token_hash, scope, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := t.tx.Exec(ctx, q, p.ID, p.UserID, p.Label, p.Prefix, p.TokenHash, p.Scope, p.ExpiresAt)
	return translate(err)
}

func (t *pgTx) AppPasswordByHash(ctx context.Context, hash []byte) (AppPassword, error) {
	const q = `SELECT id, user_id, label, prefix, token_hash, scope, created_at,
		expires_at, last_used_at, COALESCE(last_used_ip::text,''), revoked_at
		FROM app_passwords WHERE token_hash = $1`
	return scanAppPassword(t.tx.QueryRow(ctx, q, hash))
}

func (t *pgTx) TouchAppPassword(ctx context.Context, id uuid.UUID, ip string) error {
	const q = `UPDATE app_passwords SET last_used_at = now(), last_used_ip = $2 WHERE id = $1`
	_, err := t.tx.Exec(ctx, q, id, nullableString(ip))
	return translate(err)
}

func (t *pgTx) RevokeAppPassword(ctx context.Context, id uuid.UUID) error {
	const q = `UPDATE app_passwords SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	_, err := t.tx.Exec(ctx, q, id)
	return translate(err)
}

func (t *pgTx) ListAppPasswords(ctx context.Context, userID uuid.UUID) ([]AppPassword, error) {
	const q = `SELECT id, user_id, label, prefix, token_hash, scope, created_at,
		expires_at, last_used_at, COALESCE(last_used_ip::text,''), revoked_at
		FROM app_passwords WHERE user_id = $1
		ORDER BY created_at DESC`
	rows, err := t.tx.Query(ctx, q, userID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []AppPassword
	for rows.Next() {
		p, err := scanAppPassword(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}

// scanAppPassword is shared by the single-row and multi-row paths so the column
// order cannot drift between them.
func scanAppPassword(row pgx.Row) (AppPassword, error) {
	var p AppPassword
	err := row.Scan(&p.ID, &p.UserID, &p.Label, &p.Prefix, &p.TokenHash, &p.Scope,
		&p.CreatedAt, &p.ExpiresAt, &p.LastUsed, &p.LastIP, &p.RevokedAt)
	if err != nil {
		return AppPassword{}, translate(err)
	}
	return p, nil
}

var _ AppPasswordTx = (*pgTx)(nil)
var _ = errors.Is
