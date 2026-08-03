// Package fakeidp is a minimal OpenID Connect provider for end-to-end tests.
//
// It exists because the real IdP is Pocket ID, which needs passkeys and a browser.
// Without a stand-in, the login path — the one place where a mistake turns into an
// authentication bypass — would be the only part of the system never executed
// end to end.
//
// It implements just enough of the protocol: discovery, a JWKS, the authorization
// endpoint with PKCE verification, and token exchange. It is not a general-purpose
// IdP and must never run outside tests.
package fakeidp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// User is the identity the fake IdP will assert.
type User struct {
	Subject  string
	Username string
	Email    string
	Name     string
	Groups   []string
}

// Provider is a running fake IdP.
type Provider struct {
	Server   *httptest.Server
	ClientID string

	mu sync.Mutex
	// key is the key currently used to sign ID tokens.
	key *rsa.PrivateKey
	// kid names key in the JWKS. It changes when the key rotates, so a verifier that caches
	// keys by kid — which every OIDC client does — has to notice the new one.
	kid string
	// retired holds keys that are published in the JWKS but no longer used for signing.
	//
	// This is what a real IdP does during rotation: the new key is published first, tokens are
	// signed with it, and the old key stays published until every token signed with it has
	// expired. Dropping the old key immediately would invalidate tokens that are still valid,
	// which is a self-inflicted outage.
	retired []retiredKey
	codes   map[string]issuedCode
	tokens  map[string]issuedToken
	user    User
	// FailNextCode lets a test simulate a user cancelling at the IdP.
	FailNextCode bool
	// TamperNonce makes the next ID token carry a nonce of the IdP's choosing rather
	// than the one the client asked for.
	//
	// It models the attack the nonce check exists to stop: a token that is valid in
	// every other respect — right signature, issuer, audience, subject and expiry —
	// but was minted for a different authorization request. Without this hook the
	// check can only be tested by asserting the value is *sent*, which stays green
	// even if the comparison is deleted.
	TamperNonce bool
	// issuer overrides the advertised issuer. Empty means "derive from the listener",
	// which is right for httptest and wrong as soon as the listener binds 0.0.0.0.
	issuer string
}

type issuedCode struct {
	user            User
	codeChallenge   string
	challengeMethod string
	redirectURI     string
	codeVerifier    string
	// nonce is echoed into the ID token, exactly as a real IdP does. Without it the
	// client cannot perform the ID-02 check, so a development server that dropped it
	// would make the flow permanently untestable.
	nonce string
}

type issuedToken struct {
	user User
}

// retiredKey is a signing key that is still published but no longer used.
type retiredKey struct {
	kid string
	key *rsa.PrivateKey
}

// New starts a fake IdP on an ephemeral port.
func New(clientID string) (*Provider, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		ClientID: clientID,
		key:      key,
		kid:      "test-key",
		codes:    map[string]issuedCode{},
		tokens:   map[string]issuedToken{},
		user: User{
			Subject: "idp-subject-1", Username: "alice",
			Email: "alice@example.test", Name: "Alice Example",
			Groups: []string{"cloud-users"},
		},
	}
	p.Server = httptest.NewServer(http.HandlerFunc(p.handle))
	return p, nil
}

// NewAt starts a fake IdP listening on a fixed address, for manual end-to-end testing
// against a real `newcloud serve` process.
//
// issuer is advertised in discovery and in the ID token's iss claim. It must be given
// explicitly when the listener binds 0.0.0.0: deriving it from the listener would
// advertise http://[::]:port, which no client can verify against.
func NewAt(clientID, addr, issuer string) (*Provider, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		ClientID: clientID,
		key:      key,
		codes:    map[string]issuedCode{},
		tokens:   map[string]issuedToken{},
		user: User{
			Subject: "idp-subject-1", Username: "alice",
			Email: "alice@example.test", Name: "Alice Example",
			Groups: []string{"cloud-users"},
		},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p.Server = &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: http.HandlerFunc(p.handle)},
	}
	p.Server.Start()
	if issuer != "" {
		p.issuer = issuer
	}
	return p, nil
}

// Close stops the server.
func (p *Provider) Close() { p.Server.Close() }

// URL is the issuer URL.
func (p *Provider) URL() string {
	if p.issuer != "" {
		return p.issuer
	}
	return p.Server.URL
}

// SetUser changes the identity asserted for subsequent logins.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.user = u
}

// IssueCode mints an authorization code directly, for tests that need a valid code
// without driving a browser through the authorize endpoint.
func (p *Provider) IssueCode(verifier string) string {
	code := randomToken()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes[code] = issuedCode{
		user:            p.user,
		codeChallenge:   pkceChallenge(verifier),
		challengeMethod: "S256",
		codeVerifier:    verifier,
	}
	return code
}

func (p *Provider) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/openid-configuration":
		p.discovery(w, r)
	case r.URL.Path == "/keys":
		p.jwks(w, r)
	case r.URL.Path == "/authorize":
		p.authorize(w, r)
	case r.URL.Path == "/token":
		p.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.URL(),
		"authorization_endpoint":                p.URL() + "/authorize",
		"token_endpoint":                        p.URL() + "/token",
		"jwks_uri":                              p.URL() + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "groups"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	current := p.key
	currentKID := p.kid
	retired := append([]retiredKey(nil), p.retired...)
	p.mu.Unlock()

	entry := func(kid string, k *rsa.PrivateKey) map[string]any {
		pub := k.Public().(*rsa.PublicKey)
		return map[string]any{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": kid,
			"n":   b64(pub.N.Bytes()),
			"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
		}
	}

	// The retired keys are published alongside the current one, exactly as a real IdP does while
	// rotation is in flight: a token signed seconds before the switch must still verify.
	keys := []map[string]any{entry(currentKID, current)}
	for _, rk := range retired {
		keys = append(keys, entry(rk.kid, rk.key))
	}
	writeJSON(w, map[string]any{"keys": keys})
}

