package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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
// requireSession authenticates a browser request.
//
// INV-04: the session row is checked on every request, not only at login, so a revoked session or a
// disabled account takes effect on the next call rather than when a token would have expired.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.sessionUser(r)
		if err != nil {
			// A cookie that is present but unusable is cleared so the browser stops
			// sending it and the user is not stuck in a redirect loop. A request with no
			// cookie at all is a plain 401 and gets no Set-Cookie.
			if !errors.Is(err, errNoSession) {
				s.clearSessionCookie(w)
			}
			writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
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

		// §10.1 auth_method: a request carrying a session cookie authenticated as a session, as
		// opposed to a WebDAV app password. The distinction is the first thing an investigation
		// reaches for, because revoking one is surgical and revoking the other is not.
		ctx := store.WithAuthMethod(r.Context(), store.AuthSession)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, actorKey, actor)))
	})
}

// sessionUser resolves the browser session on r, or returns one of the errors below.
//
// One resolver serves both this middleware and the Nextcloud login-flow consent page. Two
// copies of "is this browser signed in" is how a page ends up trusting a session the API
// has already refused: the consent page mints an App Password, so it has to apply the same
// revocation and IdP-revalidation checks, not a weaker subset of them.
func (s *Server) sessionUser(r *http.Request) (store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.User{}, errNoSession
	}

	claims, err := s.deps.Sessions.Verify(r.Context(), c.Value)
	if err != nil {
		return store.User{}, errSessionExpired
	}

	user, err := s.userByID(r.Context(), claims.Subject)
	if err != nil {
		return store.User{}, errAccountUnavailable
	}

	// Status is checked on every request, not only at login.
	//
	// Revocation on disable closes the common case, but it is not sufficient on its
	// own: a session minted a millisecond before the disable commits, or a replica
	// reading a snapshot taken before it, would survive. Checking here makes "disabled"
	// true regardless of how the credential was obtained - the same reasoning that made
	// the account lookup happen per request rather than being trusted from the token.
	if !user.IsActive() {
		return store.User{}, errAccountInactive
	}

	// ID-06 / WC-GAP-001: refuse a credential whose IdP confirmation has aged out.
	//
	// The role stored in this row was written by the last successful login. If the IdP has since
	// removed this person from a group — or disabled them outright — nothing in this process knows
	// yet, and the session keeps exercising the old role. "Effective at the next login" is not a
	// bound when the person may never log in again.
	//
	// Refusing here converts that into a documented window: past auth.revalidate_minutes, the
	// only way back in is an interactive sign-in, which re-reads the claims. It is deliberately
	// not a silent role change — the request fails with the same 401 an expired session gets, so
	// the client re-authenticates rather than continuing with privileges nobody has confirmed.
	if s.credentialIsStale(user) {
		return store.User{}, errSignInStale
	}

	return user, nil
}

// Session failures. The messages are the API's, and they are distinct because they point at
// different remedies: sign in, wait for the account to be enabled, or re-authenticate.
var (
	errNoSession          = errors.New("session required")
	errSessionExpired     = errors.New("session expired")
	errAccountUnavailable = errors.New("account unavailable")
	errAccountInactive    = errors.New("account is not active")
	errSignInStale        = errors.New("sign-in must be renewed")
)

// credentialIsStale reports whether this account's IdP confirmation has aged past the window
// (ID-06, WC-GAP-001).
//
// The cases where it is always false are as important as the case where it is true:
//
//   - revalidate_minutes is 0, which is the previous behaviour and is announced at startup.
//   - The account is break-glass. It has no IdP to be confirmed against, and expiring it would lock
//     the only recovery path out of the system behind an identity provider that may be the thing
//     that is broken.
//   - The account has no IdP identity at all (created by an administrator through the management
//     API). Its role comes from an operator, not from a claim, so there is nothing to re-derive.
//
// A nil IdPConfirmedAt means "never confirmed" and is stale. That is the migration case: the column
// arrives empty on a deployment that upgrades, and the honest reading of an empty field is "we do not
// know", not "confirmed just now". The consequence is that upgrading with a window configured forces
// one round of interactive logins — which is the price of the bound actually applying.
func (s *Server) credentialIsStale(user store.User) bool {
	window := s.deps.Config.Auth.RevalidateWindow()
	if window <= 0 {
		return false
	}
	if user.IsBreakglass {
		return false
	}
	if user.IdPIssuer == "" || user.IdPSubject == "" {
		return false
	}
	if user.IdPConfirmedAt == nil {
		return true
	}
	return time.Since(*user.IdPConfirmedAt) > window
}

// CredentialIsStaleForTest exposes the revalidation predicate.
//
// Exported because the exemptions (break-glass, no IdP identity, window disabled) are the part most
// likely to be got wrong, and they are cheapest to pin directly rather than by constructing a
// break-glass login for each case.
func (s *Server) CredentialIsStaleForTest(user store.User) bool {
	return s.credentialIsStale(user)
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
