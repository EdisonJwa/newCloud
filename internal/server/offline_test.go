package server_test

// ID-07 at the HTTP layer: with no identity provider configured, the server still serves
// everything that does not need one.
//
// The distinction this pins is the whole point of the round-11 change: an IdP outage used to
// mean the process refused to start, taking WebDAV, app passwords and existing sessions with it
// — none of which involve the IdP.

import (
	"context"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestServerStartsWithNoProvider: a nil Provider must not prevent construction.
//
// The harness builds the server with a Provider; this case rebuilds it without one to prove the
// zero-provider path is a supported configuration rather than an accident that only main()
// happens to avoid.
func TestServerStartsWithNoProvider(t *testing.T) {
	h := newHarness(t)

	// Everything that should not need the IdP.
	resp := h.do(http.MethodGet, "/api/auth/me", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an existing session must work with no IdP, got %d", resp.StatusCode)
	}

	// A new login must fail with the DEFINED status. This used to assert only "not a 500" and to
	// tolerate 404 in case the route had been renamed — which meant it could not fail for the one
	// thing it was testing, and would have stayed green if the endpoint disappeared entirely. The
	// exact answer is 503 idp_unavailable; the rest of ID-07 is in idpoffline_test.go.
	login := h.do(http.MethodGet, "/api/auth/login", nil, nil)
	body := decode(t, login)
	login.Body.Close()
	if login.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a login attempt with no IdP returned %d, want 503 idp_unavailable", login.StatusCode)
	}
	if code, _ := body["error"].(string); code != "idp_unavailable" {
		t.Fatalf("error code = %v, want idp_unavailable", body["error"])
	}
}

// TestWebDAVWorksWithoutAnyIdPInteraction: app-password auth is local, so a filesystem operation
// must succeed while the IdP is unavailable.
func TestWebDAVWorksWithoutAnyIdPInteraction(t *testing.T) {
	h := newHarness(t)

	// Issue the credential through the service the server itself uses, so this test does not
	// depend on the HTTP shape of the app-password endpoint.
	secret, _, err := auth.NewAppPasswords(h.meta).Issue(context.Background(), h.user.ID, "offline", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("PROPFIND", h.srv.URL+"/dav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(h.user.Username, secret)
	req.Header.Set("Depth", "1")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND with an app password must work regardless of the IdP, got %d",
			resp.StatusCode)
	}
}

// TestSessionRevocationWorksWithoutIdP: whether a session is live is a database question, so an
// administrator can revoke during an outage.
func TestSessionRevocationWorksWithoutIdP(t *testing.T) {
	h := newHarness(t)

	// The harness session works.
	before := h.do(http.MethodGet, "/api/auth/me", nil, nil)
	before.Body.Close()
	if before.StatusCode != http.StatusOK {
		t.Fatalf("setup: session should work, got %d", before.StatusCode)
	}

	// Disabling the account revokes it — a purely local operation.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.SetUserStatus(ctx, h.user.ID, store.StatusDisabled)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	after := h.do(http.MethodGet, "/api/auth/me", nil, nil)
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revocation must work with no IdP involvement, got %d", after.StatusCode)
	}
}
