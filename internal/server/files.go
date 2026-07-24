package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// fileJSON is the wire shape for a node.
//
// It deliberately exposes no owner or internal id beyond what the UI needs: a
// client that can name a node id can attempt to fetch it, and the fewer ids are
// handed out the smaller that surface is.
type fileJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	Mime       string `json:"mime,omitempty"`
	ETag       string `json:"etag,omitempty"`
	ModifiedAt string `json:"modified_at"`
	CreatedAt  string `json:"created_at"`
	Trashed    bool   `json:"trashed,omitempty"`
}

func toFileJSON(n store.Node, rel string) fileJSON {
	return fileJSON{
		ID: n.ID.String(), Name: n.Name,
		Path: path.Join("/", rel), Kind: string(n.Kind), Size: n.Size,
		Mime: n.Mime, ETag: n.ETag,
		ModifiedAt: n.ModifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CreatedAt:  n.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Trashed:    n.TrashedAt != nil,
	}
}

// handleList lists a directory.
//
// The path comes from a query parameter rather than the URL so that names with
// slashes and spaces do not need re-encoding by every client.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	dir := nodes[len(nodes)-1]
	if !dir.IsDir() {
		writeError(w, http.StatusBadRequest, "not_a_directory", "path is a file")
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, dir.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermListChildren) {
		writeError(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}

	var children []store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		c, cerr := tx.Children(ctx, dir.ID, s.deps.Config.Limits.MaxProfindChildren)
		children = c
		return cerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]fileJSON, 0, len(children))
	for _, c := range children {
		out = append(out, toFileJSON(c, path.Join(rel, c.Name)))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":     path.Join("/", rel),
		"name":     dir.Name,
		"children": out,
		// The cap is returned so the UI can say "showing the first N" instead of
		// silently presenting a truncated directory.
		"truncated": len(children) >= s.deps.Config.Limits.MaxProfindChildren,
	})
}

// handleMkdir creates a directory.
func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	parentRel, name := splitParent(body.Path)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "a name is required")
		return
	}

	parentNodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), parentRel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	parent := parentNodes[len(parentNodes)-1]

	cap, err := s.deps.Authz.Authorize(actor, parent.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeError(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}

	var created store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: actor.ID(), ParentID: parent.ID, Name: name,
		})
		if cerr != nil {
			return cerr
		}
		created = n
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: n.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": name, "kind": "dir"},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The tree layout needs the directory on disk; blob layout has nothing to
	// create. Asking the store keeps both correct without branching here.
	_ = s.deps.Content.Mkdir(r.Context(), layout.Node{
		Owner: actor.ID().String(), Path: body.Path, IsDir: true,
	})

	writeJSON(w, http.StatusCreated, toFileJSON(created, body.Path))
}

// handleUpload stores one file.
//
// Multipart is used rather than a raw body so the browser can send a filename
// without relying on custom headers, which are awkward to set from a form.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	// Cap the request before reading: a client that lies about its size would
	// otherwise fill the disk while we stream.
	r.Body = http.MaxBytesReader(w, r.Body, s.deps.Config.Limits.MaxFileBytes)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload", "could not parse the upload")
		return
	}

	dirRel := r.FormValue("path")
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload", "no file in the request")
		return
	}
	defer file.Close()

	name := path.Base(header.Filename)
	if name == "" || name == "." || name == "/" {
		writeError(w, http.StatusBadRequest, "invalid_name", "the file needs a name")
		return
	}

	parentNodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), dirRel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	parent := parentNodes[len(parentNodes)-1]
	if !parent.IsDir() {
		writeError(w, http.StatusBadRequest, "not_a_directory", "target is a file")
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, parent.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeError(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}

	rel := path.Join(dirRel, name)
	res, err := s.deps.Content.Put(r.Context(), layout.Node{
		Owner: actor.ID().String(), Path: rel,
	}, file, header.Size)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	etag := etagFor(res.Hash)
	mime := header.Header.Get("Content-Type")
	if mime == "" || strings.HasPrefix(mime, "application/octet-stream") {
		mime = guessType(name)
	}

	var node store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		existing, cerr := tx.ChildByName(ctx, parent.ID, name)
		if cerr == nil {
			n, uerr := tx.UpdateFile(ctx, store.UpdateFile{
				NodeID: existing.ID, Size: res.Size, Mime: mime, ETag: etag,
				BlobID: s.deps.Meta.BlobIDFor(r.Context(), res.Hash), BlobHash: res.Hash,
				KeepVersion: true, MaxVersions: s.maxVersions(),
			})
			if uerr != nil {
				return uerr
			}
			node = n
		} else {
			n, cerr2 := tx.CreateFile(ctx, store.NewFile{
				OwnerID: actor.ID(), ParentID: parent.ID, Name: name,
				Size: res.Size, Mime: mime, ETag: etag,
				BlobID: s.deps.Meta.BlobIDFor(r.Context(), res.Hash), BlobHash: res.Hash,
			})
			if cerr2 != nil {
				return cerr2
			}
			node = n
		}
		bytes := res.Size
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Bytes: &bytes, Metadata: map[string]any{"name": name},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, toFileJSON(node, rel))
}

