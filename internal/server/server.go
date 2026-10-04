// Package server assembles the HTTP surface: the JSON API, the WebDAV endpoint,
// the share pages and the health endpoints.
//
// Route topology mirrors the origin split. Everything under /s/ and /content/ is
// user-reachable without a session and forces a download; everything under /api
// requires a session or an app password.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/dav"
	"github.com/EdisonJwa/newCloud/internal/mail"
	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/thumbnail"
	"github.com/EdisonJwa/newCloud/internal/ticket"
	"github.com/EdisonJwa/newCloud/internal/upload"
	"github.com/EdisonJwa/newCloud/internal/webassets"
)

// Deps are the collaborators the server needs. Passing them explicitly rather
// than reaching for globals keeps the wiring visible in one place (cmd/newcloud)
// and makes the handler testable with fakes.
type Deps struct {
	Config  config.Config
	Meta    store.Store
	Content layout.ContentStore
	// Blobs is the content-addressed store, set only for the blob layout. Reading a HISTORICAL
	// version needs an address, not a path: the node's path resolves to the current bytes, so the
	// path-addressing ContentStore cannot serve an old one. Nil means this deployment's layout
	// keeps no addressable history, and the version-content endpoint says so rather than serving
	// the wrong bytes.
	Blobs    blob.Store
	Authz    *authz.Service
	Tickets  *ticket.Issuer
	Sessions *auth.Sessions
	AppPass  *auth.AppPasswords
	Shares   *share.Service
	Provider *auth.Provider
	// Providers holds the OIDC provider when it may not have been discovered yet (ID-07).
	//
	// Distinct from Provider, which is the "already known at startup" case the tests use. In
	// production the holder is what makes an IdP outage a login-only degradation instead of a
	// refusal to start: the server comes up without it and discovers lazily.
	Providers *auth.ProviderHolder
	// Mailer delivers recipient verification codes. Nil means no transport is
	// configured: email shares still exist, but the server logs a warning each time a
	// code is generated rather than pretending it was sent.
	Mailer mail.Sender
	// Thumbs renders image previews. Nil disables the endpoint, which then answers 415 and
	// the client falls back to a type badge.
	Thumbs *thumbnail.Generator
	// Uploads holds in-progress resumable uploads. Nil disables the chunked endpoints, which
	// then answer 503 and the client falls back to a single-request upload.
	Uploads *upload.Manager
	// Metrics records request outcomes and operational signals. Nil means metrics are not
	// collected, which is the default for tests that do not care.
	Metrics *metrics.Registry
	Log     *slog.Logger
	Version string
}

// Server holds process-wide dependencies.
type Server struct {
	deps    Deps
	started time.Time
	ready   atomic.Bool
	// dav is the WebDAV handler, built once because it is stateless.
	dav http.Handler
	// ui serves the embedded browser client. Nil disables the SPA, which is useful
	// for an API-only deployment and for tests that assert on 404s.
	ui     *webassets.Handler
	thumbs *thumbnail.Generator
}

