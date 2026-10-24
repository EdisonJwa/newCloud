package server

// The external halves of the file routes: everything that happens below a mount point
// (ADR-0014). Each handler here is reached from a small fork at the top of its local twin
// in files.go, after mount.ResolveTarget has decided the path is external.
//
// Three rules hold across all of them, and each is enforced HERE rather than trusted to
// the providers:
//
//   - authorization is always answered against the mount point node. External subtrees
//     have no rows, so the mount root's grant (owner, ACL, group space) IS the permission
//     model — the same machinery every local file goes through.
//   - a read-only mount refuses writes after authorization, with a 404 that names the
//     reason, following the house policy that a denial is a 404, not a 403.
//   - every operation writes an audit row naming the mount, because a mount is the one
//     place this server performs actions on ANOTHER system's data.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/mount"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// extEntry builds the JSON shape from a provider entry.
func extEntry(m store.Mount, rel string, e external.Entry) externalEntryJSON {
	kind := "file"
	if e.IsDir {
		kind = "dir"
	}
	mod := ""
	if !e.ModTime.IsZero() {
		mod = e.ModTime.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return externalEntryJSON{
		ID: externalID(m, rel), Name: e.Name, Kind: kind, Size: e.Size,
		ModifiedAt: mod, IsDir: e.IsDir, Mounted: true,
	}
}

// externalEntryJSON is one remote file or directory in the SAME shape the local listing
// answers with — a client crossing a folder boundary inside one tree must not have to
// switch dialects. The id is derived (uuid SHA-1 of mount id + remote path): stable
// across listings so a UI can key on it, and deliberately not a node id — nothing below
// a mount point is addressable by node, and an id that pretended otherwise would be a
// lie the trash and version routes would eventually act on. `mounted` is the one extra
// key, so a client can label the folder as remote.
type externalEntryJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	IsDir      bool   `json:"is_dir"`
	Mounted    bool   `json:"mounted"`
}

func externalID(m store.Mount, rel string) string {
	ns := uuid.NewSHA1(uuid.NameSpaceOID, []byte("newcloud-mount:"+m.ID.String()))
	return uuid.NewSHA1(ns, []byte(rel)).String()
}

// writeExternalError maps the provider vocabulary onto statuses once.
func (s *Server) writeExternalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, external.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
	case errors.Is(err, external.ErrNotADirectory):
		writeError(w, http.StatusBadRequest, "not_a_directory", "path is a file")
	case errors.Is(err, external.ErrExists):
		writeError(w, http.StatusConflict, "conflict", "something already exists at that name")
	case errors.Is(err, external.ErrPermission):
		writeError(w, http.StatusNotFound, "not_found", "the remote refused the credentials")
	case errors.Is(err, external.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the remote listing exceeded the mount's entry limit")
	case errors.Is(err, external.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", "this mount's protocol cannot do that")
	case errors.Is(err, external.ErrUnavailable):
		writeError(w, http.StatusBadGateway, "mount_unavailable", "the mounted storage is unreachable")
	default:
		writeError(w, http.StatusInternalServerError, "mount_failed", err.Error())
	}
}

// openExternal resolves a request path into a mount target plus a live provider, after
// authorization. It is the one door every external handler goes through, which is what
// keeps the three rules above true for all of them at once.
func (s *Server) openExternal(w http.ResponseWriter, r *http.Request, need ...authz.Perm) (context.Context, mount.Target, external.Storage, bool) {
	actor, _ := actorFrom(r.Context())
	tgt, err := mount.ResolveTarget(r.Context(), s.deps.Meta, actor.ID(), r.URL.Query().Get("path"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
			return nil, mount.Target{}, nil, false
		}
		s.writeStoreError(w, err)
		return nil, mount.Target{}, nil, false
	}
	if !tgt.External {
		writeError(w, http.StatusInternalServerError, "internal", "path is not external")
		return nil, mount.Target{}, nil, false
	}
	cap, err := s.deps.Authz.Authorize(actor, tgt.Node().ID)
	if err != nil {
		s.writeStoreError(w, err)
		return nil, mount.Target{}, nil, false
	}
	for _, p := range need {
		if !cap.Can(p) {
			writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
			return nil, mount.Target{}, nil, false
		}
	}
	st, err := s.deps.Mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		s.writeExternalError(w, err)
		return nil, mount.Target{}, nil, false
	}
	return r.Context(), tgt, st, true
}

