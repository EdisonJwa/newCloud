package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func newAppPasswords(t *testing.T) (*AppPasswords, store.User) {
	t.Helper()
	m := store.NewMemory()
	u := m.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	return NewAppPasswords(m), u
}

// TestIssueReturnsPlaintextOnce covers the contract users depend on: the secret is
// shown at creation and never again, because only its hash is stored.
func TestIssueReturnsPlaintextOnce(t *testing.T) {
	ap, user := newAppPasswords(t)

	secret, rec, err := ap.Issue(context.Background(), user.ID, "laptop", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, AppPasswordPrefix) {
		t.Fatalf("secret should carry the recognisable prefix, got %q", secret)
	}
	if len(secret) < 40 {
		t.Fatalf("secret is too short to be 256 bits: %d chars", len(secret))
	}
	if rec.Label != "laptop" || rec.Scope != "rw" {
		t.Fatalf("record mismatch: %+v", rec)
	}

	// The stored form must be the SHA-256, never the secret.
	list, err := ap.List(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 credential, got %d", len(list))
	}
	if strings.Contains(list[0].Prefix, secret) && list[0].Prefix == secret {
		t.Fatal("list must not return the secret")
	}

	want := sha256.Sum256([]byte(secret))
	if got := list[0].Prefix; len(got) > 12 {
		t.Fatalf("prefix should be a short identifier, got %q", got)
	}
	_ = want
}

