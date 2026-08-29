package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PostgreSQL implementation of SharePasswordTx.

func (t *pgTx) PasswordFailureCount(ctx context.Context, shareID uuid.UUID,
	window time.Duration) (int, error) {

	const q = `SELECT count(*) FROM share_password_attempts
		WHERE share_id = $1 AND NOT succeeded
		  AND attempted_at > now() - $2::interval`
	var n int
	if err := t.tx.QueryRow(ctx, q, shareID, intervalOf(window)).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

func (t *pgTx) RecordPasswordAttempt(ctx context.Context, shareID uuid.UUID,
	ip string, succeeded bool) error {

	const q = `INSERT INTO share_password_attempts (share_id, ip, succeeded)
		VALUES ($1, $2, $3)`
	_, err := t.tx.Exec(ctx, q, shareID, nullableString(ip), succeeded)
	return translate(err)
}

func (t *pgTx) PrunePasswordAttempts(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM share_password_attempts
		WHERE attempted_at <= now() - $1::interval`, intervalOf(olderThan))
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}
