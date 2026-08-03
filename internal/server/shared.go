package server

// "Shared with me" endpoint.
//
// This is the delivery mechanism the ACL work needs. Everything else in the API
// addresses nodes by path inside the caller's own namespace — WebDAV deliberately
// discards the username segment, and the JSON handlers resolve against the caller's
// root — so a grant on another account's folder had nowhere to be used.
//
// Rather than weaken that isolation (it is what stops one account addressing another's
// tree by guessing a name), the API gains an explicit second way in: a list of the
// nodes the caller may reach, then access to each by id. Ids are unguessable UUIDs and
// every read still goes through the authorizer, so this widens what can be *found*,
// not what can be *done*.

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// handleSharedWithMe lists nodes shared with the caller.
func (s *Server) handleSharedWithMe(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var shared []store.SharedNode
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, lerr := tx.SharedWithMe(ctx, actor.ID(), s.deps.Config.Limits.MaxProfindChildren)
		shared = got
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]map[string]any, 0, len(shared))
	for _, n := range shared {
		// Re-check through the authorizer rather than trusting the listing. The two
		// queries are not in one transaction, and a grant revoked in between must not
		// produce a row the caller can see but not open.
		cap, cerr := s.deps.Authz.Authorize(actor, n.NodeID)
		if cerr != nil {
			continue
		}
		out = append(out, map[string]any{
			"id":          n.NodeID.String(),
			"name":        n.Name,
			"kind":        string(n.Kind),
			"size":        n.Size,
			"mime":        n.Mime,
			"modified_at": n.ModifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"owner_id":    n.OwnerID.String(),
			// What the caller may actually do, so the UI can hide the actions that
			// would fail. It is advisory: every action re-checks.
			"permissions": cap.Perms().String(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleSharedNode serves metadata for one node addressed by id.
//
// It exists so a shared node can be opened at all: the path-based handlers cannot
// name it, because its path lives in somebody else's namespace.
func (s *Server) handleSharedNode(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id := parseUUID(chiURLParam(r, "id"))

	var node store.Node
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, id)
		node = n
		return nerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermRead) {
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
		return
	}

	// Children, when the node is a directory and the caller may list it. Each child
	// is authorized individually: a grant that covers a folder does not automatically
	// cover a child that carries its own non-inheriting restriction.
	var children []map[string]any
	if node.IsDir() && cap.Can(authz.PermListChildren) {
		var kids []store.Node
		_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			got, cerr := tx.Children(ctx, node.ID, s.deps.Config.Limits.MaxProfindChildren)
			kids = got
			return cerr
		})
		for _, c := range kids {
			if _, cerr := s.deps.Authz.Authorize(actor, c.ID); cerr != nil {
				continue
			}
			children = append(children, map[string]any{
				"id": c.ID.String(), "name": c.Name, "kind": string(c.Kind), "size": c.Size,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          node.ID.String(),
		"name":        node.Name,
		"kind":        string(node.Kind),
		"size":        node.Size,
		"mime":        node.Mime,
		"modified_at": node.ModifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"permissions": cap.Perms().String(),
		"children":    children,
	})
}

// handleSharedContent streams a node addressed by id.
//
// This exists because `download` was, once again, a permission with no way to exercise it:
// a grantee can see a shared node's metadata through handleSharedNode, but the download
// endpoint resolves a *path* inside the caller's own namespace, so the shared file is
// unreachable by every content-serving route. Granting `download` therefore granted nothing.
//
// It is the content half of the same widening as handleSharedNode: address by unguessable
// id, authorize every request, and do not loosen the path-based isolation those routes rely
// on.
func (s *Server) handleSharedContent(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	id := parseUUID(chiURLParam(r, "id"))

	var node store.Node
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, id)
		node = n
		return nerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if node.IsDir() {
		writeError(w, http.StatusBadRequest, "is_directory", "a directory has no single content stream")
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	// The `download` primitive, not `read`: this is the copy-taking operation.
	if !cap.Can(authz.PermDownload) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	// Content is addressed by (owner, path). Reconstruct the path from the owner's root so
	// this handler can stream without knowing the caller's namespace.
	rel, perr := s.pathOfNode(r.Context(), node.OwnerID, node.ID)
	if perr != nil {
		s.writeStoreError(w, perr)
		return
	}

	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: node.OwnerID.String(), Path: rel,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer rc.Close()

	// A download from another account is exactly the event an owner would want to see.
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.download.shared", TargetKind: "node", TargetID: node.ID.String(),
			Result:   "ok",
			Metadata: map[string]any{"name": node.Name, "owner_id": node.OwnerID.String()},
		})
	})

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(node.Name))
	w.Header().Set("ETag", `"`+node.ETag+`"`)
	if node.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(node.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
