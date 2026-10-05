package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

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
	// The nonce binds the ID token to this authorization request (ID-02). It travels
	// in the same short-lived cookie as the PKCE verifier: both are one-shot values
	// that only matter until the callback.
	nonce, err := auth.RandomState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot generate nonce")
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
	http.SetCookie(w, &http.Cookie{
		Name: nonceCookie, Value: nonce, Path: "/", MaxAge: 600,
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

	provider, perr := s.oidcProvider(r)
	if perr != nil {
		// 503 and an explicit message: the user needs to know their account is fine and the
		// identity provider is unreachable, not that "login failed".
		s.deps.Log.Warn("login attempted while the identity provider is unreachable", "error", perr)
		writeError(w, http.StatusServiceUnavailable, "idp_unavailable",
			"the identity provider is unreachable; existing sessions and app passwords continue to work")
		return
	}

	http.Redirect(w, r, provider.AuthCodeURL(state, verifier, nonce), http.StatusFound)
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

	// The nonce cookie is required: a callback without one did not start a
	// nonce-bearing flow, and accepting it would leave the replay window this check
	// exists to close.
	nonceCookieVal, err := r.Cookie(nonceCookie)
	if err != nil || nonceCookieVal.Value == "" {
		writeError(w, http.StatusBadRequest, "bad_state", "missing nonce")
		return
	}

	provider, perr := s.oidcProvider(r)
	if perr != nil {
		s.deps.Log.Warn("callback reached while the identity provider is unreachable", "error", perr)
		s.auditLoginFailure(r, "", "idp_unavailable")
		writeError(w, http.StatusServiceUnavailable, "idp_unavailable",
			"the identity provider is unreachable; try again once it is back")
		return
	}

	claims, err := provider.Exchange(r.Context(), code[0], pkceCookieVal.Value, nonceCookieVal.Value)
	if err != nil {
		s.deps.Log.Warn("oidc exchange failed", "error", err)
		// A failed login is security-relevant (§10.1) and was previously only
		// logged: a burst of these is the signal that somebody is probing the
		// account, and a log line is not queryable or tamper-evident.
		//
		// The reason is recorded as a short code, never the token or the error
		// text: an audit row must not leak the credential it is describing.
		s.auditLoginFailure(r, "", "verification_failed")
		writeError(w, http.StatusUnauthorized, "login_failed", "could not verify your identity")
		return
	}

	role := provider.Role(claims.Groups)

	// Apply the provisioning policy before anything is written (ID-04).
	//
	// The existence check is a separate read because the policy's answer depends on
	// whether this identity is new, and an existing account must never be re-evaluated:
	// approval is a one-time event.
	existing := false
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		_, uerr := tx.UserByIdPSubject(ctx, s.deps.Config.Auth.Issuer, claims.Subject)
		if uerr == nil {
			existing = true
			return nil
		}
		if uerr == store.ErrNotFound {
			return nil
		}
		return uerr
	}); err != nil {
		s.deps.Log.Error("provisioning check failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not complete the login")
		return
	}

	decision := s.decideJIT(existing, claims.Email)
	if !decision.Allowed() {
		s.auditLoginFailure(r, claims.Email, "provisioning_denied")
		s.deps.Log.Warn("login refused by provisioning policy",
			"subject", claims.Subject, "reason", decision.Refusal)
		s.refuseJIT(w, decision.Refusal)
		return
	}
	// A pending decision still provisions: the whole point of the mode is that an
	// administrator has a record to approve. Refusing before the insert would leave an
	// empty queue and a user with no explanation.
	awaitingApproval := decision.Status == store.StatusPending

	// Provision or refresh the account in the same transaction as the login
	// audit, so an account cannot exist without a record of how it appeared.
	var user store.User
	var roleChange store.RoleChange
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer:      s.deps.Config.Auth.Issuer,
			Subject:     claims.Subject,
			Username:    sanitiseUsername(claims.Username, claims.Email, claims.Subject),
			Email:       claims.Email,
			DisplayName: claims.Name,
			Role:        string(role),
			QuotaBytes:  s.initialQuota(),
			// Carries the policy's decision into the row. Without this the pending mode
			// computed a status that nothing stored, so it behaved exactly like open.
			Status: decision.Status,
		})
		if uerr != nil {
			return uerr
		}
		user = u
		if _, rerr := tx.EnsureRoot(ctx, u.ID); rerr != nil {
			return rerr
		}

		// Reconcile the stored role with what the IdP now claims. On a downgrade
		// this also revokes the account's live sessions and app passwords — the
		// T-02 requirement. Without it, "removed from the admin group" only took
		// effect at the *next* login, so a demoted admin kept admin rights for the
		// rest of their session TTL, and a WebDAV app password (which never talks
		// to the IdP again) kept them indefinitely.
		//
		// It runs here, inside the login transaction, because this is the only
		// moment NewCloud learns the current group membership: there is no
		// background group sync, and inventing one without the IdP's event feed
		// would only guess at when to poll.
		rc, rerr := tx.ApplyIdPRole(ctx, u.ID, string(role))
		if rerr != nil {
			return rerr
		}
		roleChange = rc

		if rerr := tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &u.ID, ActorEmail: u.Email,
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "auth.login.success", TargetKind: "user", TargetID: u.ID.String(),
			Result: "ok", Metadata: map[string]any{"groups": claims.Groups, "role": string(role)},
		}); rerr != nil {
			return rerr
		}

		// §10.1: the creation of an account is its own event, not a property of the first login
		// that happens to produce it.
		//
		// user.create was declared in the vocabulary with no emitter, and the question it answers —
		// "when did this account appear, and by what path" — is the first one anybody asks when an
		// unexpected account turns up. A login row mentions the account but not that it did not
		// exist before, so it cannot answer that: the difference between "signed in" and "came into
		// existence" is exactly what an auditor needs.
		if !existing {
			if aerr := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "system", ActorUserID: &u.ID, ActorEmail: u.Email,
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "user.create", TargetKind: "user", TargetID: u.ID.String(),
				Result: "ok",
				Metadata: map[string]any{
					"via":    "oidc",
					"issuer": s.deps.Config.Auth.Issuer,
					"status": string(decision.Status),
					"role":   string(role),
				},
			}); aerr != nil {
				return aerr
			}
		}

		// The role change is its own event. A login that silently rewrote somebody's
		// privileges would be the one change an operator could not find afterwards,
		// and §10.1 requires role changes to be audited.
		if roleChange.Changed {
			result := "ok"
			if roleChange.Downgraded {
				result = "denied"
			}
			if aerr := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "system", ActorUserID: &u.ID, ActorEmail: u.Email,
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "user.role.change", TargetKind: "user", TargetID: u.ID.String(),
				Result: result,
				Metadata: map[string]any{
					"from":                  roleChange.From,
					"to":                    roleChange.To,
					"downgraded":            roleChange.Downgraded,
					"sessions_revoked":      roleChange.SessionsRevoked,
					"app_passwords_revoked": roleChange.AppPasswordsRevoked,
					"groups":                claims.Groups,
				},
			}); aerr != nil {
				return aerr
			}
			// A downgrade that cut sessions off is also a session revocation (§10.1). Same event
			// name as the admin path, on purpose: an operator looking for "did anything revoke this
			// user's sessions" must not have to know which subsystem did it.
			return auditSessionRevocation(ctx, tx, r, u.ID, u.Username,
				roleChange.SessionsRevoked, "role.downgrade",
				roleChange.From+"->"+roleChange.To)
		}
		return nil
	})
	if err != nil {
		// A conflict here means the address already belongs to a different identity —
		// an IdP recycled a mailbox. Answering 500 would be wrong twice over: it is not
		// a server fault, and it tells the operator nothing. The new account is refused
		// (it cannot inherit another person's files) and the attempt is recorded.
		if errors.Is(err, store.ErrConflict) {
			s.deps.Log.Warn("login refused: identity conflict",
				"email", claims.Email, "subject", claims.Subject, "error", err)
			s.auditLoginFailure(r, claims.Email, "identity_conflict")
			writeError(w, http.StatusForbidden, "identity_conflict",
				"this email address is already associated with another account; "+
					"an administrator must resolve it")
			return
		}
		s.deps.Log.Error("provisioning failed", "error", err)
		// A disabled account reaching this point is a login attempt that must be
		// visible: the person was told they may not sign in, and the owner needs
		// to know they tried.
		s.auditLoginFailure(r, claims.Email, "account_disabled")
		writeError(w, http.StatusForbidden, "account_disabled", "this account is not active")
		return
	}

	if roleChange.Changed {
		s.deps.Log.Info("role reconciled from IdP claims",
			"user", user.ID.String(), "from", roleChange.From, "to", roleChange.To,
			"downgraded", roleChange.Downgraded,
			"sessions_revoked", roleChange.SessionsRevoked,
			"app_passwords_revoked", roleChange.AppPasswordsRevoked)
	}

	if awaitingApproval {
		// The account exists but may not be used yet. No session is minted: handing back a
		// cookie for an account that cannot act would be a broken login dressed as a
		// successful one.
		s.auditLoginFailure(r, claims.Email, "awaiting_approval")
		writeError(w, http.StatusForbidden, "awaiting_approval", pendingMessage)
		return
	}

	token, err := s.deps.Sessions.Mint(r.Context(), user.ID, user.Email, clientIP(r), r.UserAgent())
	if err != nil {
		s.deps.Log.Error("session mint failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create a session")
		return
	}

	// A downgrade revoked every session the account had — including, unavoidably,
	// any that existed a moment ago. The token just minted above is the user's new,
	// correctly-privileged session, so it must be issued *after* the revocation
	// rather than before it. Minting first and revoking second would hand the user a
	// cookie that is already dead, which looks exactly like a broken login.
	s.setSessionCookie(w, token)

	// Clear the one-shot flow cookies.
	for _, name := range []string{stateCookie, pkceCookie, nonceCookie} {
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
				// The session ticket carries the subject but not the address, so it is read
				// here. It goes into the row rather than being joined at read time: an audit
				// log must show the address as it was at the event, not whatever it has been
				// changed to since.
				email := ""
				if uid := parseUUID(claims.Subject); uid != uuid.Nil {
					if u, uerr := tx.UserByID(ctx, uid); uerr == nil {
						email = u.Email
					}
				}
				return tx.Audit(ctx, store.AuditEntry{
					ActorKind: "user", ActorUserID: ptrUUID(parseUUID(claims.Subject)),
					ActorEmail: email,
					ActorIP:    clientIP(r), UserAgent: r.UserAgent(),
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

// auditLoginFailure records a refused sign-in attempt.
//
// Best-effort on purpose: an audit write failure must not turn a 401 into a 500,
// and the attempt was refused either way. The reason is a short code rather than
// the underlying error, because an audit row must not carry a token or a
// library's error string.
func (s *Server) auditLoginFailure(r *http.Request, email, reason string) {
	_ = s.deps.Meta.WithTx(context.WithoutCancel(r.Context()), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", ActorEmail: email,
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "auth.login.failure", TargetKind: "user", TargetID: "",
			Result: "denied", Metadata: map[string]any{"reason": reason},
		})
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

// oidcProvider returns the provider for a login attempt (ID-07).
//
// Order matters: an explicitly configured Provider wins (tests and any deployment that wants a
// hard startup requirement), then the holder, which lazily discovers when the IdP comes back.
// The returned error is ErrIdPUnavailable-shaped so the handler can answer 503 with a message a
// user can act on, rather than a generic failure.
func (s *Server) oidcProvider(r *http.Request) (*auth.Provider, error) {
	if s.deps.Provider != nil {
		return s.deps.Provider, nil
	}
	if s.deps.Providers == nil {
		return nil, auth.ErrIdPUnavailable
	}

	if p, ok := s.deps.Providers.Get(); ok {
		return p, nil
	}

	p, err := s.deps.Providers.Resolve(r.Context(), issuerCarrier{s.deps.Config.Auth.Issuer},
		func(ctx context.Context) (*auth.Provider, error) {
			return auth.NewProvider(ctx, s.deps.Config)
		}, idpRetryInterval)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// idpRetryInterval bounds how often a failed discovery is retried.
//
// Long enough that a burst of login attempts cannot amplify load on an IdP that is already
// struggling; short enough that recovery is visible without a restart.
const idpRetryInterval = 30 * time.Second

// issuerCarrier adapts a plain issuer string to the interface Resolve expects.
type issuerCarrier struct{ s string }

// IssuerURL returns the configured issuer.
func (i issuerCarrier) IssuerURL() string { return i.s }

// initialQuota is the quota a newly provisioned account starts with.
//
// One function, because there is more than one way an account comes into existence: an IdP login
// and the break-glass login both call UpsertIdPUser. The first version of this logic lived inline
// in the IdP path, so a break-glass account still took the schema's hard-coded default — the
// config key was obeyed for one provisioner and ignored for the other, which is harder to notice
// than ignoring it everywhere.
//
// Nil means "leave the schema default alone", which is what an operator gets by setting the key to
// zero: the migration's value is the documented default and it stays the single source of truth.
func (s *Server) initialQuota() *int64 {
	if q := s.deps.Config.Limits.DefaultFileQuotaBytes; q > 0 {
		return &q
	}
	return nil
}
