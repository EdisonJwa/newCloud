package store

// SHARE-05 速率: the upload rate limit.
//
// The other four limits in SHARE-05 are totals — file count, per-file size, total space, retention
// — and a column plus a guard in the same UPDATE keeps them exact. A rate is different in kind: it
// is a rolling window, and no column can represent "how many arrived in the last N minutes"
// because the answer changes as time passes, without any write happening. The honest representation
// is the events themselves, so this is a table.
//
// Why not an in-process token bucket, which is the obvious implementation: the worker is already
// written to tolerate a second replica, so a deployment may well have several. An in-memory limiter
// would give each replica its own allowance, making the effective limit N × replicas and changing
// it whenever a pod rescheduled. It would also reset on restart, which turns "restart the process"
// into a way to clear the budget. The rows are durable for the same reason the audit log is.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrShareRateLimited is returned when a share's upload rate has been reached.
//
// Distinct from ErrShareExhausted on purpose: exhaust means the allowance is gone for good, rate
// means wait. They lead a visitor to different next steps and an operator to different causes, so
// collapsing them would make both less useful.
var ErrShareRateLimited = errors.New("store: share upload rate exceeded")

// Recipient-code issuance limit (SHARE-02, mail-bomb half).
//
// Every verification code is an outbound email a visitor triggers, so issuance needs the same
// rolling-window treatment uploads get: the honest representation is the events themselves, and a
// counter a concurrent request could double-spend is not a limit. The events REUSE the
// share_otp_attempts table rather than adding one: it is already keyed (share_id, email), already
// indexed on (share_id, email, attempted_at), already pruned, and its rows already mean "something
// happened to this address's codes". Issuance rows carry succeeded = true and reason = 'issued', so
// OTPFailureCount — which counts NOT succeeded — is unaffected by them.
const (
	// RecipientCodeWindow is how far back issued codes count.
	RecipientCodeWindow = 15 * time.Minute
	// RecipientCodeMaxIssuance is how many codes one address may be sent inside
	// RecipientCodeWindow. Generous for a human (five codes in a quarter hour is
	// already a struggling mail client), far below what a spam run needs.
	RecipientCodeMaxIssuance = 5
)

// ErrRecipientCodeRateLimited is returned when an address has been sent its allowance
// of verification codes inside the window.
var ErrRecipientCodeRateLimited = errors.New("store: recipient code issuance rate exceeded")

// RecipientCodeRateTx is the issuance rate-limit surface.
//
// It is a separate interface rather than part of ShareOTPTx because the two answer
// different questions (how many codes were SENT vs how many guesses were REFUSED),
// and a store that can answer one is not required to answer the other; callers that
// care type-assert, and a store without the surface simply does not limit issuance.
type RecipientCodeRateTx interface {
	// AllowRecipientCodeIssue counts the codes issued for this (share, address) inside
	// the window and, when there is room, records this one — a single atomic decision,
	// for the same reason AllowShareUpload is: a separate count followed by a separate
	// insert lets two concurrent requests both observe "one under the limit".
	// It returns ErrRecipientCodeRateLimited when the window is full, without recording.
	AllowRecipientCodeIssue(ctx context.Context, shareID uuid.UUID, email string,
		limit int64, window time.Duration) error
}

// ShareRateTx is the rate-limit surface.
type ShareRateTx interface {
	// AllowShareUpload counts recent uploads and, when there is room, records this one.
	//
	// It must be a single atomic decision. A separate count followed by a separate insert lets two
	// concurrent uploads both observe "one under the limit", which is how a limit of 10 accepts 20
	// — the same failure the download cap had before it was made atomic.
	// ShareUploadCountInWindow reports how many uploads this share has accepted inside the window.
	//
	// A READ, separate from AllowShareUpload's guarded insert, because the two answer different
	// needs. The guarded insert is the authority — it refuses a burst under concurrency. This one
	// lets a caller refuse EARLY, before writing the bytes, which is the difference between a
	// rate-limited visitor costing a refused request and costing a refused request plus its
	// content in storage.
	ShareUploadCountInWindow(ctx context.Context, id uuid.UUID, window time.Duration) (int64, error)

	AllowShareUpload(ctx context.Context, id uuid.UUID, rate int64, window time.Duration,
		ip string, bytes int64) error

	// PruneShareUploadEvents deletes rate-limit rows older than the longest window in use,
	// bounding the table.
	PruneShareUploadEvents(ctx context.Context, olderThan time.Duration) (int, error)
}

// ShareUploadEvent is one accepted upload, as recorded for rate limiting.
type ShareUploadEvent struct {
	ShareID uuid.UUID
	IP      string
	Bytes   int64
}

// AllowRecipientCodeIssue is the in-memory implementation of the issuance limit.
//
// It lives in this file rather than share_rate_memory.go because the surface and both
// of its backends were introduced together and are reviewed together: the counting
// predicate here must stay byte-for-byte the same idea as the SQL predicate below, or
// the two backends disagree about when the window is full — the exact class of
// divergence the shared suites exist to catch.
func (t *memTx) AllowRecipientCodeIssue(ctx context.Context, shareID uuid.UUID, email string,
	limit int64, window time.Duration) error {

	if limit <= 0 {
		return nil
	}

	now := t.m.now()
	cutoff := now.Add(-window)

	// Transactions are serialised in this backend, so count-then-record is atomic
	// here in a way it would not be in PostgreSQL — which is why the SQL version is
	// a single statement.
	n := int64(0)
	for _, a := range t.m.otpAttempts {
		if a.ShareID == shareID && strings.EqualFold(a.Email, email) &&
			a.Reason == recipientCodeIssuedReason && a.At.After(cutoff) {
			n++
		}
	}
	if n >= limit {
		return ErrRecipientCodeRateLimited
	}

	t.m.otpAttempts = append(t.m.otpAttempts, OTPAttempt{
		ShareID: shareID, Email: strings.ToLower(email),
		Success: true, Reason: recipientCodeIssuedReason, At: now,
	})
	return nil
}

// AllowRecipientCodeIssue is the PostgreSQL implementation of the issuance limit.
//
// One statement, mirroring AllowShareUpload's guarded insert: the INSERT can only
// produce a row when the window has room, and a transaction-scoped advisory lock on
// the (share, address) pair serialises just the decision, so two concurrent requests
// cannot both count "one under the limit" and both record.
func (t *pgTx) AllowRecipientCodeIssue(ctx context.Context, shareID uuid.UUID, email string,
	limit int64, window time.Duration) error {

	if limit <= 0 {
		return nil
	}

	email = strings.ToLower(email)
	if _, err := t.tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1::text))`,
		shareID.String()+":"+email); err != nil {
		return translate(err)
	}

	const q = `INSERT INTO share_otp_attempts (share_id, email, succeeded, reason)
		SELECT $1, $2, true, $3
		WHERE (SELECT count(*) FROM share_otp_attempts
		        WHERE share_id = $1 AND email = $2 AND succeeded AND reason = $3
		          AND attempted_at > now() - $4::interval) < $5`
	tag, err := t.tx.Exec(ctx, q, shareID, email, recipientCodeIssuedReason,
		intervalOf(window), limit)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRecipientCodeRateLimited
	}
	return nil
}

// recipientCodeIssuedReason marks an issuance event in share_otp_attempts.
//
// The rows share the table with verification attempts; both backends must filter on
// this exact string.
const recipientCodeIssuedReason = "issued"

var _ RecipientCodeRateTx = (*memTx)(nil)
var _ RecipientCodeRateTx = (*pgTx)(nil)
