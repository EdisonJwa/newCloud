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
}

func newOIDCHarness(t *testing.T, account fakeidp.User) *oidcHarness {
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
	cfg.Auth.GroupRoleMap = map[string]config.Role{"cloud-users": config.RoleUser}

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

	s, err := server.New(server.Deps{
		Config: cfg, Meta: meta, Content: content,
		Authz:    authz.NewService(&accessor{m: meta}),
		Tickets:  tickets,
		Sessions: auth.NewSessions(tickets, meta, time.Hour),
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

	return &oidcHarness{idp: idp, meta: meta, b: b}
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
