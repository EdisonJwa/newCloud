// Package auth implements the two credential paths the system needs.
//
//   - Interactive login: OIDC authorization code + PKCE against Pocket ID,
//     ending in a server-side session cookie.
//   - Non-interactive clients: HTTP Basic with an app password, because no
//     desktop WebDAV client can complete an interactive redirect.
//
// The two are deliberately separate. Trying to make a WebDAV client do OIDC is
// how projects end up with a "token in the URL" scheme, and a token in a URL ends
// up in logs, browser history and Referer headers.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// Provider wraps the OIDC discovery result for Pocket ID.
type Provider struct {
	verifier  *oidc.IDTokenVerifier
	oauth     *oauth2.Config
	issuer    string
	groupRole map[string]config.Role
}

// NewProvider performs OIDC discovery against the configured issuer.
//
// Discovery is eager rather than lazy: a typo in the issuer URL should stop the
// process at startup, not appear as a failed login for the first user hours
// later.
func NewProvider(ctx context.Context, cfg config.Config) (*Provider, error) {
	ctx = oidc.ClientContext(ctx, http.DefaultClient)

	provider, err := oidc.NewProvider(ctx, cfg.Auth.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC discovery against %s: %w", cfg.Auth.Issuer, err)
	}

	oauthCfg := &oauth2.Config{
		ClientID:     cfg.Auth.ClientID,
		ClientSecret: cfg.Auth.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  strings.TrimSuffix(cfg.Server.PublicURL, "/") + "/api/auth/callback",
		// "groups" is required: Pocket ID only releases group membership when the
		// scope is requested, and roles are derived from group names.
		Scopes: cfg.Auth.Scopes,
	}

	return &Provider{
		verifier:  provider.Verifier(&oidc.Config{ClientID: cfg.Auth.ClientID}),
		oauth:     oauthCfg,
		issuer:    cfg.Auth.Issuer,
		groupRole: cfg.Auth.GroupRoleMap,
	}, nil
}

// Claims is what we take from the ID token.
//
// Only stable identifiers are trusted: sub never changes, so it is the account
// key. preferred_username and email can be edited in the IdP and are therefore
// display data, not identity.
type Claims struct {
	Subject   string
	Username  string
	Email     string
	Name      string
	Groups    []string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// AuthCodeURL returns the redirect target and sets the state and PKCE verifier
// in cookies.
//
// The cookies use the __Host- prefix so a sibling subdomain cannot set them. The
// PKCE verifier is what stops an intercepted code from being redeemed by another
// client; state stops CSRF on the callback.
// AuthCodeURL returns the redirect target with PKCE and a nonce.
//
// The nonce is the one OIDC check this flow was missing (ID-02). PKCE binds the code
// to this client and state binds the callback to this browser session; the nonce binds
// the *ID token* to this authorization request, which is the remaining replay window:
// without it, an ID token obtained from a different request to the same client would
// verify here, because signature, issuer, audience and expiry would all still hold.
//
// The token is echoed back in the ID token's nonce claim, so a verifier that compares
// them knows the token was minted for this request and not lifted from elsewhere.
func (p *Provider) AuthCodeURL(state, verifier, nonce string) string {
	opts := []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", pkceChallenge(verifier)),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}
	if nonce != "" {
		opts = append(opts, oauth2.SetAuthURLParam("nonce", nonce))
	}
	return p.oauth.AuthCodeURL(state, opts...)
}

// Exchange redeems an authorization code and verifies the ID token.
//
// expectedNonce is compared against the token's nonce claim. An empty expectation means
// the caller did not start a nonce-bearing flow; that is only reachable from tests and
// from a stale cookie, and is refused here rather than silently skipped.
func (p *Provider) Exchange(ctx context.Context, code, verifier, expectedNonce string) (*Claims, error) {
	ctx = oidc.ClientContext(ctx, http.DefaultClient)

	token, err := p.oauth.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		return nil, fmt.Errorf("auth: exchange code: %w", err)
	}

	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("auth: no id_token in the token response")
	}

	// Verifying the signature, issuer, audience and expiry is not optional: an
	// unverified ID token is just a JSON blob from the network.
	idToken, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("auth: verify id_token: %w", err)
	}

	// The nonce check lives here rather than with the other claims because go-oidc
	// does not enforce it: it verifies signature, issuer, audience and expiry, and
	// leaves the replay binding to the caller. Constant-time comparison is not needed
	// for a value the attacker must guess outright, but it is free.
	if expectedNonce == "" {
		return nil, errors.New("auth: no nonce was issued for this flow")
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(expectedNonce)) != 1 {
		return nil, errors.New("auth: id_token nonce does not match the authorization request")
	}

	var claims struct {
		PreferredUsername string   `json:"preferred_username"`
		Email             string   `json:"email"`
		EmailVerified     bool     `json:"email_verified"`
		Name              string   `json:"name"`
		Groups            []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("auth: decode claims: %w", err)
	}

	return &Claims{
		Subject:   idToken.Subject,
		Username:  claims.PreferredUsername,
		Email:     claims.Email,
		Name:      claims.Name,
		Groups:    claims.Groups,
		IssuedAt:  idToken.IssuedAt,
		ExpiresAt: idToken.Expiry,
	}, nil
}