// New builds the server but does not start listening.
func New(deps Deps) (*Server, error) {
	if deps.Meta == nil {
		return nil, fmt.Errorf("server: nil metadata store")
	}
	if deps.Content == nil {
		return nil, fmt.Errorf("server: nil content store")
	}
	if deps.Authz == nil {
		return nil, fmt.Errorf("server: nil authorizer")
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}

	// A break-glass account that is configured but unusable is worse than none: an
	// operator relies on it during an outage and only then discovers the hash file is
	// unreadable. Checking at startup turns that into a failure that happens at a time
	// somebody can fix it.
	if deps.Config.Auth.AllowLocalAdmin {
		if _, err := readFileTrimmed(deps.Config.Auth.LocalAdmin.PasswordFile); err != nil {
			return nil, fmt.Errorf("break-glass: read %s: %w",
				deps.Config.Auth.LocalAdmin.PasswordFile, err)
		}
	}

	thumbs := deps.Thumbs
	if thumbs == nil {
		thumbs = thumbnail.New()
	}

	s := &Server{deps: deps, started: time.Now(), thumbs: thumbs}
	if deps.AppPass != nil {
		s.dav = dav.New(deps.Config, deps.Meta, deps.Content, deps.Authz, deps.AppPass).WithLogger(deps.Log)
	}

	// The client is embedded in the binary, so there is nothing to configure. A
	// failure here means the build is corrupt, not that the operator got something
	// wrong.
	ui, err := webassets.New()
	if err != nil {
		return nil, fmt.Errorf("server: embedded client: %w", err)
	}
	s.ui = ui

	return s, nil
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(s.clientAddress)
	r.Use(s.requestLogger)
	r.Use(s.securityHeaders)
	r.Use(middleware.Recoverer)
	// After Recoverer, so a panicking handler still records the 500 the client saw rather
	// than being invisible to the error-rate signal.
	r.Use(s.observeRequest)

	// WebDAV must be intercepted before routing: chi validates methods against a
	// fixed list, so PROPFIND/MKCOL/COPY/MOVE/LOCK/UNLOCK cannot be registered and
	// would be answered 405 by the router instead of reaching the handler.
	r.Use(s.davInterceptor)

	// Health must not require authentication: an orchestrator probing it has no
	// session.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	// /metrics is deliberately unauthenticated but bound by the same listener as
	// everything else: a scraper cannot use a session cookie, and the values it exposes
	// are operational counts rather than user data. A deployment that must not expose it
	// publicly should not route this path through its reverse proxy.
	if s.deps.Metrics != nil {
		r.Method(http.MethodGet, "/metrics", s.deps.Metrics.Handler())
	}

	r.Route("/api", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Get("/login", s.handleLogin)
			r.Get("/callback", s.handleCallback)
			r.Post("/logout", s.handleLogout)
			// Break-glass is a *credential* submission, not a redirect: it is the
			// path that works when the IdP does not. It answers 404 when the feature
			// is not configured, so it never advertises a login that cannot work.
			r.Post("/breakglass", s.handleBreakglassLogin)
		})

		// /me needs the session, so it lives outside the pre-auth group.
		r.Group(func(r chi.Router) {
			r.Use(s.requireSession)
			r.Get("/auth/me", s.handleMe)
		})

		// Authenticated API. Every route below goes through requireSession or
		// requireAppPassword; none of them is reachable anonymously.
		r.Group(func(r chi.Router) {
			r.Use(s.requireSession)

			r.Route("/files", func(r chi.Router) {
				r.Get("/", s.handleList)
				r.Get("/recent", s.handleRecent)
				r.Get("/search", s.handleSearch)
				r.Post("/mkdir", s.handleMkdir)
				r.Post("/upload", s.handleUpload)
				r.Get("/download", s.handleDownload)
				r.Post("/rename", s.handleRename)
				r.Post("/move", s.handleMove)
				r.Delete("/", s.handleDelete)
				r.Get("/trash", s.handleTrashList)
				r.Post("/restore", s.handleRestore)
				r.Get("/thumbnail", s.handleThumbnail)
				r.Route("/uploads", func(r chi.Router) {
					r.Post("/", s.handleUploadCreate)
					r.Put("/{id}", s.handleUploadChunk)
					r.Post("/{id}/complete", s.handleUploadComplete)
					r.Delete("/{id}", s.handleUploadAbort)
				})
				r.Get("/versions", s.handleVersions)
				// Historical content: §8.1 lists 版本下载 among the entry points that must follow the same
				// authorization as everything else.
				r.Get("/version", s.handleVersionContent)
				r.Post("/restore-version", s.handleRestoreVersion)
			})

			// "Shared with me": the way into another account's tree once an ACL
			// grant exists. Path-based handlers cannot name those nodes, because
			// their path is in somebody else's namespace.
			r.Get("/shared", s.handleSharedWithMe)
			r.Get("/shared/{id}", s.handleSharedNode)
			// The content half: without it `download` on a shared node was a permission with
			// no route that could exercise it.
			r.Get("/shared/{id}/content", s.handleSharedContent)

			// Account administration. Approving a JIT-provisioned account is the
			// remedy the provisioning policy names, and until this existed the policy
			// had no way out of the state it created.
			r.Route("/admin/users", func(r chi.Router) {
				r.Get("/", s.handleAdminUsers)
				r.Post("/{id}/status", s.handleAdminSetUserStatus)
				r.Delete("/{id}", s.handleAdminDeleteUser)
			})

			// ACL management. Creating a grant is a permission of its own
			// (manage_permissions) rather than an owner-only right, so an owner can
			// delegate the ability to delegate — and revoke it like anything else.
			r.Route("/acl", func(r chi.Router) {
				r.Get("/", s.handleACLList)
				r.Post("/", s.handleACLGrant)
				r.Delete("/{id}", s.handleACLRevoke)
			})

			r.Route("/shares", func(r chi.Router) {
				r.Get("/", s.handleShareList)
				r.Post("/", s.handleShareCreate)
				r.Patch("/{id}", s.handleShareUpdate)
				r.Delete("/{id}", s.handleShareRevoke)
			})

			// The audit log: read-only, exportable. Inside the authenticated group
			// because it needs a session, unlike the share pages.
			r.Get("/audit", s.handleAuditExport)
			// The view feeds a screen; the export above produces a file.
			r.Get("/audit/events", s.handleAuditList)

			r.Route("/apppasswords", func(r chi.Router) {
				r.Get("/", s.handleAppPasswordList)
				r.Post("/", s.handleAppPasswordCreate)
				r.Delete("/{id}", s.handleAppPasswordRevoke)
			})
		})
	})

	// WebDAV. Authentication is HTTP Basic with an app password: no desktop
	// client can complete an OIDC redirect.
	//
	// It is dispatched from a middleware rather than a route because chi validates
	// methods against a fixed list. PROPFIND, MKCOL, COPY, MOVE, LOCK and UNLOCK
	// therefore cannot be registered at all: r.Handle covers only GET, r.Method
	// panics on an extension verb, and Mount accepts the request but its
	// sub-router answers 405 before the handler runs.
	//
	// Intercepting ahead of routing keeps the handler in charge of its own Allow
	// header and status codes, which is what WebDAV clients need to behave.
	// (The interception is installed below, before routing, because middleware
	// must be registered before routes.)

	// Share landing pages and the content origin are anonymous by design.
	r.Get("/s/{token}", s.handleSharePage)
	r.Post("/s/{token}/verify", s.handleShareVerify)
	// Email shares: request a code for an invited address, then exchange it.
	r.Post("/s/{token}/recipient", s.handleShareRecipientRequest)
	r.Post("/s/{token}/recipient/verify", s.handleShareRecipientVerify)
	r.Get("/s/{token}/download", s.handleShareDownload)
	// Previews are content: same token, same gate, and no permanent URL of their own.
	r.Get("/s/{token}/thumbnail", s.handleShareThumbnail)
	r.Post("/s/{token}/upload", s.handleShareUpload)

	// Content origin: user bytes, always as an attachment.
	r.Get("/content/*", s.handleContent)

	// The browser client is mounted last so its catch-all fallback can never shadow
	// an API route: every handler above wins, and only genuinely unmatched paths
	// reach the SPA shell.
	if s.ui != nil {
		r.NotFound(s.ui.ServeHTTP)
	}

	s.ready.Store(true)
	return r
}

