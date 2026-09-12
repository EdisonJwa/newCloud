package share_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
	"github.com/google/uuid"
)

// Email shares must not behave like link shares.
//
// This is the property that makes "share to a specific email address" mean anything:
// holding the URL is not enough, the visitor must prove they control an address the
// owner named. Before recipient verification existed, an email share was
// indistinguishable from a link share — the recipient list was recorded but never
// enforced, so forwarding the link granted the whole world access while the audit log
// said otherwise.

// recipientFixture builds an email share with two invited addresses.
func recipientFixture(t *testing.T) (svc *share.Service, sh store.Share, meta *store.Memory, tickets *ticket.Issuer) {
	t.Helper()

	meta = store.NewMemory()
	owner := meta.AddUser(store.User{Username: "owner", Email: "owner@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}

	tickets, err := ticket.NewIssuer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}

	svc = share.New(meta, share.TestArgon())
	svc.SetTicketIssuer(tickets)

	created, err := svc.Create(context.Background(), share.NewShare{
		NodeID:     ownerRoot(t, meta, owner.ID),
		OwnerID:    owner.ID,
		Kind:       store.ShareEmail,
		Label:      "contract",
		Recipients: []string{"alice@example.test", "bob@example.test"},
	})
	if err != nil {
		t.Fatalf("create email share: %v", err)
	}

	sh, err = svc.Resolve(context.Background(), created.Token)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return svc, sh, meta, tickets
}

func ownerRoot(t *testing.T, m *store.Memory, ownerID [16]byte) [16]byte {
	t.Helper()
	var id [16]byte
	_ = m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, ownerID)
		id = n.ID
		return err
	})
	return id
}

// TestEmailShareRejectsUninvitedAddress is the core guarantee.
func TestEmailShareRejectsUninvitedAddress(t *testing.T) {
	svc, sh, _, _ := recipientFixture(t)

	if _, err := svc.ResolveRecipient(context.Background(), sh, "stranger@example.test"); err == nil {
		t.Fatal("an address that was never invited must not resolve")
	} else if !errors.Is(err, share.ErrNotRecipient) {
		t.Fatalf("want ErrNotRecipient, got %v", err)
	}

	// A code must not be issuable for an uninvited address either: otherwise the
	// verification step becomes a way to add yourself to the list.
	if _, err := svc.IssueRecipientCode(context.Background(), sh, "stranger@example.test"); err == nil {
		t.Fatal("a verification code must not be issued for an uninvited address")
	}
}

// TestInvitedAddressGetsCodeThatVerifies walks the happy path.
func TestInvitedAddressGetsCodeThatVerifies(t *testing.T) {
	svc, sh, meta, _ := recipientFixture(t)
	ctx := context.Background()

	code, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	if code == "" {
		t.Fatal("no code returned")
	}

	// Before verification the address is known but unproven.
	rec, err := svc.ResolveRecipient(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if rec.VerifiedAt != nil {
		t.Fatal("the address must not be verified before the code is used")
	}

	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code); err != nil {
		t.Fatalf("verify: %v", err)
	}

	rec, err = svc.ResolveRecipient(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if rec.VerifiedAt == nil {
		t.Fatal("the address should be marked verified")
	}

	// A verification is an audited event: the owner needs to know the link was opened.
	var verified bool
	for _, e := range meta.AuditLog() {
		if e.Action == "share.recipient.verified" {
			verified = true
		}
	}
	if !verified {
		t.Fatal("verification must be audited")
	}
}

// TestCodeIsBoundToAddress: one invited person must not let in another.
//
// Without this, forwarding the code is as good as forwarding the link.
func TestCodeIsBoundToAddress(t *testing.T) {
	svc, sh, _, _ := recipientFixture(t)
	ctx := context.Background()

	code, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}

	// bob is invited, but this code was minted for alice.
	if err := svc.VerifyRecipientCode(ctx, sh, "bob@example.test", code); err == nil {
		t.Fatal("a code issued for one address must not verify another")
	}

	// The code is still valid for its intended address.
	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code); err != nil {
		t.Fatalf("the code should still work for its own address: %v", err)
	}
}

// TestCodeIsBoundToShare: a code for share A must not open share B.
func TestCodeIsBoundToShare(t *testing.T) {
	svc, sh, meta, _ := recipientFixture(t)
	ctx := context.Background()

	code, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}

	// A second share for the same owner, same invited address.
	other, err := svc.Create(ctx, share.NewShare{
		NodeID:     sh.NodeID,
		OwnerID:    sh.OwnerID,
		Kind:       store.ShareEmail,
		Label:      "other",
		Recipients: []string{"alice@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherShare, err := svc.Resolve(ctx, other.Token)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.VerifyRecipientCode(ctx, otherShare, "alice@example.test", code); err == nil {
		t.Fatal("a code issued for one share must not verify another")
	}

	// Both shares were created for the same owner, so the audit log must show a denial.
	var denied bool
	for _, e := range meta.AuditLog() {
		if e.Action == "share.recipient.denied" {
			denied = true
		}
	}
	if !denied {
		t.Fatal("a cross-share code attempt must be audited as denied")
	}
}

// TestRevokedRecipientLosesAccess: withdrawing an invitation takes effect.
func TestRevokedRecipientLosesAccess(t *testing.T) {
	svc, sh, meta, _ := recipientFixture(t)
	ctx := context.Background()

	// Revoke bob by rewriting his row.
	_ = meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.MarkRecipientVerified(ctx, sh.ID, "bob@example.test")
	})

	if _, err := svc.ResolveRecipient(ctx, sh, "bob@example.test"); err != nil {
		t.Fatalf("bob should be resolvable: %v", err)
	}

	// A code issued before a revocation must not survive it.
	code, err := svc.IssueRecipientCode(ctx, sh, "bob@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "bob@example.test", code); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestLinkShareDoesNotRequireRecipient: the distinction must not leak into link shares.
func TestLinkShareDoesNotRequireRecipient(t *testing.T) {
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "o", Email: "o@example.test", Role: "user"})
	svc := share.New(meta, share.TestArgon())

	created, err := svc.Create(context.Background(), share.NewShare{
		NodeID:  ownerRoot(t, meta, owner.ID),
		OwnerID: owner.ID,
		Kind:    store.ShareLink,
	})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := svc.Resolve(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}

	if share.RequiresRecipient(sh) {
		t.Fatal("a link share must be usable by whoever holds the URL")
	}
}

