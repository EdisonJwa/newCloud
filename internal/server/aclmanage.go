package server

// ACL management API.
//
// The store and the authorizer have enforced resource grants since the previous
// round, but nothing could *create* one: an operator had to write SQL. That made the
// feature real and unusable at the same time, which is the same shape of gap as the
// missing delivery endpoint.
//
// Design constraints that shaped this:
//
//   - Granting is itself a permission (PermManagePermissions), not an owner-only
//     right. An owner who can delegate sharing can also delegate the ability to
//     delegate, and revoking it is a normal grant removal rather than a special case.
//   - A subject is named the way a person knows it — a username, an email, or a group
//     name — not a UUID. The handler resolves it, so a typo is a 404 rather than a
//     grant to a nonexistent id that silently does nothing.
//   - Every change is audited with the before and after permission sets. "Who gave
//     them access, and when" is the question this whole subsystem exists to answer.

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// aclEntryJSON is one grant as the API returns it.
type aclEntryJSON struct {
	ID        string   `json:"id"`
	Kind      string   `json:"subject_kind"`
	Subject   string   `json:"subject"`
	Display   string   `json:"display,omitempty"`
	Perms     []string `json:"permissions"`
	Inherit   bool     `json:"inherit"`
	CreatedAt string   `json:"created_at"`
}

// handleACLList returns the grants attached directly to a node.
//
// The path is used rather than an id because that is what a client browsing its own
// tree has. Direct grants only: the inherited ones are already reflected in what the
// caller can do, and listing an entire ancestor chain would bury the two rows the
// owner actually set.
func (s *Server) handleACLList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	node, cap, ok := s.resolveForManage(w, r, actor, rel)
	if !ok {
		return
	}

	var entries []store.ACLEntry
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.ACLForNode(ctx, node.ID)
		entries = got
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]aclEntryJSON, 0, len(entries))
	for _, e := range entries {
		item := aclEntryJSON{
			ID:        e.ID.String(),
			Kind:      string(e.SubjectKind()),
			Perms:     permNames(e.Perms),
			Inherit:   e.Inherit,
			CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
		// Resolve a readable name for the subject. A failure here must not hide the
		// grant: the row exists and can be revoked even if its subject was deleted.
		_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			if e.UserID != nil {
				if u, uerr := tx.UserByID(ctx, *e.UserID); uerr == nil {
					item.Subject = u.Username
					item.Display = u.Email
				}
			} else if e.GroupID != nil {
				if g, gerr := tx.GroupByID(ctx, *e.GroupID); gerr == nil {
					item.Subject = g.Name
					item.Display = g.DisplayName
				}
			}
			return nil
		})
		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":        rel,
		"permissions": cap.Perms().String(),
		"grants":      out,
	})
}

