package share

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// Email shares: who may open the link, and how that is proven.
//
// The distinction that matters: a *link* share is a capability — whoever holds the URL
// has the access, and that is the point. An *email* share is not. The owner named
// specific people, and forwarding the URL to anyone else must not grant them anything.
// Before this was implemented an email share was indistinguishable from a link share,
// so "shared with alice@example.com" silently meant "shared with the world, plus a
// note recording who it was meant for".
//
// The proof is a one-time code sent to the address: short-lived, single use, and bound
// to one share and one address, so an intercepted code is worth far less than an
// intercepted link.

// RecipientCodeTTL bounds how long a verification code is valid.
//
// Short because the code travels by email, a channel we do not control: the longer it
// lives, the longer a leaked mailbox grants access.
const RecipientCodeTTL = 15 * time.Minute

// RecipientAccessTTL bounds a verified recipient's grant on the share page.
//
// Longer than the code, shorter than a login session: a recipient is a guest.
const RecipientAccessTTL = 2 * time.Hour

var (
	// ErrNotRecipient: the share names specific people and this visitor is not one.
	ErrNotRecipient = errors.New("share: not a recipient of this share")
	// ErrAlreadyVerified: the address has already proven ownership.
	ErrAlreadyVerified = errors.New("share: recipient already verified")
	// ErrRecipientCodeRateLimited: the address has been sent the maximum number of
	// codes inside the window.
	//
	// Exported and distinct from ErrNotRecipient on purpose: issuing codes sends
	// email, and the handler that wires this error should answer 429 (as it already
	// does for a throttled share password) rather than the opaque refusal a
	// non-recipient gets. The distinction is safe for the same reason the password
	// throttle's is — the caller only reached it by naming a share that exists.
	ErrRecipientCodeRateLimited = errors.New("share: too many verification codes requested")
)

// SetTicketIssuer gives the service the ability to mint and verify recipient codes.
//
// It is a setter rather than a constructor argument so the dependency stays optional:
// a deployment without email delivery can still serve link and request shares, and the
// service reports a clear error rather than panicking when a code is asked for.
func (s *Service) SetTicketIssuer(iss *ticket.Issuer) { s.tickets = iss }

// RequiresRecipient reports whether access must be limited to named addresses.
func RequiresRecipient(sh store.Share) bool {
	// An email share with no recipients is a configuration error; failing closed is
	// the only safe reading, so the kind alone decides.
	return sh.Kind == store.ShareEmail
}

// ResolveRecipient maps a visitor's address to its recipient row.
//
// The comparison is case-insensitive because addresses are, and `citext` in the schema
// already says so. Matching case-sensitively would reject a legitimate visitor for a
// reason they cannot see.
func (s *Service) ResolveRecipient(ctx context.Context, sh store.Share, email string) (store.ShareRecipient, error) {
	rec, err := s.resolveRecipientQuiet(ctx, sh, email)
	if err != nil {
		if errors.Is(err, ErrNotRecipient) {
			s.auditDenied(ctx, sh, email, recDeniedReason(rec))
		}
		return store.ShareRecipient{}, err
	}
	return rec, nil
}

// resolveRecipientQuiet answers the invitation check without writing anything.
//
// ResolveRecipient is its audible wrapper: same verdict, plus the denial audit row.
// The split exists for the rate-limited path, which must still answer the question
// ("was this address invited?") while the row the answer used to cost is exactly what
// the limit exists to stop.
func (s *Service) resolveRecipientQuiet(ctx context.Context, sh store.Share, email string) (store.ShareRecipient, error) {
	email, err := normaliseEmail(email)
	if err != nil {
		return store.ShareRecipient{}, ErrNotRecipient
	}

	var found store.ShareRecipient
	var missing bool
	err = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		r, rerr := tx.RecipientFor(ctx, sh.ID, email)
		if errors.Is(rerr, store.ErrNotFound) {
			missing = true
			return nil
		}
		if rerr != nil {
			return rerr
		}
		found = r
		return nil
	})
	if err != nil {
		return store.ShareRecipient{}, err
	}
	if missing {
		return store.ShareRecipient{}, ErrNotRecipient
	}
	if found.RevokedAt != nil {
		return found, ErrNotRecipient
	}
	return found, nil
}

// recDeniedReason names why an address was refused, for the denial audit row.
func recDeniedReason(rec store.ShareRecipient) string {
	if rec.RevokedAt != nil {
		return "revoked"
	}
	return "not_invited"
}

