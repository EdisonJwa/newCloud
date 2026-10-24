package store

// Session retention (2026-10-25 review: the sessions table grew without bound).
//
// A row is inserted per login and the only exits were per-session revocation calls. Expired
// sessions kept their rows forever: SessionLive reads expiry, it does not delete, so a
// deployment that has been running for a year holds a year of every user's logins — address,
// agent, count — in a table nothing reads again. Retention turns "we keep sessions revocable"
// into "we keep sessions revocable for a bounded window".
//
// The window is deliberately longer than any session: rows are removed only once they have
// been dead for the retention period, so a still-live (or recently expired, or recently
// revoked) session can never be collected by a sweep racing a login. Being late costs a few
// rows of disk; being early costs an auditable credential record.

import (
	"context"
	"time"
)

// SessionRetention is how long a dead (expired or revoked) session row is kept past its
// death before the prune removes it.
//
// Thirty days covers any investigation that starts from "who was signed in when this
// happened": session TTLs are hours, so a month past expiry keeps the record long after the
// credential could ever authenticate, while still bounding the table for a busy deployment.
const SessionRetention = 30 * 24 * time.Hour

// PruneSessions deletes session rows that died more than olderThan ago, returning how many
// rows went.
//
// A row's death is the moment it became unauthenticatable: a revoked row dies at
// revoked_at, an unrevoked one at expires_at. A live session is never touched, even with
// olderThan of zero — a sweep racing a login must not be able to collect the credential
// that login just minted.
func (t *pgTx) PruneSessions(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM sessions
		WHERE (revoked_at IS NOT NULL AND revoked_at <= now() - $1::interval)
		   OR (revoked_at IS NULL AND expires_at <= now() - $1::interval)`,
		intervalOf(olderThan))
	if err != nil {
		return 0, translate(err)
	}
	return int64(tag.RowsAffected()), nil
}

// PruneSessions mirrors the PostgreSQL rule on the in-memory store, so the worker's duty
// behaves identically in tests and in production and the two backends cannot disagree about
// which rows survive a prune.
func (t *memTx) PruneSessions(ctx context.Context, olderThan time.Duration) (int64, error) {
	horizon := t.m.now().Add(-olderThan)
	var n int64
	for id, rec := range t.m.sessions {
		dead := false
		switch {
		case rec.RevokedAt != nil:
			dead = !rec.RevokedAt.After(horizon)
		default:
			dead = !rec.ExpiresAt.After(horizon)
		}
		if dead {
			delete(t.m.sessions, id)
			n++
		}
	}
	return n, nil
}
