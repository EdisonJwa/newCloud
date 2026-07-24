package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// handleLogin starts the OIDC authorization-code flow with PKCE.
//
// The PKCE verifier and the state parameter live in cookies rather than server
// session: there is no session yet at this point, and putting them in the server
// would require a pre-auth session store for no benefit.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := auth.RandomState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot generate state")
		return
	}
	verifier, err := auth.RandomState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot generate pkce verifier")
		return
	}

	// SameSite=Lax on the state cookie: the IdP will redirect back with a
	// top-level GET, which Lax permits, while a cross-site POST would not carry it.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: pkceCookie, Value: verifier, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})

	// Next is restricted to same-site paths: an open redirect after login is a
	// phishing primitive.
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: "__Host-wc_next", Value: next, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, s.deps.Provider.AuthCodeURL(state, verifier), http.StatusFound)
}

// handleCallback completes the flow.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Refuse a callback that did not arrive on the configured host: otherwise an
	// attacker who can point DNS at this process could receive codes.
	if !s.hostMatches(r) {
		writeError(w, http.StatusBadRequest, "bad_host", "callback host mismatch")
		return
	}

	q := r.URL.Query()
	code, gotCode := q["code"]
	state, gotState := q["state"]
	if !gotCode || !gotState {
		writeError(w, http.StatusBadRequest, "bad_request", "missing code or state")
		return
	}

	stateCookieVal, err := r.Cookie(stateCookie)
	if err != nil || stateCookieVal.Value != state[0] {
		writeError(w, http.StatusBadRequest, "bad_state", "state mismatch")
		return
	}
	pkceCookieVal, err := r.Cookie(pkceCookie)
	if err != nil || pkceCookieVal.Value == "" {
		writeError(w, http.StatusBadRequest, "bad_state", "missing pkce verifier")
		return
	}

	claims, err := s.deps.Provider.Exchange(r.Context(), code[0], pkceCookieVal.Value)
	if err != nil {
		s.deps.Log.Warn("oidc exchange failed", "error", err)
		writeError(w, http.StatusUnauthorized, "login_failed", "could not verify your identity")
		return
	}

	role := s.deps.Provider.Role(claims.Groups)

	// Provision or refresh the account in the same transaction as the login
	// audit, so an account cannot exist without a record of how it appeared.
	var user store.User
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer:      s.deps.Config.Auth.Issuer,
			Subject:     claims.Subject,
			Username:    sanitiseUsername(claims.Username, claims.Email, claims.Subject),
			Email:       claims.Email,
			DisplayName: claims.Name,
			Role:        string(role),
		})
		if uerr != nil {
			return uerr
		}
		user = u
		if _, rerr := tx.EnsureRoot(ctx, u.ID); rerr != nil {
			return rerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &u.ID, ActorEmail: u.Email,
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "auth.login.success", TargetKind: "user", TargetID: u.ID.String(),
			Result: "ok", Metadata: map[string]any{"groups": claims.Groups, "role": string(role)},
		})
	})
	if err != nil {
		s.deps.Log.Error("provisioning failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not prepare your account")
		return
	}

	token, err := s.deps.Sessions.Mint(r.Context(), user.ID, user.Email, clientIP(r), r.UserAgent())
	if err != nil {
		s.deps.Log.Error("session mint failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create a session")
		return
	}

	s.setSessionCookie(w, token)

	// Clear the one-shot flow cookies.
	for _, name := range []string{stateCookie, pkceCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}

	next := "/"
	if c, err := r.Cookie("__Host-wc_next"); err == nil && strings.HasPrefix(c.Value, "/") && !strings.HasPrefix(c.Value, "//") {
		next = c.Value
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// handleLogout revokes the session server-side, not just the cookie.
//
// Clearing the cookie alone leaves a valid token that anyone who copied it can
// keep using.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if claims, verr := s.deps.Sessions.Verify(r.Context(), c.Value); verr == nil {
			_ = s.deps.Sessions.Revoke(r.Context(), claims.ID)
			_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
				return tx.Audit(ctx, store.AuditEntry{
					ActorKind: "user", ActorUserID: ptrUUID(parseUUID(claims.Subject)),
					ActorIP: clientIP(r), UserAgent: r.UserAgent(),
					Action: "auth.logout", TargetKind: "session", TargetID: claims.ID,
					Result: "ok",
				})
			})
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the current account, which the SPA uses to decide what to
// render.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}

	var user store.User
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UserByID(ctx, actor.ID())
		user = u
		return err
	})

	used, _ := s.deps.Meta.Usage(r.Context(), actor.ID())
	caps := s.deps.Content.Capabilities()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"email":        user.Email,
		"display_name": user.DisplayName,
		"role":         user.Role,
		"quota_bytes":  user.QuotaBytes,
		"used_bytes":   used,
		"storage": map[string]any{
			"layout":         string(s.deps.Content.Mode()),
			"deduplicated":   caps.Deduplicated,
			"cheap_versions": caps.CheapVersions,
		},
	})
}

// sanitiseUsername produces a namespace segment from IdP data.
//
// It falls back through username → email local part → subject hash, because the
// namespace appears in WebDAV URLs and must exist even when the IdP sends nothing
// usable. The result is also validated: an IdP-supplied name containing "/" would
// otherwise become a path separator.
func sanitiseUsername(username, email, subject string) string {
	candidate := strings.TrimSpace(username)
	if candidate == "" && email != "" {
		if at := strings.IndexByte(email, '@'); at > 0 {
			candidate = email[:at]
		}
	}
	if candidate == "" {
		// A short, stable, collision-resistant fallback derived from the subject.
		candidate = "u" + strings.ReplaceAll(subject, "-", "")[:12]
	}

	var b strings.Builder
	for _, r := range candidate {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		out = "user"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func parseUUID(s string) uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return uuid.Nil
	}
	return id
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }
