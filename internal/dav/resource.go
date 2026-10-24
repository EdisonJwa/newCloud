package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/dircommit"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// handleGet serves a file, with Range support.
//
// The whole body goes through our own endpoint rather than a signed storage URL:
// that is what makes "who downloaded this" recordable, and what lets a download
// counter be exact. A presigned URL to the object store would make those events
// invisible.
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request, a authz.Actor, headOnly bool) {
	cap, node, err := h.resolvePath(r.Context(), a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !cap.Can(authz.PermRead) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	if node.IsDir() {
		// GET on a collection is not defined; clients that try get a 405 like a
		// web server would return.
		w.Header().Set("Allow", allowHeader())
		http.Error(w, "is a collection", http.StatusMethodNotAllowed)
		return
	}

	_, rel, _ := h.splitPath(r.URL.Path)
	owner := a.ID().String()

	rc, err := h.content.Open(r.Context(), layout.Node{
		Owner: owner, Path: rel, IsDir: false,
	})
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	defer rc.Close()

	etag := node.ETag
	if etag == "" {
		etag = fmt.Sprintf(`"%x"`, node.ModifiedAt.UnixNano())
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", node.ModifiedAt.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", contentTypeFor(node))
	// Content is never rendered as a document, even on this origin: the download
	// path is the same one the content origin uses.
	w.Header().Set("Content-Disposition", contentDisposition(node.Name))

	// Conditional requests let a sync client avoid re-downloading unchanged files.
	if h.notModified(r, etag, node.ModifiedAt) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Range handling: a single range is the common case (resume, video seek).
	// Multipart ranges are legal but clients that need them are rare, so they
	// fall back to the whole entity, which is always allowed.
	if rng := r.Header.Get("Range"); rng != "" && !headOnly {
		if start, end, ok := parseSingleRange(rng, node.Size); ok {
			if r.Header.Get("If-Range") != "" && !ifRangeMatches(r, etag, node.ModifiedAt) {
				// The resource changed since the client last saw it: serve the
				// whole thing rather than a range of the new content.
			} else {
				w.Header().Set("Content-Range",
					fmt.Sprintf("bytes %d-%d/%d", start, end, node.Size))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.WriteHeader(http.StatusPartialContent)
				if _, err := rc.Seek(start, io.SeekStart); err != nil {
					return
				}
				_, _ = io.CopyN(w, rc, end-start+1)
				h.afterDownload(r, cap, node)
				return
			}
		}
	}

	w.Header().Set("Content-Length", strconv.FormatInt(node.Size, 10))
	w.WriteHeader(http.StatusOK)
	if headOnly {
		return
	}
	_, _ = io.Copy(w, rc)
	h.afterDownload(r, cap, node)
}

// afterDownload records the read. It is best-effort: a failure to record must not
// fail a download that already streamed to the client.
func (h *Handler) afterDownload(r *http.Request, cap authz.Cap, n store.Node) {
	bytes := n.Size
	_ = h.meta.WithTx(context.WithoutCancel(r.Context()), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: cap.Actor().Ptr(),
			ActorEmail: cap.Actor().Email(), ActorIP: clientIP(r),
			UserAgent: r.UserAgent(),
			Action:    "file.read", TargetKind: "node", TargetID: n.ID.String(),
			Result: "ok", Bytes: &bytes,
			Metadata: map[string]any{"name": n.Name},
		})
	})
}

// notModified evaluates If-None-Match and If-Modified-Since.
func (h *Handler) notModified(r *http.Request, etag string, mod time.Time) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		return etagMatches(inm, etag)
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil {
			return !mod.Truncate(time.Second).After(t)
		}
	}
	return false
}

func ifRangeMatches(r *http.Request, etag string, mod time.Time) bool {
	ir := r.Header.Get("If-Range")
	if strings.HasPrefix(ir, `"`) {
		return ir == etag
	}
	if t, err := http.ParseTime(ir); err == nil {
		return !mod.Truncate(time.Second).After(t)
	}
	return false
}

// etagMatches handles the comma-separated list and the * wildcard.
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// parseSingleRange parses "bytes=start-end".
//
// It returns ok=false for anything it does not handle (multiple ranges, malformed,
// unsatisfiable) so the caller serves the full entity, which is always a valid
// response.
func parseSingleRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(header[len(prefix):])
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}

	switch {
	case first == "": // suffix range: last N bytes
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true

	case last == "": // open-ended
		s, err := strconv.ParseInt(first, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, false
		}
		return s, size - 1, true

	default:
		s, err1 := strconv.ParseInt(first, 10, 64)
		e, err2 := strconv.ParseInt(last, 10, 64)
		if err1 != nil || err2 != nil || s < 0 || e < s || s >= size {
			return 0, 0, false
		}
		if e >= size {
			e = size - 1
		}
		return s, e, true
	}
}