// handleDownload streams a file for the web UI.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]
	if node.IsDir() {
		writeError(w, http.StatusBadRequest, "is_directory", "use the archive endpoint for directories")
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermRead) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: actor.ID().String(), Path: rel,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", contentTypeFor(node))
	w.Header().Set("Content-Disposition", contentDisposition(node.Name))
	w.Header().Set("Content-Length", strconv.FormatInt(node.Size, 10))
	w.Header().Set("ETag", node.ETag)
	w.Header().Set("Accept-Ranges", "bytes")

	if _, err := io.Copy(w, rc); err != nil {
		// The client disconnected or the disk failed; headers are already sent so
		// the best we can do is log it.
		s.deps.Log.Warn("download interrupted", "path", rel, "error", err)
		return
	}

	bytes := node.Size
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.read", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Bytes: &bytes, Metadata: map[string]any{"name": node.Name},
		})
	})
}

// handleRename renames a file or directory in place.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), body.Path)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]
	if node.ParentID == nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "cannot rename the root")
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	var renamed store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, rerr := tx.Rename(ctx, node.ID, body.Name)
		if rerr != nil {
			return rerr
		}
		renamed = n
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.move", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"from": node.Name, "to": body.Name},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	parentRel, _ := splitParent(body.Path)
	// The tree layout mirrors names on disk; blob layout treats this as metadata.
	_ = s.deps.Content.Move(r.Context(),
		layout.Node{Owner: actor.ID().String(), Path: body.Path, IsDir: node.IsDir()},
		layout.Node{Owner: actor.ID().String(), Path: path.Join(parentRel, body.Name), IsDir: node.IsDir()})

	writeJSON(w, http.StatusOK, toFileJSON(renamed, path.Join(parentRel, body.Name)))
}

// handleMove reparents a file or directory.
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), body.From)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	parentRel, name := splitParent(body.To)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "a target name is required")
		return
	}

	parentNodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), parentRel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	parent := parentNodes[len(parentNodes)-1]

	parentCap, err := s.deps.Authz.Authorize(actor, parent.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !parentCap.Can(authz.PermWrite) {
		writeError(w, http.StatusNotFound, "not_found", "no such target directory")
		return
	}

	var moved store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n := node
		if n.Name != name {
			renamed, rerr := tx.Rename(ctx, node.ID, name)
			if rerr != nil {
				return rerr
			}
			n = renamed
		}
		if node.ParentID == nil || *node.ParentID != parent.ID {
			movedNode, merr := tx.Move(ctx, node.ID, parent.ID)
			if merr != nil {
				return merr
			}
			n = movedNode
		}
		moved = n
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.move", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"from": body.From, "to": body.To},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	_ = s.deps.Content.Move(r.Context(),
		layout.Node{Owner: actor.ID().String(), Path: body.From, IsDir: node.IsDir()},
		layout.Node{Owner: actor.ID().String(), Path: body.To, IsDir: node.IsDir()})

	writeJSON(w, http.StatusOK, toFileJSON(moved, body.To))
}

