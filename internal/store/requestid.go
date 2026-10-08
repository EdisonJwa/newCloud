package store

// Request correlation for audit events (§10.1 request_id).
//
// The id travels in the context rather than as a field on every call: there are around thirty call
// sites, most of them inside transactions in handlers, and threading a parameter through all of them
// would be churn that the next call site would forget. The server puts the id in the context once per
// request; the audit writer reads it.
//
// An explicit AuditEntry.RequestID wins over the context, so a background job that has its own notion
// of a run can record that instead.

import "context"

type requestIDKey struct{}

// WithRequestID attaches a request id to the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext returns the request id carried by ctx, or "" when there is none.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type authMethodKey struct{}

// Authentication methods recorded on audit events (§10.1 auth_method).
const (
	// AuthSession is a browser session cookie.
	AuthSession = "session"
	// AuthAppPassword is a WebDAV app password. Distinguishing it from a session matters because
	// the remedy differs: an app password can be revoked on its own.
	AuthAppPassword = "app_password"
	// AuthBreakglass is the local administrator credential.
	AuthBreakglass = "breakglass"
	// AuthOIDC is the identity provider's flow, recorded on the login events themselves.
	AuthOIDC = "oidc"
	// AuthShareLink is a public share token, with no account behind it.
	AuthShareLink = "share_link"
)

// WithAuthMethod attaches the authentication method to the context.
func WithAuthMethod(ctx context.Context, method string) context.Context {
	if method == "" {
		return ctx
	}
	return context.WithValue(ctx, authMethodKey{}, method)
}

// AuthMethodFromContext returns the method carried by ctx, or "" when there is none.
func AuthMethodFromContext(ctx context.Context) string {
	m, _ := ctx.Value(authMethodKey{}).(string)
	return m
}
