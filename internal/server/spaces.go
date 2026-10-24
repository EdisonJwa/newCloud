package server

// Group spaces: a folder that belongs to a group rather than to a person.
//
// The data layer was already in place — `nodes.owner_group_id`, `SpacesFor`, and id-addressed reads —
// so what this file adds is the two routes that make a space usable: one that creates it and grants
// the group, and one that lists the spaces the caller belongs to.
//
// A space is creator-bound, deliberately. Its root lives under the creating account's tree, because
// `nodes_root_uniq` allows one root per owner and the alternative — a synthetic non-human account per
// space — brings a login-less user row, quota accounting and an admin user list that has to hide it.
// The consequence is stated in limitations.md rather than hidden: if that account is deleted, the
// space goes with it, exactly as any other folder in that tree would.

import (
	"context"
	"errors"
	"net/http"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// spaceMemberPerms is what a space grants its group.
//
// Everything an ordinary collaborator needs and nothing that manages the space itself: a member can
// read, write, delete and list, but cannot re-grant, because a shared space whose members can
// re-share it is one nobody can reason about.
//
// The fine-grained primitives are granted explicitly, because the coarse bits no longer carry them:
// `read` deliberately does not imply `download` (internal/authz), and `write` implies only
// create/overwrite. Omitting them made a space a folder its members could see the listing of and
// could never take a file out of, rename, move, or read the history of — every content route
// answered 404 while the metadata routes worked.
const spaceMemberPerms = store.PermRead | store.PermWrite | store.PermDelete | store.PermListChildren |
	store.PermDownload | store.PermRename | store.PermMove | store.PermReadVersions

// handleSpacesList returns the group spaces the caller belongs to.
func (s *Server) handleSpacesList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var nodes []store.Node
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.SpacesFor(ctx, actor.ID())
		nodes = list
		return lerr
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]nodeJSON, 0, len(nodes))
	for _, n := range nodes {
		cap := authz.Cap{}
		if c, cerr := s.deps.Authz.Authorize(actor, n.ID); cerr == nil {
			cap = c
		}
		out = append(out, toNodeJSON(n, cap))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleSpaceCreate creates a group space and grants the group.
func (s *Server) handleSpaceCreate(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Name  string `json:"name"`
		Group string `json:"group"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" || body.Group == "" {
		writeError(w, http.StatusBadRequest, "invalid_target", "a space needs a name and a group")
		return
	}

	var space store.Node
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		g, gerr := tx.GroupByName(ctx, body.Group)
		if gerr != nil {
			return gerr
		}
		// Who may create a space for a group: its managers, and a deployment administrator. A
		// viewer cannot, or a group's shape would be decided by anyone who joined it.
		if !actor.IsAdmin() {
			members, merr := tx.GroupMembers(ctx, g.ID)
			if merr != nil {
				return merr
			}
			allowed := false
			for _, m := range members {
				if m.UserID == actor.ID() && m.Role == "manager" {
					allowed = true
					break
				}
			}
			if !allowed {
				return errNotSpaceManager
			}
		}

		root, rerr := tx.EnsureRoot(ctx, actor.ID())
		if rerr != nil {
			return rerr
		}
		n, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: actor.ID(), ParentID: root.ID, Name: body.Name, OwnerGroupID: &g.ID,
		})
		if cerr != nil {
			return cerr
		}
		space = n

		// The grant is what makes the space openable rather than merely visible: listing is by
		// membership, but every read and write goes through Authorize, which knows about grants.
		if _, gerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: n.ID, GroupID: &g.ID, Perms: spaceMemberPerms, Inherit: true,
		}); gerr != nil {
			return gerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "space.create", TargetKind: "node", TargetID: n.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": body.Name, "group": body.Group},
		})
	}); err != nil {
		if errors.Is(err, errNotSpaceManager) {
			writeError(w, http.StatusForbidden, "not_group_manager",
				"only a manager of that group, or an administrator, can create a space for it")
			return
		}
		s.writeStoreError(w, err)
		return
	}

	cap := authz.Cap{}
	if c, cerr := s.deps.Authz.Authorize(actor, space.ID); cerr == nil {
		cap = c
	}
	writeJSON(w, http.StatusCreated, toNodeJSON(space, cap))
}

// errNotSpaceManager reports a caller who is not allowed to shape a group.
var errNotSpaceManager = errors.New("server: not a manager of that group")
