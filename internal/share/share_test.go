package share

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

type fixture struct {
	svc   *Service
	meta  *store.Memory
	owner store.User
	node  store.Node
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	root, err := meta.EnsureRoot(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	var node store.Node
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner.ID, ParentID: root.ID, Name: "shared"})
		node = n
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Cheap Argon parameters: these tests exercise behaviour, not KDF strength,
	// and 64 MiB per hash would make the suite take minutes.
	return &fixture{svc: New(meta, TestArgon()), meta: meta, owner: owner, node: node}
}

func (f *fixture) create(t *testing.T, in NewShare) Created {
	t.Helper()
	if in.NodeID == uuid.Nil {
		in.NodeID = f.node.ID
	}
	if in.OwnerID == uuid.Nil {
		in.OwnerID = f.owner.ID
	}
	c, err := f.svc.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	return c
}

func TestCreateAndResolve(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink})

	if c.Token == "" {
		t.Fatal("a share must have a token")
	}
	if len(c.Share.TokenHash) != 32 {
		t.Fatalf("only the SHA-256 may be stored, got %d bytes", len(c.Share.TokenHash))
	}
	if strings.Contains(string(c.Share.TokenHash), c.Token) {
		t.Fatal("the token must not appear in the stored hash")
	}

	got, err := f.svc.Resolve(context.Background(), c.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != c.Share.ID {
		t.Fatal("resolved the wrong share")
	}
}

// TestTokenIsNotRecoverableFromStorage: a database dump must not yield working
// links.
func TestTokenIsNotRecoverableFromStorage(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink})

	for _, s := range f.meta.AllShares() {
		if strings.Contains(string(s.TokenHash), c.Token) {
			t.Fatal("plaintext token found in storage")
		}
	}
}

func TestUnknownTokenIsNotFound(t *testing.T) {
	f := newFixture(t)
	for _, tok := range []string{"", "bogus", strings.Repeat("A", 22)} {
		if _, err := f.svc.Resolve(context.Background(), tok); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%q: want ErrNotFound, got %v", tok, err)
		}
	}
}

func TestPasswordProtection(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink, Password: "s3cret-pass"})

	sh, err := f.svc.Resolve(context.Background(), c.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !sh.NeedsPassword() {
		t.Fatal("share should require a password")
	}

	if err := f.svc.VerifyPassword(context.Background(), sh, "wrong"); !errors.Is(err, store.ErrSharePassword) {
		t.Fatalf("wrong password: want ErrSharePassword, got %v", err)
	}
	if err := f.svc.VerifyPassword(context.Background(), sh, "s3cret-pass"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
}

// TestPasswordFailureIsAudited: a burst of failures on one share is the only
// signal an owner gets that someone is guessing.
func TestPasswordFailureIsAudited(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink, Password: "pw"})
	sh, _ := f.svc.Resolve(context.Background(), c.Token)

	before := len(f.meta.AuditLog())
	_ = f.svc.VerifyPassword(context.Background(), sh, "nope")
	after := len(f.meta.AuditLog())

	if after <= before {
		t.Fatal("a failed password check must be audited")
	}
}

func TestPasswordIsSaltedPerShare(t *testing.T) {
	f := newFixture(t)
	a := f.create(t, NewShare{Kind: store.ShareLink, Password: "same"})
	b := f.create(t, NewShare{Kind: store.ShareLink, Password: "same"})

	if string(a.Share.PasswordHash) == string(b.Share.PasswordHash) {
		t.Fatal("identical passwords must not produce identical hashes (salt missing)")
	}
}

func TestExpiredShareIsNotResolved(t *testing.T) {
	f := newFixture(t)
	past := time.Now().Add(-time.Hour)
	c, err := f.svc.Create(context.Background(), NewShare{
		NodeID: f.node.ID, OwnerID: f.owner.ID,
		Kind: store.ShareLink, ExpiresAt: &past,
	})
	// Creating a share that is already expired must be refused outright.
	if err == nil {
		_, rerr := f.svc.Resolve(context.Background(), c.Token)
		if !errors.Is(rerr, store.ErrShareExpired) {
			t.Fatalf("want ErrShareExpired, got %v", rerr)
		}
	}
}