func contentTypeFor(n store.Node) string {
	if n.Mime != "" {
		return n.Mime
	}
	return guessType(n.Name)
}

// contentDisposition forces a download with a name that survives non-ASCII.
//
// RFC 5987 encoding is required: without it, a Chinese filename arrives as
// mojibake in every browser.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	encoded := urlPathEscape(name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, encoded)
}

// urlPathEscape percent-encodes for the filename* parameter.
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// handlePut stores a file body.
//
// Order matters: the bytes are published to the content store first, then the
// metadata row is created in a transaction that also writes the audit record. If
// the transaction loses a race (the name was taken meanwhile), the blob is left
// unreferenced and the refcount sweep collects it — cheaper than holding a
// database transaction open for the duration of a multi-gigabyte upload.
func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	ctx := r.Context()

	// The parent must exist and be writable; PUT does not create directories.
	_, rel, _ := h.splitPath(r.URL.Path)
	if rel == "" {
		writeXMLError(w, http.StatusConflict, "")
		return
	}
	parentRel, name := path.Split(strings.TrimSuffix(rel, "/"))
	parentRel = strings.TrimSuffix(parentRel, "/")

	parentCap, parentNode, err := h.resolvePath(ctx, a, "/dav/"+a.Username()+"/"+parentRel)
	if err != nil || !parentNode.IsDir() {
		writeXMLError(w, http.StatusConflict, "")
		return
	}
	if !parentCap.Can(authz.PermWrite) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	// Did this path exist before? That decides create-vs-update, which matters three ways:
	// the status code, whether a version is kept, and which lock rule applies.
	var existing *store.Node
	_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, parentNode.ID, name)
		if cerr == nil {
			existing = &n
		}
		return nil
	})

	// Locks (§5.3, RFC 4918 §7.4). Which ancestor rule applies depends on whether this PUT adds
	// a name or replaces content:
	//
	//   - a new child is a membership change, so a Depth: 0 lock on the parent blocks it;
	//   - an existing child is content, and only a Depth: infinity ancestor lock reaches it.
	//
	// A lock on the target itself blocks either way, because it protects that resource's bytes.
	if existing != nil {
		if !h.requireUnlocked(w, ctx, a, existing.ID, r.Header.Get("If")) {
			return
		}
		if h.lockAncestorsInfinityOnly(ctx, a, parentNode.ID, r.Header.Get("If")) {
			writeXMLError(w, http.StatusLocked, "lock-token-submitted")
			return
		}
	} else if h.lockAncestors(ctx, a, parentNode.ID, r.Header.Get("If")) {
		writeXMLError(w, http.StatusLocked, "lock-token-submitted")
		return
	}

	// The declared length is refused cheaply. A chunked body declares nothing
	// (ContentLength is -1), so the same limit is enforced on the read itself below —
	// without that second half, `Transfer-Encoding: chunked` was an unlimited write.
	if r.ContentLength > h.cfg.Limits.MaxFileBytes {
		writeXMLError(w, http.StatusRequestEntityTooLarge, "")
		return
	}

	// Preconditions are decided BEFORE a single byte is written.
	//
	// The content store on the tree layout publishes to the live path, so deciding
	// after writing destroyed the old bytes whenever the decision was "no": a stale
	// If-Match answered 412 with the previous content already gone, and metadata that
	// still described bytes that no longer existed. Deciding first means a refusal
	// leaves the resource exactly as it was. The store re-evaluates the same
	// precondition inside the UPDATE (UpdateFile's Expect), so a writer that commits
	// between this check and the write still fails safely with 412 — this fast path
	// exists so a doomed upload never reaches the content store at all.
	expect, ok := h.putPrecondition(r, existing)
	if !ok {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	owner := a.ID().String()
	// The cap on the read, for bodies that never declared a length. MaxBytesReader
	// aborts the copy at the limit, and Put reports the failure; the tree store's
	// stage-then-rename means the previous content is untouched.
	body := io.Reader(r.Body)
	if h.cfg.Limits.MaxFileBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, h.cfg.Limits.MaxFileBytes)
	}
	res, err := h.content.Put(ctx, layout.Node{
		Owner: owner, Path: rel, IsDir: false,
	}, body, r.ContentLength)
	if err != nil {
		// Logged before mapping to a status: the client only sees a code, and a
		// storage failure that is invisible in the log is indistinguishable from a
		// permissions problem six months later.
		h.logf("put: content store rejected owner=%s path=%q len=%d err=%v",
			owner, rel, r.ContentLength, err)
		writeXMLError(w, statusForError(err), "")
		return
	}

	etag := etagFor(res.Hash)
	mime := r.Header.Get("Content-Type")
	if mime == "" || strings.HasPrefix(mime, "application/octet-stream") {
		mime = guessType(name)
	}

	// The existence check happened above, before the lock rule needed its answer.
	if existing != nil {
		err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, uerr := tx.UpdateFile(ctx, store.UpdateFile{
				NodeID: existing.ID, Size: res.Size, Mime: mime, ETag: etag,
				BlobID:   h.blobIDFor(ctx, res.Hash, res.Size),
				BlobHash: res.Hash,
				// Expect re-evaluates the request's conditional inside the UPDATE,
				// under the row lock: a writer that committed between the handler's
				// check above and this statement fails here with ErrPrecondition
				// instead of silently discarding the other client's work.
				Expect: expect,
				// Keep a version only when the content actually changed: the
				// client may retry an upload, and duplicating identical versions
				// fills the history with noise.
				KeepVersion: existing.Size != res.Size || existing.ETag != etag,
				MaxVersions: h.maxVersions(),
				// §8.1 操作者引用: a WebDAV PUT names the account that made the version.
				By: a.Ptr(),
			})
			if uerr != nil {
				return uerr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(),
				ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: existing.ID.String(),
				Result: "ok", Bytes: &res.Size,
				Metadata: map[string]any{"name": name, "replaced": true},
			})
		})
	} else {
		err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			node, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: a.ID(), ParentID: parentNode.ID, Name: name,
				Size: res.Size, Mime: mime, ETag: etag,
				BlobID: h.blobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
			})
			if cerr != nil {
				return cerr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(),
				ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.write", TargetKind: "node", TargetID: node.ID.String(),
				Result: "ok", Bytes: &res.Size,
				Metadata: map[string]any{"name": name, "created": true},
			})
		})
	}

	if err != nil {
		// The bytes are orphaned; the sweep collects them. Reporting the metadata
		// failure is what matters, because that is what the client can act on.
		writeXMLError(w, statusForError(err), "")
		return
	}

	w.Header().Set("ETag", etag)
	// RFC 4918 §9.7.1: a replaced resource answers 204, a created one 201. Saying 201 for both
	// makes a client believe every save created a new file.
	if existing != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) checkIfMatch(r *http.Request, etag string) bool {
	im := r.Header.Get("If-Match")
	if im == "" {
		return true
	}
	// Strong comparison: If-Match is defined to never match a weak tag, whichever
	// method carries it. The weak list form stays available to If-None-Match paths
	// via etagMatches.
	return strongEtagMatches(im, etag)
}

