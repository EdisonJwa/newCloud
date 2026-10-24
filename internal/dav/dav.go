// Package dav implements the WebDAV surface.
//
// Design notes that shaped this package:
//
//  1. It does not use golang.org/x/net/webdav's FileSystem abstraction. That
//     interface is string-path-only, has no caller identity and no transaction,
//     so it cannot express quota, versions, share permissions or durable locks.
//     Reusing the request handler with a custom FileSystem would force identity
//     into the name, which is the "path is authorization" bug this project
//     explicitly avoids.
//
//  2. PROPFIND is hand-written and streams. A directory with 100k entries must
//     not be materialised in memory to serialise an XML response.
//
//  3. Locks are durable (PostgreSQL), because the in-memory lock system in the
//     stdlib breaks under restart and under a second process — and a broken lock
//     is how Office co-authoring silently corrupts a file.
package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"log/slog"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/mount"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/swap"
)

// Handler serves WebDAV at a configured prefix.
type Handler struct {
	cfg     config.Config
	meta    store.Store
	content layout.ContentStore
	authz   *authz.Service
	appPass *auth.AppPasswords
	caps    layout.Caps
	now     func() time.Time
	log     *slog.Logger
	// trashLimit is the retained-usage cap for the trash (§4.3). Nil means unlimited, which is the
	// safe default: a WebDAV client deleting a file must not hit a limit the deployment never set.
	trashLimit func(context.Context) int64
	// revalidate reports whether an account's credentials have aged past the IdP revalidation
	// window (ID-06, WC-GAP-001). Nil disables the check.
	//
	// Injected rather than recomputed here so the WebDAV and session paths cannot disagree about
	// when an account is stale: two implementations of one policy is how one of them ends up
	// letting a demoted admin through.
	revalidate func(store.User) bool
	// mounts serves external storage (ADR-0014). Nil keeps the handler entirely local.
	mounts *mount.Service
}

// WithRevalidation supplies the staleness predicate used to expire App Passwords.
func (h *Handler) WithRevalidation(fn func(store.User) bool) *Handler {
	h.revalidate = fn
	return h
}

// WithTrashLimit supplies the retained-usage cap used by DELETE and by an overwrite that replaces
// an existing file.
func (h *Handler) WithTrashLimit(fn func(context.Context) int64) *Handler {
	h.trashLimit = fn
	return h
}

// trashCap resolves the configured cap for this request.
func (h *Handler) trashCap(ctx context.Context) int64 {
	if h.trashLimit == nil {
		return 0
	}
	return h.trashLimit(ctx)
}

// logf emits a diagnostic line.
//
// WebDAV failures are hard to reproduce from a client-side status code alone, and the
// alternative — reasoning about which of a dozen branches returned 404 — is how hours
// disappear. The logger is optional so tests can build a handler without one.
func (h *Handler) logf(format string, args ...any) {
	if h.log != nil {
		h.log.Debug(fmt.Sprintf(format, args...))
	}
}

// WithLogger attaches a logger for diagnostics.
func (h *Handler) WithLogger(l *slog.Logger) *Handler {
	h.log = l
	return h
}

// New builds the handler.
func New(cfg config.Config, meta store.Store, content layout.ContentStore,
	authorizer *authz.Service, appPass *auth.AppPasswords) *Handler {
	return &Handler{
		cfg: cfg, meta: meta, content: content, authz: authorizer,
		appPass: appPass, caps: content.Capabilities(), now: time.Now,
	}
}

