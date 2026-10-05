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

	var member store.User
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
		member = u
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
	writeJSON(w, http.StatusOK, groupJSON{
		ID: group.ID.String(), Name: group.Name, DisplayName: group.DisplayName,
		Members: []string{member.Username},
	})
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