// refuseReadOnly answers the write-refusal for a read-only mount. It is called after
// authorization, so the caller has already established the actor may see the mount at all.
func refuseReadOnly(w http.ResponseWriter, m store.Mount) bool {
	if m.ReadOnly {
		writeError(w, http.StatusNotFound, "read_only_mount",
			"this mount is read-only; the remote is never modified through it")
		return true
	}
	return false
}

func (s *Server) auditExternal(r *http.Request, m store.Mount, action, rel, result string, bytes int64) {
	actor, _ := actorFrom(r.Context())
	entry := store.AuditEntry{
		ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
		Action: action, TargetKind: "mount", TargetID: m.ID.String(),
		Result:   result,
		Metadata: map[string]any{"via": "mount", "path": rel, "mount_name": m.Name},
	}
	if bytes >= 0 {
		b := bytes
		entry.Bytes = &b
	}
	_ = s.deps.Meta.WithTx(context.WithoutCancel(r.Context()), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, entry)
	})
}

// handleExternalList lists a remote directory through the same shape as handleList.
func (s *Server) handleExternalList(w http.ResponseWriter, r *http.Request, rel string) {
	ctx, tgt, st, ok := s.openExternal(w, r, authz.PermListChildren)
	if !ok {
		return
	}
	if st, err := st.Stat(ctx, rel); err == nil && !st.IsDir {
		writeError(w, http.StatusBadRequest, "not_a_directory", "path is a file")
		return
	} else if err != nil && !errors.Is(err, external.ErrNotFound) {
		// A missing dir is indistinguishable from an empty one through List; Stat
		// decides which, so an outage cannot masquerade as either.
		s.writeExternalError(w, err)
		return
	}
	entries, err := st.List(ctx, rel)
	if err != nil {
		s.writeExternalError(w, err)
		return
	}
	if len(entries) > s.deps.Mounts.MaxList() {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("the remote directory has more than %d entries", s.deps.Mounts.MaxList()))
		return
	}
	after := r.URL.Query().Get("cursor")
	out := make([]externalEntryJSON, 0, len(entries))
	for _, e := range entries {
		if after != "" && e.Name <= after {
			continue
		}
		out = append(out, extEntry(tgt.Mount, joinRel(rel, e.Name), e))
	}
	truncated := false
	limit := parseLimit(r.URL.Query().Get("limit"), s.deps.Config.Limits.MaxProfindChildren)
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	next := ""
	if truncated && len(out) > 0 {
		next = out[len(out)-1].Name
	}
	s.auditExternal(r, tgt.Mount, "file.read", rel, "ok", -1)
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path.Join("/", tgt.Node().Name, rel), "name": path.Base(rel),
		"children": out, "cursor": next, "truncated": truncated,
	})
}