// Routes enumerates every registered method and path pattern.
//
// Added so a coverage test can enumerate the real route table instead of a hand-maintained copy of
// it. The IDOR guard did the latter — under a function named idRoutesTakenFromTheServer that
// returned a literal slice — so it compared one fixed list against another and could not notice a
// route added to this file. chi.Walk reports the FULL pattern including group prefixes, which a
// source-level parse cannot reconstruct.
//
// Calls Handler(), which flips the ready flag; that is harmless for a test process and the
// alternative is a second copy of the route table, which is the problem this solves.
func (s *Server) Routes() []string {
	router, ok := s.Handler().(chi.Router)
	if !ok {
		return nil
	}
	var out []string
	_ = chi.Walk(router, func(method, route string, _ http.Handler,
		_ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	})
	sort.Strings(out)
	return out
}

// clientAddress decides what r.RemoteAddr means, and replaces chi's RealIP.
//
// RealIP is not usable here. It rewrites r.RemoteAddr from X-Forwarded-For for every caller, and
// this server derives the outcome of several controls from that value: the break-glass CIDR
// restriction, the per-IP share password and recipient throttles, the upload rate limit, and the
// ActorIP written to the audit log. Trusting the header from anyone means each of those is decided
// by a value the client chose.
//
// It was not theoretical. With RealIP in place, break-glass refused a caller outside its allowed
// range and then accepted the same request with `X-Forwarded-For: 10.1.2.3` — an unauthenticated
// admin login from the internet, through a control that exists to prevent exactly that.
//
// The policy, in order:
//
//	trusted_proxies non-empty  honour the header, but only when the direct peer is in the list,
//	                           and take the rightmost address that is not itself a trusted proxy
//	trust_proxy: true          honour the header from any peer (deprecated; only safe when the
//	                           listener is unreachable except through the proxy)
//	neither                    ignore the header entirely — the default, and the correct one
func (s *Server) clientAddress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := s.forwardedClientIP(r); ip != "" {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}