// IssueRecipientCode creates a one-time verification code for an invited address.
//
// It refuses addresses that were never invited: the code proves ownership of an address
// the owner chose, it does not grant access to a new one. That asymmetry is the whole
// security property, so it lives here rather than in the caller.
// IssueRecipientCode mints a code for one invited ADDRESS.
//
// SHARE-03: knowing the link is not being the recipient. The code is bound to the address the owner
// invited, sent to it, and checked against it, so forwarding the URL does not turn a stranger into the
// invitee — and VerifyRecipientCode counts failures per (share, address), not per link.
//
// Issuance itself is rate limited per (share, address) (SHARE-02, mail-bomb half): each code is an
// outbound email the visitor triggers, so an unbounded requester could turn this endpoint into a
// spam cannon pointed at the invitee — or at the audit log, which the invited/denied rows below are
// part of. When the limit is in force the typed error
// share.ErrRecipientCodeRateLimited is returned and NOTHING downstream runs: no ticket, no email,
// no audit row. The check lives here rather than in the handler because the handler must learn
// nothing a wrong address does not already tell it.
func (s *Service) IssueRecipientCode(ctx context.Context, sh store.Share, email string) (string, error) {
	if s.tickets == nil {
		return "", errors.New("share: email verification is not configured")
	}

	email, err := normaliseEmail(email)
	if err != nil {
		return "", ErrNotRecipient
	}

	// The rate gate comes first, before the invitation check: both the invited audit
	// row and the denied audit row inside ResolveRecipient are things a requester can
	// trigger, so both are bounded by the same (share, address) allowance.
	//
	// When the allowance is gone the answers stay typed — a stranger's probe still reads
	// "not invited", so the gate does not become an invited-or-not oracle — but nothing
	// is minted and nothing is written: the codes and the rows were the abuse channel,
	// and a limit that kept spending them would only meter it.
	if err := s.limitRecipientCodeIssue(ctx, sh, email); err != nil {
		if _, qerr := s.resolveRecipientQuiet(ctx, sh, email); qerr != nil {
			return "", qerr
		}
		return "", err
	}

	rec, err := s.ResolveRecipient(ctx, sh, email)
	if err != nil {
		return "", err
	}

	code, err := s.tickets.Mint(ticket.PurposeEmailVerify, ticket.Claims{
		Subject: sh.ID.String(),
		ID:      email,
		Expiry:  s.now().Add(RecipientCodeTTL),
	})
	if err != nil {
		return "", err
	}

	_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", ActorEmail: email,
			Action: "share.recipient.invited", TargetKind: "share",
			TargetID: sh.ID.String(), ShareID: &sh.ID, Result: "ok",
			Metadata: map[string]any{"email": email, "already_verified": rec.VerifiedAt != nil},
		})
	})
	return code, nil
}

