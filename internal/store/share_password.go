package store

// SHARE-02: password verification throttle.
//
// The requirement asks for a rate limit, a validity period, an attempt cap and replay protection on
// both password and OTP verification. OTP had all four; password verification had none. A wrong
// password was audited — so an owner could see a burst afterwards — but nothing slowed it down.
//
// A share password is usually short and human-chosen, which makes online guessing the realistic
// attack rather than a theoretical one. The recording is deliberately the same shape as the OTP
// one, so the two throttles cannot drift into different policies.
//
// The limit key is the SHARE, not the address. A share password is not bound to any email — the
// same link is opened by whoever holds it — so an address-keyed counter would start from zero for
// every typed address and never fill.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// PasswordWindow is how far back failed guesses count.
//
// Longer than the OTP window because the guessing surface is the same link rather than one
// address, and a legitimate visitor mistyping a password should not be locked out for long.
const PasswordWindow = 15 * time.Minute

// PasswordMaxAttempts is how many failures inside PasswordWindow lock the link.
//
// Lower than the OTP allowance: a password is checked against a hash the server already holds, so
// there is no delivery step to slow an attacker down, and 5 guesses against a human-chosen password
// over a 15-minute window is already generous.
const PasswordMaxAttempts = 5

// ErrSharePasswordThrottled is returned when a share's password guessing limit is in force.
//
// Distinct from ErrSharePassword on purpose: the caller maps this to 429 with Retry-After rather
// than to the same opaque refusal a wrong password gets. The distinction is safe here for the same
// reason the upload rate limit's is — the caller already proved they hold a working link by
// triggering it, so nothing about the share is being revealed.
var ErrSharePasswordThrottled = errors.New("store: share password attempts throttled")

// SharePasswordTx is the password-throttle surface.
type SharePasswordTx interface {
	// PasswordFailureCount reports recent failed attempts for this share.
	PasswordFailureCount(ctx context.Context, shareID uuid.UUID, window time.Duration) (int, error)
	// RecordPasswordAttempt appends one try.
	//
	// Recording BEFORE the hash comparison is what makes the cap hold under parallel guessing: a
	// caller that recorded only failures would leave a window in which concurrent guesses all
	// observed "under the limit". The share service records optimistically as a failure — inside
	// the same transaction that reads the count — and records a second, success row when the
	// comparison succeeds. The optimistic failure row keeps counting toward the cap; that is the
	// accepted price of a cap that a parallel burst cannot talk its way around.
	RecordPasswordAttempt(ctx context.Context, shareID uuid.UUID, ip string, succeeded bool) error
	// PrunePasswordAttempts deletes attempts older than the given age, bounding the table.
	PrunePasswordAttempts(ctx context.Context, olderThan time.Duration) (int, error)
}
