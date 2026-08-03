package server

// Break-glass local administrator.
//
// The configuration for this has existed since the first migration: a username, an
// Argon2 hash file, and an allow-list of source networks. Nothing ever read it — the
// password file was validated at startup and then never opened, so an operator who
// configured a break-glass account had a setting that did nothing. That is worse than
// not offering the feature: it is a recovery path people count on and would discover
// was fictional during an outage.
//
// What makes this account different from an OIDC login:
//
//   - It is a local password, so it is the one path that works when the IdP is down.
//   - It is restricted to configured source networks. Without that it is a
//     password-protected root login reachable from the internet, which the config
//     validation already refuses to start with.
//   - It is deliberately narrow: the account gets the admin role for *metadata and
//     recovery*, and the authorizer's admin branch never grants content reads. A
//     break-glass session can disable an account, fix a quota, and audit — it cannot
//     browse users' files.
//   - Every attempt, successful or not, is audited. This is the account whose use
//     should be reviewed after the fact.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/password"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// breakglassUsername is the account record a break-glass login acts as.
//
// It is a real user row rather than a synthetic actor so its sessions, audit rows and
// timestamps behave like everything else's, and so an operator can see it in the user
// list and disable it.
const breakglassUsername = "breakglass"

// handleBreakglassLogin authenticates the local administrator.
//
// The endpoint is registered unconditionally but refuses unless the feature is
// configured, so a deployment that never enables it does not advertise a login path
// that does not exist.
func (s *Server) handleBreakglassLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Auth

	if !cfg.AllowLocalAdmin {
		// 404, not 403: the same rule as everywhere else. A distinguishable refusal
		// tells a prober that a local admin exists on this deployment.
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}

	// Network restriction first, before touching the password. Brute force should not
	// even reach the KDF from outside the allow-list.
	if !s.breakglassAllowedFrom(r) {
		s.auditBreakglass(r, "", "network_denied")
		writeError(w, http.StatusForbidden, "not_allowed", "this address may not use local login")
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Compare the username in constant time as well. It is not secret, but answering
	// faster for a wrong username than for a right one is a free hint.
	if !strings.EqualFold(strings.TrimSpace(body.Username), cfg.LocalAdmin.Username) {
		s.auditBreakglass(r, body.Username, "unknown_user")
		writeError(w, http.StatusUnauthorized, "login_failed", "invalid credentials")
		return
	}

	ok, err := s.verifyBreakglassPassword(body.Password)
	if err != nil {
		// A malformed or unreadable hash file is an operator error, and it must be
		// loud: silently refusing every login would look like a wrong password.
		s.deps.Log.Error("break-glass hash could not be read; local login is unavailable",
			"file", cfg.LocalAdmin.PasswordFile, "error", err)
		s.auditBreakglass(r, body.Username, "hash_unreadable")
		writeError(w, http.StatusServiceUnavailable, "misconfigured",
			"the local administrator credential is not usable")
		return
	}
	if !ok {
		s.auditBreakglass(r, body.Username, "bad_password")
		writeError(w, http.StatusUnauthorized, "login_failed", "invalid credentials")
		return
	}

	// Provision or find the account and start the session.
	var user store.User
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			// The issuer string marks this identity as local rather than IdP-backed.
			// It shares the unique (issuer, subject) key with everything else, so a
			// real IdP subject can never collide with it.
			Issuer:      "local://breakglass",
			Subject:     breakglassUsername,
			Username:    breakglassUsername,
			Email:       "",
			DisplayName: "Break-glass administrator",
			Role:        string(config.RoleAdmin),
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
			Action: "auth.breakglass.used", TargetKind: "user", TargetID: u.ID.String(),
			Result: "ok", Metadata: map[string]any{"username": breakglassUsername},
		})
	})
	if err != nil {
		s.deps.Log.Error("break-glass provisioning failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not start a session")
		return
	}

	token, err := s.deps.Sessions.Mint(r.Context(), user.ID, user.Email, clientIP(r), r.UserAgent())
	if err != nil {
		s.deps.Log.Error("break-glass session mint failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not start a session")
		return
	}
	s.setSessionCookie(w, token)

	s.deps.Log.Warn("break-glass login used; review this event",
		"ip", clientIP(r), "user_agent", r.UserAgent())

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"role":          user.Role,
		// Stated in the response because it is a property of the account, not a
		// policy the client should have to infer: this session can audit and
		// administer, and cannot read user content.
		"content_access": false,
	})
}

// breakglassAllowedFrom reports whether the request's peer address is permitted.
//
// RemoteAddr is used rather than a forwarded header: this process cannot verify a
// header it did not set, and an attacker-supplied X-Forwarded-For would defeat the one
// restriction protecting the most powerful account in the deployment. A proxy in front
// of it must therefore be configured to set the peer address, or the allow-list must
// include the proxy.
func (s *Server) breakglassAllowedFrom(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	for _, cidr := range s.deps.Config.Auth.LocalAdmin.AllowedCIDRs {
		_, network, perr := net.ParseCIDR(strings.TrimSpace(cidr))
		if perr != nil {
			// Config validation already rejected malformed entries, so reaching this
			// means the list changed after startup. Skipping is the safe reading: an
			// entry that cannot be parsed must not grant access.
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// verifyBreakglassPassword checks a presented password against the configured hash.
//
// The file is read on every attempt rather than cached at startup. That is deliberate:
// rotating the break-glass credential during an incident must not require a restart —
// the whole point of the account is that it is used when normal operations are already
// broken. The cost is one file read on a path taken a handful of times a year.
func (s *Server) verifyBreakglassPassword(secret string) (bool, error) {
	if secret == "" {
		return false, nil
	}
	raw, err := readFileTrimmed(s.deps.Config.Auth.LocalAdmin.PasswordFile)
	if err != nil {
		return false, err
	}
	return password.VerifyHex(secret, raw, password.Params{
		// The break-glass hash is generated with the production cost, and there is no
		// test-mode variant for it: a cheap parameter here would weaken the one
		// credential that bypasses the IdP.
		Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32,
	})
}

// auditBreakglass records an attempt.
//
// Best-effort: an audit failure must not turn a refused login into a 500, and a
// successful login is already recorded inside the provisioning transaction.
func (s *Server) auditBreakglass(r *http.Request, username, reason string) {
	_ = s.deps.Meta.WithTx(context.WithoutCancel(r.Context()), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", ActorEmail: username,
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "auth.breakglass.denied", TargetKind: "user",
			Result: "denied", Metadata: map[string]any{"reason": reason},
		})
	})
}

// errBreakglassDisabled is returned by the startup self-check when the feature is
// configured but unusable; it is never returned to a client.
var errBreakglassDisabled = errors.New("server: break-glass login is not configured")

// readFileTrimmed reads a small text file and trims surrounding whitespace.
//
// It lives here rather than using os.ReadFile directly because every secret file this
// process reads (SMTP password, OIDC client secret, break-glass hash) is produced by
// an editor or a shell redirect, and all three end in a newline.
func readFileTrimmed(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
