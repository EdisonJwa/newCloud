package share_test

// Regression tests for the 2026-10-25 recipient fixes: an address whose first code
// was spent must be able to verify AGAIN with a freshly issued one (fix 8), and code
// issuance itself must have a per-(share, address) rate limit (fix 10).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// TestARecipientCanVerifyAgainAfterTheirAccessExpires is the lockout fix.
//
// VerifyRecipientCode used to hard-reject any address whose row was consumed, but
// nothing ever un-consumed it — so the SECOND code the page helpfully emailed (after
// the recipient's two-hour access grant expired) could never verify. Every later code
// answered ErrNotRecipient, permanently. The consumed record stays, as metadata and as
// the serialization point; it is no longer a lifetime lock.
func TestARecipientCanVerifyAgainAfterTheirAccessExpires(t *testing.T) {
	svc, sh, meta, _ := recipientFixture(t)
	ctx := context.Background()

	code1, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code1); err != nil {
		t.Fatalf("first verification: %v", err)
	}
	first, err := svc.ResolveRecipient(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if first.VerifiedAt == nil {
		t.Fatal("the address should be verified after the first code")
	}
	if first.ConsumedAt == nil {
		t.Fatal("the first verification should still be recorded as consumed")
	}

	// The access grant expires. At the service level nothing else changes — the
	// recipient is still invited, still unrevoked — which is exactly the state a
	// returning visitor is in.
	meta.AdvanceForTest(3 * time.Hour)

	code2, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatalf("re-issue after the grant expired: %v", err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code2); err != nil {
		t.Fatalf("a recipient must be able to verify again after their access expires, "+
			"got %v. The old behaviour rejected every later code forever once the first "+
			"one was consumed.", err)
	}

	// The re-verification is idempotent: the FIRST verified_at stands, rather than
	// being rewritten by the second proof.
	again, err := svc.ResolveRecipient(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !again.VerifiedAt.Equal(*first.VerifiedAt) {
		t.Fatalf("re-verification rewrote verified_at: %v -> %v",
			first.VerifiedAt, again.VerifiedAt)
	}
}

// TestRevokedRecipientStillCannotVerify: re-issuability must not become a way back in
// for an address the owner withdrew.
func TestRevokedRecipientStillCannotVerify(t *testing.T) {
	svc, sh, meta, tickets := recipientFixture(t)
	ctx := context.Background()

	revokedAt := time.Now()
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.AddRecipient(ctx, store.ShareRecipient{
			ShareID: sh.ID, Email: "revoked@example.test", RevokedAt: &revokedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.IssueRecipientCode(ctx, sh, "revoked@example.test"); !errors.Is(err, share.ErrNotRecipient) {
		t.Fatalf("a revoked address must not be issued a code, got %v", err)
	}

	// Even a perfectly valid, correctly bound code must not verify: the invitation,
	// not the code, is what grants access.
	code, err := tickets.Mint(ticket.PurposeEmailVerify, ticket.Claims{
		Subject: sh.ID.String(),
		ID:      "revoked@example.test",
		Expiry:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "revoked@example.test", code); !errors.Is(err, share.ErrNotRecipient) {
		t.Fatalf("a revoked recipient must not verify, got %v", err)
	}
}

// TestRecipientCodeIssuanceIsRateLimited is the mail-bomb fix: each code is an email
// the visitor triggers, so issuance is capped per (share, address) inside a rolling
// window — generous for a person re-checking their inbox, useless for a spam run.
func TestRecipientCodeIssuanceIsRateLimited(t *testing.T) {
	svc, sh, meta, _ := recipientFixture(t)
	ctx := context.Background()

	for i := 0; i < store.RecipientCodeMaxIssuance; i++ {
		if _, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test"); err != nil {
			t.Fatalf("issue %d: %v", i+1, err)
		}
	}
	if _, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test"); !errors.Is(err, share.ErrRecipientCodeRateLimited) {
		t.Fatalf("code %d must be refused with ErrRecipientCodeRateLimited, got %v",
			store.RecipientCodeMaxIssuance+1, err)
	}

	// The limit is per address: another invitee is untouched.
	if _, err := svc.IssueRecipientCode(ctx, sh, "bob@example.test"); err != nil {
		t.Fatalf("another address must keep its own allowance: %v", err)
	}

	// And the window rolls: after it passes, the address may be mailed again.
	meta.AdvanceForTest(store.RecipientCodeWindow + time.Minute)
	if _, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test"); err != nil {
		t.Fatalf("once the window has passed the address may be mailed again: %v", err)
	}
}

// TestIssuanceLimitAlsoBoundsTheDeniedAuditRow: the uninvited-address check inside
// IssueRecipientCode writes a denial audit row, which is itself something a requester
// can spam. The rate gate runs FIRST, so repeated probes of one address stop writing
// rows once the allowance is gone.
func TestIssuanceLimitAlsoBoundsTheDeniedAuditRow(t *testing.T) {
	svc, sh, meta, _ := recipientFixture(t)
	ctx := context.Background()

	denialsBefore := 0
	for _, e := range meta.AuditLog() {
		if e.Action == "share.recipient.denied" {
			denialsBefore++
		}
	}

	for i := 0; i < store.RecipientCodeMaxIssuance+3; i++ {
		_, err := svc.IssueRecipientCode(ctx, sh, "stranger@example.test")
		if err != nil && !errors.Is(err, share.ErrNotRecipient) {
			t.Fatalf("probe %d: %v", i+1, err)
		}
	}

	denialsAfter := 0
	for _, e := range meta.AuditLog() {
		if e.Action == "share.recipient.denied" {
			denialsAfter++
		}
	}
	if denialsAfter-denialsBefore > store.RecipientCodeMaxIssuance {
		t.Fatalf("%d denial rows written for one address in one window; the issuance "+
			"limit must gate the audit write too", denialsAfter-denialsBefore)
	}
}