// handleExternalDownload streams a remote file. Range is not offered: the providers read
// whole entities, and a Range header on a route that cannot honour it is a lie.
func (s *Server) handleExternalDownload(w http.ResponseWriter, r *http.Request, rel string) {
	ctx, tgt, st, ok := s.openExternal(w, r, authz.PermRead, authz.PermDownload)
	if !ok {
		return
	}
	entry, err := st.Stat(ctx, rel)
	if err != nil {
		s.writeExternalError(w, err)
		return
	}
	if entry.IsDir {
		writeError(w, http.StatusBadRequest, "not_a_directory", "path is a directory")
		return
	}
	rc, err := st.Open(ctx, rel)
	if err != nil {
		s.writeExternalError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(entry.Name))
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	s.auditExternal(r, tgt.Mount, "file.read", rel, "ok", entry.Size)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handleExternalUpload writes one multipart upload into a mount.
func (s *Server) handleExternalUpload(w http.ResponseWriter, r *http.Request, dirRel string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST the file")
		return
	}
	_, tgt, st, ok := s.openExternalWithPath(w, r, dirRel, authz.PermCreate)
	if !ok {
		return
	}
	if refuseReadOnly(w, tgt.Mount) {
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the upload needs a file part")
		return
	}
	defer file.Close()
	name := path.Base(header.Filename)
	if err := layout.ValidateSegment(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "the file needs a usable name")
		return
	}
	// The remote path is below the mount root: tgt.Rel, never the caller's full path
	// (which still carries the mount point's own name).
	rel := joinRel(tgt.Rel, name)

	// Overwriting an existing remote name needs the overwrite permission, the same split
	// the local upload honours (§4.2): a collaborator may be allowed to add and not to
	// replace.
	if _, serr := st.Stat(r.Context(), rel); serr == nil {
		// Overwriting a remote name needs the overwrite bit (§4.2), checked through the
		// same door every other check uses.
		if _, _, _, ok := s.openExternalWithPath(w, r, dirRel, authz.PermOverwrite); !ok {
			return
		}
	} else if !errors.Is(serr, external.ErrNotFound) {
		s.writeExternalError(w, serr)
		return
	}

	limit := s.deps.Mounts.MaxWrite(s.maxFileBytes(r.Context()))
	if header.Size > 0 && header.Size > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			"mounted storage accepts files up to the configured mount limit")
		return
	}
	written, err := st.Create(r.Context(), rel, io.LimitReader(file, limit+1))
	if err != nil {
		s.writeExternalError(w, err)
		return
	}
	if written > limit {
		// The stream was capped mid-flight; the remote may hold a truncated file, which
		// the error names rather than hiding.
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the upload exceeded the mount's file limit")
		return
	}
	s.auditExternal(r, tgt.Mount, "file.write", rel, "ok", written)
	writeJSON(w, http.StatusCreated, extEntry(tgt.Mount, rel, external.Entry{
		Name: name, Size: written, ModTime: time.Now(),
	}))
}

// openExternalWithPath is openExternal for the body-carried paths the write routes use
// (upload/mkdir take their directory in the body or form, not the query).
func (s *Server) openExternalWithPath(w http.ResponseWriter, r *http.Request, dirRel string, need ...authz.Perm) (context.Context, mount.Target, external.Storage, bool) {
	actor, _ := actorFrom(r.Context())
	tgt, err := mount.ResolveTarget(r.Context(), s.deps.Meta, actor.ID(), dirRel)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such directory")
			return nil, mount.Target{}, nil, false
		}
		s.writeStoreError(w, err)
		return nil, mount.Target{}, nil, false
	}
	if !tgt.External {
		writeError(w, http.StatusInternalServerError, "internal", "path is not external")
		return nil, mount.Target{}, nil, false
	}
	cap, err := s.deps.Authz.Authorize(actor, tgt.Node().ID)
	if err != nil {
		s.writeStoreError(w, err)
		return nil, mount.Target{}, nil, false
	}
	for _, p := range need {
		if !cap.Can(p) {
			writeError(w, http.StatusNotFound, "not_found", "no such directory")
			return nil, mount.Target{}, nil, false
		}
	}
	st, err := s.deps.Mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		s.writeExternalError(w, err)
		return nil, mount.Target{}, nil, false
	}
	return r.Context(), tgt, st, true
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// handleExternalMkdir creates one remote directory.
func (s *Server) handleExternalMkdir(w http.ResponseWriter, r *http.Request, dirRel, name string) {
	if err := layout.ValidateSegment(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "the folder needs a usable name")
		return
	}
	_, tgt, st, ok := s.openExternalWithPath(w, r, dirRel, authz.PermCreate)
	if !ok {
		return
	}
	if refuseReadOnly(w, tgt.Mount) {
		return
	}
	rel := joinRel(tgt.Rel, name)
	if err := st.Mkdir(r.Context(), rel); err != nil {
		s.writeExternalError(w, err)
		return
	}
	s.auditExternal(r, tgt.Mount, "file.write", rel, "ok", 0)
	writeJSON(w, http.StatusCreated, extEntry(tgt.Mount, rel, external.Entry{
		Name: name, IsDir: true,
	}))
}

