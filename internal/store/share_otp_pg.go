package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of ShareOTPTx.

func (t *pgTx) OTPFailureCount(ctx context.Context, shareID uuid.UUID, email string,
	window time.Duration) (int, error) {

	const q = `SELECT count(*) FROM share_otp_attempts
		WHERE share_id = $1 AND email = $2 AND NOT succeeded
		  AND attempted_at > now() - $3::interval`
	var n int
	if err := t.tx.QueryRow(ctx, q, shareID, email, intervalOf(window)).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

func (t *pgTx) RecordOTPAttempt(ctx context.Context, a OTPAttempt) error {
	const q = `INSERT INTO share_otp_attempts (share_id, email, succeeded, ip, reason)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := t.tx.Exec(ctx, q, a.ShareID, a.Email, a.Success,
		nullableString(a.IP), a.Reason)
	return translate(err)
}

// ConsumeRecipientCode marks the code used exactly once.
//
// The UPDATE's WHERE clause is the concurrency control: two simultaneous
// verifications of the same code both reach here, and only one sees
// consumed_at IS NULL. A read-then-write would let both through.
func (t *pgTx) ConsumeRecipientCode(ctx context.Context, shareID uuid.UUID, email string) error {
	const q = `UPDATE share_recipients SET consumed_at = now()
		WHERE share_id = $1 AND email = $2 AND consumed_at IS NULL
		  AND revoked_at IS NULL`
	tag, err := t.tx.Exec(ctx, q, shareID, email)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCodeConsumed
	}
	return nil
}

func (t *pgTx) PruneOTPAttempts(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM share_otp_attempts
		WHERE attempted_at <= now() - $1::interval`, intervalOf(olderThan))
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// intervalOf renders a duration as a PostgreSQL interval literal.
//
// The driver has no native Duration mapping, so passing a time.Duration through a
// parameter sends nanoseconds and PostgreSQL rejects it as an unknown type.
// intervalOf renders a duration as a PostgreSQL interval literal.
//
// time.Duration.String is not valid SQL: it emits "1ns" or "1h30m", neither of which
// PostgreSQL parses ("invalid input syntax for type interval"). Microseconds are the
// unit because it is PostgreSQL's own smallest interval resolution and every value this
// package passes is a lease or a backoff, where sub-microsecond precision is
// meaningless.
func intervalOf(d time.Duration) string {
	return fmt.Sprintf("%d microseconds", d.Microseconds())
}

var _ ShareOTPTx = (*pgTx)(nil)
var _ = pgx.ErrNoRows
