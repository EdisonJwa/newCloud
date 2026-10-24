package dav

// The external halves of the WebDAV methods (ADR-0014): a URL below a mount point has no
// node to resolve against, so the answer comes from the remote, live.
//
// The fork lives in ServeHTTP, before the local handlers, and only when a mount service
// is wired and the feature is on: without it the method table is byte-for-byte what it
// was. Authorization is answered against the mount point node — the ACL machinery this
// server already runs — and the permission split mirrors the JSON API's (`download` for
// reads, `create`/`overwrite` for writes) so the two surfaces cannot drift apart.
//
// What is deliberately NOT here:
//   - LOCK/UNLOCK below a mount: the lock register fences local writers; a remote with
//     its own writers cannot be fenced by a local token. Answered 501/403, honestly.
//   - PROPPATCH: dead properties are stored on node rows; below a mount there are none.
//   - COPY out of a mount and MOVE across a boundary: both are copies wearing another
//     verb's name, and copying is exactly what a mount promises not to do silently.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/mount"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// WithMounts wires the external-mount service. Nil or disabled keeps the handler
// entirely local, which is also the zero-value behaviour of every test that does not
// call this.
func (h *Handler) WithMounts(s *mount.Service) *Handler {
	h.mounts = s
	return h
}

// externalTarget resolves the request URL to a mount target, authorized. ok=false means
// "not external, continue locally" — including every error a local path can still answer
// for itself, so a disabled or unwired surface cannot change local behaviour.
func (h *Handler) externalTarget(ctx context.Context, a authz.Actor, urlPath string) (mount.Target, authz.Cap, bool) {
	if h.mounts == nil || !h.mounts.Enabled() {
		return mount.Target{}, authz.Cap{}, false
	}
	_, rel, err := h.splitPath(urlPath)
	if err != nil {
		return mount.Target{}, authz.Cap{}, false
	}
	tgt, err := mount.ResolveTarget(ctx, h.meta, a.ID(), rel)
	if err != nil || !tgt.External {
		return mount.Target{}, authz.Cap{}, false
	}
	cap, err := h.authz.Authorize(a, tgt.Node().ID)
	if err != nil {
		return mount.Target{}, authz.Cap{}, false
	}
	return tgt, cap, true
}

// mapExternalError answers the provider vocabulary once, DAV-shaped.
func (h *Handler) mapExternalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, external.ErrNotFound):
		writeXMLError(w, http.StatusNotFound, "")
	case errors.Is(err, external.ErrNotADirectory):
		writeXMLError(w, http.StatusConflict, "not-a-collection")
	case errors.Is(err, external.ErrExists):
		writeXMLError(w, http.StatusPreconditionFailed, "")
	case errors.Is(err, external.ErrPermission):
		writeXMLError(w, http.StatusNotFound, "")
	case errors.Is(err, external.ErrTooLarge):
		writeXMLError(w, http.StatusRequestEntityTooLarge, "")
	case errors.Is(err, external.ErrUnsupported):
		writeXMLError(w, http.StatusNotImplemented, "")
	case errors.Is(err, external.ErrUnavailable):
		// A dead remote is a gateway failure, and must not look like an empty folder.
		writeXMLError(w, http.StatusBadGateway, "mount-unavailable")
	default:
		writeXMLError(w, http.StatusInternalServerError, "")
	}
}

func (h *Handler) auditExternal(ctx context.Context, a authz.Actor, m store.Mount, action, rel, result string, bytes int64) {
	entry := store.AuditEntry{
		ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
		Action: action, TargetKind: "mount", TargetID: m.ID.String(),
		Result: result, AuthMethod: "app_password",
		Metadata: map[string]any{"via": "mount", "path": rel, "mount_name": m.Name},
	}
	if bytes >= 0 {
		b := bytes
		entry.Bytes = &b
	}
	_ = h.meta.WithTx(context.WithoutCancel(ctx), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, entry)
	})
}

func hrefFor(a authz.Actor, mountName, rel string) string {
	return "/dav/" + a.Username() + "/" + mountName + "/" + rel
}

