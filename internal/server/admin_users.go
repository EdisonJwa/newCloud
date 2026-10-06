package server

// Account administration API.
//
// The provisioning policy (ID-04) creates accounts in `pending` on purpose so somebody can
// approve them — and until now there was no operation anywhere that could do it. A user
// provisioned in that mode was permanently locked out, and the only remedy was editing the
// database by hand. This closes that loop, and in doing so adds the account-administration
// surface the requirements also ask for (ID-08).
//
// Access control: every handler here requires the admin role, checked through the same
// authorizer as everything else rather than by looking at the session's role string. That
// keeps one answer to "is this actor an administrator" instead of two that can disagree.

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// userJSON is one account as the admin API returns it.
type userJSON struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	QuotaBytes  int64  `json:"quota_bytes"`
	UsedBytes   int64  `json:"used_bytes"`
	// IsBreakglass is exposed because it changes what the account can do: it bypasses the
	// IdP, and an operator auditing the account list should be able to see it.
	IsBreakglass bool   `json:"is_breakglass"`
	CreatedAt    string `json:"created_at"`
}

func toUserJSON(u store.User) userJSON {
	return userJSON{
		ID: u.ID.String(), Username: u.Username, Email: u.Email,
		DisplayName: u.DisplayName, Role: u.Role, Status: u.Status,
		QuotaBytes: u.QuotaBytes, UsedBytes: u.UsedBytes,
		IsBreakglass: u.IsBreakglass,
		CreatedAt:    u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// requireAdmin answers 404 for a non-admin.
//
// Not 403: the project's rule is that a distinguishable refusal tells a caller that an
// admin-only surface exists and that they are not on it. The same 404 as an unknown path
// makes the endpoint invisible.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (authz.Actor, bool) {
	actor, _ := actorFrom(r.Context())
	if !actor.IsAdmin() {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return authz.Actor{}, false
	}
	return actor, true
}

// handleAdminUsers lists accounts, optionally filtered by status.
//
// The filter exists because the only reason to call this is usually "show me the pending
// ones" — an administrator approving accounts should not have to page through everybody.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	status := r.URL.Query().Get("status")
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	var users []store.User
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.ListUsers(ctx, status, limit)
		users = got
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, toUserJSON(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleAdminSetUserStatus is the approval / disable / re-enable operation.
//
// One endpoint rather than three verbs: approving, disabling and re-enabling are the same
// state change with a different target value, and three endpoints would be three places to
// forget to audit or to forget to revoke credentials.
//
// The complete policy for an inactive account (ID-08), each part enforced where the request
// arrives rather than by a sweep that could lag:
//
//	sessions      refused on every request: the middleware re-reads the account and requires
//	              status "active", so a session minted just before the change dies with it
//	app passwords refused by the WebDAV entry point, which checks the same status
//	share links   suspended, not deleted: every public entry point answers 404 while the owner
//	              is inactive, and the links work again if the account is re-enabled
//	files         retained, ownership unchanged; an administrator can list them but not read
//	              their content, so disabling does not quietly hand one user's files to another
//	audit         retained; the events of an inactive account are exactly what an investigation
//	              needs, and the hash chain makes deleting them detectable
//
// Nothing is destroyed, because disabling is reversible. Deletion is a separate, deliberate act
// that this system does not offer through the API yet.
func (s *Server) handleAdminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}

	target := parseUUID(chiURLParam(r, "id"))

	var body struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var (
		user   store.User
		change store.UserStatusChange
	)
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		c, cerr := tx.SetUserStatus(ctx, target, body.Status)
		if cerr != nil {
			return cerr
		}
		change = c
		u, uerr := tx.UserByID(ctx, target)
		if uerr != nil {
			return uerr
		}
		user = u

		// The audit row names the change and the credentials it cut off. "Who approved this
		// account" and "who cut off this user's sessions" are the questions an
		// administrator has to be able to answer afterwards.
		if aerr := tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "user.status.change", TargetKind: "user", TargetID: target.String(),
			Result: "ok",
			Metadata: map[string]any{
				"username":              u.Username,
				"from":                  change.From,
				"to":                    change.To,
				"changed":               change.Changed,
				"sessions_revoked":      change.SessionsRevoked,
				"app_passwords_revoked": change.AppPasswordsRevoked,
			},
		}); aerr != nil {
			return aerr
		}
		// The revocation gets its own event as well as the count above.
		//
		// auth.session.revoke was declared in the vocabulary and emitted nowhere, so filtering for
		// it returned nothing whether or not any session had been cut off. An operator asking "was
		// this session revoked, and by what" needs one event they can filter, not a field buried in
		// the metadata of several unrelated causes.
		return auditSessionRevocation(ctx, tx, r, target, u.Username,
			change.SessionsRevoked, "admin.status", string(change.To))
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user":                  toUserJSON(user),
		"changed":               change.Changed,
		"sessions_revoked":      change.SessionsRevoked,
		"app_passwords_revoked": change.AppPasswordsRevoked,
	})
}

