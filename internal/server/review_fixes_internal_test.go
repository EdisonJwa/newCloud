package server

// Regression tests for the 2026-10-25 review fixes that live in unexported code.
//
// These are pure-function and small-object properties: the interesting behaviour is
// "what does this predicate answer", and driving a full HTTP flow for each would hide
// the answer behind harness noise. The end-to-end halves of the same fixes are in
// review_fixes_test.go (package server_test).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestSanitiseUsernameHandlesShortSubjects pins the fix for the login-path panic.
//
// The fallback segment used to slice the dash-stripped subject at a fixed 12 bytes,
// so an IdP whose sub was shorter than that — and which sent no username and no
// email — panicked on every login, for that identity, forever. The segment must be
// clamped, and when nothing usable is left it must come from a stable hash of the
// subject instead.
func TestSanitiseUsernameHandlesShortSubjects(t *testing.T) {
	// The original panic: a 3-byte subject.
	if got := sanitiseUsername("", "", "abc"); got == "" {
		t.Fatal("a short subject must still produce a namespace segment")
	}
	// An all-dashes subject strips to nothing; the hash fallback answers.
	got := sanitiseUsername("", "", "---")
	if got == "" || got == "u" {
		t.Fatalf("an all-dashes subject must fall back to the hash segment, got %q", got)
	}
	// Stable: the same subject always yields the same segment.
	if again := sanitiseUsername("", "", "---"); again != got {
		t.Fatalf("the fallback must be deterministic: %q then %q", got, again)
	}
	// Distinct subjects do not collide on the fallback.
	if other := sanitiseUsername("", "", "&&&"); other == got {
		t.Fatalf("distinct subjects must not share a fallback segment: %q", got)
	}
	// A long subject is truncated exactly as before, so existing accounts keep
	// their namespace.
	long := "aaaa-bbbb-cccc-dddd-eeee"
	want := "u" + "aaaabbbbcccc"
	if got := sanitiseUsername("", "", long); got != want {
		t.Fatalf("long subject segment changed: got %q, want %q", got, want)
	}
}

