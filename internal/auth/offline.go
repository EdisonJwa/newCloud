package auth

// IdP outage handling (ID-07).
//
// The requirement asks that "IdP 离线时的登录、新建会话、已有会话续期和撤销行为有明确规则" — the
// behaviour of login, new sessions, existing-session renewal and revocation while the IdP is
// unreachable must be defined, not incidental.
//
// The behaviour before this file was accidental and bad: `NewProvider` ran OIDC discovery at
// startup and returned an error on failure, so an IdP outage meant the process refused to
// start. That takes down WebDAV, app-password authentication, already-issued browser sessions
// and every file operation — none of which touch the IdP. A single-sign-on dependency was
// silently promoted to an uptime dependency for the entire platform.
//
// The rules now, decided deliberately:
//
//  1. Startup does not require the IdP. The server starts, and says so loudly.
//  2. Existing sessions keep working. A session is our own signed credential; verifying it
//     never involves the IdP, and it expires on its own schedule. An IdP outage is not a
//     reason to log everyone out.
//  3. Existing-session *renewal* is unaffected for the same reason: "renewal" here means
//     sliding the expiry of a session we already issued, not re-validating with the IdP.
//  4. New interactive logins fail, cleanly, with a message saying the identity provider is
//     unreachable rather than a generic error.
//  5. Revocation is unaffected: whether a session is live is a database question
//     (`sessions.revoked_at`), so an administrator can still revoke during an outage. What
//     cannot happen during an outage is *role reconciliation* from IdP claims, so a role
//     change made at the IdP takes effect at the next successful login — which is already the
//     documented behaviour (ID-06).
//  6. App passwords keep working. They are locally verified hashes and never contact the IdP.
//
// The one thing that genuinely cannot work offline is a first-time login, and no amount of
// engineering changes that: the IdP holds the credential.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ProviderHolder holds the OIDC provider, which may be absent while the IdP is unreachable.
//
// It is deliberately not "a provider that retries internally": callers ask it whether a
// provider is available, so the error path is explicit at every call site rather than hidden
// behind a stub that fails later with a confusing message.
type ProviderHolder struct {
	mu sync.RWMutex
	p  *Provider

	// lastErr is the most recent discovery failure, so the readiness endpoint and the login
	// error can say what actually went wrong instead of "unavailable".
	lastErr error
	// lastAttempt is when discovery was last tried, to rate-limit retries.
	lastAttempt time.Time
}

// NewProviderHolder returns an empty holder.
func NewProviderHolder() *ProviderHolder { return &ProviderHolder{} }

// Set stores a successfully discovered provider.
func (h *ProviderHolder) Set(p *Provider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p = p
	h.lastErr = nil
	h.lastAttempt = time.Now()
}

// Get returns the provider, or false when the IdP has not been reached yet.
func (h *ProviderHolder) Get() (*Provider, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.p == nil {
		return nil, false
	}
	return h.p, true
}

// LastError returns the most recent discovery failure, if any.
func (h *ProviderHolder) LastError() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastErr
}

// ShouldRetry reports whether discovery should be attempted again now.
//
// Rate limiting matters: a login page hit in a loop during an outage would otherwise generate
// one discovery request per page load against an already-struggling IdP.
func (h *ProviderHolder) ShouldRetry(interval time.Duration) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.p != nil {
		return false
	}
	return time.Since(h.lastAttempt) >= interval
}

// Resolve returns the provider, attempting discovery if it is missing and the retry interval
// has elapsed.
//
// This is what the login handler calls: it converts "the IdP was down at startup but is back"
// into a working login without requiring a restart, which matters because an IdP restart is a
// routine event and a full application restart to recover from it is not.
func (h *ProviderHolder) Resolve(ctx context.Context, cfg interface{ IssuerURL() string },
	discover func(context.Context) (*Provider, error), interval time.Duration) (*Provider, error) {

	if p, ok := h.Get(); ok {
		return p, nil
	}

	h.mu.Lock()
	// Re-check under the write lock: two concurrent logins both arriving after the interval
	// would otherwise both run discovery.
	if h.p != nil {
		p := h.p
		h.mu.Unlock()
		return p, nil
	}
	tooSoon := time.Since(h.lastAttempt) < interval
	prevErr := h.lastErr
	if tooSoon {
		h.mu.Unlock()
		if prevErr != nil {
			return nil, fmt.Errorf("auth: identity provider unreachable (last attempt: %v): %w",
				time.Since(h.lastAttempt).Round(time.Second), prevErr)
		}
		return nil, fmt.Errorf("auth: identity provider not reachable yet")
	}
	h.lastAttempt = time.Now()
	h.mu.Unlock()

	p, err := discover(ctx)
	if err != nil {
		h.mu.Lock()
		h.lastErr = err
		h.lastAttempt = time.Now()
		h.mu.Unlock()
		return nil, fmt.Errorf("auth: identity provider unreachable: %w", err)
	}

	h.Set(p)
	return p, nil
}

// ErrIdPUnavailable is returned by the login path when no provider could be reached.
//
// It exists so the HTTP layer can map it to a specific status and message rather than
// reporting a generic internal error for a condition the user can understand and retry.
var ErrIdPUnavailable = fmt.Errorf("auth: identity provider is unavailable")