// Role maps the user's IdP groups to a local role.
//
// Fail-closed: an unknown user lands on guest, never on user. A misconfigured
// group name in the IdP should under-privilege someone (annoying) rather than
// over-privilege them (a breach).
func (p *Provider) Role(groups []string) config.Role {
	best := config.RoleGuest
	for _, g := range groups {
		if role, ok := p.groupRole[g]; ok && rank(role) > rank(best) {
			best = role
		}
	}
	return best
}

func rank(r config.Role) int {
	switch r {
	case config.RoleAdmin:
		return 3
	case config.RoleUser:
		return 2
	case config.RoleGuest:
		return 1
	default:
		return 0
	}
}

// pkceChallenge computes the S256 challenge for a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomState returns a URL-safe random string for state or a PKCE verifier.
//
// 32 bytes: state only needs to be unguessable, but the same helper produces PKCE
// verifiers, where the spec requires 43-128 characters of entropy-backed input.
func RandomState() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Sessions issues and verifies browser sessions.
type Sessions struct {
	tickets *ticket.Issuer
	meta    store.Store
	ttl     time.Duration
}

// NewSessions wires session issuance.
func NewSessions(t *ticket.Issuer, meta store.Store, ttl time.Duration) *Sessions {
	return &Sessions{tickets: t, meta: meta, ttl: ttl}
}

// Mint creates a session token and records the jti so it can be revoked.
//
// The token itself is stateless (HMAC), but the jti row is what makes "log out
// everywhere" possible. A purely stateless session could only be revoked by
// rotating the signing key, which logs everyone out.
func (s *Sessions) Mint(ctx context.Context, userID uuid.UUID, email, ip, ua string) (string, error) {
	// The jti is a UUID because the sessions table keys on uuid: it is an
	// identifier, not a capability. The capability is the HMAC-signed ticket; the
	// jti only exists so a session can be revoked individually.
	jti := uuid.NewString()
	now := time.Now()
	exp := now.Add(s.ttl)

	tok, err := s.tickets.Mint(ticket.PurposeSession, ticket.Claims{
		Subject: userID.String(),
		ID:      jti,
		Issued:  now,
		Expiry:  exp,
		Extra:   map[string]string{"email": email},
	})
	if err != nil {
		return "", err
	}

	if err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.CreateSession(ctx, store.Session{
			ID: jti, UserID: userID, ExpiresAt: exp, IP: ip, UserAgent: ua,
			AuthMethod: "oidc",
		})
	}); err != nil {
		return "", err
	}
	return tok, nil
}

// ErrInvalidSession is returned for a token that is expired, malformed, or
// revoked. Callers must not distinguish these in the HTTP response.
var ErrInvalidSession = errors.New("auth: invalid session")

// Verify checks the token and that its jti is still live.
func (s *Sessions) Verify(ctx context.Context, tok string) (ticket.Claims, error) {
	claims, err := s.tickets.Verify(ticket.PurposeSession, tok)
	if err != nil {
		return ticket.Claims{}, ErrInvalidSession
	}

	live := false
	err = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		ok, err := tx.SessionLive(ctx, claims.ID)
		live = ok
		return err
	})
	if err != nil {
		return ticket.Claims{}, err
	}
	if !live {
		return ticket.Claims{}, ErrInvalidSession
	}
	return claims, nil
}

// Revoke kills a single session.
func (s *Sessions) Revoke(ctx context.Context, jti string) error {
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.RevokeSession(ctx, jti)
	})
}

// RevokeAll kills every session for a user, which is what "log out everywhere"
// and "the account was disabled" both need.
func (s *Sessions) RevokeAll(ctx context.Context, userID string) error {
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.RevokeUserSessions(ctx, userID)
	})
}

// constantTimeEqual is used for any secret comparison the crypto packages do not
// already perform in constant time.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
