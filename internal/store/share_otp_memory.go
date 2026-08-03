package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// In-memory implementation of ShareOTPTx.
//
// It applies the same window and the same single-use rule as PostgreSQL. A throttle
// that only exists in production is worse than none: the tests would prove the
// happy path works while an attacker had an unthrottled oracle.

func (t *memTx) OTPFailureCount(ctx context.Context, shareID uuid.UUID, email string,
	window time.Duration) (int, error) {

	cutoff := t.m.now().Add(-window)
	n := 0
	for _, a := range t.m.otpAttempts {
		if a.ShareID == shareID && strings.EqualFold(a.Email, email) &&
			!a.Success && a.At.After(cutoff) {
			n++
		}
	}
	return n, nil
}

func (t *memTx) RecordOTPAttempt(ctx context.Context, a OTPAttempt) error {
	if a.At.IsZero() {
		a.At = t.m.now()
	}
	t.m.otpAttempts = append(t.m.otpAttempts, a)
	return nil
}

func (t *memTx) ConsumeRecipientCode(ctx context.Context, shareID uuid.UUID, email string) error {
	for _, r := range t.m.recipients {
		if r.ShareID != shareID || !strings.EqualFold(r.Email, email) {
			continue
		}
		if r.RevokedAt != nil || r.ConsumedAt != nil {
			return ErrCodeConsumed
		}
		at := t.m.now()
		r.ConsumedAt = &at
		return nil
	}
	return ErrNotFound
}

func (t *memTx) PruneOTPAttempts(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := t.m.now().Add(-olderThan)
	kept := t.m.otpAttempts[:0]
	removed := 0
	for _, a := range t.m.otpAttempts {
		if a.At.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, a)
	}
	t.m.otpAttempts = kept
	return removed, nil
}

var _ ShareOTPTx = (*memTx)(nil)