// putPrecondition translates a PUT's conditional headers into the store's Precondition
// and evaluates it against the resource's current state. ok=false means the request is
// refused with 412 and nothing — bytes or metadata — has been changed.
//
// The rules (RFC 7232, as they apply to a state-changing request):
//
//   - If-Match is evaluated with the STRONG comparison: a weak tag never matches (§2.1),
//     "*" matches any current entity, and on a resource that does not exist the
//     condition fails outright (§3.1).
//   - If-None-Match is evaluated with the weak comparison, and on PUT a match is a
//     refusal (§3.2) — this is how a client says "only create what I do not already
//     see", including with "*".
//   - When both headers are present both must hold.
//
// The returned Precondition carries the SAME decision into the store so that
// UpdateFile re-evaluates it inside the UPDATE under the row lock. A list-valued or
// wildcard header is normalised to the matched entity-tag for that purpose: the
// store compares one exact value, and this function has already established which
// value that is.
func (h *Handler) putPrecondition(r *http.Request, existing *store.Node) (*store.Precondition, bool) {
	im := strings.TrimSpace(r.Header.Get("If-Match"))
	inm := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if im == "" && inm == "" {
		return nil, true
	}

	exists := existing != nil
	var etag string
	if exists {
		etag = existing.ETag
	}

	expect := &store.Precondition{}

	if im != "" {
		if !exists {
			// If-Match on a resource that does not exist: the condition fails. This
			// used to fall through to the create path and answer 201, which told a
			// client its guard had passed when it had not been evaluated at all.
			return nil, false
		}
		if !strongEtagMatches(im, etag) {
			return nil, false
		}
		if im != "*" {
			expect.IfMatch = etag
		} else {
			expect.IfMatch = "*"
		}
	}

	if inm != "" {
		// Weak comparison: a weak spelling of the current tag counts as a match.
		if exists && (inm == "*" || etagMatches(inm, etag)) {
			return nil, false
		}
		// Hand the store the same must-not-be condition. On the create path the
		// unique (parent, name) index is what enforces it atomically.
		expect.IfNoneMatch = inm
	}

	return expect, true
}

// strongEtagMatches evaluates an If-Match list with the strong comparison: a member
// matches when it is "*" or is exactly the current entity-tag, and a weakly-tagged
// member never matches (RFC 7232 §2.1). If-Match is the one header where accepting a
// weak tag would let a client overwrite content it only weakly validated.
func strongEtagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "W/") {
			continue
		}
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

