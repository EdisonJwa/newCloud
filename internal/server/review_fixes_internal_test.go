package server

// Regression tests for the 2026-10-25 review fixes that live in unexported code.
//
// These are pure-function and small-object properties: the interesting behaviour is
// "what does this predicate answer", and driving a full HTTP flow for each would hide
// the answer behind harness noise. The end-to-end halves of the same fixes are in
// review_fixes_test.go (package server_test).

import (
	"testing"
	"time"

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