// handleExternalRename renames within one remote directory. The target arrives from the
// fork (which resolved the body's path); the mount point itself refuses renaming — its
// name is the mount's identity in listings and audit rows.
func (s *Server) handleExternalRename(w http.ResponseWriter, r *http.Request, tgt mount.Target, newName string) {
	if err := layout.ValidateSegment(newName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "the new name is not usable")
		return
	}
	oldRel := tgt.Rel
	actor, _ := actorFrom(r.Context())
	cap, err := s.deps.Authz.Authorize(actor, tgt.Node().ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermRename) {
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
		return
	}
	if refuseReadOnly(w, tgt.Mount) {
		return
	}
	st, err := s.deps.Mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		s.writeExternalError(w, err)
		return
	}
	dir := path.Dir(oldRel)
	if dir == "." {
		dir = ""
	}
	newRel := joinRel(dir, newName)
	if err := st.Rename(r.Context(), oldRel, newRel); err != nil {
		s.writeExternalError(w, err)
		return
	}
	s.auditExternal(r, tgt.Mount, "file.move", oldRel+" -> "+newRel, "ok", -1)
	writeJSON(w, http.StatusOK, extEntry(tgt.Mount, newRel, external.Entry{Name: newName}))
}

// handleExternalMove handles the cross-boundary refusal and the same-mount rename.
func (s *Server) externalMoveDecision(r *http.Request, srcRel, dstRel string) (mount.Target, mount.Target, error) {
	actor, _ := actorFrom(r.Context())
	srcTgt, err := mount.ResolveTarget(r.Context(), s.deps.Meta, actor.ID(), srcRel)
	if err != nil {
		return mount.Target{}, mount.Target{}, err
	}
	// A move's destination usually does not exist yet; that is not an error, it is the
	// common case, and it means the destination is wherever its PARENT lives. The walk
	// reports the parent's answer through the same ErrNotFound a missing file gives,
	// so resolve the parent and decide from there.
	dstTgt, err := mount.ResolveTarget(r.Context(), s.deps.Meta, actor.ID(), dstRel)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return mount.Target{}, mount.Target{}, err
		}
		parent := path.Dir(dstRel)
		if parent == "." {
			parent = ""
		}
		dstTgt, err = mount.ResolveTarget(r.Context(), s.deps.Meta, actor.ID(), parent)
		if err != nil {
			return mount.Target{}, mount.Target{}, err
		}
	}
	if srcTgt.External != dstTgt.External || (srcTgt.External && srcTgt.Mount.ID != dstTgt.Mount.ID) {
		return mount.Target{}, mount.Target{}, errCrossStorage
	}
	if !srcTgt.External {
		return mount.Target{}, mount.Target{}, errNotExternal
	}
	return srcTgt, dstTgt, nil
}

var (
	errCrossStorage = errors.New("move crosses a storage boundary")
	errNotExternal  = errors.New("move is not external")
)