// blobIDFor maps a content address to the blobs row id, registering it on first
// use. The dedup index lives in the metadata layer, not in the storage backend.
// blobIDFor maps a content address to its blobs row.
//
// The metadata layer owns this mapping (see Store.BlobIDFor): the storage backend
// only knows "these bytes hash to X", not which row represents them. Returning a
// zero UUID here — which this used to do — makes every WebDAV write fail the
// blob_id foreign key, and the client sees only a 404.
func (h *Handler) blobIDFor(ctx context.Context, hash [32]byte, size int64) uuid.UUID {
	return h.meta.BlobIDFor(ctx, hash, size)
}

// maxVersions adapts the retention policy to the storage layout.
//
// The tree layout stores each version as a full copy, so keeping ten of them
// multiplies the footprint; the blob layout only stores differences and can afford
// more history. This is exactly the kind of decision that belongs behind Caps
// rather than in a type switch at every call site.
func (h *Handler) maxVersions() int {
	if h.caps.CheapVersions {
		return 10
	}
	return 3
}

// handleDelete moves a node to the trash.
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	cap, node, err := h.resolvePath(r.Context(), a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !cap.Can(authz.PermDelete) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
	if node.ParentID == nil {
		// Deleting the namespace root would orphan everything under it.
		writeXMLError(w, http.StatusForbidden, "")
		return
	}

	if !h.checkIfMatch(r, node.ETag) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	// A lock on the node blocks deleting it, and a Depth: infinity lock on an ancestor does too.
	// Removing a node also changes its parent's membership, which the ancestor walk covers.
	if !h.requireUnlocked(w, r.Context(), a, node.ID, r.Header.Get("If")) {
		return
	}
	if h.lockAncestors(r.Context(), a, node.ID, r.Header.Get("If")) {
		writeXMLError(w, http.StatusLocked, "lock-token-submitted")
		return
	}

	err = h.meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		if terr := tx.TrashCapped(ctx, node.ID, h.trashCap(ctx)); terr != nil {
			return terr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: a.Ptr(),
			ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.delete", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"name": node.Name},
		})
	})
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMkcol creates a collection.
func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	ctx := r.Context()

	// MKCOL with a body is not a MKCOL; RFC 4918 says 415.
	if r.ContentLength > 0 {
		writeXMLError(w, http.StatusUnsupportedMediaType, "")
		return
	}

	_, rel, _ := h.splitPath(r.URL.Path)
	if rel == "" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parentRel, name := path.Split(strings.TrimSuffix(rel, "/"))
	parentRel = strings.TrimSuffix(parentRel, "/")

	parentCap, parentNode, err := h.resolvePath(ctx, a, "/dav/"+a.Username()+"/"+parentRel)
	if err != nil || !parentNode.IsDir() {
		writeXMLError(w, http.StatusConflict, "")
		return
	}
	if !parentCap.Can(authz.PermWrite) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	// Creating a collection is a membership change on the parent, so a Depth: 0 lock on it
	// blocks this (RFC 4918 §7.4). This is the DAV mirror of the API's guardParentUnlocked.
	if h.lockAncestors(ctx, a, parentNode.ID, r.Header.Get("If")) {
		writeXMLError(w, http.StatusLocked, "lock-token-submitted")
		return
	}

	// Creating over an existing resource is 405 per RFC 4918 §9.3.1.
	exists := false
	_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.ChildByName(ctx, parentNode.ID, name); err == nil {
			exists = true
		}
		return nil
	})
	if exists {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// The content store is written before the metadata row, and a failure from either is
	// reported instead of being ignored (NC-R6-001). Creating the node first and then
	// discarding the Mkdir error made a collection appear in the listing and the audit log
	// while the tree layout had no directory on disk.
	err = dircommit.Create(ctx, h.content,
		layout.Node{Owner: a.ID().String(), Path: rel, IsDir: true},
		func(ctx context.Context) error {
			return h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				node, cerr := tx.CreateDir(ctx, store.NewNode{
					OwnerID: a.ID(), ParentID: parentNode.ID, Name: name,
				})
				if cerr != nil {
					return cerr
				}
				return tx.Audit(ctx, store.AuditEntry{
					ActorKind: "user", ActorUserID: a.Ptr(),
					ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
					Action: "file.write", TargetKind: "node", TargetID: node.ID.String(),
					Result: "ok", Metadata: map[string]any{"name": name, "created": true, "kind": "dir"},
				})
			})
		})
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}

	w.WriteHeader(http.StatusCreated)
}

var errUnsupported = errors.New("dav: unsupported")

var _ = blob.Hash{}
