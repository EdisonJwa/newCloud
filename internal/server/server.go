// Package server assembles the HTTP surface: the JSON API, the WebDAV endpoint,
// the share pages and the health endpoints.
//
// Route topology mirrors the origin split. Everything under /s/ and /content/ is
// user-reachable without a session and forces a download; everything under /api
// requires a session or an app password.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/dav"
	"github.com/EdisonJwa/newCloud/internal/mail"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
	"github.com/EdisonJwa/newCloud/internal/webassets"
)

// Deps are the collaborators the server needs. Passing them explicitly rather
// than reaching for globals keeps the wiring visible in one place (cmd/newcloud)
// and makes the handler testable with fakes.
type Deps struct {
	Config   config.Config
	Meta     store.Store
	Content  layout.ContentStore
	Authz    *authz.Service
	Tickets  *ticket.Issuer
	Sessions *auth.Sessions
	AppPass  *auth.AppPasswords
	Shares   *share.Service
	Provider *auth.Provider
	// Mailer delivers recipient verification codes. Nil means no transport is
	// configured: email shares still exist, but the server logs a warning each time a
	// code is generated rather than pretending it was sent.
	Mailer  mail.Sender
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
	ui *webassets.Handler
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

	s := &Server{deps: deps, started: time.Now()}
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
	r.Use(middleware.RealIP)
	r.Use(s.requestLogger)
	r.Use(s.securityHeaders)
	r.Use(middleware.Recoverer)

	// WebDAV must be intercepted before routing: chi validates methods against a
	// fixed list, so PROPFIND/MKCOL/COPY/MOVE/LOCK/UNLOCK cannot be registered and
	// would be answered 405 by the router instead of reaching the handler.
	r.Use(s.davInterceptor)

	// Health must not require authentication: an orchestrator probing it has no
	// session.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route("/api", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Get("/login", s.handleLogin)
			r.Get("/callback", s.handleCallback)
			r.Post("/logout", s.handleLogout)
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
				r.Post("/mkdir", s.handleMkdir)
				r.Post("/upload", s.handleUpload)
				r.Get("/download", s.handleDownload)
				r.Post("/rename", s.handleRename)
				r.Post("/move", s.handleMove)
				r.Delete("/", s.handleDelete)
				r.Get("/trash", s.handleTrashList)
				r.Post("/restore", s.handleRestore)
				r.Get("/versions", s.handleVersions)
				r.Post("/restore-version", s.handleRestoreVersion)
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

// handleReadyz reports whether the instance can serve traffic, including which
// storage layout is active. A deployment that meant to use "tree" but got "blob"
// has its data somewhere completely different on disk, and that is worth seeing
// without reading the config file.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
		return
	}
	caps := s.deps.Content.Capabilities()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ready",
		"version": s.deps.Version,
		"backend": s.deps.Config.Storage.Backend,
		"layout":  string(s.deps.Content.Mode()),
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