func (h *Handler) handleExternalPropfind(w http.ResponseWriter, r *http.Request, a authz.Actor, tgt mount.Target, cap authz.Cap) {
	if !cap.Can(authz.PermListChildren) && !cap.Can(authz.PermRead) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	st, err := h.mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	ctx := r.Context()
	target, err := st.Stat(ctx, tgt.Rel)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<D:multistatus xmlns:D="DAV:">` + "\n")
	writeExtEntry := func(rel string, e external.Entry) {
		href := hrefFor(a, tgt.Node().Name, rel)
		b.WriteString(`<D:response><D:href>` + xmlText(href) + `</D:href>`)
		b.WriteString(`<D:propstat><D:prop>`)
		if e.IsDir {
			b.WriteString(`<D:resourcetype><D:collection/></D:resourcetype>`)
		} else {
			b.WriteString(`<D:resourcetype/>`)
			fmt.Fprintf(&b, `<D:getcontentlength>%d</D:getcontentlength>`, e.Size)
		}
		if !e.ModTime.IsZero() {
			fmt.Fprintf(&b, `<D:getlastmodified>%s</D:getlastmodified>`, e.ModTime.UTC().Format(http.TimeFormat))
		}
		b.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` + "\n")
	}
	writeExtEntry(tgt.Rel, target)
	if depth(r) != "0" && target.IsDir {
		entries, err := st.List(ctx, tgt.Rel)
		if err != nil {
			// Headers are sent; the honest remaining signal is a response-status entry.
			b.WriteString(`</D:multistatus>`)
			io.WriteString(w, b.String())
			return
		}
		for _, e := range entries {
			child := e.Name
			if tgt.Rel != "" {
				child = tgt.Rel + "/" + e.Name
			}
			writeExtEntry(child, e)
		}
	}
	b.WriteString(`</D:multistatus>` + "\n")
	io.WriteString(w, b.String())
	h.auditExternal(ctx, a, tgt.Mount, "file.read", tgt.Rel, "ok", -1)
}

// xmlText escapes the handful of characters that would break an XML round trip. Kept
// local because encoding/xml's escaping of quotes differs from what DAV clients expect
// inside hrefs.
func xmlText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func depth(r *http.Request) string {
	d := strings.ToLower(strings.TrimSpace(r.Header.Get("Depth")))
	if d == "" {
		return "infinity"
	}
	return d
}