func (s *Server) handleExternalMove(w http.ResponseWriter, r *http.Request, srcRel, dstRel string) {
	srcTgt, dstTgt, err := s.externalMoveDecision(r, srcRel, dstRel)
	switch {
	case errors.Is(err, errCrossStorage):
		writeError(w, http.StatusConflict, "cross_storage",
			"moving between a mount and local storage would copy the bytes; copy them deliberately or move them on the remote")
		return
	case err != nil:
		s.writeStoreError(w, err)
		return
	}
	actor, _ := actorFrom(r.Context())
	cap, err := s.deps.Authz.Authorize(actor, srcTgt.Node().ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermMove) || !cap.Can(authz.PermDelete) {
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
		return
	}
	if refuseReadOnly(w, srcTgt.Mount) {
		return
	}
	st, err := s.deps.Mounts.Storage(r.Context(), srcTgt.Mount)
	if err != nil {
		s.writeExternalError(w, err)
		return
	}
	if err := st.Rename(r.Context(), srcTgt.Rel, dstTgt.Rel); err != nil {
		s.writeExternalError(w, err)
		return
	}
	s.auditExternal(r, srcTgt.Mount, "file.move", srcTgt.Rel+" -> "+dstTgt.Rel, "ok", -1)
	writeJSON(w, http.StatusOK, map[string]any{"moved": true})
}

// handleExternalDelete deletes remotely, immediately, without a trash. The response says
// so: a user must not discover after the fact that "delete" meant something stronger here.
func (s *Server) handleExternalDelete(w http.ResponseWriter, r *http.Request, rel string) {
	ctx, tgt, st, ok := s.openExternal(w, r, authz.PermDelete)
	if !ok {
		return
	}
	_ = ctx
	if refuseReadOnly(w, tgt.Mount) {
		return
	}
	if _, err := st.Stat(r.Context(), rel); err != nil {
		s.writeExternalError(w, err)
		return
	}
	if err := st.Remove(r.Context(), rel, true); err != nil {
		s.writeExternalError(w, err)
		return
	}
	s.auditExternal(r, tgt.Mount, "file.delete", rel, "ok", -1)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "trash": "none",
		"note": "mounted storage has no trash; the remote deletion was immediate"})
}

// externalPath answers "does this request's path live below a mount point?" It is the
// fork every local handler consults before resolving: false means the local path continues
// exactly as before (including its own 404 for paths that do not exist), so the mount
// surface cannot change a single local behaviour.
func (s *Server) externalPath(r *http.Request, p string) (mount.Target, bool) {
	if s.deps.Mounts == nil || !s.deps.Mounts.Enabled() {
		return mount.Target{}, false
	}
	actor, _ := actorFrom(r.Context())
	tgt, err := mount.ResolveTarget(r.Context(), s.deps.Meta, actor.ID(), p)
	if err != nil || !tgt.External {
		return mount.Target{}, false
	}
	return tgt, true
}

// mountPointChildren answers the id-addressed children question for a mount point: its
// children are the remote's root, live, not an empty folder. Wired into
// handleSharedNode/handleNodeChildren so a grantee with `list` on a granted mount point
// browses the remote's first level through the same door they browse every other shared
// folder. Deeper levels need a path-in-mount surface, which does not exist yet — see
// limitations.md (ADR-0014 consequences).
func (s *Server) mountPointChildren(r *http.Request, node store.Node) ([]map[string]any, bool, error) {
	if s.deps.Mounts == nil || !s.deps.Mounts.Enabled() {
		return nil, false, nil
	}
	m, err := mount.MountAt(r.Context(), s.deps.Meta, node.ID)
	if err != nil {
		return nil, false, nil // not a mount point: the ordinary children path continues
	}
	st, derr := s.deps.Mounts.Storage(r.Context(), m)
	if derr != nil {
		return nil, true, derr
	}
	entries, lerr := st.List(r.Context(), "")
	if lerr != nil {
		return nil, true, lerr
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"id": externalID(m, e.Name), "name": e.Name,
			"kind": "file", "size": e.Size, "mounted": true,
		})
		if e.IsDir {
			out[len(out)-1]["kind"] = "dir"
		}
	}
	s.auditExternal(r, m, "file.read", "", "ok", -1)
	return out, true, nil
}