// TestIsSameSitePathRejectsBackslashTargets pins the open-redirect fix.
//
// Browsers normalise "\" to "/", so "/\evil.com" passed the old
// "starts with / and not with //" check and then resolved to the host evil.com.
// The rule is strict on purpose: a same-origin target has no use for a backslash
// at all, and a second slash or backslash makes the rest an authority.
func TestIsSameSitePathRejectsBackslashTargets(t *testing.T) {
	refused := []string{
		"",                 // not a path
		"https://evil.com", // absolute URL
		"//evil.com",       // protocol-relative
		`/\evil.com`,       // the browser-normalised bypass
		`/\\evil.com`,      // double backslash, same destination
		`/ok\../../evil`,   // backslash anywhere disqualifies
		`/\`,               // nothing but the bypass shape
	}
	for _, p := range refused {
		if isSameSitePath(p) {
			t.Errorf("isSameSitePath(%q) = true, want false", p)
		}
	}
	allowed := []string{"/", "/ok/path", "/files?sort=name"}
	for _, p := range allowed {
		if !isSameSitePath(p) {
			t.Errorf("isSameSitePath(%q) = false, want true", p)
		}
	}
}

// TestEffectiveLoginEmailTreatsUnverifiedAsAbsent pins the email_verified parsing
// contract: nil (claim absent) keeps the address, explicit false strips it.
func TestEffectiveLoginEmailTreatsUnverifiedAsAbsent(t *testing.T) {
	tr := true
	fls := false
	cases := []struct {
		email string
		v     *bool
		want  string
	}{
		{"a@x.test", nil, "a@x.test"}, // IdP sent no claim: historical behaviour
		{"a@x.test", &tr, "a@x.test"}, // verified
		{"a@x.test", &fls, ""},        // explicitly unverified: unusable
		{"", nil, ""},                 // no address at all
	}
	for _, c := range cases {
		if got := effectiveLoginEmail(c.email, c.v); got != c.want {
			t.Errorf("effectiveLoginEmail(%q, %v) = %q, want %q", c.email, c.v, got, c.want)
		}
	}
}

// TestDecideJITTreatsUnverifiedEmailAsAbsentFromAllowlist pins the JIT half of the
// email_verified fix: an allow-listed domain must not be satisfied by an address the
// IdP explicitly declined to vouch for. IdPs that omit the claim are unaffected.
func TestDecideJITTreatsUnverifiedEmailAsAbsentFromAllowlist(t *testing.T) {
	tr := true
	fls := false
	s := &Server{deps: Deps{Config: config.Config{
		Auth: config.AuthConfig{JIT: config.JITConfig{
			Mode:           config.JITOpen,
			AllowedDomains: []string{"example.test"},
		}},
	}}}

	if d := s.decideJIT(false, "in@other.test", nil); d.Allowed() {
		t.Fatal("a different domain must be refused regardless of the claim")
	}
	if d := s.decideJIT(false, "a@example.test", &fls); d.Allowed() {
		t.Fatal("an explicitly unverified address must not satisfy the allow-list")
	}
	if d := s.decideJIT(false, "a@example.test", nil); !d.Allowed() {
		t.Fatal("a claim-less address keeps the historical behaviour")
	}
	if d := s.decideJIT(false, "a@example.test", &tr); !d.Allowed() {
		t.Fatal("a verified address satisfies the allow-list")
	}
}

// TestBreakglassThrottleResetsOnSuccess exercises the limiter directly: five
// failures must close the window, and a success must open it again — the property
// that keeps an operator who mistypes their recovery password locked out of the
// recovery path mid-incident.
func TestBreakglassThrottleResetsOnSuccess(t *testing.T) {
	l := newFailureThrottle()
	now := time.Now()

	for i := 0; i < store.PasswordMaxAttempts; i++ {
		if !l.allow("10.0.0.1", now) {
			t.Fatalf("attempt %d must be allowed", i+1)
		}
		l.recordFailure("10.0.0.1", now)
	}
	if l.allow("10.0.0.1", now) {
		t.Fatal("after the allowance is spent, the next attempt must be refused")
	}
	// A different source is unaffected.
	if !l.allow("10.0.0.2", now) {
		t.Fatal("another source must not inherit this one's failures")
	}
	// Success clears the count.
	l.reset("10.0.0.1", now)
	if !l.allow("10.0.0.1", now) {
		t.Fatal("after a successful login the source must be allowed again")
	}
	// Entries age out of the window without a reset.
	l2 := newFailureThrottle()
	for i := 0; i < store.PasswordMaxAttempts; i++ {
		l2.recordFailure("10.0.0.9", now)
	}
	if l2.allow("10.0.0.9", now.Add(store.PasswordWindow)) {
		t.Fatal("failures must count for the whole window")
	}
	if !l2.allow("10.0.0.9", now.Add(store.PasswordWindow+time.Minute)) {
		t.Fatal("failures older than the window must stop counting")
	}
}

// TestTheSubtreeDownloadWalkDemandsDownloadOfEveryMember pins the primitive the zip route hands
// its decision to. The route resolves `path` in the caller's own tree, where the caller is the
// owner and holds every permission, so the refusal is not reachable through HTTP today — the
// walk is what an id-addressed zip will call, and it must demand `download` of every member,
// not `read`, and refuse when any member lacks it.
func TestTheSubtreeDownloadWalkDemandsDownloadOfEveryMember(t *testing.T) {
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "owner", Email: "owner@example.test", Role: "user"})
	root, err := meta.EnsureRoot(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	mate := meta.AddUser(store.User{Username: "zipper", Email: "zipper@example.test", Role: "user"})

	var vault, inside store.Node
	err = meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		v, verr := tx.CreateDir(ctx, store.NewNode{OwnerID: owner.ID, ParentID: root.ID, Name: "vault"})
		if verr != nil {
			return verr
		}
		vault = v
		inside, verr = tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner.ID, ParentID: vault.ID, Name: "inside.txt",
			Size: 4, Mime: "text/plain", ETag: "etag-1",
			BlobID: uuid.New(), BlobHash: [32]byte{1},
		})
		return verr
	})
	if err != nil {
		t.Fatal(err)
	}

	grant := func(node uuid.UUID, perms store.Perm, inherit bool) {
		t.Helper()
		err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, gerr := tx.Grant(ctx, store.ACLEntry{NodeID: node, UserID: &mate.ID, Perms: perms, Inherit: inherit})
			return gerr
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The folder: visible and takeable, but the grant does NOT inherit (the child's own entry
	// decides it). The child: visible, not takeable — the exact split §4.2's download bit is for.
	grant(vault.ID, store.PermRead|store.PermDownload|store.PermListChildren, false)
	grant(inside.ID, store.PermRead|store.PermListChildren, true)

	s := &Server{deps: Deps{
		Meta:  meta,
		Authz: authz.NewService(&reviewAccessor{m: meta}),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	actor := authz.NewUserActor(mate.ID, mate.Email, authz.RoleUser, authz.ScopeRead|authz.ScopeWrite)

	if s.checkSubtreePermCtx(context.Background(), actor, vault.ID, authz.PermDownload) {
		t.Fatal("a subtree with a non-downloadable member must not pass the download walk")
	}
	if !s.checkSubtreePermCtx(context.Background(), actor, vault.ID, authz.PermRead) {
		t.Fatal("the same subtree is readable; the refusal must be about download, not access")
	}

	// Grant download on the child and the walk passes — one grant on one member, nothing else
	// changed. This is what proves the refusal above is the missing bit and not a broken setup.
	grant(inside.ID, store.PermRead|store.PermListChildren|store.PermDownload, true)
	if !s.checkSubtreePermCtx(context.Background(), actor, vault.ID, authz.PermDownload) {
		t.Fatal("after the member is takeable the download walk must pass")
	}
}

// reviewAccessor adapts the in-memory store to the authorizer. It mirrors the accessor in
// e2e_test.go (which lives in the external test package and cannot be shared); if the two
// drift, the conformance suite is what holds both to the store's real answers.
type reviewAccessor struct{ m *store.Memory }

func (a *reviewAccessor) LoadNode(id uuid.UUID) (authz.Node, error) {
	var n store.Node
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, gerr := tx.Node(ctx, id)
		n = got
		return gerr
	})
	if err != nil {
		return authz.Node{}, authz.ErrNotFound
	}
	return authz.Node{ID: n.ID, OwnerID: n.OwnerID, ParentID: n.ParentID}, nil
}

func (a *reviewAccessor) OwnerGroupPerm(actorID, nodeID uuid.UUID) (authz.Perm, bool, error) {
	var (
		perms store.Perm
		ok    bool
	)
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, found, gerr := tx.EffectivePermsFor(ctx, actorID, nodeID)
		perms, ok = p, found
		return gerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return authz.Perm(perms), ok, nil
}

func (a *reviewAccessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	return nil, nil
}
