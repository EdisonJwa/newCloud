package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// Cookies.
//
// The __Host- prefix is not cosmetic: the browser refuses to set such a cookie
// unless it is Secure, has no Domain attribute and uses Path=/. That rules out a
// sibling-subdomain attack, where a compromised other host sets a cookie this
// origin would then send.
const (
	sessionCookie = "__Host-wc_session"
	stateCookie   = "__Host-wc_state"
	pkceCookie    = "__Host-wc_pkce"
	nonceCookie   = "__Host-wc_nonce"
)

type ctxKey int

const actorKey ctxKey = iota

// actorFrom returns the authenticated actor stored by the middleware.
func actorFrom(ctx context.Context) (authz.Actor, bool) {
	a, ok := ctx.Value(actorKey).(authz.Actor)
	return a, ok
}

// requireSession rejects requests without a valid session cookie.
//
// The actor is resolved once and put in the request context; handlers never touch
// the cookie or the store themselves, so there is exactly one place that decides
// who the caller is.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "session required")
			return
		}

		claims, err := s.deps.Sessions.Verify(r.Context(), c.Value)
		if err != nil {
			// Clear the dead cookie so the browser stops sending it and the user is
			// not stuck in a redirect loop.
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "session expired")
			return
		}

		user, err := s.userByID(r.Context(), claims.Subject)
		if err != nil {
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "account unavailable")
			return
		}

		// Status is checked on every request, not only at login.
		//
		// Revocation on disable closes the common case, but it is not sufficient on its
		// own: a session minted a millisecond before the disable commits, or a replica
		// reading a snapshot taken before it, would survive. Checking here makes "disabled"
		// true regardless of how the credential was obtained - the same reasoning that made
		// the account lookup happen per request rather than being trusted from the token.
		if !user.IsActive() {
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "account is not active")
			return
		}

		role := authz.RoleGuest
		switch user.Role {
		case "admin":
			role = authz.RoleAdmin
		case "user":
			role = authz.RoleUser
		}

		actor := authz.NewUserActor(user.ID, user.Email, role, authz.ScopeRead|authz.ScopeWrite).
			WithUsername(user.Username)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey, actor)))
	})
}

// userByID loads an account from the session's subject.
func (s *Server) userByID(ctx context.Context, subject string) (store.User, error) {
	var user store.User
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UserByID(ctx, parseUUID(subject))
		user = u
		return err
	})
	return user, err
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// hostMatches reports whether r arrived on the configured public host.
//
// It guards the OIDC callback: a redirect URI pointing at an attacker-controlled
// host would let them receive the authorization code.
//
// Comparison is on hostname plus port, treating a missing port as the scheme
// default. Comparing raw Host strings would reject a valid deployment where the
// proxy terminates on 443 but forwards "Host: cloud.example.com:443".
func (s *Server) hostMatches(r *http.Request) bool {
	pub, err := url.Parse(s.deps.Config.Server.PublicURL)
	if err != nil || pub.Host == "" {
		return false
	}
	return strings.EqualFold(canonicalHostPort(r.Host, pub.Scheme),
		canonicalHostPort(pub.Host, pub.Scheme))
}

// canonicalHostPort normalises "host", "host:port" and an IPv6 literal to a
// comparable "host:port" form.
func canonicalHostPort(hostport, scheme string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port present.
		host = strings.Trim(hostport, "[]")
		port = ""
	}
	if port == "" {
		switch strings.ToLower(scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return strings.ToLower(host) + ":" + port
}

var errUnauthenticated = errors.New("unauthenticated")
