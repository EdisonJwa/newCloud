package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// In-memory implementation of ShareRateTx.
//
// Mirrors the PostgreSQL semantics rather than approximating them: the conformance suite runs both
// backends against the same cases, and a limiter that behaves differently in tests than in
// production is the failure mode that suite exists to prevent.

func (t *memTx) AllowShareUpload(ctx context.Context, id uuid.UUID, rate int64, window time.Duration,
	ip string, bytes int64) error {

	if rate <= 0 {
		return nil
	}

	now := t.m.now()
	cutoff := now.Add(-window)

	// Count inside the window, then decide. The in-memory store serialises transactions, so this
	// pair is atomic here in a way it would not be in PostgreSQL — which is precisely why the
	// PostgreSQL version is a single statement and this one only has to be correct, not clever.
	n := 0
	for _, e := range t.m.shareUploadEvents {
		if e.ShareID == id && e.OccurredAt.After(cutoff) {
			n++
		}
	}
	if int64(n) >= rate {
		return ErrShareRateLimited
	}

	t.m.shareUploadEvents = append(t.m.shareUploadEvents, memShareUploadEvent{
		ShareID:    id,
		IP:         ip,
		Bytes:      bytes,
		OccurredAt: now,
	})
	return nil
}

// PruneShareUploadEvents deletes rows older than the longest window in use.
func (t *memTx) PruneShareUploadEvents(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := t.m.now().Add(-olderThan)
	kept := t.m.shareUploadEvents[:0]
	removed := 0
	// "<=", not "<", to match the SQL's `occurred_at <= now() - interval`.
	//
	// The difference is a real divergence, not a nit: PostgreSQL's now() advances between the
	// insert and the delete, so a zero keep window removes everything. The in-memory store's clock
	// is frozen for determinism, so an event created "now" and pruned "now" compares EQUAL — and a
	// strict "<" would keep it, leaving the table growing forever while PostgreSQL emptied it.
	for _, e := range t.m.shareUploadEvents {
		if !e.OccurredAt.After(cutoff) {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	t.m.shareUploadEvents = kept
	return removed, nil
}

// memShareUploadEvent is one recorded upload in the in-memory model.
type memShareUploadEvent struct {
	ShareID    uuid.UUID
	IP         string
	Bytes      int64
	OccurredAt time.Time
}
