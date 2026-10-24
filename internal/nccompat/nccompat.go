// Package nccompat implements the part of Nextcloud's client protocol that the
// official desktop and mobile clients insist on before they will talk to a server.
//
// Those clients do not simply speak RFC 4918. They first probe /status.php for a
// version string, read capabilities from the OCS API, authenticate through "login flow
// v2" (a browser round trip that ends in an App Password), and then sync against
// /remote.php/dav/files/<user>/. A server that only offers /dav is invisible to them,
// which is what this package changes.
//
// It is a compatibility layer, not an emulation. Everything it advertises is either
// implemented here or by internal/dav; nothing is reported as available because
// Nextcloud would have it. ADR-0015 records what is deliberately absent — WebDAV
// chunking v2, REPORT filters, checksums at rest, sharing APIs — and why a client that
// asks for one of them degrades instead of failing.
package nccompat

import (
	"crypto/sha256"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/store"
)

const (
	// ProtocolVersion is the Nextcloud SERVER VERSION this layer reports in status.php
	// and in the capabilities API.
	//
	// It is a protocol version, not a claim about this software: every client checks it
	// against the oldest server it supports and refuses anything lower, so the number has
	// to be one those clients accept. The features actually offered are the ones listed in
	// capabilities — which is where a client is supposed to look.
	ProtocolVersion = "33.0.0"

	// versionFull is the four-part form status.php reports as "version"; clients parse it
	// as integers, so it cannot be the same string as versionString.
	versionFull = ProtocolVersion + ".0"

	// productName is what this server calls itself. Reported verbatim; clients show it in
	// "About" and do not compare it with "Nextcloud".
	productName = "newCloud"

	// loginFlowTTL is how long a started flow may take. Nextcloud uses twenty minutes,
	// and the desktop client polls with no other timeout of its own, so matching it keeps
	// a slow browser login working.
	loginFlowTTL = 20 * time.Minute

	// maxClientName bounds what a client can put in its own label, which ends up in the
	// App Password list the account sees.
	maxClientName = 100
)

// Deps are the collaborators the compatibility layer needs.
type Deps struct {
	// PublicURL is the external base URL without a trailing slash. Every absolute URL
	// this layer hands out (the login page, the poll endpoint, the "server" field of the
	// credentials reply) is built from it.
	PublicURL string
	Meta      store.Store
	AppPass   *auth.AppPasswords
	// SessionUser resolves the browser session on a request. It is supplied by the
	// server package because the session cookie, its revocation check and its IdP
	// revalidation window all live there; duplicating that logic here would be a second
	// answer to "is this browser signed in", which is how the two drift apart.
	SessionUser func(r *http.Request) (store.User, error)
	// Stale reports whether an account's credentials have aged past the IdP
	// revalidation window. Nil disables the check.
	Stale func(store.User) bool
	Log   *slog.Logger
	// Now overrides the clock in tests. Nil means time.Now.
	Now func() time.Time
}

// Handler serves the compatibility endpoints.
type Handler struct {
	deps Deps
	now  func() time.Time
	log  *slog.Logger
}

// New builds the handler.
func New(d Deps) *Handler {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	d.PublicURL = strings.TrimSuffix(d.PublicURL, "/")
	h := &Handler{deps: d, now: d.Now, log: d.Log}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// ErrUnauthenticated is what SessionUser returns when there is no usable browser
// session. It is not exported for its message: the flow page answers it with a redirect
// to the IdP, never with the error.
var ErrUnauthenticated = errors.New("nccompat: no browser session")

// hasOCSRequestHeader reports whether the client marked the request as an OCS call.
//
// Nextcloud requires this header on every OCS request and answers 412 without it,
// because OCS routes are also reachable with a browser session and the header is what
// proves the caller is an API client rather than a cross-site form. Matching that
// behaviour is not pedantry: a client library built against Nextcloud sends it, and a
// silent acceptance would hide a genuinely broken integration.
func hasOCSRequestHeader(r *http.Request) bool {
	v := strings.TrimSpace(r.Header.Get("OCS-APIRequest"))
	return strings.EqualFold(v, "true")
}

// clientIP is the audit address. The server's middleware has already decided what
// RemoteAddr means (proxy trust, X-Forwarded-For), so this must not re-parse headers.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// randomToken returns 32 bytes of crypto/rand as an unpadded base64url string.
func randomToken() (string, error) {
	return auth.RandomState()
}

// tokenHash is the stored form of a bearer token.
func tokenHash(tok string) [32]byte { return sha256.Sum256([]byte(tok)) }

// shortClientName turns a User-Agent into a label a person can recognise in their App
// Password list, without letting a client write an unbounded string into the database.
func shortClientName(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "Nextcloud client"
	}
	// The parenthesised platform details are noise in a label; keep the product part.
	if i := strings.IndexByte(ua, '('); i > 0 {
		ua = strings.TrimSpace(ua[:i])
	}
	if len(ua) > maxClientName {
		ua = ua[:maxClientName]
	}
	if ua == "" {
		return "Nextcloud client"
	}
	return ua
}
