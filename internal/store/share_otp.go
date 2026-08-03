package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Recipient verification hardening (SHARE-02, T-09).
//
// The three properties this covers, and why each one is not optional:
//
//   - A code is single-use. A one-time code that works until it expires is a
//     password with a short lifetime, and the whole point of emailing it is that
//     possessing it once is enough.
//   - Failures are counted per (share, address), not per IP. The attacker chooses
//     their address list and their source addresses; the pair the owner named is
//     the only thing they cannot vary.
//   - The count is enforced in the same statement that records the attempt, so
//     parallel guesses cannot each see "one under the limit".

// OTPWindow is how far back failed attempts count. Short enough that a legitimate
// visitor who mistyped can try again; long enough that online guessing is hopeless
// at any plausible rate.
const OTPWindow = 15 * time.Minute

// OTPMaxAttempts is the number of failures allowed inside OTPWindow before the
// address is locked out for the rest of the window.
const OTPMaxAttempts = 5

// ErrTooManyAttempts is returned when the throttle is in force. Callers map it to
// the same opaque failure as a wrong code: telling an attacker they are throttled
// confirms the share and address are real.
var ErrTooManyAttempts = errors.New("store: too many verification attempts")

// ErrCodeConsumed is returned when a code was already used successfully.
var ErrCodeConsumed = errors.New("store: verification code already used")

// OTPAttempt is one recorded verification try.
type OTPAttempt struct {
	ShareID uuid.UUID
	Email   string
	IP      string
	Success bool
	Reason  string
	At      time.Time
}

// ShareOTPTx is the verification-throttle surface.
type ShareOTPTx interface {
	// OTPFailureCount reports recent failures for this share and address.
	OTPFailureCount(ctx context.Context, shareID uuid.UUID, email string, window time.Duration) (int, error)
	// RecordOTPAttempt appends one try. Recording the failure *before* the code is
	// checked is what makes the limit hold under parallel guessing.
	RecordOTPAttempt(ctx context.Context, a OTPAttempt) error
	// ConsumeRecipientCode atomically marks the address's code used. It returns
	// ErrCodeConsumed when it was already consumed, so the caller cannot accept the
	// same code twice even with two concurrent requests.
	ConsumeRecipientCode(ctx context.Context, shareID uuid.UUID, email string) error
	// PruneOTPAttempts deletes attempts older than the window, bounding the table.
	PruneOTPAttempts(ctx context.Context, olderThan time.Duration) (int, error)
}
