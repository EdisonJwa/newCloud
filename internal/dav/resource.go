package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/dircommit"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/swap"
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
	ownerSeg, rel, serr := h.splitPath(r.URL.Path)
	if serr != nil {
		// splitPath refuses a swap's staging name, and PUT used to discard that error and fall
		// through to 409. Writing to a staging name would corrupt a swap another request is in
		// the middle of, so the refusal has to be honoured here (2026-10-24 review, M4).
		writeXMLError(w, statusForError(serr), "")
		return
	}
	// A URL that names another account's namespace is refused before anything is written.
	// The bytes would land in the caller's own tree (the store never sees the URL), so
	// without this a client could upload "to /remote.php/dav/files/someone/" and be told it
	// worked.
	if ownerSeg != "" && ownerSeg != a.Username() {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
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

	// A name that differs from the found sibling only by case or Unicode folding is a
	// CONFLICT, not the file the caller meant to write.
	//
	// ChildByName compares the folded name_key, so a PUT for "readme.md" finds a node created
	// as "README.md" — that is what makes name lookup case-robust. Reusing that answer for a
	// content write updated the OTHER file's row while publishing bytes under the path the
	// client asked for: the response was 204, the original's content was replaced, and a GET
	// of the requested name then answered 404 because Resolve (the read path) is case-exact.
	// The store's WriteFile guard (pg.go / memory.go) refuses exactly this, and ADR-0004 §4
	// says a second write under a conflicting name must fail; the handler now does the same,
	// before any lock check or byte is touched, so the folded sibling is untouched and
	// everything downstream (locks, putPrecondition) only ever sees the exact-name node
	// (2026-10-25 review).
	if existing != nil && existing.Name != name {
		writeXMLError(w, http.StatusConflict, "name-conflict")
		return
	}

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

	// Nextcloud clients send the file's modification time as X-OC-Mtime and expect
	// "X-OC-MTime: accepted" in reply. Without it the server stamps its own clock on every
	// upload, and the sync client then sees a remote mtime it never recorded and downloads
	// its own file back. An unparseable value is ignored rather than refused: the upload is
	// still valid, it just keeps the server's clock, and the missing header says so.
	clientMtime, mtimeAccepted := parseOCMtime(r.Header.Get("X-OC-Mtime"))
	if mtimeAccepted {
		w.Header().Set("X-OC-MTime", "accepted")
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
				// X-OC-Mtime, when the client sent one.
				ModifiedAt: clientMtime,
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
				ModifiedAt: clientMtime,
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

// parseOCMtime reads the Nextcloud X-OC-Mtime request header.
//
// The value is Unix seconds; some clients send a fractional part, so a float is accepted
// and truncated. A value outside the range a filesystem can represent is ignored, which
// leaves the server's clock in place and omits the "accepted" reply — the contract the
// client checks, so it can tell the difference between "stored" and "ignored".
func parseOCMtime(v string) (*time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, false
	}
	// 253402300799 is 9999-12-31T23:59:59Z, the largest timestamp HTTP dates represent.
	if n < 0 || n > 253402300799 {
		return nil, false
	}
	t := time.Unix(int64(n), 0).UTC()
	return &t, true
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

	// A lock anywhere in the subtree blocks the delete.
	//
	// DELETE removes every descendant, so a lock on a child is a lock on something this
	// request is about to destroy. The node's own lock and its ancestors were checked above;
	// the children were not, and a client that locked a file could watch a DELETE of its
	// parent collection return 204 (2026-10-24 review, H2).
	if node.IsDir() {
		var held []store.Lock
		if lerr := h.meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			l, err := tx.LocksInSubtree(ctx, node.ID)
			held = l
			return err
		}); lerr != nil {
			writeXMLError(w, statusForError(lerr), "")
			return
		}
		for _, l := range held {
			if lockBlocksFor(l, a, r.Header.Get("If")) {
				writeXMLError(w, http.StatusLocked, "lock-token-submitted")
				return
			}
		}
	}

	// Move the content out of the live path, then trash the row.
	//
	// Trashing is metadata-only, so on the tree layout a deleted file used to keep occupying its
	// path until the retention sweep: the name was not free, which is why no-replace publish could
	// not be enabled there, and the retained copy sat at a path derived from a name that no longer
	// existed in the namespace (2026-10-24 review, phase 0 / ADR-0013).
	_, rel, _ := h.splitPath(r.URL.Path)
	parentRel := path.Dir(rel)
	if parentRel == "." {
		parentRel = ""
	}
	err = swap.Retire(r.Context(),
		swap.Deps{Meta: h.meta, Content: h.content, Log: h.log, TrashCap: h.trashCap(r.Context())},
		node, parentRel,
		func(ctx context.Context, tx store.Tx, retired store.Node) error {
			// The trash is conditional on the node still being what this request resolved.
			//
			// A DELETE resolves the node, checks its lock and its ACL, then trashes it in this
			// transaction. A PUT that updated the row in between would be trashed along with it:
			// the writer was told 201/204 and its file vanishes from the namespace with no error
			// addressed to it (2026-10-24 review, G1). The precondition is evaluated inside the
			// statement that trashes, so a writer that committed first wins and this DELETE
			// answers 412 — the same arbitration a stale If-Match gets.
			//
			// Directories carry no strong validator, so the expectation is only applied to files;
			// a concurrent write inside a directory is still a membership change that the lock
			// rules cover.
			// The validator is the one captured BEFORE the retirement, not the one on the renamed
			// row: the point is to refuse the delete if a writer changed this file in the window
			// between the request resolving it and this transaction. Reading it after the rename
			// would compare the row against itself and always pass.
			var expect *store.Precondition
			if !retired.IsDir() && node.ETag != "" {
				expect = &store.Precondition{IfMatch: node.ETag}
			}
			if terr := tx.TrashCappedExpect(ctx, retired.ID, h.trashCap(ctx), expect); terr != nil {
				return terr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: a.Ptr(),
				ActorEmail: a.Email(), ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.delete", TargetKind: "node", TargetID: retired.ID.String(),
				// The name the user deleted, not the reserved one the row carries in the trash.
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

	ownerSeg, rel, serr := h.splitPath(r.URL.Path)
	if serr != nil {
		// The refusal is a real answer about the NAME (an unsafe path, or a swap's reserved
		// staging segment), and discarding it made an unsafe MKCOL fall into the 405 branch
		// below — telling the client the method was wrong when the name was. PUT honours the
		// same error (2026-10-25 review).
		writeXMLError(w, statusForError(serr), "")
		return
	}
	// Same namespace rule as PUT: MKCOL resolves its PARENT from the caller's tree, so a
	// URL naming another account would create the folder under the caller while reporting
	// success against somebody else's name.
	if ownerSeg != "" && ownerSeg != a.Username() {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}
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