func TestExpiryEnforced(t *testing.T) {
	f := newFixture(t)
	exp := time.Now().Add(50 * time.Millisecond)
	c := f.create(t, NewShare{Kind: store.ShareLink, ExpiresAt: &exp})

	if _, err := f.svc.Resolve(context.Background(), c.Token); err != nil {
		t.Fatalf("fresh share should resolve: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := f.svc.Resolve(context.Background(), c.Token); !errors.Is(err, store.ErrShareExpired) {
		t.Fatalf("want ErrShareExpired, got %v", err)
	}
}

func TestRevokedShareIsNotResolved(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink})

	if err := f.svc.Revoke(context.Background(), c.Share.ID, f.owner.ID, f.owner.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Resolve(context.Background(), c.Token); !errors.Is(err, store.ErrShareRevoked) {
		t.Fatalf("want ErrShareRevoked, got %v", err)
	}
}

// TestDownloadLimitIsExact is the headline feature: "this link may be used three
// times" must mean exactly three.
func TestDownloadLimitIsExact(t *testing.T) {
	f := newFixture(t)
	limit := 3
	c := f.create(t, NewShare{Kind: store.ShareLink, MaxDownloads: &limit})
	sh, _ := f.svc.Resolve(context.Background(), c.Token)

	for i := 0; i < limit; i++ {
		if err := f.svc.RecordDownload(context.Background(), sh, DownloadMeta{ActorKind: "anonymous"}); err != nil {
			t.Fatalf("download %d should succeed: %v", i+1, err)
		}
	}
	if err := f.svc.RecordDownload(context.Background(), sh, DownloadMeta{ActorKind: "anonymous"}); !errors.Is(err, store.ErrShareExhausted) {
		t.Fatalf("download %d should be refused, got %v", limit+1, err)
	}
}

// TestConcurrentDownloadsRespectLimit is the race the guarded increment exists
// for: read-then-write would let every goroutine see "one copy left".
func TestConcurrentDownloadsRespectLimit(t *testing.T) {
	f := newFixture(t)
	limit := 1
	c := f.create(t, NewShare{Kind: store.ShareLink, MaxDownloads: &limit})
	sh, _ := f.svc.Resolve(context.Background(), c.Token)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.svc.RecordDownload(context.Background(), sh, DownloadMeta{ActorKind: "anonymous"})
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != limit {
		t.Fatalf("exactly %d downloads must be allowed, got %d", limit, ok)
	}
}

func TestUploadLimitForRequestShare(t *testing.T) {
	f := newFixture(t)
	limit := 2
	c := f.create(t, NewShare{Kind: store.ShareRequest, MaxUploads: &limit})
	sh, _ := f.svc.Resolve(context.Background(), c.Token)

	for i := 0; i < limit; i++ {
		if err := f.svc.RecordUpload(context.Background(), sh, DownloadMeta{ActorKind: "anonymous"}); err != nil {
			t.Fatalf("upload %d: %v", i+1, err)
		}
	}
	if err := f.svc.RecordUpload(context.Background(), sh, DownloadMeta{ActorKind: "anonymous"}); !errors.Is(err, store.ErrShareExhausted) {
		t.Fatalf("upload over the cap must be refused, got %v", err)
	}
}

// TestRequestShareIsUploadOnly is the security property of a drop box: a caller
// cannot create one that also publishes the directory contents.
func TestRequestShareIsUploadOnly(t *testing.T) {
	f := newFixture(t)

	// Ask for read access on a request share; it must be dropped.
	c := f.create(t, NewShare{
		Kind:        store.ShareRequest,
		Permissions: []string{"read", "create"},
	})

	if c.Share.CanRead() {
		t.Fatal("a request share must never grant read")
	}
	if !c.Share.CanCreate() {
		t.Fatal("a request share must grant create")
	}
	if !c.Share.HideContents {
		t.Fatal("a request share must hide its contents")
	}
	if strings.Join(c.Share.Permissions, ",") != "create" {
		t.Fatalf("permissions: %v", c.Share.Permissions)
	}
}

func TestDefaultPermissionsAreReadForLinkShares(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink})
	if !c.Share.CanRead() {
		t.Fatal("a link share defaults to read")
	}
	if c.Share.CanCreate() {
		t.Fatal("a link share must not allow uploads by default")
	}
	if c.Share.HideContents {
		t.Fatal("a link share should show its directory")
	}
}

