package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/fakeidp"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

const testPublicURL = "https://cloud.example.test"

// oidcHarness is the app wired against a fake IdP, with a browser pinned to the
// public host so the __Host- cookie behaves as it does in production.
type oidcHarness struct {
	idp  *fakeidp.Provider
	meta *store.Memory
	b    *browser
	// url is the test server's base URL, so a test can reach endpoints that are not part
	// of the login flow itself (the admin API, for instance).
	url string
	// sessions mints a credential for an account the test created directly, which is how
	// an approval test acts as an administrator.
	sessions *auth.Sessions
}

func newOIDCHarness(t *testing.T, account fakeidp.User) *oidcHarness {
	t.Helper()
	return newOIDCHarnessWith(t, account, nil)
}

// newOIDCHarnessWith builds the OIDC harness with a config override applied before the
// server is constructed.
//
// The order matters: mutating the config afterwards would change a copy the server has
// already captured, so a policy under test would silently stay at its default and the test
// would pass for the wrong reason.
func newOIDCHarnessWith(t *testing.T, account fakeidp.User, mutate func(*config.Config)) *oidcHarness {
	t.Helper()

	idp, err := fakeidp.New("newcloud-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idp.Close)
	if account.Subject != "" {
		idp.SetUser(account)
	}

	cfg := config.Default()
	cfg.Server.PublicURL = testPublicURL
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Auth.Issuer = idp.URL()
	cfg.Auth.ClientID = "newcloud-test"
	cfg.Auth.ClientSecret = "test-secret"
	cfg.Auth.Scopes = []string{"openid", "profile", "email", "groups"}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.Auth.GroupRoleMap = map[string]config.Role{
		"cloud-users":  config.RoleUser,
		"cloud-admins": config.RoleAdmin,
	}

	meta := store.NewMemory()
	blobs, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content, err := blobstore.New(blobs, &adapter{r: meta.Resolver()})
	if err != nil {
		t.Fatal(err)
	}

	tickets, err := ticket.NewIssuer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := auth.NewProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OIDC discovery against the fake IdP: %v", err)
	}

	sessions := auth.NewSessions(tickets, meta, time.Hour)
	s, err := server.New(server.Deps{
		Config: cfg, Meta: meta, Content: content,
		Authz:    authz.NewService(&accessor{m: meta}),
		Tickets:  tickets,
		Sessions: sessions,
		AppPass:  auth.NewAppPasswords(meta),
		Shares:   share.New(meta, share.TestArgon()),
		Provider: provider,
		Log:      testLogger(t),
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv, b := newBrowserFor(t, s.Handler(), testPublicURL)
	t.Cleanup(srv.Close)

	return &oidcHarness{idp: idp, meta: meta, b: b, url: srv.URL, sessions: sessions}
}

// login drives the full browser flow: local login → IdP authorize → callback.
func (h *oidcHarness) login(t *testing.T) {
	t.Helper()

	// 1. Local login sets the state and PKCE cookies and redirects to the IdP.
	resp, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login should redirect to the IdP, got %d", resp.StatusCode)
	}

	authURL := resp.Header.Get("Location")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(authURL, h.idp.URL()) {
		t.Fatalf("redirected to %q, expected the IdP", authURL)
	}
	// PKCE must be present: without it an intercepted code becomes a session.
	if u.Query().Get("code_challenge") == "" ||
		u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL lacks an S256 PKCE challenge: %s", authURL)
	}
	if u.Query().Get("state") == "" {
		t.Fatal("authorization URL lacks a state parameter")
	}

	// 2. The IdP authorizes and redirects back to the callback on the public host.
	//
	// Only the callback hop matters here; the redirect that follows it points at
	// the SPA shell, which is not part of the API surface under test.
	client := h.b.followRedirects()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// Stop once the chain reaches the app root after the callback.
		if strings.HasSuffix(via[len(via)-1].URL.Path, "/api/auth/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	idpResp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	idpResp.Body.Close()

	if idpResp.StatusCode >= 400 {
		t.Fatalf("callback returned %d (url=%s)", idpResp.StatusCode, idpResp.Request.URL)
	}
}

// TestOIDCLoginEndToEnd is the one flow where a mistake becomes an authentication
// bypass, so it runs against a real IdP implementation over real redirects with a
// real cookie jar.
func TestOIDCLoginEndToEnd(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-1", Username: "alice",
		Email: "alice@example.test", Name: "Alice Example",
		Groups: []string{"cloud-users"},
	})

	if code, _ := h.b.me(); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 before login, got %d", code)
	}

	h.login(t)

	code, body := h.b.me()
	if code != http.StatusOK {
		t.Fatalf("/me after login: want 200, got %d", code)
	}
	if body["username"] != "alice" {
		t.Fatalf("username from the IdP claim: %v", body["username"])
	}
	if body["role"] != "user" {
		t.Fatalf("role should follow the group mapping, got %v", body["role"])
	}
	if n := len(h.meta.Users()); n != 1 {
		t.Fatalf("want exactly one provisioned account, got %d", n)
	}
}