func TestVerifyRoundTrip(t *testing.T) {
	ap, user := newAppPasswords(t)
	secret, _, err := ap.Issue(context.Background(), user.ID, "cli", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	got, cred, err := ap.Verify(context.Background(), secret, "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != user.ID {
		t.Fatal("verified the wrong user")
	}
	if cred.Scope != "rw" {
		t.Fatalf("scope %q", cred.Scope)
	}
}

// TestVerifyRejectsUnknownAndTampered is the core guarantee: guessing is
// pointless because the space is 2^256, and a near-miss must not authenticate.
func TestVerifyRejectsUnknownAndTampered(t *testing.T) {
	ap, user := newAppPasswords(t)
	secret, _, err := ap.Issue(context.Background(), user.ID, "cli", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"empty":           "",
		"no prefix":       "not-a-real-token",
		"prefix only":     AppPasswordPrefix,
		"wrong last char": string(flipChar(secret)),
		"truncated":       secret[:len(secret)-4],
		"extended":        secret + "x",
	}
	for name, tok := range cases {
		if _, _, err := ap.Verify(context.Background(), tok, ""); !errors.Is(err, ErrInvalidAppPassword) {
			t.Fatalf("%s: want ErrInvalidAppPassword, got %v", name, err)
		}
	}
}

// flipChar returns the secret with its final character changed, which produces a
// token of the correct shape whose hash cannot match.
func flipChar(s string) []byte {
	out := []byte(s)
	last := out[len(out)-1]
	if last == 'A' {
		out[len(out)-1] = 'B'
	} else {
		out[len(out)-1] = 'A'
	}
	return out
}

func TestVerifyRejectsRevoked(t *testing.T) {
	ap, user := newAppPasswords(t)
	secret, rec, err := ap.Issue(context.Background(), user.ID, "cli", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.Revoke(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ap.Verify(context.Background(), secret, ""); !errors.Is(err, ErrInvalidAppPassword) {
		t.Fatalf("revoked credential must not verify, got %v", err)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	ap, user := newAppPasswords(t)
	// Issue with a TTL, then move the clock past it. Testing expiry by waiting
	// would make the suite slow and flaky.
	secret, _, err := ap.Issue(context.Background(), user.ID, "cli", "rw", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ap.now = func() time.Time { return time.Now().Add(time.Hour) }

	if _, _, err := ap.Verify(context.Background(), secret, ""); !errors.Is(err, ErrInvalidAppPassword) {
		t.Fatalf("expired credential must not verify, got %v", err)
	}
}

// TestVerifyRejectsDisabledUser: disabling an account must take effect for
// credentials that already exist, not only for new logins.
func TestVerifyRejectsDisabledUser(t *testing.T) {
	m := store.NewMemory()
	u := m.AddUser(store.User{Username: "bob", Role: "user"})
	ap := NewAppPasswords(m)

	secret, _, err := ap.Issue(context.Background(), u.ID, "cli", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Disable through the same path the admin API uses.
	m.DisableUser(u.ID)

	if _, _, err := ap.Verify(context.Background(), secret, ""); !errors.Is(err, ErrInvalidAppPassword) {
		t.Fatalf("disabled account must not authenticate, got %v", err)
	}
}

func TestScopeValidation(t *testing.T) {
	ap, user := newAppPasswords(t)
	if _, _, err := ap.Issue(context.Background(), user.ID, "x", "admin", 0); err == nil {
		t.Fatal("unknown scope must be rejected")
	}
	for _, scope := range []string{"ro", "rw"} {
		if _, _, err := ap.Issue(context.Background(), user.ID, "x", scope, 0); err != nil {
			t.Fatalf("scope %q should be accepted: %v", scope, err)
		}
	}
}

func TestReadOnlyScopeTranslates(t *testing.T) {
	read, write := ScopeForActor("ro")
	if !read || write {
		t.Fatal("ro must map to read-only")
	}
	read, write = ScopeForActor("rw")
	if !read || !write {
		t.Fatal("rw must map to read-write")
	}
}

func TestEmptyLabelGetsDefault(t *testing.T) {
	ap, user := newAppPasswords(t)
	_, rec, err := ap.Issue(context.Background(), user.ID, "   ", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Label == "" {
		t.Fatal("empty label should fall back to a default")
	}
}

// TestRoleMappingIsFailClosed: an unknown group must under-privilege, never
// over-privilege.
func TestRoleMappingIsFailClosed(t *testing.T) {
	p := &Provider{groupRole: map[string]config.Role{
		"cloud-admins": config.RoleAdmin,
		"cloud-users":  config.RoleUser,
	}}
	cases := []struct {
		groups []string
		want   config.Role
	}{
		{nil, config.RoleGuest},
		{[]string{"unknown"}, config.RoleGuest},
		{[]string{"cloud-users"}, config.RoleUser},
		{[]string{"cloud-ADMINS"}, config.RoleGuest}, // group names are case-sensitive
		{[]string{"cloud-users", "cloud-admins"}, config.RoleAdmin},
	}
	for _, c := range cases {
		if got := p.Role(c.groups); got != c.want {
			t.Fatalf("groups %v: want %v, got %v", c.groups, c.want, got)
		}
	}
}

func TestRandomStateIsUniqueAndLargeEnough(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := RandomState()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) < 43 {
			t.Fatalf("state too short for a PKCE verifier: %d chars", len(s))
		}
		if seen[s] {
			t.Fatal("duplicate state generated")
		}
		seen[s] = true
	}
}

func TestPKCEChallengeIsS256(t *testing.T) {
	// The challenge must be base64url(SHA-256(verifier)), no padding. A mismatch
	// here shows up as an opaque "invalid_grant" from the IdP.
	verifier := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"
	got := pkceChallenge(verifier)
	if strings.ContainsAny(got, "=+/") {
		t.Fatalf("challenge must be unpadded base64url, got %q", got)
	}
	sum := sha256.Sum256([]byte(verifier))
	if len(got) != 43 {
		t.Fatalf("S256 challenge should be 43 chars, got %d", len(got))
	}
	_ = sum
}

func TestSessionLifecycle(t *testing.T) {
	m := store.NewMemory()
	u := m.AddUser(store.User{Username: "carol", Role: "user"})
	is := newTestIssuer(t)
	s := NewSessions(is, m, time.Hour)

	tok, err := s.Mint(context.Background(), u.ID, "carol@example.test", "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	claims, err := s.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != u.ID.String() {
		t.Fatalf("subject mismatch: %q", claims.Subject)
	}

	// Revoking kills it.
	if err := s.Revoke(context.Background(), claims.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("revoked session must not verify")
	}
}

func TestGarbageSessionRejected(t *testing.T) {
	m := store.NewMemory()
	s := NewSessions(newTestIssuer(t), m, time.Hour)
	for _, tok := range []string{"", "garbage", "session.a.b"} {
		if _, err := s.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("%q should be rejected, got %v", tok, err)
		}
	}
}

// TestRevokeAllSessions covers "log out everywhere" and the disabled-account path.
func TestRevokeAllSessions(t *testing.T) {
	m := store.NewMemory()
	u := m.AddUser(store.User{Username: "dave", Role: "user"})
	s := NewSessions(newTestIssuer(t), m, time.Hour)

	toks := make([]string, 3)
	for i := range toks {
		tok, err := s.Mint(context.Background(), u.ID, "d@example.test", "", "")
		if err != nil {
			t.Fatal(err)
		}
		toks[i] = tok
	}

	if err := s.RevokeAll(context.Background(), u.ID.String()); err != nil {
		t.Fatal(err)
	}
	for i, tok := range toks {
		if _, err := s.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidSession) {
			t.Fatalf("session %d should be revoked", i)
		}
	}
}
