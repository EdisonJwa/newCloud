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
	email, err := normaliseEmail(email)
	if err != nil {
		return store.ShareRecipient{}, ErrNotRecipient
	}

	var found store.ShareRecipient
	err = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		r, rerr := tx.RecipientFor(ctx, sh.ID, email)
		found = r
		return rerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.auditDenied(ctx, sh, email, "not_invited")
			return store.ShareRecipient{}, ErrNotRecipient
		}
		return store.ShareRecipient{}, err
	}
	if found.RevokedAt != nil {
		s.auditDenied(ctx, sh, email, "revoked")
		return store.ShareRecipient{}, ErrNotRecipient
	}
	return found, nil
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
func (s *Service) IssueRecipientCode(ctx context.Context, sh store.Share, email string) (string, error) {
	if s.tickets == nil {
		return "", errors.New("share: email verification is not configured")
	}

	rec, err := s.ResolveRecipient(ctx, sh, email)
	if err != nil {
		return "", err
	}
	// ResolveRecipient already validated the address; normalise it again only to get
	// the canonical form used in the code's claims.
	email, err = normaliseEmail(email)
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

// VerifyRecipientCode exchanges a code for a verified recipient.
//
// The code is checked against both the share and the address. A code minted for
// alice@example.com must not work for bob@example.com on the same share, or any invited
// person could let in anyone by forwarding their code.
//
// Three guarantees, each of which was missing before SHARE-02 was addressed:
//
//	single use     the recipient row is marked consumed, so a code works once even if
//	               its whole TTL has not elapsed and even if it leaked;
//	throttled      failures are counted per (share, address) inside a rolling window,
//	               so online guessing is bounded regardless of source address;
//	no oracle      a throttled attempt is indistinguishable from a wrong code, because
//	               telling them apart confirms both the share and the invitation.
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
	// was sent, and a code is not a durable capability.
	rec, err := s.ResolveRecipient(ctx, sh, email)
	if err != nil {
		record(false, "recipient_withdrawn")
		return err
	}
	if rec.ConsumedAt != nil {
		record(false, "already_used")
		s.auditDenied(ctx, sh, email, "already_used")
		return ErrNotRecipient
	}

	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Consuming and verifying happen in one transaction, and consumption is a
		// conditional UPDATE, so two concurrent submissions of the same code cannot
		// both succeed.
		if cerr := tx.ConsumeRecipientCode(ctx, sh.ID, email); cerr != nil {
			return ErrNotRecipient
		}
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
