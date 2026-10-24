package server

// Admin group management.
//
// Group sharing (SHARE-08) was supported at every layer except the one that matters: the store
// creates groups and members, the authorizer expands a group grant into its members, and the ACL API
// already accepts "share this with group:engineering". Nothing could create a group or put anybody in
// one, so Grant to a group answered "no user or group matches that name" for every group that could
// possibly exist. A whole sharing mode was reachable in code and unreachable in the product.
//
// These endpoints are admin-only, and every change is audited as group.member.change, which retires
// an action the vocabulary ratchet had pinned as never emitted.

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

type groupJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Members     []string `json:"members,omitempty"`
}

// groupMemberJSON is one membership row, resolved to the account it names.
type groupMemberJSON struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
}

// groupWithMembersJSON is a group and everyone in it.
type groupWithMembersJSON struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name"`
	Members     []groupMemberJSON `json:"members"`
}

// handleAdminListGroups lists every group and its members.
//
// Group creation and membership existed as write-only endpoints: an operator could create a group
// and put somebody in it, and could not then find out what existed or who was in it. That is the
// same shape as a permission with no route that exercises it — the capability was there and the
// product could not show it, which is precisely the class of gap the review counted as "backend
// implemented, unreachable".
func (s *Server) handleAdminListGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	out := make([]groupWithMembersJSON, 0, 8)
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		groups, gerr := tx.Groups(ctx)
		if gerr != nil {
			return gerr
		}
		out = make([]groupWithMembersJSON, 0, len(groups))
		for _, g := range groups {
			members, merr := tx.GroupMembers(ctx, g.ID)
			if merr != nil {
				return merr
			}
			entry := groupWithMembersJSON{
				ID: g.ID.String(), Name: g.Name, DisplayName: g.DisplayName,
				Members: make([]groupMemberJSON, 0, len(members)),
			}
			for _, m := range members {
				u, uerr := tx.UserByID(ctx, m.UserID)
				if uerr != nil {
					// A membership row whose account no longer resolves is not a reason to fail the
					// whole listing: this screen is for looking, and the other groups are still
					// readable. The row is simply not shown.
					continue
				}
				entry.Members = append(entry.Members, groupMemberJSON{
					ID: u.ID.String(), Username: u.Username, Email: u.Email, Role: m.Role,
				})
			}
			out = append(out, entry)
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// handleAdminCreateGroup creates a group, or returns the existing one with its id.
//
// Idempotent by name, matching CreateGroup: a sync job that creates the same group twice should not
// have to distinguish "created" from "already there".
func (s *Server) handleAdminCreateGroup(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "a group name is required")
		return
	}

	var g store.Group
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		created, cerr := tx.CreateGroup(ctx, store.Group{
			Name: body.Name, DisplayName: body.DisplayName,
		})
		if cerr != nil {
			return cerr
		}
		g = created
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "group.create", TargetKind: "group", TargetID: created.ID.String(),
			Result:   "ok",
			Metadata: map[string]any{"name": created.Name},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, groupJSON{ID: g.ID.String(), Name: g.Name, DisplayName: g.DisplayName})
}

// handleAdminAddGroupMember puts an account into a group, or updates its role.
func (s *Server) handleAdminAddGroupMember(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	groupID, err := uuid.Parse(chiURLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return
	}
	var body struct {
		Username string `json:"username"`
		UserID   string `json:"user_id"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var group store.Group
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		g, gerr := tx.GroupByID(ctx, groupID)
		if gerr != nil {
			return gerr
		}
		group = g
		var u store.User
		if body.UserID != "" {
			id, perr := uuid.Parse(body.UserID)
			if perr != nil {
				return store.ErrNotFound
			}
			got, uerr := tx.UserByID(ctx, id)
			if uerr != nil {
				return uerr
			}
			u = got
		} else {
			got, uerr := tx.UserByUsername(ctx, body.Username)
			if uerr != nil {
				return uerr
			}
			u = got
		}
		if aerr := tx.AddGroupMember(ctx, g.ID, u.ID, body.Role); aerr != nil {
			return aerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "group.member.change", TargetKind: "group", TargetID: g.ID.String(),
			Result: "ok",
			Metadata: map[string]any{
				"change": "add", "group": g.Name,
				"member": u.Username, "member_id": u.ID.String(), "role": body.Role,
			},
		})
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such group or account")
			return
		}
		s.writeStoreError(w, err)
		return
	}
	// The response is the group as the list endpoint would return it, not just the name that was
	// added. Two shapes for one concept is how a client ends up needing two parsers, and the screen
	// that called this is about to redraw the group anyway.
	full, ferr := s.groupWithMembers(r.Context(), group.ID)
	if ferr != nil {
		s.writeStoreError(w, ferr)
		return
	}
	writeJSON(w, http.StatusOK, full)
}

// groupWithMembers resolves one group and the accounts in it.
func (s *Server) groupWithMembers(ctx context.Context, id uuid.UUID) (groupWithMembersJSON, error) {
	out := groupWithMembersJSON{Members: []groupMemberJSON{}}
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		g, gerr := tx.GroupByID(ctx, id)
		if gerr != nil {
			return gerr
		}
		out = groupWithMembersJSON{
			ID: g.ID.String(), Name: g.Name, DisplayName: g.DisplayName,
			Members: []groupMemberJSON{},
		}
		members, merr := tx.GroupMembers(ctx, g.ID)
		if merr != nil {
			return merr
		}
		for _, m := range members {
			u, uerr := tx.UserByID(ctx, m.UserID)
			if uerr != nil {
				continue
			}
			out.Members = append(out.Members, groupMemberJSON{
				ID: u.ID.String(), Username: u.Username, Email: u.Email, Role: m.Role,
			})
		}
		return nil
	})
	return out, err
}

// handleAdminRemoveGroupMember takes an account out of a group.
func (s *Server) handleAdminRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	groupID, err := uuid.Parse(chiURLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return
	}
	userID, err := uuid.Parse(chiURLParam(r, "userID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such account")
		return
	}

	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		g, gerr := tx.GroupByID(ctx, groupID)
		if gerr != nil {
			return gerr
		}
		u, uerr := tx.UserByID(ctx, userID)
		if uerr != nil {
			return uerr
		}
		if rerr := tx.RemoveGroupMember(ctx, g.ID, u.ID); rerr != nil {
			return rerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "group.member.change", TargetKind: "group", TargetID: g.ID.String(),
			Result: "ok",
			Metadata: map[string]any{
				"change": "remove", "group": g.Name,
				"member": u.Username, "member_id": u.ID.String(),
			},
		})
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such group or account")
			return
		}
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