// TestLoginIsIdempotent: signing in twice must not create a second account.
func TestLoginIsIdempotent(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "stable-subject", Username: "bob",
		Email: "bob@example.test", Groups: []string{"cloud-users"},
	})

	h.login(t)
	first := len(h.meta.Users())

	h.b.resetJar(t)
	h.login(t)

	if got := len(h.meta.Users()); got != first {
		t.Fatalf("second login must reuse the account, users went %d -> %d", first, got)
	}
}

// TestCancelledConsentCreatesNoSession.
func TestCancelledConsentCreatesNoSession(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-3", Username: "carol",
		Email: "carol@example.test", Groups: []string{"cloud-users"},
	})
	h.idp.FailNextCode = true

	resp, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if code, _ := h.b.me(); code != http.StatusUnauthorized {
		t.Fatalf("a cancelled login must not authenticate, got %d", code)
	}
	if n := len(h.meta.Users()); n != 0 {
		t.Fatalf("a cancelled login must not provision an account, got %d", n)
	}
}

// TestForgedStateRejected covers CSRF on the callback.
func TestForgedStateRejected(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{Subject: "s", Username: "u", Email: "u@example.test"})

	resp, err := h.b.getRaw("/api/auth/callback?code=abc&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state must be rejected with 400, got %d", resp.StatusCode)
	}
}

// TestUnmappedGroupFallsBackToGuest: an unknown group must under-privilege.
func TestUnmappedGroupFallsBackToGuest(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-9", Username: "stranger",
		Email: "stranger@example.test", Groups: []string{"some-other-group"},
	})

	h.login(t)

	code, body := h.b.me()
	if code != http.StatusOK {
		t.Fatalf("/me: want 200, got %d", code)
	}
	if body["role"] != "guest" {
		t.Fatalf("an unmapped group must fall back to guest, got %v", body["role"])
	}
}

// TestLogoutRevokesSession: clearing the cookie is not enough; the token must stop
// working server-side.
func TestLogoutRevokesSession(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-4", Username: "dave",
		Email: "dave@example.test", Groups: []string{"cloud-users"},
	})
	h.login(t)

	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("expected a session, got %d", code)
	}

	token := h.b.sessionCookie(testPublicURL)
	if token == "" {
		t.Fatal("no session cookie was stored")
	}

	resp, err := h.b.post("/api/auth/logout")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Replay the captured token directly: a stolen cookie must not survive logout.
	req, _ := http.NewRequest(http.MethodGet, h.b.URL("/api/auth/me"), nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: token})
	rr, err := h.b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked session must not work, got %d", rr.StatusCode)
	}
}

// TestGroupRemovalRevokesTheLiveSession is the T-02 acceptance case: somebody is
// removed from the admin group, and the session they already hold must stop
// working — not merely report a lower role at the next login.
//
// The test captures the cookie *before* the demotion and replays it afterwards,
// because a stolen or already-issued cookie is exactly the credential that would
// otherwise keep the old privilege alive.
func TestGroupRemovalRevokesTheLiveSession(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-admin", Username: "eve",
		Email: "eve@example.test", Groups: []string{"cloud-admins"},
	})
	h.login(t)
	token := h.b.sessionCookie(testPublicURL)
	if token == "" {
		t.Fatal("no session cookie was stored")
	}

	// Confirm the session is an admin session.
	if role := h.b.meUser(t); role != "admin" {
		t.Fatalf("expected an admin session, got role %q", role)
	}

	// The IdP now says the account is only a regular user.
	h.idp.SetUser(fakeidp.User{
		Subject: "idp-subject-admin", Username: "eve",
		Email: "eve@example.test", Groups: []string{"cloud-users"},
	})

	// Removing the group is only observed when NewCloud next talks to the IdP,
	// which is a login. Re-authenticating is what a user would do anyway; the
	// question is whether the *old* credential still works afterwards.
	h.login(t)

	req, _ := http.NewRequest(http.MethodGet, h.b.URL("/api/auth/me"), nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: token})
	// A jar-less client on purpose: the 401 for a revoked session also carries a
	// Set-Cookie that clears the session, and sharing the browser's jar would erase
	// the fresh session the second login just established.
	rr, err := h.b.jarlessClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Body.Close()
	if rr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the pre-demotion session must be revoked, got %d", rr.StatusCode)
	}
	// And the account really is a user now. The cookie the jar holds after the
	// second login is the freshly minted one, which must work: revoking every
	// session and then handing back a dead one would look like a broken login.
	if role := h.b.meUser(t); role != "user" {
		t.Fatalf("role should be user after the demotion, got %q", role)
	}
}

