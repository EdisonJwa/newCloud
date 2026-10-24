package server_test

// End-to-end regression tests for the 2026-10-25 auth review fixes.
//
// The fake IdP in internal/fakeidp always asserts email_verified: true, and these
// tests need to control that claim (the unverified-address cases are the point), so
// reviewStaticIdp below is a second, even smaller stand-in whose claims the test
// sets directly. Everything else about the flow — discovery, JWKS, PKCE, nonce — is
// driven for real, because the defect class here is exactly "the claim was parsed
// but never enforced".

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/fakeidp"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ---------------------------------------------------------------------------
// The claims-controllable IdP.
// ---------------------------------------------------------------------------

type staticIdp struct {
	srv      *httptest.Server
	url      string
	clientID string
	key      *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]staticCode

	sub      string
	username string
	email    string
	groups   []string
	// emailVerified is the email_verified claim of the next ID token. Nil omits the
	// claim entirely, which is its own case: some IdPs never send it.
	emailVerified *bool
}

type staticCode struct{ challenge, nonce string }

func newStaticIdp(t *testing.T, clientID string) *staticIdp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &staticIdp{
		clientID: clientID, key: key,
		codes: map[string]staticCode{},
		sub:   "static-sub-1", username: "static", email: "static@example.test",
		groups: []string{"cloud-users"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/keys", p.jwks)
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewServer(mux)
	p.url = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

func boolPtr(b bool) *bool { return &b }

func (p *staticIdp) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (p *staticIdp) discovery(w http.ResponseWriter, r *http.Request) {
	p.writeJSON(w, map[string]any{
		"issuer":                                p.url,
		"authorization_endpoint":                p.url + "/authorize",
		"token_endpoint":                        p.url + "/token",
		"jwks_uri":                              p.url + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *staticIdp) jwks(w http.ResponseWriter, r *http.Request) {
	pub := p.key.Public().(*rsa.PublicKey)
	p.writeJSON(w, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "static-key",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (p *staticIdp) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "S256 PKCE required", http.StatusBadRequest)
		return
	}
	code := staticRandom()
	p.mu.Lock()
	p.codes[code] = staticCode{challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	p.mu.Unlock()
	http.Redirect(w, r, q.Get("redirect_uri")+"?code="+url.QueryEscape(code)+
		"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
}

func (p *staticIdp) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.FormValue("code")
	verifier := r.FormValue("code_verifier")

	p.mu.Lock()
	issued, ok := p.codes[code]
	if ok {
		delete(p.codes, code)
	}
	p.mu.Unlock()
	if !ok {
		p.writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != issued.challenge {
		p.writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}

	idToken, err := p.signIDToken(issued.nonce)
	if err != nil {
		http.Error(w, "signing failed", http.StatusInternalServerError)
		return
	}
	p.writeJSON(w, map[string]any{
		"access_token": staticRandom(), "token_type": "Bearer", "expires_in": 3600,
		"id_token": idToken,
	})
}

func (p *staticIdp) signIDToken(nonce string) (string, error) {
	now := time.Now()
	payload := map[string]any{
		"iss": p.url, "sub": p.sub, "aud": p.clientID,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"preferred_username": p.username, "email": p.email,
		"name": p.username, "groups": p.groups,
	}
	if p.emailVerified != nil {
		payload["email_verified"] = *p.emailVerified
	}
	if nonce != "" {
		payload["nonce"] = nonce
	}
	head, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "static-key"})
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func staticRandom() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// newStaticHarness wires the OIDC harness against the claims-controllable IdP.
func newStaticHarness(t *testing.T, p *staticIdp, mutate func(*config.Config)) *oidcHarness {
	t.Helper()
	return newOIDCHarnessWith(t, fakeidp.User{}, func(c *config.Config) {
		c.Auth.Issuer = p.url
		if mutate != nil {
			mutate(c)
		}
	})
}

// ---------------------------------------------------------------------------
// Fix 2: the post-login `next` must refuse backslash targets.
// ---------------------------------------------------------------------------

// TestLoginNextRejectsBackslashTargets drives the login endpoint and inspects the
// cookie the callback will later honour. Browsers normalise "\" to "/", so the old
// prefix checks waved `/\evil.com` through and the login redirected off-origin.
func TestLoginNextRejectsBackslashTargets(t *testing.T) {
	h := newHarness(t) // no IdP: the login answers 503, but the cookies are already set

	cases := []struct{ next, want string }{
		{`/\evil.com`, "/"},     // the bypass itself
		{`/\\evil.com`, "/"},    // double backslash, same destination
		{`//evil.com`, "/"},     // protocol-relative
		{`/ok\../../evil`, "/"}, // a backslash anywhere disqualifies
		{"/ok/path", "/ok/path"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet,
			h.srv.URL+"/api/auth/login?next="+url.QueryEscape(tc.next), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		found := false
		for _, c := range resp.Cookies() {
			if c.Name == "__Host-wc_next" {
				got, found = c.Value, true
			}
		}
		resp.Body.Close()
		if !found {
			t.Fatalf("next=%q: the login must set the __Host-wc_next cookie", tc.next)
		}
		if got != tc.want {
			t.Errorf("next=%q stored %q, want %q", tc.next, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Fix 4: an explicitly unverified email confers nothing.
// ---------------------------------------------------------------------------

// TestUnverifiedEmailCannotSatisfyDomainAllowlist: the domain allow-list answers
// "this address belongs to the permitted tenant". An IdP that says email_verified
// false has declined to vouch for the address, so it must not satisfy the gate —
// the login is refused exactly as if no address had been sent.
func TestUnverifiedEmailCannotSatisfyDomainAllowlist(t *testing.T) {
	p := newStaticIdp(t, "newcloud-test")
	p.sub, p.username, p.email = "unverified-1", "sneaky", "sneaky@example.test"
	p.emailVerified = boolPtr(false)

	h := newStaticHarness(t, p, func(c *config.Config) {
		c.Auth.JIT = config.JITConfig{Mode: config.JITOpen, AllowedDomains: []string{"example.test"}}
	})

	if got := h.loginStatus(t); got != http.StatusForbidden {
		t.Fatalf("an unverified address must not satisfy the allow-list, got %d", got)
	}
	if n := len(h.meta.Users()); n != 0 {
		t.Fatalf("a refused login must not provision, got %d accounts", n)
	}
}

// TestUnverifiedEmailDoesNotBindVictimAccount: an attacker whose token asserts the
// victim's address with email_verified false must not reach the victim's account —
// the address confers nothing, so the login provisions a fresh subject-keyed
// identity with no email and the victim's row is untouched.
func TestUnverifiedEmailDoesNotBindVictimAccount(t *testing.T) {
	p := newStaticIdp(t, "newcloud-test")
	p.sub, p.username, p.email = "attacker-sub", "", "bob@x.test"
	p.emailVerified = boolPtr(false)

	h := newStaticHarness(t, p, nil)

	victim := h.meta.AddUser(store.User{Username: "victim", Email: "bob@x.test", Role: "user"})

	if got := h.loginStatus(t); got != http.StatusFound {
		t.Fatalf("without an allow-list the login proceeds on the subject alone, got %d", got)
	}

	// The victim's account is exactly as it was: no identity bound, address intact.
	var after store.User
	for _, u := range h.meta.Users() {
		if u.ID == victim.ID {
			after = u
		}
	}
	if after.IdPSubject != "" || after.IdPIssuer != "" {
		t.Fatalf("the victim's account must stay unclaimed, got subject %q", after.IdPSubject)
	}
	if after.Email != "bob@x.test" {
		t.Fatalf("the victim's address must be untouched, got %q", after.Email)
	}
	// The attacker got their own, subject-keyed account with NO email.
	var attacker store.User
	for _, u := range h.meta.Users() {
		if u.IdPSubject == "attacker-sub" {
			attacker = u
		}
	}
	if attacker.ID.String() == "" {
		t.Fatal("the attacker's login should have provisioned a subject-based identity")
	}
	if attacker.Email != "" {
		t.Fatalf("an unverified address must not be stored, got %q", attacker.Email)
	}
	if attacker.ID == victim.ID {
		t.Fatal("the attacker must not land on the victim's account")
	}
}

// ---------------------------------------------------------------------------
// Fix 5: a pre-provisioned account is claimable only with its own verified address.
// ---------------------------------------------------------------------------

// TestPreProvisionedAccountIsNotClaimedByAUsernameCollision: an administrator creates
// "bob" with an address; an IdP identity whose preferred_username is also "bob" but
// whose (different, verified) address does not match must be refused without binding.
func TestPreProvisionedAccountIsNotClaimedByAUsernameCollision(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "attacker-sub", Username: "bob", Email: "mallory@example.test",
		Groups: []string{"cloud-users"},
	})
	// fakeidp asserts email_verified: true, so the ONLY thing standing between the
	// attacker and this account is the address match.
	victim := h.meta.AddUser(store.User{Username: "bob", Email: "bob@x.test", Role: "user"})

	if got := h.loginStatus(t); got != http.StatusForbidden {
		t.Fatalf("a username-only collision must be refused, got %d", got)
	}

	// The refusal is on the audit trail as an identity conflict, like its
	// subject-mismatch sibling.
	var recorded bool
	for _, e := range h.meta.AuditLog() {
		if e.Action == "auth.login.failure" {
			if reason, ok := e.Metadata["reason"].(string); ok && reason == "identity_conflict" {
				recorded = true
			}
		}
	}
	if !recorded {
		t.Fatal("the refused claim must write the login-failure audit row")
	}

	// And the account is untouched.
	var after store.User
	for _, u := range h.meta.Users() {
		if u.ID == victim.ID {
			after = u
		}
	}
	if after.IdPSubject != "" {
		t.Fatalf("the pre-provisioned account must not be bound, got subject %q", after.IdPSubject)
	}
	if n := len(h.meta.Users()); n != 1 {
		t.Fatalf("a refused login must not create an account, got %d", n)
	}
}

// TestPreProvisionedAccountIsClaimedByMatchingVerifiedEmail keeps the legitimate half
// of the claim path working: same username, and the IdP asserts the account's own
// address (verified, as fakeidp always does).
func TestPreProvisionedAccountIsClaimedByMatchingVerifiedEmail(t *testing.T) {
	h := newOIDCHarness(t, fakeidp.User{
		Subject: "real-sub", Username: "bob", Email: "bob@example.test",
		Groups: []string{"cloud-users"},
	})
	victim := h.meta.AddUser(store.User{Username: "bob", Email: "bob@example.test", Role: "user"})

	if got := h.loginStatus(t); got != http.StatusFound {
		t.Fatalf("the account's own verified address must claim it, got %d", got)
	}
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("the claimed account must get a session, got %d", code)
	}
	users := h.meta.Users()
	if len(users) != 1 {
		t.Fatalf("claiming must not create a second account, got %d", len(users))
	}
	if users[0].ID != victim.ID || users[0].IdPSubject != "real-sub" {
		t.Fatalf("the identity must be bound to the pre-provisioned row, got %+v", users[0])
	}
}

// ---------------------------------------------------------------------------
// Fix 6: the break-glass credential has a brute-force cap.
// ---------------------------------------------------------------------------

// TestBreakglassLoginIsThrottledAfterRepeatedFailures: five wrong passwords close the
// window, and the sixth attempt is refused WITHOUT running the KDF — even when it
// carries the correct password. The cap is what stops the one credential that bypasses
// the IdP from being guessed online.
func TestBreakglassLoginIsThrottledAfterRepeatedFailures(t *testing.T) {
	server.ResetBreakglassThrottleForTest()
	t.Cleanup(server.ResetBreakglassThrottleForTest)

	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	for i := 0; i < store.PasswordMaxAttempts; i++ {
		code, _ := h.loginBreakglass(t, "breakglass-admin", fmt.Sprintf("wrong-%d", i))
		if code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d must answer 401, got %d", i+1, code)
		}
	}

	code, body := h.loginBreakglass(t, "breakglass-admin", "correct horse battery")
	if code != http.StatusTooManyRequests {
		t.Fatalf("the attempt after the allowance is spent must answer 429 even with the "+
			"correct password, got %d (%v)", code, body)
	}

	// The throttled attempt is visible as a refusal, like every other one.
	var throttled bool
	for _, e := range h.meta.AuditLog() {
		if e.Action == "auth.breakglass.denied" {
			if reason, ok := e.Metadata["reason"].(string); ok && reason == "throttled" {
				throttled = true
			}
		}
	}
	if !throttled {
		t.Fatal("a throttled break-glass attempt must be audited with reason 'throttled'")
	}
}

// ---------------------------------------------------------------------------
// Fix 7: the break-glass row actually carries is_breakglass.
// ---------------------------------------------------------------------------

// TestBreakglassRowCarriesTheBreakglassFlag: the revalidation exemption reads
// users.is_breakglass, and nothing used to write it — the exemption was dead code and
// the recovery account aged out of its own session like an IdP-backed one. The login
// must stamp the row, and keep it stamped across a second login (the refresh path).
func TestBreakglassRowCarriesTheBreakglassFlag(t *testing.T) {
	server.ResetBreakglassThrottleForTest()
	t.Cleanup(server.ResetBreakglassThrottleForTest)

	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	if code, body := h.loginBreakglass(t, "breakglass-admin", "correct horse battery"); code != http.StatusOK {
		t.Fatalf("first break-glass login: %d (%v)", code, body)
	}

	flagOn := func(t *testing.T) {
		t.Helper()
		// One snapshot: Users() returns a fresh copy per call, so indexing it three
		// times in a loop reads three different slices and a pointer into one of them
		// says nothing about the others.
		users := h.meta.Users()
		var found *store.User
		for i := range users {
			if users[i].Username == "breakglass" {
				found = &users[i]
			}
		}
		if found == nil {
			t.Fatal("the break-glass account must exist as a user row")
		}
		if !found.IsBreakglass {
			t.Fatalf("the break-glass row must carry is_breakglass; the revalidation " +
				"exemption reads exactly this column and never fired without it")
		}
	}
	flagOn(t)

	if code, _ := h.loginBreakglass(t, "breakglass-admin", "correct horse battery"); code != http.StatusOK {
		t.Fatalf("second break-glass login: %d", code)
	}
	flagOn(t)
}

// TestBreakglassThrottleSuccessResetsAndProceeds pins the reset-on-success half over
// HTTP: failures within the allowance do not stop a correct password, and a success
// clears the count so the next batch of mistypes is answered as ordinary 401s again.
func TestBreakglassThrottleSuccessResetsAndProceeds(t *testing.T) {
	server.ResetBreakglassThrottleForTest()
	t.Cleanup(server.ResetBreakglassThrottleForTest)

	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	for i := 0; i < store.PasswordMaxAttempts-1; i++ {
		if code, _ := h.loginBreakglass(t, "breakglass-admin", "nope"); code != http.StatusUnauthorized {
			t.Fatalf("setup failure %d: %d", i, code)
		}
	}
	if code, body := h.loginBreakglass(t, "breakglass-admin", "correct horse battery"); code != http.StatusOK {
		t.Fatalf("a correct password within the allowance must succeed, got %d (%v)", code, body)
	}
	// Success resets the counter: the same address can fail a few more times without
	// being throttled.
	for i := 0; i < store.PasswordMaxAttempts-1; i++ {
		if code, _ := h.loginBreakglass(t, "breakglass-admin", "nope-again"); code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d must be a plain 401, got %d", i, code)
		}
	}
}