func (h *Handler) handleExternalGet(w http.ResponseWriter, r *http.Request, a authz.Actor, tgt mount.Target, cap authz.Cap, headOnly bool) {
	if !cap.Can(authz.PermRead) || !cap.Can(authz.PermDownload) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	st, err := h.mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	entry, err := st.Stat(r.Context(), tgt.Rel)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	if entry.IsDir {
		writeXMLError(w, http.StatusConflict, "not-a-file")
		return
	}
	rc, err := st.Open(r.Context(), tgt.Rel)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Last-Modified", entry.ModTime.UTC().Format(http.TimeFormat))
	h.auditExternal(r.Context(), a, tgt.Mount, "file.read", tgt.Rel, "ok", entry.Size)
	if headOnly {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (h *Handler) handleExternalPut(w http.ResponseWriter, r *http.Request, a authz.Actor, tgt mount.Target, cap authz.Cap) {
	if tgt.Mount.ReadOnly {
		writeXMLError(w, http.StatusForbidden, "read-only-mount")
		return
	}
	st, err := h.mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	// A name that already exists remotely needs the overwrite bit; a new one needs
	// create — the same split the local PUT honours.
	if existing, serr := st.Stat(r.Context(), tgt.Rel); serr == nil && !existing.IsDir {
		if !cap.Can(authz.PermOverwrite) {
			writeXMLError(w, http.StatusNotFound, "")
			return
		}
	} else if serr != nil && !errors.Is(serr, external.ErrNotFound) {
		h.mapExternalError(w, serr)
		return
	} else if serr != nil && !cap.Can(authz.PermCreate) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	if r.ContentLength > h.mounts.MaxWrite(h.cfg.Limits.MaxFileBytes) {
		writeXMLError(w, http.StatusRequestEntityTooLarge, "")
		return
	}
	written, err := st.Create(r.Context(), tgt.Rel, r.Body)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	h.auditExternal(r.Context(), a, tgt.Mount, "file.write", tgt.Rel, "ok", written)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) handleExternalMkcol(w http.ResponseWriter, r *http.Request, a authz.Actor, tgt mount.Target, cap authz.Cap) {
	if tgt.Mount.ReadOnly {
		writeXMLError(w, http.StatusForbidden, "read-only-mount")
		return
	}
	if !cap.Can(authz.PermCreate) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	if r.ContentLength > 0 {
		writeXMLError(w, http.StatusUnsupportedMediaType, "")
		return
	}
	st, err := h.mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	if err := st.Mkdir(r.Context(), tgt.Rel); err != nil {
		h.mapExternalError(w, err)
		return
	}
	h.auditExternal(r.Context(), a, tgt.Mount, "file.write", tgt.Rel, "ok", 0)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) handleExternalDelete(w http.ResponseWriter, r *http.Request, a authz.Actor, tgt mount.Target, cap authz.Cap) {
	if tgt.Rel == "" {
		// The mount point itself is not deletable through the file surface; unmounting
		// is a mount operation and never touches the remote's data.
		writeXMLError(w, http.StatusForbidden, "is-a-mount-point")
		return
	}
	if tgt.Mount.ReadOnly {
		writeXMLError(w, http.StatusForbidden, "read-only-mount")
		return
	}
	if !cap.Can(authz.PermDelete) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	st, err := h.mounts.Storage(r.Context(), tgt.Mount)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	if err := st.Remove(r.Context(), tgt.Rel, true); err != nil {
		h.mapExternalError(w, err)
		return
	}
	h.auditExternal(r.Context(), a, tgt.Mount, "file.delete", tgt.Rel, "ok", -1)
	w.WriteHeader(http.StatusNoContent)
}

// handleExternalMove decides the three MOVE shapes below or across mounts.
func (h *Handler) handleExternalMove(w http.ResponseWriter, r *http.Request, a authz.Actor, src mount.Target, cap authz.Cap) {
	_, dstRel, err := h.splitPath(destPathOf(r))
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}
	dstTgt, derr := mount.ResolveTarget(r.Context(), h.meta, a.ID(), dstRel)
	if derr != nil {
		writeXMLError(w, http.StatusConflict, "")
		return
	}
	if !dstTgt.External || dstTgt.Mount.ID != src.Mount.ID {
		// Cross-storage: a copy wearing a move's name. Refused, with the reason in the
		// body rather than only in a log.
		writeXMLError(w, http.StatusConflict, "cross-storage-move")
		return
	}
	if src.Mount.ReadOnly {
		writeXMLError(w, http.StatusForbidden, "read-only-mount")
		return
	}
	if !cap.Can(authz.PermMove) || !cap.Can(authz.PermDelete) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	st, err := h.mounts.Storage(r.Context(), src.Mount)
	if err != nil {
		h.mapExternalError(w, err)
		return
	}
	if err := st.Rename(r.Context(), src.Rel, dstTgt.Rel); err != nil {
		h.mapExternalError(w, err)
		return
	}
	h.auditExternal(r.Context(), a, src.Mount, "file.move", src.Rel+" -> "+dstTgt.Rel, "ok", -1)
	w.WriteHeader(http.StatusCreated)
}

// destPathOf extracts the Destination header's path; the local MOVE validates it fully,
// so this is the minimal read the fork needs.
func destPathOf(r *http.Request) string {
	dest := r.Header.Get("Destination")
	if dest == "" {
		return ""
	}
	// Strip scheme://host if present.
	if i := strings.Index(dest, "://"); i >= 0 {
		rest := dest[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			dest = rest[j:]
		}
	}
	return path.Clean(dest)
}