// ServeHTTP dispatches by method.
//
// Auth is HTTP Basic with an app password. Every path below assumes a resolved
// actor; there is exactly one place that turns a request into an actor, so there
// is no method that can forget it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// OPTIONS is answered without authentication so clients can discover
	// capabilities before prompting for credentials. Windows Explorer depends on
	// this; denying it makes the "map network drive" dialog fail with a generic
	// error instead of a credential prompt.
	if r.Method == http.MethodOptions {
		h.handleOptions(w, r)
		return
	}

	actor, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	// The external fork (ADR-0014): a URL below a mount point never reaches the local
	// handlers. Everything else — the whole local surface — is byte-for-byte unchanged
	// when no mount service is wired.
	if tgt, cap, external := h.externalTarget(r.Context(), actor, r.URL.Path); external {
		switch r.Method {
		case "PROPFIND":
			h.handleExternalPropfind(w, r, actor, tgt, cap)
		case http.MethodGet:
			h.handleExternalGet(w, r, actor, tgt, cap, false)
		case http.MethodHead:
			h.handleExternalGet(w, r, actor, tgt, cap, true)
		case http.MethodPut:
			h.handleExternalPut(w, r, actor, tgt, cap)
		case "MKCOL":
			h.handleExternalMkcol(w, r, actor, tgt, cap)
		case http.MethodDelete:
			h.handleExternalDelete(w, r, actor, tgt, cap)
		case "MOVE":
			h.handleExternalMove(w, r, actor, tgt, cap)
		case "COPY", "PROPPATCH":
			// A copy out of a mount duplicates the bytes the mount promised not to
			// duplicate silently; dead properties have no rows below a mount point.
			writeXMLError(w, http.StatusForbidden, "external-not-supported")
		case "LOCK", "UNLOCK":
			// The lock register fences local writers. A remote with concurrent writers
			// of its own cannot be fenced by a local token, and a LOCK that pretended
			// otherwise would be a lie a client schedules work around.
			writeXMLError(w, http.StatusForbidden, "external-not-supported")
		default:
			writeXMLError(w, http.StatusNotImplemented, "")
		}
		return
	}

	switch r.Method {
	case "PROPFIND":
		h.handlePropfind(w, r, actor)
	case "PROPPATCH":
		h.handleProppatch(w, r, actor)
	case http.MethodGet:
		h.handleGet(w, r, actor, false)
	case http.MethodHead:
		h.handleGet(w, r, actor, true)
	case http.MethodPut:
		h.handlePut(w, r, actor)
	case http.MethodDelete:
		h.handleDelete(w, r, actor)
	case "MKCOL":
		h.handleMkcol(w, r, actor)
	case "COPY":
		h.handleCopyMove(w, r, actor, false)
	case "MOVE":
		h.handleCopyMove(w, r, actor, true)
	case "LOCK":
		h.handleLock(w, r, actor)
	case "UNLOCK":
		h.handleUnlock(w, r, actor)
	case "REPORT":
		// sync-collection is the incremental-sync primitive. Until it is
		// implemented, report it as unsupported rather than returning an empty
		// multistatus, which a client would interpret as "nothing changed".
		w.Header().Set("DAV", daClasses(h.cfg))
		http.Error(w, "REPORT not implemented", http.StatusNotImplemented)
	default:
		w.Header().Set("Allow", allowHeader())
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authenticate resolves the request to an actor.
//
// The challenge is emitted with the Basic realm on every failure: without it,
// Windows and macOS clients give up instead of prompting.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (authz.Actor, bool) {
	user, pass, ok := basicAuth(r)
	if !ok {
		h.challenge(w)
		return authz.Actor{}, false
	}

	// The username is advisory: the credential decides the identity. Accepting a
	// mismatched username would be harmless but confusing in logs, so it is used
	// only for diagnostics.
	_ = user

	u, cred, err := h.appPass.Verify(r.Context(), pass, clientIP(r))
	if err != nil {
		h.challenge(w)
		return authz.Actor{}, false
	}

	role := authz.RoleGuest
	switch u.Role {
	case "admin":
		role = authz.RoleAdmin
	case "user":
		role = authz.RoleUser
	}

	if u.Status != "active" {
		h.challenge(w)
		return authz.Actor{}, false
	}

	// ID-06 / WC-GAP-001: an App Password is a long-lived credential that never talks to the IdP
	// again, so it is the worst case for "removed from the admin group" — the revocation only ever
	// happened at the next *browser* login, which a WebDAV-only user may never do. Past the
	// revalidation window the credential is refused and the client is challenged, which is the only
	// way this path can be re-confirmed: a Basic-auth prompt the user answers by signing in.
	//
	// The same exemptions as the session path apply (break-glass, no IdP identity, window disabled),
	// which is why the predicate is shared rather than reimplemented here.
	if h.revalidate != nil && h.revalidate(u) {
		h.challenge(w)
		return authz.Actor{}, false
	}

	// The credential's scope narrows the actor, and it has to. Granting
	// ScopeRead|ScopeWrite unconditionally — which this used to do — made every
	// "read-only" app password a full read-write credential over WebDAV, so the
	// one place a read-only token is most useful was the one place it was ignored.
	readOnly, writable := auth.ScopeForActor(cred.Scope)
	var scope authz.Scope
	if readOnly {
		scope |= authz.ScopeRead
	}
	if writable {
		scope |= authz.ScopeWrite
	}

	// The namespace segment comes from the account record, never from the URL the
	// client sent: taking it from the request would let a client address another
	// user's namespace by writing a different username in the path.
	actor := authz.NewUserActor(u.ID, u.Email, role, scope)
	// §10.1 auth_method: every audit row written while serving this WebDAV request says the
	// credential was an app password.
	*r = *r.WithContext(store.WithAuthMethod(r.Context(), store.AuthAppPassword))
	return actor.WithUsername(ownerName(u)), true
}

