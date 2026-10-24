package server

// Browsing by node id, not by path.
//
// Every other read route takes a `path`, which only ever names the caller's own tree: resolution is
// `Resolve(actor.ID(), rel)`, so a folder in somebody else's namespace is unaddressable even when
// the caller has been granted access to it. That is the gap that makes a group space impossible to
// build today — a member can be granted permissions on it and still have no way to reach it — and it
// is also why "shared with me" can only be browsed through the share-token routes.
//
// Authorization here is by node, through the same Authorize used by every other route, so a caller
// without a grant gets the same 404 as one asking about an id that does not exist. An id is not a
// capability; the check is.

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// nodeJSON is the shape returned for a node addressed by id.
//
// Deliberately not fileJSON: that one carries a path, and a path is exactly what the caller does not
// have here. The id is the handle, and the parent id is what lets a client walk upwards.
type nodeJSON struct {
	ID         string `json:"id"`
	ParentID   string `json:"parent_id,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	Mime       string `json:"mime,omitempty"`
	ETag       string `json:"etag,omitempty"`
	ModifiedAt string `json:"modified_at"`
	IsDir      bool   `json:"is_dir"`
	CanRead    bool   `json:"can_read"`
	CanWrite   bool   `json:"can_write"`
	CanDelete  bool   `json:"can_delete"`
	CanList    bool   `json:"can_list"`
}

func toNodeJSON(n store.Node, cap authz.Cap) nodeJSON {
	out := nodeJSON{
		ID: n.ID.String(), Name: n.Name, Size: n.Size, Mime: n.Mime,
		ETag: n.ETag, ModifiedAt: n.ModifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Kind: "file", IsDir: n.IsDir(),
		CanRead: cap.Can(authz.PermRead), CanWrite: cap.Can(authz.PermWrite),
		CanDelete: cap.Can(authz.PermDelete), CanList: cap.Can(authz.PermListChildren),
	}
	if n.IsDir() {
		out.Kind = "dir"
	}
	if n.ParentID != nil {
		out.ParentID = n.ParentID.String()
	}
	return out
}

// handleNodeMeta returns one node addressed by id.
func (s *Server) handleNodeMeta(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	cap, cerr := s.deps.Authz.Authorize(actor, id)
	if cerr != nil {
		// Same answer for "forbidden" and "absent": an id is guessable, but whether it exists is
		// not the caller's business.
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	var node store.Node
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, id)
		node = n
		return nerr
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeJSON(node, cap))
}

// handleNodeChildren lists a node's children by id.
func (s *Server) handleNodeChildren(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	cap, cerr := s.deps.Authz.Authorize(actor, id)
	if cerr != nil || !cap.Can(authz.PermListChildren) {
		writeError(w, http.StatusNotFound, "not_found", "no such item")
		return
	}

	limit := s.deps.Config.Limits.MaxProfindChildren
	cursor := r.URL.Query().Get("cursor")
	truncated := false
	next := ""
	var kids []store.Node
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.ChildrenAfter(ctx, id, cursor, limit+1)
		if lerr != nil {
			return lerr
		}
		if len(got) > limit {
			truncated = true
			got = got[:limit]
		}
		kids = got
		if truncated && len(got) > 0 {
			next = got[len(got)-1].Name
		}
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "the cursor is not one this server issued")
			return
		}
		s.writeStoreError(w, err)
		return
	}

	out := make([]nodeJSON, 0, len(kids))
	for _, k := range kids {
		// Each child gets its own permission set: a grant on the parent does not automatically
		// describe what the caller may do to a child whose grants differ.
		childCap := cap
		if c, err := s.deps.Authz.Authorize(actor, k.ID); err == nil {
			childCap = c
		}
		out = append(out, toNodeJSON(k, childCap))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        id.String(),
		"children":  out,
		"truncated": truncated,
		"cursor":    next,
	})
}