func TestEmailShareRecipients(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{
		Kind:       store.ShareEmail,
		Recipients: []string{"Dana@Example.TEST", "evan@example.test"},
	})

	var recipients []store.ShareRecipient
	_ = f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		r, err := tx.RecipientsFor(ctx, c.Share.ID)
		recipients = r
		return err
	})
	if len(recipients) != 2 {
		t.Fatalf("want 2 recipients, got %d", len(recipients))
	}
	// Addresses are normalised so the same person cannot be invited twice under
	// different casing.
	for _, r := range recipients {
		if r.Email != strings.ToLower(r.Email) {
			t.Fatalf("recipient not normalised: %q", r.Email)
		}
	}
}

func TestInvalidRecipientRejected(t *testing.T) {
	bad := []string{"", "   ", "not-an-email", "@example.com", "user@"}
	for _, addr := range bad {
		if err := ValidateRecipient(addr); err == nil {
			t.Fatalf("%q should be rejected", addr)
		}
	}
	for _, addr := range []string{"a@example.test", "First Last <a@example.test>"} {
		if err := ValidateRecipient(addr); err != nil {
			t.Fatalf("%q should be accepted: %v", addr, err)
		}
	}
}

func TestDuplicateRecipientIsIdempotent(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{
		Kind:       store.ShareEmail,
		Recipients: []string{"a@example.test", "A@EXAMPLE.TEST"},
	})

	var recipients []store.ShareRecipient
	_ = f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		r, err := tx.RecipientsFor(ctx, c.Share.ID)
		recipients = r
		return err
	})
	if len(recipients) != 1 {
		t.Fatalf("case-different duplicates must collapse to one, got %d", len(recipients))
	}
}

// TestAuditRecordsWhoDownloaded is the whole point of proxying downloads.
func TestAuditRecordsWhoDownloaded(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink})
	sh, _ := f.svc.Resolve(context.Background(), c.Token)

	err := f.svc.RecordDownload(context.Background(), sh, DownloadMeta{
		ActorKind: "anonymous", ActorIP: "203.0.113.5", UserAgent: "curl/8",
		Name: "report.pdf", Bytes: 4096, Recipient: "dana@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, e := range f.meta.AuditLog() {
		if e.Action != "share.download" {
			continue
		}
		found = true
		if e.ActorIP != "203.0.113.5" {
			t.Fatalf("IP not recorded: %q", e.ActorIP)
		}
		if e.Bytes == nil || *e.Bytes != 4096 {
			t.Fatal("byte count not recorded")
		}
		if e.Metadata["name"] != "report.pdf" {
			t.Fatalf("filename not recorded: %v", e.Metadata)
		}
		if e.Metadata["recipient"] != "dana@example.test" {
			t.Fatalf("recipient not recorded: %v", e.Metadata)
		}
	}
	if !found {
		t.Fatal("no share.download audit entry was written")
	}
}

func TestCreateIsAudited(t *testing.T) {
	f := newFixture(t)
	f.create(t, NewShare{Kind: store.ShareLink, Password: "x"})

	var found bool
	for _, e := range f.meta.AuditLog() {
		if e.Action == "share.create" {
			found = true
			// The password itself must never be in the audit trail.
			for _, v := range e.Metadata {
				if s, ok := v.(string); ok && s == "x" {
					t.Fatal("password leaked into audit metadata")
				}
			}
		}
	}
	if !found {
		t.Fatal("share creation must be audited")
	}
}

func TestTokenUniqueness(t *testing.T) {
	f := newFixture(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		c := f.create(t, NewShare{Kind: store.ShareLink})
		if seen[c.Token] {
			t.Fatal("token collision")
		}
		seen[c.Token] = true
	}
}

func TestTokenLengthIsAdequate(t *testing.T) {
	f := newFixture(t)
	c := f.create(t, NewShare{Kind: store.ShareLink})
	// 16 random bytes encode to 22 base64url characters.
	if len(c.Token) < 22 {
		t.Fatalf("token too short to resist guessing: %d chars", len(c.Token))
	}
}