func (h *Handler) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="newcloud", charset="UTF-8"`)
	w.Header().Set("DAV", daClasses(h.cfg))
	w.Header().Set("MS-Author-Via", "DAV")
	w.WriteHeader(http.StatusUnauthorized)
}

// resolvePath turns a URL path into an authorization capability plus the node.
//
// Every miss returns ErrDenied, never a distinct "forbidden": telling a client
// which paths exist but are off-limits is a free enumeration oracle.
func (h *Handler) resolvePath(ctx context.Context, a authz.Actor, urlPath string) (authz.Cap, store.Node, error) {
	user, rel, err := h.splitPath(urlPath)
	if err != nil {
		h.logf("resolve: split failed url=%q err=%v", urlPath, err)
		return authz.Cap{}, store.Node{}, authz.ErrDenied
	}

	// The name in the URL is not authorization — the credential is — but a URL that
	// names somebody else's namespace must not answer with the caller's own file under
	// the other account's label. Nextcloud clients address /remote.php/dav/files/<user>/,
	// so a mismatched segment is a 404, which is also this project's rule for denials.
	if user != "" && user != a.Username() {
		h.logf("resolve: namespace mismatch url=%q user=%q actor=%q", urlPath, user, a.Username())
		return authz.Cap{}, store.Node{}, authz.ErrNotFound
	}

	nodes, err := h.meta.Resolve(ctx, a.ID(), rel)
	if err != nil {
		h.logf("resolve: metadata miss url=%q rel=%q err=%v", urlPath, rel, err)
		if errors.Is(err, store.ErrNotFound) {
			return authz.Cap{}, store.Node{}, authz.ErrNotFound
		}
		return authz.Cap{}, store.Node{}, err
	}
	leaf := nodes[len(nodes)-1]

	cap, err := h.authz.Authorize(a, leaf.ID)
	if err != nil {
		h.logf("resolve: authorize denied url=%q rel=%q leaf=%s err=%v", urlPath, rel, leaf.ID, err)
		return authz.Cap{}, store.Node{}, err
	}
	h.logf("resolve: ok url=%q rel=%q perms=%v", urlPath, rel, cap.Perms())
	return cap, leaf, nil
}

// splitPath extracts the owner namespace and relative path from a URL.
//
// Three URL shapes are accepted, because they are the shapes real clients send:
//
//	/dav/<username>/<path>                       this server's native form
//	/remote.php/dav/files/<username>/<path>      Nextcloud desktop and mobile clients
//	/remote.php/webdav/<path>                    the legacy ownCloud/Nextcloud form
//
// plus the two root forms (no trailing path). The owner segment is returned rather than
// trusted: resolvePath refuses a URL that names a different account. A path outside those
// shapes is ErrNotFound, never a relative name — the old default branch silently treated
// any unprefixed path as relative to the caller's root.
func (h *Handler) splitPath(urlPath string) (owner, rel string, err error) {
	p := urlPath
	switch {
	case p == "/remote.php/webdav" || strings.HasPrefix(p, "/remote.php/webdav/"):
		// The legacy endpoint carries no owner: the account comes from the credential.
		p = strings.TrimPrefix(p, "/remote.php/webdav")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			return "", "", nil
		}
	case strings.HasPrefix(p, "/remote.php/dav/files/"):
		p = strings.TrimPrefix(p, "/remote.php/dav/files/")
		owner, p, _ = strings.Cut(p, "/")
		if p == "" {
			return owner, "", nil
		}
	case p == "/remote.php/dav" || p == "/remote.php/dav/":
		return "", "", nil
	case p == "/dav" || strings.HasPrefix(p, "/dav/"):
		p = strings.TrimPrefix(p, "/dav")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			return "", "", nil
		}
		owner, p, _ = strings.Cut(p, "/")
		if p == "" {
			return owner, "", nil
		}
	default:
		// /remote.php/dav/uploads/..., /remote.php/dav/versions/... and every other
		// address belong to a Nextcloud feature this server does not implement. They must
		// not fall through to a relative name that could resolve inside somebody's tree.
		return "", "", store.ErrNotFound
	}

	rel = p
	// Clients address a collection with a trailing slash (".../folder/"). It is a
	// separator, not part of the name, and layout.ValidatePath would reject or normalise
	// it differently — a 404 for the URL a client just created a folder at, which is
	// exactly what a Nextcloud client asks for on every listing.
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" {
		return owner, "", nil
	}
	segs, err := layout.ValidatePath(rel)
	if err != nil {
		return "", "", err
	}
	// A swap's staging name is a by-product of another request, not a resource. 404 rather than
	// 403 so the refusal cannot be used to probe whether an overwrite is in progress here.
	for _, seg := range segs {
		if swap.IsReservedSegment(seg) {
			return "", "", store.ErrNotFound
		}
	}
	return owner, rel, nil
}

// ---- OPTIONS ----

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", daClasses(h.cfg))
	w.Header().Set("MS-Author-Via", "DAV")
	w.Header().Set("Allow", allowHeader())
	w.Header().Set("Accept-Ranges", "bytes")
	// Some clients probe the max upload size via these headers before starting.
	w.Header().Set("Max-Upload-Size", fmt.Sprint(h.cfg.Limits.MaxFileBytes))
	w.WriteHeader(http.StatusOK)
}

func daClasses(cfg config.Config) string {
	// Class 2 is locking. Class 3 (RFC 4918bis) is not claimed because REPORT is
	// not implemented; claiming it would make clients skip their fallback path.
	return "1, 2"
}

func allowHeader() string {
	return strings.Join([]string{
		http.MethodOptions, "PROPFIND", "PROPPATCH", http.MethodGet, http.MethodHead,
		http.MethodPut, http.MethodDelete, "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK",
	}, ", ")
}

// ---- helpers used by the method handlers ----

func (h *Handler) capabilityFor(ctx context.Context, a authz.Actor, nodes []store.Node) (authz.Cap, error) {
	return h.authz.Authorize(a, nodes[len(nodes)-1].ID)
}

// ownerName resolves the actor's namespace segment for URL construction.
func ownerName(u store.User) string {
	if u.Username != "" {
		return u.Username
	}
	return u.ID.String()
}

// guessType returns a content type from the name.
//
// It is used for display only. Responses never let the browser treat user content
// as active: the content origin forces attachment, so a misidentified type cannot
// become script execution.
func guessType(name string) string {
	if ext := path.Ext(name); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	return "application/octet-stream"
}

// etagFor derives a strong ETag from a content address.
func etagFor(h [32]byte) string {
	return fmt.Sprintf(`"%x"`, h[:8])
}

// writeXMLError emits a DAV:error body so clients can show something useful.
func writeXMLError(w http.ResponseWriter, status int, condition string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	if condition == "" {
		return
	}
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
<D:error xmlns:D="DAV:"><D:`+condition+`/></D:error>`+"\n")
}