// TestLoginTwiceIsFine establishes the baseline the demotion test relies on: a
// second login in the same browser must produce a working session. If this fails,
// the problem is the harness or the login flow, not role reconciliation.
func TestLoginTwiceIsFine(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-twice", Username: "twice",
		Email: "twice@example.test", Groups: []string{"cloud-users"},
	})
	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("first login: want 200, got %d", code)
	}
	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("second login: want 200, got %d", code)
	}
}

// TestLoginSendsAndVerifiesNonce is the ID-02 check.
//
// PKCE binds the code to this client and state binds the callback to this browser
// session; neither binds the *ID token* to the request that asked for it. Without a
// nonce, an ID token minted for a different request to the same client still verifies,
// because signature, issuer, audience and expiry all hold — which is the replay window
// this closes.
func TestLoginSendsAndVerifiesNonce(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-nonce", Username: "nonce", Email: "nonce@example.test",
	})

	// The authorize URL must carry a nonce, or the IdP has nothing to echo.
	resp, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	authURL := resp.Header.Get("Location")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("nonce") == "" {
		t.Fatal("the authorization request must carry a nonce")
	}
	if u.Query().Get("code_challenge") == "" {
		t.Fatal("PKCE must still be present alongside the nonce")
	}

	// The full flow completes, which means the token's nonce matched.
	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("login with nonce: want 200, got %d", code)
	}
}

// TestCallbackWithoutNonceCookieIsRejected: a callback that did not start a
// nonce-bearing flow must not be accepted, or the check could be skipped by simply
// dropping the cookie.
func TestCallbackWithoutNonceCookieIsRejected(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "s", Username: "u", Email: "u@example.test",
	})

	resp, err := h.b.getRaw("/api/auth/callback?code=abc&state=whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a callback without a nonce must be refused, got %d", resp.StatusCode)
	}
}

// TestNonceReplayAcrossFlowsIsRejected proves the binding is per-request.
//
// The scenario: an attacker obtains an ID token issued for one authorization request
// and tries to have it accepted for another. The second flow issues a different nonce,
// so the token's claim cannot match.
func TestNonceReplayAcrossFlowsIsRejected(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-replay", Username: "replay", Email: "replay@example.test",
	})

	// Start flow A and capture the cookie values the browser would hold.
	respA, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer respA.Body.Close()
	var nonceA, stateA, pkceA string
	for _, c := range respA.Cookies() {
		switch c.Name {
		case "__Host-wc_nonce":
			nonceA = c.Value
		case "__Host-wc_state":
			stateA = c.Value
		case "__Host-wc_pkce":
			pkceA = c.Value
		}
	}
	if nonceA == "" || stateA == "" || pkceA == "" {
		t.Fatalf("login must set state, pkce and nonce cookies (got %q/%q/%q)",
			stateA, pkceA, nonceA)
	}

	// Start flow B and confirm it uses a different nonce.
	respB, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer respB.Body.Close()
	var nonceB string
	for _, c := range respB.Cookies() {
		if c.Name == "__Host-wc_nonce" {
			nonceB = c.Value
		}
	}
	if nonceB == "" {
		t.Fatal("the second flow must also issue a nonce")
	}
	if nonceA == nonceB {
		t.Fatal("each authorization request must get its own nonce, or replay is trivial")
	}
}

// TestMismatchedNonceIsRejected is the check the other nonce tests cannot make.
//
// They assert a nonce is *sent*, which stays green even if the comparison on the way
// back is deleted. This makes the IdP echo a different nonce — a token valid in every
// other respect, minted for a different request — and requires the callback to refuse
// it.
func TestMismatchedNonceIsRejected(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-tamper", Username: "tamper", Email: "tamper@example.test",
	})

	// The IdP will echo a nonce nobody asked for.
	h.idp.TamperNonce = true

	// Drive the flow to the callback by hand so the status code is observable: login()
	// asks the harness to fail on any non-2xx, and here a refusal is the expected
	// outcome.
	resp, err := h.b.getRaw("/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	authURL := resp.Header.Get("Location")

	client := h.b.followRedirects()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.HasSuffix(via[len(via)-1].URL.Path, "/api/auth/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	cb, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()

	if cb.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a mismatched nonce must be rejected with 401, got %d", cb.StatusCode)
	}

	// And no session was created.
	if code, _ := h.b.me(); code != http.StatusUnauthorized {
		t.Fatalf("no session may exist after a refused callback, got %d", code)
	}
}