// RotateKey switches to a fresh signing key and publishes it, keeping the previous key in the
// JWKS so tokens signed just before the switch keep verifying.
//
// ID-03 requires that key rotation work. Without a way to rotate, that requirement cannot be
// tested at all — the fake IdP's single fixed key made "rotation is supported" an unverified
// claim, since a client that aggressively cached the first JWKS it saw would pass every test.
func (p *Provider) RotateKey() error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.retired = append(p.retired, retiredKey{kid: p.kid, key: p.key})
	p.key = key
	p.kid = fmt.Sprintf("test-key-%d", len(p.retired)+1)
	return nil
}

// RetireOldKeys drops every previous key from the JWKS, modelling the end of a rotation window.
//
// After this a token signed with a retired key must fail verification. That is the property the
// rotation window exists to bound, and it is worth being able to test: a client that never
// notices new keys would otherwise look correct forever.
func (p *Provider) RetireOldKeys() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retired = nil
}

// authorize immediately redirects back, asserting the configured user.
//
// A real IdP shows a passkey prompt; the protocol contract from this process's
// point of view is identical.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	p.mu.Lock()
	fail := p.FailNextCode
	p.FailNextCode = false
	user := p.user
	p.mu.Unlock()

	tamper := p.TamperNonce
	if tamper {
		p.TamperNonce = false
	}

	if fail {
		// Simulate the user pressing "cancel" at the IdP.
		redirect(w, r, q.Get("redirect_uri"), map[string]string{
			"error": "access_denied", "state": q.Get("state"),
		})
		return
	}

	if q.Get("code_challenge") == "" {
		http.Error(w, "code_challenge required (PKCE is mandatory)", http.StatusBadRequest)
		return
	}

	code := randomToken()
	p.mu.Lock()
	p.codes[code] = issuedCode{
		user:            user,
		codeChallenge:   q.Get("code_challenge"),
		challengeMethod: q.Get("code_challenge_method"),
		redirectURI:     q.Get("redirect_uri"),
		nonce:           tamperedNonce(q.Get("nonce"), tamper),
	}
	p.mu.Unlock()

	redirect(w, r, q.Get("redirect_uri"), map[string]string{
		"code": code, "state": q.Get("state"),
	})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	code := r.FormValue("code")
	verifier := r.FormValue("code_verifier")

	p.mu.Lock()
	issued, ok := p.codes[code]
	if ok {
		delete(p.codes, code) // single use
	}
	p.mu.Unlock()

	if !ok {
		writeOAuthError(w, "invalid_grant", "unknown or already used code")
		return
	}

	// PKCE verification. Without this the whole point of the flow is lost, so it is
	// enforced rather than assumed.
	if issued.codeChallenge != "" {
		if verifier == "" {
			writeOAuthError(w, "invalid_grant", "code_verifier required")
			return
		}
		if pkceChallenge(verifier) != issued.codeChallenge {
			writeOAuthError(w, "invalid_grant", "code_verifier does not match challenge")
			return
		}
	}

	idToken, err := p.signIDToken(issued.user, issued.nonce)
	if err != nil {
		http.Error(w, "signing failed", http.StatusInternalServerError)
		return
	}

	access := randomToken()
	p.mu.Lock()
	p.tokens[access] = issuedToken{user: issued.user}
	p.mu.Unlock()

	writeJSON(w, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

// signIDToken produces an RS256 JWT with the claims the app reads.
func (p *Provider) signIDToken(u User, nonce string) (string, error) {
	now := time.Now()

	// Read the signing key and its kid under the lock: a rotation concurrent with a token mint
	// must not produce a token whose header names a kid different from the key that signed it.
	p.mu.Lock()
	signingKey := p.key
	signingKID := p.kid
	p.mu.Unlock()

	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": signingKID}
	payload := map[string]any{
		"iss":                p.URL(),
		"sub":                u.Subject,
		"aud":                p.ClientID,
		"exp":                now.Add(time.Hour).Unix(),
		"iat":                now.Unix(),
		"auth_time":          now.Unix(),
		"preferred_username": u.Username,
		"email":              u.Email,
		"email_verified":     true,
		"name":               u.Name,
		"groups":             u.Groups,
	}
	if nonce != "" {
		// Echoed only when the client asked for it, which is how a real IdP behaves:
		// a nonce claim on a request that carried none would be meaningless.
		payload["nonce"] = nonce
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)
	signing := b64(headerJSON) + "." + b64(payloadJSON)

	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, signingKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

func redirect(w http.ResponseWriter, r *http.Request, target string, params map[string]string) {
	u := target
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	parts := make([]string, 0, len(params))
	for k, v := range params {
		parts = append(parts, k+"="+urlEncode(v))
	}
	http.Redirect(w, r, u+sep+strings.Join(parts, "&"), http.StatusFound)
}

func urlEncode(s string) string {
	return strings.NewReplacer(
		"%", "%25", " ", "%20", "+", "%2B", "&", "%26", "=", "%3D",
		"?", "%3F", "#", "%23", "/", "%2F", ":", "%3A",
	).Replace(s)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": code, "error_description": desc,
	})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("fakeidp: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// tamperedNonce replaces the echoed nonce when a test asked for a replay.
func tamperedNonce(requested string, tamper bool) string {
	if !tamper || requested == "" {
		return requested
	}
	return "replayed-from-another-request"
}