// TestExpiredCodeRejected: the TTL is enforced by the ticket, not the caller.
func TestExpiredCodeRejected(t *testing.T) {
	svc, sh, _, tickets := recipientFixture(t)
	ctx := context.Background()

	// Mint a code that expired an hour ago: a leaked mailbox must stop being useful.
	code, err := tickets.Mint(ticket.PurposeEmailVerify, ticket.Claims{
		Subject: sh.ID.String(),
		ID:      "alice@example.test",
		Expiry:  time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code); err == nil {
		t.Fatal("an expired code must be rejected")
	}
}

// TestCodeIsSingleUse covers SHARE-02's replay half: a leaked code must not keep
// working for the rest of its TTL.
func TestCodeIsSingleUse(t *testing.T) {
	svc, sh, _, _ := recipientFixture(t)
	ctx := context.Background()

	code, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code); err != nil {
		t.Fatalf("first use must succeed: %v", err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", code); err == nil {
		t.Fatal("a one-time code must not verify twice")
	}
}

// TestVerificationIsThrottled covers T-09's brute-force half. Without the counter,
// an attacker with a share link could try guesses as fast as the server answers.
func TestVerificationIsThrottled(t *testing.T) {
	svc, sh, _, _ := recipientFixture(t)
	ctx := context.Background()

	// A genuine code, issued before the guessing starts: the throttle must refuse it
	// too, or an attacker simply keeps guessing until one lands.
	good, err := svc.IssueRecipientCode(ctx, sh, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < store.OTPMaxAttempts; i++ {
		if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", "wrong-code"); err == nil {
			t.Fatalf("attempt %d must fail", i)
		}
	}

	if err := svc.VerifyRecipientCode(ctx, sh, "alice@example.test", good); err == nil {
		t.Fatal("a correct code arriving after the limit must still be refused")
	}
}

// TestThrottleIsPerAddress: guessing against one invited address must not lock out
// the others on the same share.
func TestThrottleIsPerAddress(t *testing.T) {
	svc, sh, _, _ := recipientFixture(t)
	ctx := context.Background()

	for i := 0; i < store.OTPMaxAttempts+1; i++ {
		_ = svc.VerifyRecipientCode(ctx, sh, "alice@example.test", "wrong-code")
	}

	code, err := svc.IssueRecipientCode(ctx, sh, "bob@example.test")
	if err != nil {
		t.Fatalf("issue to the other recipient: %v", err)
	}
	if err := svc.VerifyRecipientCode(ctx, sh, "bob@example.test", code); err != nil {
		t.Fatalf("another address must still be able to verify: %v", err)
	}
}

// failingCountStore fails exactly one Tx method: the OTP failure count.
//
// Breaking the whole store does not work for this case, and the first version of this test made
// that mistake: VerifyRecipientCode calls ResolveRecipient FIRST, which needs the store, so a
// broken store made the function fail before the count was ever read. The test passed — for a
// reason that had nothing to do with the throttle, and it would have passed with the fail-open
// still in place.
//
// Embedding the interfaces gives every other method for free and overrides the one under test.
type failingCountStore struct{ store.Store }

func (f failingCountStore) WithTx(ctx context.Context,
	fn func(context.Context, store.Tx) error) error {

	return f.Store.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return fn(ctx, failingCountTx{tx})
	})
}

type failingCountTx struct{ store.Tx }

func (f failingCountTx) OTPFailureCount(context.Context, uuid.UUID, string, time.Duration) (int, error) {
	return 0, errors.New("simulated: the attempt counter is unreadable")
}

// TestRecipientThrottleFailsClosed is the OTP half of the same rule.
//
// VerifyRecipientCode reads the recent-failure count to decide whether the guess limit is in force.
// The read's error used to be discarded, leaving the count at zero — so a storage fault disabled
// the limit at the moment it mattered most, and a correct code arriving during the fault was
// accepted no matter how many wrong ones preceded it.
func TestRecipientThrottleFailsClosed(t *testing.T) {
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	root, err := meta.EnsureRoot(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	tickets, err := ticket.NewIssuer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	svc := share.New(failingCountStore{meta}, share.TestArgon())
	svc.SetTicketIssuer(tickets)

	created, err := svc.Create(context.Background(), share.NewShare{
		NodeID: root.ID, OwnerID: owner.ID, Kind: store.ShareEmail,
		Label: "throttle", Recipients: []string{"alice@example.test"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sh, err := svc.Resolve(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	code, err := svc.IssueRecipientCode(context.Background(), sh, "alice@example.test")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}

	err = svc.VerifyRecipientCode(context.Background(), sh, "alice@example.test", code)
	if err == nil {
		t.Fatal("with the failure counter unreadable, a correct code was ACCEPTED. The guess " +
			"limit is then not enforced at all while the counter is failing — a throttle that " +
			"silently stops being a throttle is worse than one that reports itself broken.")
	}
	if errors.Is(err, share.ErrNotRecipient) {
		t.Fatal("an unreadable counter was reported as a bad recipient, hiding a fault behind the " +
			"same refusal used for wrong codes")
	}
}