// handleDelete moves a node to the trash.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]
	if node.ParentID == nil {
		writeError(w, http.StatusBadRequest, "invalid_target", "cannot delete the root")
		return
	}

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermDelete) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		if terr := tx.Trash(ctx, node.ID); terr != nil {
			return terr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.delete", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": node.Name},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTrashList lists the trash.
//
// Only the roots of deleted subtrees are returned: a deleted folder and its contents
// are one item, because restoring is recursive and listing the children separately
// would offer an operation that cannot be performed on its own.
func (s *Server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var nodes []store.Node
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.Trashed(ctx, actor.ID(), s.deps.Config.Limits.MaxProfindChildren)
		nodes = list
		return lerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]fileJSON, 0, len(nodes))
	for _, n := range nodes {
		item := toFileJSON(n, path.Join("/", n.Name))
		item.ID = n.ID.String()
		item.Trashed = true
		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleRestore un-deletes a node and its subtree.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id := parseUUID(body.ID)

	var restored store.Node
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, rerr := tx.Restore(ctx, id)
		if rerr != nil {
			return rerr
		}
		restored = n
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.restore", TargetKind: "node", TargetID: id.String(),
			Result: "ok", Metadata: map[string]any{"name": n.Name},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toFileJSON(restored, restored.Name))
}

// handleVersions lists a file's history.
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermReadVersions) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	var versions []store.Version
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		v, verr := tx.Versions(ctx, node.ID, 50)
		versions = v
		return verr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		out = append(out, map[string]any{
			"version":    v.VersionNo,
			"size":       v.Size,
			"created_at": v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":     path.Join("/", rel),
		"versions": out,
		// The layout decides how much history is affordable: tree stores a full
		// copy per version, blob stores only the differences.
		"cheap_versions": s.deps.Content.Capabilities().CheapVersions,
	})
}

// handleRestoreVersion makes an old version current again.
func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path    string `json:"path"`
		Version int    `json:"version"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), body.Path)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]

	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermRestoreVersion) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	var restored store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, rerr := tx.RestoreVersion(ctx, node.ID, body.Version)
		if rerr != nil {
			return rerr
		}
		restored = n
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.restore", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"version": body.Version},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toFileJSON(restored, body.Path))
}

// maxVersions adapts retention to the layout: the tree layout pays for a full
// copy per version, the blob layout only for the differences.
func (s *Server) maxVersions() int {
	if s.deps.Content.Capabilities().CheapVersions {
		return 10
	}
	return 3
}

// writeStoreError maps metadata and storage errors onto HTTP status codes.
//
// A denied authorization maps to 404, never 403: telling a caller that a path
// exists but is off-limits is a free enumeration oracle.
func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, authz.ErrDenied), errors.Is(err, authz.ErrNotFound),
		errors.Is(err, store.ErrNotFound), errors.Is(err, layout.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "something with that name already exists")
	case errors.Is(err, store.ErrQuota):
		writeError(w, http.StatusInsufficientStorage, "quota_exceeded", "your storage quota is full")
	case errors.Is(err, store.ErrTooLarge), errors.Is(err, layout.ErrSizeMismatch):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the file is larger than allowed")
	case errors.Is(err, store.ErrInvalidName), errors.Is(err, layout.ErrUnsafePath),
		errors.Is(err, layout.ErrInvalidNode):
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, store.ErrCycle):
		writeError(w, http.StatusConflict, "cycle", "cannot move a directory into itself")
	default:
		s.deps.Log.Error("unhandled error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}

// splitParent splits "a/b/c" into ("a/b", "c").
func splitParent(p string) (parent, name string) {
	p = strings.Trim(p, "/")
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	// Cap the body: a JSON endpoint has no legitimate reason to accept megabytes.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := jsonDecode(r.Body, dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not parse the request body")
		return false
	}
	return true
}

var _ = uuid.Nil
