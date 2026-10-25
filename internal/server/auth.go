package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
		Name: s.hostCookieName(stateCookieName), Value: state, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: s.hostCookieName(pkceCookieName), Value: verifier, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: s.hostCookieName(nonceCookieName), Value: nonce, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
	})

	// Next is restricted to same-site paths: an open redirect after login is a
	// phishing primitive.
	next := r.URL.Query().Get("next")
	if !isSameSitePath(next) {
		next = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.hostCookieName(nextCookieName), Value: next, Path: "/", MaxAge: 600,
		HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
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

	stateCookieVal, err := r.Cookie(s.hostCookieName(stateCookieName))
	// Constant time, like the nonce comparison below: the state is not secret once
	// it has been used, but the comparison is free and the habit should not depend
	// on remembering which cookie values are worth the care.
	if err != nil || subtle.ConstantTimeCompare([]byte(stateCookieVal.Value), []byte(state[0])) != 1 {
		writeError(w, http.StatusBadRequest, "bad_state", "state mismatch")
		return
	}
	pkceCookieVal, err := r.Cookie(s.hostCookieName(pkceCookieName))
	if err != nil || pkceCookieVal.Value == "" {
		writeError(w, http.StatusBadRequest, "bad_state", "missing pkce verifier")
		return
	}

	// The nonce cookie is required: a callback without one did not start a
	// nonce-bearing flow, and accepting it would leave the replay window this check
	// exists to close.
	nonceCookieVal, err := r.Cookie(s.hostCookieName(nonceCookieName))
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

	// §10.1 auth_method: the events written by this handler describe an OIDC login, including the
	// account creation it may have caused.
	*r = *r.WithContext(store.WithAuthMethod(r.Context(), store.AuthOIDC))

	// ID-05: the IdP's groups map to a platform role, and an unknown group or a missing claim must not
	// grant elevated rights. provider.Role defaults to the lowest role for exactly that reason.
	role := provider.Role(claims.Groups)

	// Apply the provisioning policy before anything is written (ID-04).
	//
	// The existence check is a separate read because the policy's answer depends on
	// whether this identity is new, and an existing account must never be re-evaluated:
	// approval is a one-time event.
	//
	// The same transaction also decides whether this login would CLAIM a pre-provisioned
	// account by username (the store's byName fallback). That claim is only allowed when the
	// IdP's address is verified and matches the account's stored email exactly; the check
	// lives here, before the upsert, so the refusal is the same identity_conflict the store
	// itself answers — and so the in-memory backend (tests, pre-database wiring) enforces
	// the identical rule, not only PostgreSQL.
	storeEmail := effectiveLoginEmail(claims.Email, claims.EmailVerified)
	claimRefused := false
	existing := false
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		_, uerr := tx.UserByIdPSubject(ctx, s.deps.Config.Auth.Issuer, claims.Subject)
		if uerr == nil {
			existing = true
			return nil
		}
		if uerr == store.ErrNotFound {
			// New identity. If a username-claim is on the table, it must be earned.
			return s.checkClaimEligibility(ctx, tx, claims, storeEmail, &claimRefused)
		}
		return uerr
	}); err != nil {
		s.deps.Log.Error("provisioning check failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not complete the login")
		return
	}

	if claimRefused {
		// Same answer the store gives for a subject mismatch: the address cannot be
		// attached to that account, and the attempt is on the audit trail.
		s.deps.Log.Warn("login refused: identity conflict",
			"email", claims.Email, "subject", claims.Subject)
		s.auditLoginFailure(r, claims.Email, "identity_conflict")
		writeError(w, http.StatusForbidden, "identity_conflict",
			"this email address is already associated with another account; "+
				"an administrator must resolve it")
		return
	}

	decision := s.decideJIT(existing, claims.Email, claims.EmailVerified)
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
			Issuer:  s.deps.Config.Auth.Issuer,
			Subject: claims.Subject,
			// storeEmail is empty when the IdP marked the address unverified, so an
			// explicitly unverified address can neither claim an account by email,
			// collide with the UNIQUE email column, nor be copied onto an existing
			// row. It falls back to the subject-based identity (ID-01: (iss, sub) is
			// the key; email is never the key).
			Username:    sanitiseUsername(claims.Username, storeEmail, claims.Subject),
			Email:       storeEmail,
			DisplayName: claims.Name,
			Role:        string(role),
			QuotaBytes:  s.initialQuota(ctx),
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

		// ID-06 / WC-GAP-001: this is the moment the IdP vouched for the account's role and group
		// membership, so it is the moment the revalidation clock restarts. Recorded explicitly rather
		// than inferred from updated_at, because updated_at moves for reasons that have nothing to do
		// with the IdP (a quota change, an administrator renaming the account) and would silently
		// extend the window for an account nobody has re-confirmed.
		if merr := tx.MarkIdPConfirmed(ctx, u.ID); merr != nil {
			return merr
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
	for _, name := range []string{stateCookieName, pkceCookieName, nonceCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: s.hostCookieName(name), Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
		})
	}

	next := "/"
	if c, err := r.Cookie(s.hostCookieName(nextCookieName)); err == nil && isSameSitePath(c.Value) {
		next = c.Value
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// handleLogout revokes the session server-side, not just the cookie.
//
// Clearing the cookie alone leaves a valid token that anyone who copied it can
// keep using.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.hostCookieName(sessionCookieName)); err == nil && c.Value != "" {
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

// isSameSitePath reports whether a post-login redirect target stays on this origin.
//
// The historical check refused anything not starting with "/" and anything starting
// with "//". That misses "/\evil.com": browsers normalise a backslash to a slash
// before resolving, so the URL after the second character is parsed as an authority
// and the login lands on an attacker's host. The rule is deliberately stricter than
// the minimum — ANY backslash disqualifies the target, and so does a second
// character of "/" or "\" — because a same-origin redirect has no legitimate use for
// either, and a lenient parse of ambiguous input is exactly how the last one slipped
// through.
func isSameSitePath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	if strings.HasPrefix(p, "//") {
		return false
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return false
	}
	return !strings.ContainsRune(p, '\\')
}