// statusForError maps metadata errors onto the DAV status codes clients expect.
//
// Getting these wrong is user-visible: a quota failure reported as 500 makes
// Windows show "unexpected error" instead of "the disk is full".
func statusForError(err error) int {
	var maxBytes *http.MaxBytesError
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &maxBytes):
		// A body that ran past limits.max_file_bytes mid-read. Chunked uploads
		// declare no length, so this — not the ContentLength check — is the only
		// thing standing between them and an unbounded write.
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, store.ErrNotFound), errors.Is(err, authz.ErrNotFound), errors.Is(err, authz.ErrDenied):
		// Deliberately 404 rather than 403 for denied: see resolvePath.
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict):
		return http.StatusMethodNotAllowed // RFC 4918 for MKCOL/PUT onto a collection
	case errors.Is(err, store.ErrTrashFull):
		// 507 is what a WebDAV client understands as "not enough storage", and the trash IS a storage
		// resource (§4.3). Reporting 500 here would make a full bin look like a server fault.
		return http.StatusInsufficientStorage
	case errors.Is(err, store.ErrQuota), errors.Is(err, layout.ErrSizeMismatch):
		return http.StatusInsufficientStorage
	case errors.Is(err, store.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, store.ErrCycle):
		return http.StatusConflict
	case errors.Is(err, store.ErrPrecondition):
		// The target changed under the client. RFC 9110 §15.5.13.
		return http.StatusPreconditionFailed
	case errors.Is(err, store.ErrInvalidName), errors.Is(err, layout.ErrInvalidNode),
		errors.Is(err, layout.ErrUnsafePath):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrAuditUnavailable):
		// Fail-closed (T-16): the write was refused because it could not be recorded, and nothing
		// changed. 503 says "retry when the recorder is back"; 500 would send an operator looking at
		// the data, which is intact. The JSON API answers the same way.
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