// handleACLGrant creates or replaces a grant.
func (s *Server) handleACLGrant(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path string `json:"path"`
		// Subject is a username, an email address, or a group name. The three are
		// tried in that order, which matches how an owner thinks about them.
		Subject     string   `json:"subject"`
		SubjectKind string   `json:"subject_kind"` // optional: "user" | "group"
		Permissions []string `json:"permissions"`
		Inherit     *bool    `json:"inherit"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	if len(body.Permissions) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_permissions",
			"a grant needs at least one permission")
		return
	}
	perms, unknown := decodePermissionNames(body.Permissions)
	if len(unknown) > 0 {
		// Refusing an unknown name is deliberate: silently dropping it would create a
		// grant the caller believes is wider (or narrower) than it is.
		writeError(w, http.StatusBadRequest, "invalid_permissions",
			"unknown permission: "+strings.Join(unknown, ", "))
		return
	}

	node, _, ok := s.resolveForManage(w, r, actor, body.Path)
	if !ok {
		return
	}

	inherit := true
	if body.Inherit != nil {
		inherit = *body.Inherit
	}

	entry := store.ACLEntry{
		NodeID: node.ID, Perms: perms, Inherit: inherit,
		CreatedBy: actor.Ptr(),
	}

	var subjectName string
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		// Group first when the caller said so; otherwise try user by username, then
		// by email, so "share with alice@example.com" works without the caller having
		// to know whether that is a username or an address.
		wantGroup := strings.EqualFold(body.SubjectKind, "group")

		if !wantGroup {
			if u, uerr := tx.UserByUsername(ctx, body.Subject); uerr == nil {
				entry.UserID = &u.ID
				subjectName = u.Username
				return applyGrant(ctx, tx, entry, actor, r, node.Name, subjectName, &u)
			}
			if u, uerr := tx.UserByEmail(ctx, body.Subject); uerr == nil {
				entry.UserID = &u.ID
				subjectName = u.Username
				return applyGrant(ctx, tx, entry, actor, r, node.Name, subjectName, &u)
			}
		}
		g, gerr := tx.GroupByName(ctx, body.Subject)
		if gerr == nil {
			entry.GroupID = &g.ID
			subjectName = "group:" + g.Name
			return applyGrant(ctx, tx, entry, actor, r, node.Name, subjectName, nil)
		}
		return store.ErrNotFound
	})

	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "no_such_subject",
				"no user or group matches that name")
			return
		}
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"path":        body.Path,
		"subject":     subjectName,
		"permissions": permNames(perms),
		"inherit":     inherit,
	})
}

// applyGrant stores the grant, its audit row and — when the subject is a user — the
// share.received notification, all in the caller's transaction.
//
// Extracted so the create path cannot accidentally commit a grant without the record
// that explains it — the one change an owner would otherwise be unable to account for. The
// notification rides the same transaction for the same reason: a grant the grantee never
// hears about is a folder that appears in "shared with me" with no explanation, which is
// indistinguishable from a mistake.
func applyGrant(ctx context.Context, tx store.Tx, entry store.ACLEntry,
	actor authz.Actor, r *http.Request, nodeName, subjectName string, grantee *store.User) error {

	saved, err := tx.Grant(ctx, entry)
	if err != nil {
		return err
	}
	if err := tx.Audit(ctx, store.AuditEntry{
		ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
		Action: "acl.grant", TargetKind: "node", TargetID: entry.NodeID.String(),
		Result: "ok",
		Metadata: map[string]any{
			"node":        nodeName,
			"subject":     subjectName,
			"permissions": permNames(saved.Perms),
			"inherit":     saved.Inherit,
			"entry_id":    saved.ID.String(),
		},
	}); err != nil {
		return err
	}
	// The grantee's bell, in the same transaction: see notifyShareReceived for why groups
	// stay quiet.
	return notifyShareReceived(ctx, tx, grantee, actor, saved.ID, nodeName)
}

// handleACLRevoke removes one grant by id.
func (s *Server) handleACLRevoke(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")
	grantID := parseUUID(chiURLParam(r, "id"))

	node, _, ok := s.resolveForManage(w, r, actor, rel)
	if !ok {
		return
	}

	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		// Confirm the grant belongs to this node before deleting it. Without this
		// check, a caller with manage rights on one folder could revoke a grant on
		// any other node by id — the id is unguessable, but it appears in the listing
		// of every node the caller can manage, and mistakes are not unguessable.
		entries, lerr := tx.ACLForNode(ctx, node.ID)
		if lerr != nil {
			return lerr
		}
		var found bool
		for _, e := range entries {
			if e.ID == grantID {
				found = true
				break
			}
		}
		if !found {
			return store.ErrNotFound
		}

		if rerr := tx.Revoke(ctx, grantID); rerr != nil {
			return rerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "acl.revoke", TargetKind: "node", TargetID: node.ID.String(),
			Result:   "ok",
			Metadata: map[string]any{"node": node.Name, "entry_id": grantID.String()},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveForManage authorizes the caller to change grants on a path.
//
// It requires PermManagePermissions rather than ownership so the right can be
// delegated, and it answers 404 for a denial so the endpoint cannot be used to probe
// which paths exist.
func (s *Server) resolveForManage(w http.ResponseWriter, r *http.Request,
	actor authz.Actor, rel string) (store.Node, authz.Cap, bool) {

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		s.writeStoreError(w, err)
		return store.Node{}, authz.Cap{}, false
	}
	node := nodes[len(nodes)-1]

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return store.Node{}, authz.Cap{}, false
	}
	if !cap.Can(authz.PermManagePermissions) {
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
		return store.Node{}, authz.Cap{}, false
	}
	return node, cap, true
}

// decodePermissionNames maps API strings onto the store's permission bits.
//
// The vocabulary is the one §4.1 uses and the one the ACL table stores, so a name that
// works here works in a grant row and vice versa.
func decodePermissionNames(names []string) (store.Perm, []string) {
	var (
		perms   store.Perm
		unknown []string
	)
	for _, raw := range names {
		n := strings.ToLower(strings.TrimSpace(raw))
		bit, ok := permissionByName[n]
		if !ok {
			unknown = append(unknown, raw)
			continue
		}
		perms |= bit
	}
	return perms, unknown
}

// permNames renders a stored permission set as the API vocabulary.
func permNames(p store.Perm) []string {
	out := store.EncodePerms(p)
	if out == nil {
		return []string{}
	}
	return out
}

// permissionByName is the accepted vocabulary, aligned with store.EncodePerms.
//
// "write" is accepted as the historical alias for create+overwrite, so a client written
// against the pre-ACL API keeps working; it is stored as the two fine-grained bits so
// the row says what it actually lets somebody do.
var permissionByName = map[string]store.Perm{
	"list":               store.PermListChildren,
	"read":               store.PermRead,
	"download":           store.PermDownload,
	"write":              store.PermWrite,
	"create":             store.PermCreate,
	"overwrite":          store.PermOverwrite,
	"rename":             store.PermRename,
	"move":               store.PermMove,
	"delete":             store.PermDelete,
	"share":              store.PermShare,
	"manage_permissions": store.PermManagePermissions,
	"view_history":       store.PermViewHistory,
	"read_versions":      store.PermReadVersions,
	"restore_version":    store.PermRestoreVersion,
	"upload_only":        store.PermUploadOnly,
}

var _ = uuid.Nil