// handleAdminDeleteUser applies the account-removal policy (ID-08).
//
// Deletion is irreversible, so it demands an explicit confirmation in the body rather than
// being a DELETE on a URL an operator might hit by accident. The two-step flow (disable, then
// delete) is enforced in the store, so a slip here fails rather than destroying an active
// account.
func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	target := parseUUID(chiURLParam(r, "id"))

	var body struct {
		// Confirm must equal the account's username. Typing it is the point: it makes the
		// request impossible to issue without having identified the account.
		Confirm string `json:"confirm_username"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Confirm == "" {
		writeError(w, http.StatusBadRequest, "confirmation_required",
			"set confirm_username to the account's username")
		return
	}

	var (
		result   store.DeleteUserResult
		username string
	)
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		// The confirmation is checked inside the transaction, against the row being deleted,
		// so it cannot be satisfied by a name read at a different moment.
		u, uerr := tx.UserByID(ctx, target)
		if uerr != nil {
			return uerr
		}
		username = u.Username
		if body.Confirm != u.Username {
			return errConfirmationMismatch
		}

		res, derr := tx.DeleteUser(ctx, target)
		if derr != nil {
			return derr
		}
		result = res

		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "user.delete", TargetKind: "user", TargetID: target.String(),
			Result: "ok",
			Metadata: map[string]any{
				"username":              username,
				"nodes_trashed":         result.NodesTrashed,
				"shares_revoked":        result.SharesRevoked,
				"acl_grants_removed":    result.ACLGrantsRemoved,
				"acl_grants_authored":   result.ACLGrantsAuthored,
				"sessions_revoked":      result.SessionsRevoked,
				"app_passwords_revoked": result.AppPasswordsRevoked,
				"locks_released":        result.LocksReleased,
				"audit_rows_kept":       result.AuditRowsKept,
			},
		})
	})
	if err == errConfirmationMismatch {
		writeError(w, http.StatusBadRequest, "confirmation_mismatch",
			"confirm_username does not match the account")
		return
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":               true,
		"username":              username,
		"nodes_trashed":         result.NodesTrashed,
		"shares_revoked":        result.SharesRevoked,
		"acl_grants_removed":    result.ACLGrantsRemoved,
		"acl_grants_authored":   result.ACLGrantsAuthored,
		"sessions_revoked":      result.SessionsRevoked,
		"app_passwords_revoked": result.AppPasswordsRevoked,
		"locks_released":        result.LocksReleased,
		// Stated explicitly because it is the policy decision an operator is most likely to
		// wonder about: the audit history is not deleted with the account.
		"audit_rows_kept": result.AuditRowsKept,
	})
}

// errConfirmationMismatch is a sentinel so the handler can answer 400 rather than 500.
var errConfirmationMismatch = errors.New("server: confirmation does not match")

// handleAdminSetQuota changes one account's byte limit.
//
// limits.default_file_quota_bytes is applied once, at provisioning. Until this endpoint there was no
// way to change it afterwards: an operator whose user hit the limit could edit the database or tell
// the user to delete files, and the second is not a policy. §4 makes quotas an administrative
// control, so changing one is an administrative act, and it is audited as its own event rather than
// inferred from a login.
func (s *Server) handleAdminSetQuota(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	target := parseUUID(chiURLParam(r, "id"))
	var body struct {
		Bytes int64 `json:"bytes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Bytes < 0 {
		writeError(w, http.StatusBadRequest, "invalid_quota", "a quota cannot be negative")
		return
	}

	var (
		before store.User
		after  store.User
	)
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		cur, uerr := tx.UserByID(ctx, target)
		if uerr != nil {
			return uerr
		}
		before = cur
		if serr := tx.SetUserQuota(ctx, target, body.Bytes); serr != nil {
			return serr
		}
		updated, uerr := tx.UserByID(ctx, target)
		if uerr != nil {
			return uerr
		}
		after = updated
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "user.quota.change", TargetKind: "user", TargetID: target.String(),
			Result: "ok",
			Metadata: map[string]any{
				"username":   after.Username,
				"from_bytes": before.QuotaBytes,
				"to_bytes":   after.QuotaBytes,
				"used_bytes": after.UsedBytes,
				"over_quota": after.QuotaBytes > 0 && after.UsedBytes > after.QuotaBytes,
			},
		})
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such account")
			return
		}
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         after.ID.String(),
		"quota":      after.QuotaBytes,
		"used_bytes": after.UsedBytes,
	})
}
