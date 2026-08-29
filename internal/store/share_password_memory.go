package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// In-memory implementation of SharePasswordTx.
//
// Mirrors the PostgreSQL semantics rather than approximating them: the conformance suite runs both
// backends against the same cases, and a throttle that behaves differently in tests than in
// production is the failure mode that suite exists to prevent.

func (t *memTx) PasswordFailureCount(ctx context.Context, shareID uuid.UUID,
	window time.Duration) (int, error) {

	cutoff := t.m.now().Add(-window)
	n := 0
	for _, a := range t.m.sharePasswordAttempts {
		if a.ShareID == shareID && !a.Succeeded && a.AttemptedAt.After(cutoff) {
			n++
		}
	}
	return n, nil
}

func (t *memTx) RecordPasswordAttempt(ctx context.Context, shareID uuid.UUID,
	ip string, succeeded bool) error {

	t.m.sharePasswordAttempts = append(t.m.sharePasswordAttempts, memPasswordAttempt{
		ShareID:     shareID,
		IP:          ip,
		Succeeded:   succeeded,
		AttemptedAt: t.m.now(),
	})
	return nil
}

func (t *memTx) PrunePasswordAttempts(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := t.m.now().Add(-olderThan)
	kept := t.m.sharePasswordAttempts[:0]
	removed := 0
	for _, a := range t.m.sharePasswordAttempts {
		// <= not <, matching the SQL's `attempted_at <= now() - interval`. With a zero window and
		// a frozen test clock the timestamps compare equal, and a strict comparison would keep
		// them while PostgreSQL emptied the table — a divergence the share-rate prune already
		// produced once.
		if !a.AttemptedAt.After(cutoff) {
			removed++
			continue
		}
		kept = append(kept, a)
	}
	t.m.sharePasswordAttempts = kept
	return removed, nil
}

// memPasswordAttempt is one recorded guess in the in-memory model.
type memPasswordAttempt struct {
	ShareID     uuid.UUID
	IP          string
	Succeeded   bool
	AttemptedAt time.Time
}