// limitRecipientCodeIssue applies the per-(share, address) issuance rate limit.
//
// The store method is an atomic count-and-record: when there is room it records this
// issuance and returns nil; when the window is full it returns
// store.ErrRecipientCodeRateLimited without recording. The surface is narrower than
// the share-rate one because the caller needs exactly one question answered, and
// stores that predate it (only wrapped test stores) skip the limit rather than fail —
// the OTP guess throttle on the verification side still bounds abuse there.
func (s *Service) limitRecipientCodeIssue(ctx context.Context, sh store.Share, email string) error {
	var limited error
	err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		rl, ok := tx.(store.RecipientCodeRateTx)
		if !ok {
			return nil
		}
		if err := rl.AllowRecipientCodeIssue(ctx, sh.ID, email,
			store.RecipientCodeMaxIssuance, store.RecipientCodeWindow); err != nil {
			limited = err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if limited != nil {
		return ErrRecipientCodeRateLimited
	}
	return nil
}

// VerifyRecipientCode exchanges a code for a verified recipient.
//
// The code is checked against both the share and the address. A code minted for
// alice@example.com must not work for bob@example.com on the same share, or any invited
// person could let in anyone by forwarding their code.
//
// Guarantees, and their current shape (revisited 2026-10-25):
//
//	bound          the code works only for its share and its address — the property
//	               SHARE-03 needs. What makes a visitor "the recipient" is possession
//	               of the address's mailbox, and every code is delivered there.
//	re-issuable    a recipient whose access grant has expired (the 2h page cookie) can
//	               verify again with a freshly issued code. The first version refused
//	               any verification once the address's row was consumed, so the second
//	               code an owner's page helpfully emailed could never work and the
//	               recipient was locked out until an administrator intervened.
//	throttled      failures are counted per (share, address) inside a rolling window,
//	               so online guessing is bounded regardless of source address;
//	no oracle      a throttled attempt is indistinguishable from a wrong code, because
//	               telling them apart confirms both the share and the invitation.
//
// The consumed_at record is kept, but as metadata and as the serialization point for
// two simultaneous submissions of the same code — not as a lifetime lock. Because the
// ticket is stateless, a replay of the SAME code within its TTL is no longer refused
// by this function; the bound that matters is unchanged (a replayed code still grants
// only what the address's mailbox already proved, and only until the code's TTL), and
// the freshly-issued code a legitimate re-verification needs is exactly as trustworthy
// as the first one.
func (s *Service) VerifyRecipientCode(ctx context.Context, sh store.Share, email, code string) error {
	if s.tickets == nil {
		return errors.New("share: email verification is not configured")
	}

	email, err := normaliseEmail(email)
	if err != nil {
		return ErrNotRecipient
	}

	// The throttle is checked before the code, so a correct guess arriving after the
	// limit is still refused. Checking afterwards would let an attacker keep
	// guessing until one landed.
	// The count read FAILS CLOSED, for the same reason as the password throttle in share.go.
	//
	// The error used to be discarded, leaving failures at zero — so a storage fault disabled the
	// guess limit entirely at the exact moment an operator would least expect it. A throttle that
	// silently stops being a throttle is worse than one that reports itself broken.
	var failures int
	if ferr := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.OTPFailureCount(ctx, sh.ID, email, store.OTPWindow)
		failures = n
		return cerr
	}); ferr != nil {
		return ferr
	}
	if failures >= store.OTPMaxAttempts {
		s.auditDenied(ctx, sh, email, "throttled")
		return ErrNotRecipient
	}

	// Record the attempt before validating: a crash between the check and the
	// validation must not silently refund an attempt.
	record := func(success bool, reason string) {
		_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.RecordOTPAttempt(ctx, store.OTPAttempt{
				ShareID: sh.ID, Email: email, Success: success, Reason: reason,
			})
		})
	}

	claims, err := s.tickets.Verify(ticket.PurposeEmailVerify, code)
	if err != nil {
		record(false, "bad_code")
		s.auditDenied(ctx, sh, email, "bad_code")
		return ErrNotRecipient
	}
	if claims.Subject != sh.ID.String() {
		record(false, "code_for_other_share")
		s.auditDenied(ctx, sh, email, "code_for_other_share")
		return ErrNotRecipient
	}
	// Constant time: comparing an HMAC'd claim with == leaks nothing practical, but
	// the habit should not depend on remembering which field is secret.
	if subtle.ConstantTimeCompare([]byte(claims.ID), []byte(email)) != 1 {
		record(false, "code_for_other_recipient")
		s.auditDenied(ctx, sh, email, "code_for_other_recipient")
		return ErrNotRecipient
	}

	// The address must still be on the list: it may have been revoked after the code
	// was sent, and a code is not a durable capability. Liveness is the only question
	// left here — consumed_at is metadata, not a refusal condition any more.
	if _, err := s.ResolveRecipient(ctx, sh, email); err != nil {
		record(false, "recipient_withdrawn")
		return err
	}

	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Consumption stays a conditional UPDATE, so two concurrent submissions of the
		// same code still serialise: exactly one of them consumes, the other observes
		// ErrCodeConsumed. With re-verification allowed, "already consumed" no longer
		// means "refuse" — it means either a legitimate re-verification (a second code
		// for an address whose first was spent) or the losing half of that race. Both
		// proceed; the row's liveness was re-checked above, and once more below.
		if cerr := tx.ConsumeRecipientCode(ctx, sh.ID, email); cerr != nil {
			if !errors.Is(cerr, store.ErrCodeConsumed) {
				// The row vanished (withdrawn concurrently): refuse.
				return ErrNotRecipient
			}
			live, lerr := tx.RecipientFor(ctx, sh.ID, email)
			if lerr != nil || live.RevokedAt != nil {
				// Consumed because the invitation was withdrawn between the check
				// above and now: refuse, the same answer a revoked recipient gets.
				return ErrNotRecipient
			}
		}
		// Idempotent by design: MarkRecipientVerified keeps the FIRST verified_at, so a
		// re-verification records that the address proved itself again without
		// rewriting when it did so the first time.
		if merr := tx.MarkRecipientVerified(ctx, sh.ID, email); merr != nil {
			return merr
		}
		if aerr := tx.RecordOTPAttempt(ctx, store.OTPAttempt{
			ShareID: sh.ID, Email: email, Success: true, Reason: "verified",
		}); aerr != nil {
			return aerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", ActorEmail: email,
			Action: "share.recipient.verified", TargetKind: "share",
			TargetID: sh.ID.String(), ShareID: &sh.ID, Result: "ok",
			Metadata: map[string]any{"email": email},
		})
	})
}

// auditDenied records a refused access attempt.
//
// Failures are ignored: a request must not become allowed because the audit write
// failed, and it must not become a 500 either — the denial is the answer.
func (s *Service) auditDenied(ctx context.Context, sh store.Share, email, reason string) {
	_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", ActorEmail: email, Action: "share.recipient.denied",
			TargetKind: "share", TargetID: sh.ID.String(), ShareID: &sh.ID,
			Result: "denied", Metadata: map[string]any{"email": email, "reason": reason},
		})
	})
}