// effectiveLoginEmail returns the address this login may store, match against
// accounts, and satisfy policy with.
//
// The IdP's email_verified claim is parsed as a pointer: nil means the IdP did not
// send the claim at all, and those deployments keep the historical behaviour — the
// address is used as before (ID-01 never makes email the account key, so the trust
// placed in it is bounded). An EXPLICIT false is different: the IdP has said the
// address was not proven, so acting on it would let whoever controls the claim
// (not the mailbox) bind a victim's address to their own identity. Such an address
// is treated as absent everywhere an email would confer something: the JIT domain
// allow-list, the UNIQUE email column, and the pre-provisioned-account claim.
func effectiveLoginEmail(email string, verified *bool) string {
	if verified != nil && !*verified {
		return ""
	}
	return email
}

// checkClaimEligibility refuses a login that would otherwise CLAIM a pre-provisioned
// account it has not earned (ID-01, ID-09).
//
// The store's byName fallback lets a first-time identity adopt the account whose
// username matches, which is how an administrator pre-creates accounts. The defect:
// whichever identity first produced the same sanitiseUsername output claimed the row,
// verified address or not. The rule now: the claim is allowed only when the IdP
// asserted an address that is not explicitly unverified (effectiveLoginEmail blanks
// those) AND it matches the account's stored address case-insensitively. A
// username-only collision refuses with the same identity_conflict answer the store
// uses for a subject mismatch, and the caller writes the login-failure audit row.
//
// This mirrors the rule lookupIdPUser enforces inside the store. It is duplicated at
// this layer on purpose: the check must run BEFORE the upsert (so nothing is bound
// and the refusal is observable in the audit trail), and the in-memory backend must
// behave exactly like PostgreSQL — "works in tests, differs in production" is the
// divergence this codebase refuses to ship.
func (s *Server) checkClaimEligibility(ctx context.Context, tx store.Tx,
	claims *auth.Claims, storeEmail string, refused *bool) error {

	candidate := sanitiseUsername(claims.Username, storeEmail, claims.Subject)
	u, err := tx.UserByUsername(ctx, candidate)
	if err != nil {
		// No such account: nothing to claim, the login provisions normally.
		return nil
	}
	if u.IdPIssuer != "" || u.IdPSubject != "" {
		// Already bound: the store's own subject-mismatch handling answers this.
		return nil
	}
	if storeEmail != "" && strings.EqualFold(u.Email, storeEmail) {
		// The identity presents the account's own verified address: the claim is the
		// documented path and proceeds.
		return nil
	}
	*refused = true
	return nil
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
		//
		// The dash-stripped subject is clamped, not sliced: an IdP is free to send a
		// sub shorter than the 12 bytes the old expression assumed, and a slice past
		// the end panics — turning that identity's every login into a 500, forever.
		trimmed := strings.ReplaceAll(subject, "-", "")
		if len(trimmed) > 12 {
			trimmed = trimmed[:12]
		}
		if trimmed == "" {
			// Empty or all-dashes subject: there is nothing to truncate, so derive
			// the segment from a hash instead. Deterministic, so the same identity
			// keeps the same namespace across logins.
			sum := sha256.Sum256([]byte(subject))
			candidate = "u" + hex.EncodeToString(sum[:])[:12]
		} else {
			candidate = "u" + trimmed
		}
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
func (s *Server) initialQuota(ctx context.Context) *int64 {
	// The runtime override wins over the config file, so an operator can change what NEW accounts get
	// without a restart. Existing accounts are untouched: that is the per-user quota endpoint, and
	// conflating the two would make "change the default" quietly rewrite people's limits.
	q := s.deps.Settings.Int(ctx, s.deps.Meta, settingKeyDefaultQuota,
		s.deps.Config.Limits.DefaultFileQuotaBytes)
	if q > 0 {
		return &q
	}
	return nil
}