// forwardedClientIP returns the address to trust, or "" to leave r.RemoteAddr alone.
func (s *Server) forwardedClientIP(r *http.Request) string {
	cfg := s.deps.Config.Server
	if len(cfg.TrustedProxies) == 0 && !cfg.TrustProxy {
		return ""
	}

	peer := hostOnly(r.RemoteAddr)
	peerIP := net.ParseIP(peer)
	if peerIP == nil {
		return ""
	}
	// With an explicit list, the peer must be in it. TrustProxy keeps the old blanket behaviour
	// for deployments that relied on it, now that it is a decision instead of an accident.
	if len(cfg.TrustedProxies) > 0 && !s.isTrustedProxy(peerIP) {
		return ""
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return ""
	}
	// Walk right to left: the rightmost entry is the one the nearest proxy appended, so it is the
	// one least able to be forged. Skip entries that are themselves trusted proxies; the first
	// untrusted address is the client. Taking the LEFTMOST instead would let a client prepend a
	// value of its own and have it chosen.
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		cand := strings.TrimSpace(parts[i])
		ip := net.ParseIP(cand)
		if ip == nil {
			// A malformed hop makes the whole header unusable: continuing past it would mean
			// trusting an address whose position we cannot reason about.
			return ""
		}
		if len(cfg.TrustedProxies) > 0 && s.isTrustedProxy(ip) && i > 0 {
			continue
		}
		return cand
	}
	return ""
}

// isTrustedProxy reports whether ip is in server.trusted_proxies.
func (s *Server) isTrustedProxy(ip net.IP) bool {
	for _, cidr := range s.deps.Config.Server.TrustedProxies {
		_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// hostOnly strips the port from a host:port, tolerating a bare host.
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// securityHeaders applies the baseline to every response.
//
// The CSP is strict because this app needs no inline script, and a strict policy
// is the cheapest defence against a stored-XSS payload that survives the content
// origin split.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
				"style-src 'self' 'unsafe-inline'; object-src 'none'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// requestLogger emits one structured line per request.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.deps.Log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
			"ip", r.RemoteAddr,
		)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"uptime": time.Since(s.started).Round(time.Second).String(),
	})
}

// handleReadyz reports whether the instance can actually serve traffic.
//
// "Ready" has to mean "its dependencies answer", not "the process started". The
// previous version returned 200 with a static body, so a container whose database had
// gone away still reported healthy and the orchestrator kept routing to it — the one
// failure a readiness probe exists to prevent.
//
// The checks are deliberately shallow: a round trip to the metadata store and the
// ability to answer a tiny read from the content store. Deep checks (row counts,
// cross-region latency) belong in monitoring, not in a probe that every load balancer
// calls twice a second.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
		return
	}

	// A probe must not hang: an unresponsive dependency has to produce a failed probe,
	// not a stalled one.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	healthy := true

	// The metadata store, exercised with the cheapest read that proves the connection
	// and the schema are both usable. A query against nodes would fail on a
	// half-migrated database, which is exactly the state worth reporting.
	probeID := uuid.Nil
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, nerr := tx.Node(ctx, probeID)
		// ErrNotFound is the healthy answer for a nil uuid: it proves the query ran.
		if nerr != nil && !errors.Is(nerr, store.ErrNotFound) {
			return nerr
		}
		return nil
	})
	if err != nil {
		checks["metadata"] = "error: " + err.Error()
		healthy = false
	} else {
		checks["metadata"] = "ok"
	}

	// The content store. Opening a path that does not exist proves the backend is
	// reachable — for S3 that is a real request, not a cached capability.
	if _, err := s.deps.Content.Open(ctx, layout.Node{
		Owner: uuid.Nil.String(), Path: ".readiness-probe",
	}); err != nil && !errors.Is(err, layout.ErrNotFound) {
		checks["content"] = "error: " + err.Error()
		healthy = false
	} else {
		checks["content"] = "ok"
	}

	caps := s.deps.Content.Capabilities()
	status := "ready"
	code := http.StatusOK
	if !healthy {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, map[string]any{
		"status":  status,
		"version": s.deps.Version,
		"backend": s.deps.Config.Storage.Backend,
		"layout":  string(s.deps.Content.Mode()),
		"checks":  checks,
		"capabilities": map[string]bool{
			"deduplicated":      caps.Deduplicated,
			"cheap_versions":    caps.CheapVersions,
			"direct_fs_inspect": caps.DirectFSInspect,
			"efficient_rename":  caps.EfficientRename,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the single shape every API error uses, so clients need one decoder.
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: code, Message: msg})
}

// davInterceptor routes every request under /dav to the WebDAV handler.
//
// It runs before the router because chi cannot register extension methods, and a
// 405 from the router would be indistinguishable to a client from "this server has
// no WebDAV".
func (s *Server) davInterceptor(next http.Handler) http.Handler {
	if s.dav == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/dav" || strings.HasPrefix(p, "/dav/") {
			s.dav.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
