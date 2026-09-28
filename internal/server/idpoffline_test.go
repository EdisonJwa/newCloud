package server_test

// ID-07: what happens when the identity provider is away.
//
// The requirement asks for defined rules rather than good intentions:
//
//	login        a new session cannot be created, and the refusal says so — 503 idp_unavailable,
//	             not a generic failure that leaves the user thinking their account is broken
//	existing     a session keeps working. Authentication is local; the IdP is only consulted to
//	             establish identity, and an outage must not lock people out of their own files
//	renewal      there is no silent re-authentication to fail: sessions are renewed by the
//	             application, so an outage does not shorten or extend anyone's session
//	revocation   still works, because it is a local decision
//
// The existing test for this only asserted "not a 500" and even tolerated 404 — a check that
// cannot fail is worse than none, because it reads as coverage. These assert the defined answers.

import (
	"context"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/server"
)

// noProviderHarness builds a server that cannot reach any IdP, the way a deployment looks when
// discovery has never succeeded.
func noProviderHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessTuned(t, nil, func(d *server.Deps) {
		d.Provider = nil
		d.Providers = nil
	})
}

func TestLoginWithoutAnIdentityProviderIsADefinedRefusal(t *testing.T) {
	h := noProviderHarness(t)

	resp := h.do(http.MethodGet, "/api/auth/login", nil, nil)
	body := decode(t, resp)
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login with no identity provider returned %d, want 503. The answer is defined; a "+
			"404 would mean the route moved, a 500 would mean the condition is unexpected.",
			resp.StatusCode)
	}
	// The code is what a client can act on, and the message is what tells the user their account
	// is not the problem.
	if code, _ := body["error"].(string); code != "idp_unavailable" {
		t.Fatalf("error code = %v, want idp_unavailable", body["error"])
	}
}

func TestExistingSessionSurvivesAnIdPOutageAndCanStillBeRevoked(t *testing.T) {
	h := noProviderHarness(t)

	// The session was minted before the outage.
	if resp := h.do(http.MethodGet, "/api/auth/me", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("an existing session must keep working with no IdP, got %d", resp.StatusCode)
	}

	// And files stay reachable: this is the difference between "cannot sign in" and "locked out".
	resp := h.do(http.MethodGet, "/api/files/", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing files must not depend on the IdP, got %d", resp.StatusCode)
	}

	// Revocation is a local decision, so it must still work while the IdP is unreachable.
	admin, _ := h.adminClient(t)
	if code, payload := h.adminStatus(t, admin, h.user.ID.String(), "disabled"); code != http.StatusOK {
		t.Fatalf("disable during an outage: %d %v", code, payload)
	}
	if resp := h.do(http.MethodGet, "/api/auth/me", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the disabled account's session returned %d during an outage; revocation must not "+
			"depend on the identity provider", resp.StatusCode)
	}
}

// TestWebDAVStaysAvailableDuringAnOutage: app passwords are verified locally, so a desktop client
// keeps syncing while the IdP is down. This is the half of ID-07 that makes the outage survivable.
func TestWebDAVStaysAvailableDuringAnOutage(t *testing.T) {
	h := noProviderHarness(t)

	// Issued through the same service the server uses, so this does not depend on the HTTP shape
	// of the app-password endpoint.
	issued, _, err := auth.NewAppPasswords(h.meta).Issue(
		context.Background(), h.user.ID, "offline", "rw", 0)
	if err != nil {
		t.Fatalf("issue app password: %v", err)
	}

	req, err := http.NewRequest("PROPFIND", h.srv.URL+"/dav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(h.user.Username, issued)
	req.Header.Set("Depth", "0")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND with an app password during an outage returned %d, want 207",
			resp.StatusCode)
	}
}
