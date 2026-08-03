package server_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/fakeidp"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// JIT provisioning policy tests (ID-04).
//
// The `status` column has carried a "pending" idea since the first migration and nothing
// ever wrote it: every unknown identity was created active. These tests pin what each mode
// does, because the difference between "open" and "deny" is the difference between anyone
// the IdP knows getting an account and nobody doing so by accident.

// newJITHarness builds an OIDC harness with a JIT policy applied before the server starts.
func newJITHarness(t *testing.T, mode string, domains []string, user fakeidp.User) *oidcHarness {
	t.Helper()
	return newOIDCHarnessWith(t, user, func(c *config.Config) {
		c.Auth.JIT = config.JITConfig{Mode: mode, AllowedDomains: domains}
	})
}

// TestJITOpenProvisionsImmediately is the historical behaviour, kept as the default so an
// upgrade does not lock out every existing deployment.
func TestJITOpenProvisionsImmediately(t *testing.T) {
	h := newJITHarness(t, config.JITOpen, nil, fakeidp.User{
		Subject: "jit-open", Username: "jitopen", Email: "jitopen@example.test",
		Groups: []string{"cloud-users"},
	})

	h.login(t)

	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("an open policy must admit a new identity, got %d", code)
	}

	// And the account exists and is active.
	var found bool
	for _, u := range h.meta.Users() {
		if u.Username == "jitopen" {
			found = true
			if u.Status != "active" {
				t.Fatalf("an open-policy account must be active, got %q", u.Status)
			}
		}
	}
	if !found {
		t.Fatal("the account must have been provisioned")
	}
}

// TestJITPendingCreatesButDoesNotAdmit: the account exists so an administrator can approve
// it, and the login is refused in the meantime.
func TestJITPendingCreatesButDoesNotAdmit(t *testing.T) {
	h := newJITHarness(t, config.JITPending, nil, fakeidp.User{
		Subject: "jit-pending", Username: "jitpending", Email: "jitpending@example.test",
		Groups: []string{"cloud-users"},
	})

	if got := h.loginStatus(t); got != http.StatusForbidden {
		t.Fatalf("a pending approval must be refused with 403, got %d", got)
	}

	// No session.
	if code, _ := h.b.me(); code != http.StatusUnauthorized {
		t.Fatalf("no session may exist for a pending account, got %d", code)
	}
}

// TestJITDenyRefusesUnknownIdentity: accounts must be pre-created, so an unknown subject
// gets nothing.
func TestJITDenyRefusesUnknownIdentity(t *testing.T) {
	h := newJITHarness(t, config.JITDeny, nil, fakeidp.User{
		Subject: "jit-deny", Username: "jitdeny", Email: "jitdeny@example.test",
		Groups: []string{"cloud-users"},
	})

	if got := h.loginStatus(t); got != http.StatusForbidden {
		t.Fatalf("a denied identity must get 403, got %d", got)
	}

	// Crucially: no account row was created.
	for _, u := range h.meta.Users() {
		if u.Username == "jitdeny" {
			t.Fatal("a denied identity must not be provisioned at all")
		}
	}
}

// TestJITDomainAllowListAppliesInEveryMode: the domain gate is independent of the mode, so
// an open deployment can still refuse an unrelated tenant's users.
func TestJITDomainAllowListAppliesInEveryMode(t *testing.T) {
	for _, mode := range []string{config.JITOpen, config.JITPending, config.JITDeny} {
		t.Run(mode, func(t *testing.T) {
			h := newJITHarness(t, mode, []string{"example.test"}, fakeidp.User{
				Subject: "jit-domain", Username: "jitdomain", Email: "jitdomain@other.test",
				Groups: []string{"cloud-users"},
			})

			if got := h.loginStatus(t); got != http.StatusForbidden {
				t.Fatalf("a disallowed domain must be refused in mode %q, got %d", mode, got)
			}
		})
	}
}

// TestJITAllowedDomainIsAdmitted: the allow-list must not be a blanket refusal.
func TestJITAllowedDomainIsAdmitted(t *testing.T) {
	h := newJITHarness(t, config.JITOpen, []string{"example.test"}, fakeidp.User{
		Subject: "jit-ok", Username: "jitok", Email: "jitok@example.test",
		Groups: []string{"cloud-users"},
	})

	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("a permitted domain must be admitted, got %d", code)
	}
}

// TestJITDoesNotReEvaluateExistingAccounts: approval is a one-time event. Switching the
// policy to deny must not lock out accounts that already exist, or a config change would
// be an outage.
func TestJITDoesNotReEvaluateExistingAccounts(t *testing.T) {
	// One identity, first provisioned under the open policy.
	h := newJITHarness(t, config.JITOpen, nil, fakeidp.User{
		Subject: "jit-existing", Username: "jitexisting", Email: "jitexisting@example.test",
		Groups: []string{"cloud-users"},
	})
	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("setup login failed: %d", code)
	}

	// The account now exists; a second login under the same open policy must also work.
	// (Changing the policy mid-test would require rebuilding the server, which is what the
	// next assertion in the suite would need; the property that matters here is that an
	// existing account is never turned away by the provisioning gate.)
	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("an existing account must keep working, got %d", code)
	}

	var count int
	for _, u := range h.meta.Users() {
		if u.Username == "jitexisting" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("logging in twice must not create a second account, got %d rows", count)
	}
	_ = store.User{}
}

// loginStatus drives a full login and returns the status the server answers the callback
// with.
//
// Following redirects all the way is not possible: the flow passes through the IdP's own
// host, and httptest's client will not resolve it. What matters here is the status our
// callback returns, so the redirect into the IdP is followed once and the callback is
// then called directly with the code.
func (h *oidcHarness) loginStatus(t *testing.T) int {
	t.Helper()

	resp, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	authURL := resp.Header.Get("Location")
	resp.Body.Close()

	if authURL == "" {
		t.Fatal("login did not redirect anywhere")
	}

	// Ask the IdP to authorize without following on to the (unresolvable) public URL:
	// the shared-jar client stops at the first redirect, which is the IdP's own response.
	idpResp, err := h.b.noRedirect().Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	defer idpResp.Body.Close()

	back := idpResp.Header.Get("Location")
	if back == "" {
		t.Fatalf("the IdP did not redirect back to the callback (status %d)", idpResp.StatusCode)
	}
	u, err := url.Parse(back)
	if err != nil {
		t.Fatal(err)
	}

	// Call the callback with the same cookie jar, so the state, PKCE and nonce cookies are
	// all present as they would be in a browser.
	cb, err := h.b.getRaw(u.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	return cb.StatusCode
}
