package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// RetainUntil is when this file is due to move to the trash because of the share it was
	// delivered through (SHARE-05 保留期限). Exposed so a user whose delivery disappears can see
	// why, rather than concluding the file was lost.
	RetainUntil string `json:"retain_until,omitempty"`
	// PurgeAt is when a trashed item will be deleted for good (§8.1 保留期清理). Exposed for the
	// same reason RetainUntil is: trash_retention_days is enforced by a background sweep, so an
	// item disappears with no further interaction. A user who cannot see the deadline cannot act
	// before it, and the loss then looks arbitrary. Absent when retention is zero, because "kept
	// forever" and "deleted at some date" are different promises.
	PurgeAt string `json:"purge_at,omitempty"`
	// TrashedAt is when the item was deleted, which is the reference point PurgeAt counts from.
	TrashedAt string `json:"trashed_at,omitempty"`
	// Preview tells the client which renderer to use, or omits the field when the file
	// downloads instead. It is advisory: the download endpoint re-checks the type and the
	// bytes, so a stale value cannot make the server inline something it refuses.
	Preview string `json:"preview,omitempty"`

	// ShareCount and HasVersions make the audit capability visible without opening anything:
	// a row says at a glance whether the object is shared and whether history exists.
	// Omitted when zero, which is the common case and needs no ink.
	ShareCount  int  `json:"share_count,omitempty"`
	HasVersions bool `json:"has_versions,omitempty"`
}

func toFileJSON(n store.Node, rel string) fileJSON {
	preview := ""
	if !n.IsDir() {
		preview = string(previewKindForNode(n.Name, n.Mime))
	}
	return fileJSON{
		Preview:     preview,
		ID:          n.ID.String(),
		Name:        n.Name,
		Path:        path.Join("/", rel),
		Kind:        string(n.Kind),
		Size:        n.Size,
		Mime:        n.Mime,
		ETag:        n.ETag,
		ModifiedAt:  n.ModifiedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CreatedAt:   n.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Trashed:     n.TrashedAt != nil,
		RetainUntil: formatRetainUntil(n.RetainUntil),
	}
}

// handleList lists a directory.
//
// The path comes from a query parameter rather than the URL so that names with
// slashes and spaces do not need re-encoding by every client.
//
// Pages are addressed by an opaque `cursor` returned with the previous page, not by
// an offset. An offset over a name sort returns duplicates and drops rows whenever a
// sibling is created or deleted between requests — the exact failure T-19 describes.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")
	cursor := r.URL.Query().Get("cursor")
	limit := parseLimit(r.URL.Query().Get("limit"), s.deps.Config.Limits.MaxProfindChildren)

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
		// One extra row is fetched to tell "this is the last page" from "there is
		// more" without a second count query.
		c, cerr := tx.ChildrenAfter(ctx, dir.ID, cursor, limit+1)
		children = c
		return cerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	next := ""
	if len(children) > limit {
		children = children[:limit]
		// The cursor is the last name on this page: the next page starts strictly
		// after it, so a row inserted meanwhile either belongs to a later page or
		// is simply visible on refresh — never skipped.
		next = children[len(children)-1].Name
	}

	markers := s.nodeMarkers(r.Context(), actor.ID(), children)

	out := make([]fileJSON, 0, len(children))
	for _, c := range children {
		item := toFileJSON(c, path.Join(rel, c.Name))
		if m, ok := markers[c.ID]; ok {
			item.ShareCount = m.ShareCount
			item.HasVersions = m.HasVersions
		}
		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":     path.Join("/", rel),
		"name":     dir.Name,
		"children": out,
		// `truncated` is kept for clients that only understand the old shape; the
		// cursor is what actually lets them continue rather than merely notice.
		"truncated": next != "",
		"cursor":    next,
	})
}

// defaultPageSize is the page size when the client does not ask for one.
//
// Large enough that a normal folder arrives in one request, small enough that a
// folder with a hundred thousand entries cannot be used to make one response the
// size of the database.
const defaultPageSize = 500

// parseLimit clamps a client-supplied page size.
//
// Unclamped, `?limit=1000000` is a one-line denial of service against the database
// and the browser.
func parseLimit(raw string, max int) int {
	if raw == "" {
		if max > 0 && max < defaultPageSize {
			return max
		}
		return defaultPageSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultPageSize
	}
	if max > 0 && n > max {
		return max
	}
	return n
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
	// Creating a name inside a directory is the `create` primitive, not the composite `write`.
	//
	// §4.2 lists create and overwrite separately, and the ACL layer stores them separately, so a
	// grant can say "you may add files here but not replace existing ones". Checking PermWrite
	// asked for both and made that grant unexpressible.
	if !cap.Can(authz.PermCreate) {
		writeError(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}

	// Adding a child changes the parent's membership, so a Depth: 0 lock on the parent blocks
	// it (RFC 4918 §7.4). This mirrors MKCOL on the DAV side, which already refused.
	if !s.guardParentUnlocked(w, r, actor, parent.ID) {
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

// Two sentinel errors so writableParent can say why without choosing a status code. The
// handler maps them, and both are reported to the client as 404: a write target the caller may
// not see must not be distinguishable from one that does not exist.
var (
	errNotADirectory   = errors.New("target is a file")
	errNoSuchDirectory = errors.New("no such directory")
)

// writableParent resolves a directory path and checks the caller holds need on it.
//
// Both the single-request upload and the resumable endpoints need exactly this, and the check
// has to be identical in both: an authorization rule that differs between two ways of writing
// the same bytes is a way in.
//
// need is a parameter rather than a fixed PermWrite because the correct primitive depends on
// whether the write creates a new name or replaces an existing one (§4.2 lists create and
// overwrite separately). A caller that has resolved the target passes the one it means.
func (s *Server) writableParent(ctx context.Context, actor authz.Actor, dirRel string,
	need authz.Perm) (store.Node, []store.Node, error) {
	nodes, err := s.deps.Meta.Resolve(ctx, actor.ID(), dirRel)
	if err != nil {
		return store.Node{}, nil, err
	}
	parent := nodes[len(nodes)-1]
	if !parent.IsDir() {
		return store.Node{}, nil, errNotADirectory
	}

	cap, err := s.deps.Authz.Authorize(actor, parent.ID)
	if err != nil {
		return store.Node{}, nil, err
	}
	if !cap.Can(need) {
		return store.Node{}, nil, errNoSuchDirectory
	}
	return parent, nodes, nil
}

// publishFile records stored bytes as a node, keeping a version when the name already exists,
// and writes the audit entry in the same transaction.
//
// Extracted so the single-request upload and the resumable one cannot diverge: the versioning
// policy and the audit record are part of the write, not an afterthought one of them might
// forget to keep in step.
//
// expect carries the client's precondition. It defaults to "overwrite freely" because a browser
// upload of the same name is a deliberate replace; what it must never do is decide that in the
// handler and then write in a separate step, which is how two concurrent saves both win and one
// silently disappears.
func (s *Server) publishFile(
	ctx context.Context, r *http.Request, actor authz.Actor,
	parentID uuid.UUID, name, rel, mime string, res layout.PutResult, expect *store.Precondition,
) (store.Node, error) {
	etag := etagFor(res.Hash)
	var node store.Node

	// Every successful upload passes through here — the simple handler and the chunked
	// completion alike — so the outcome is recorded once, at the point that actually decides it,
	// rather than at each caller where one could be forgotten. Failures are counted at their own
	// return sites because they do not share a path.
	defer func() {
		if s.deps.Metrics != nil && node.ID != uuid.Nil {
			s.deps.Metrics.ObserveUpload("ok")
		}
	}()

	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, _, werr := tx.WriteFile(ctx, store.WriteFile{
			NewFile: store.NewFile{
				OwnerID: actor.ID(), ParentID: parentID, Name: name,
				Size: res.Size, Mime: mime, ETag: etag,
				BlobID: s.deps.Meta.BlobIDFor(ctx, res.Hash, res.Size), BlobHash: res.Hash,
			},
			Expect:      expect,
			KeepVersion: true,
			MaxVersions: s.maxVersions(),
			// §8.1 操作者引用: the version this write supersedes is attributed to whoever saved it.
			By: actor.Ptr(),
		})
		if werr != nil {
			return werr
		}
		node = n

		bytes := res.Size
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.write", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Bytes: &bytes, Metadata: map[string]any{"name": name},
		})
	})
	if err != nil {
		return store.Node{}, err
	}
	return node, nil
}

// nodeFor builds the content-store node a path maps to.
func nodeFor(actor authz.Actor, rel string) layout.Node {
	return layout.Node{Owner: actor.ID().String(), Path: rel}
}

// uploadStagingDir is where finished sessions leave their bytes for cleanup.
func (s *Server) uploadStagingDir() string {
	if s.deps.Uploads == nil {
		return ""
	}
	return s.deps.Uploads.Dir()
}

// handleUpload stores one file.
//
// Multipart is used rather than a raw body so the browser can send a filename
// without relying on custom headers, which are awkward to set from a form.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	// Cap the request before reading: a client that lies about its size would otherwise fill the
	// disk while we stream. The cap is the EFFECTIVE one: a runtime override (an administrator
	// lowering the limit without a restart) has to reach this reader, or the setting is a number in
	// a table that nothing obeys.
	limit := s.maxFileBytes(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// MaxBytesReader's failure is a size refusal, not a malformed request. Answering 400 for it
		// told a client its upload was broken when it was merely too large — and it was the same
		// answer the handler gave before the cap was read at all, which is how a limit that was not
		// enforced still looked like it was.
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
				fmt.Sprintf("the file exceeds the %d-byte limit", limit))
			return
		}
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

	parent, _, err := s.writableParent(r.Context(), actor, dirRel, authz.PermCreate)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	rel := path.Join(dirRel, name)

	// One resolution, used for both the membership and the overwrite decision. Asking twice also
	// meant asking wrongly: existingFile reports "no such file" for a new name, so a guard written
	// after it never ran for exactly the case it was there for.
	exists, existingNode, xerr := s.existingFile(r.Context(), actor.ID(), rel)
	if xerr != nil {
		s.writeStoreError(w, xerr)
		return
	}
	_ = existingNode
	if !exists {
		// Creating a new name is a membership change on the parent, so a Depth: 0 parent lock
		// blocks it (RFC 4918 §7.4).
		if !s.guardParentUnlocked(w, r, actor, parent.ID) {
			return
		}
	}

	// Replacing an existing file needs `overwrite`, which `create` alone does not imply.
	//
	// §4.2 separates them, and the distinction is the difference between "add files here" and
	// "may replace my work". The existence check is a read; the authoritative guard is the
	// precondition inside WriteFile, so a file created between here and the write still cannot be
	// clobbered by a caller who lacks the bit — it fails the precondition instead.
	if existing, node, xerr := s.existingFile(r.Context(), actor.ID(), rel); xerr == nil && existing {
		// A WebDAV lock on the target blocks an HTTP overwrite (§5.3). Without this the two write
		// paths disagree and the lock is advisory on one of them.
		if !s.guardUnlocked(w, r, actor, node.ID) {
			return
		}

		// A Depth: infinity lock on an ancestor also covers this child content (RFC 4918
		// 7.4), which is the half that makes a subtree lock worth taking.
		if !s.guardAncestorContentUnlocked(w, r, actor, node.ID) {
			return
		}
		parentCap, cerr := s.deps.Authz.Authorize(actor, parent.ID)
		if cerr != nil {
			s.writeStoreError(w, cerr)
			return
		}
		if !parentCap.Can(authz.PermOverwrite) {
			writeError(w, http.StatusNotFound, "not_found", "no such directory")
			return
		}
	}

	res, err := s.deps.Content.Put(r.Context(), nodeFor(actor, rel), file, header.Size)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	mime := header.Header.Get("Content-Type")
	if mime == "" || strings.HasPrefix(mime, "application/octet-stream") {
		mime = guessType(name)
	}

	// A browser upload replaces by design, so the default precondition is "none".
	// A caller that wants optimistic concurrency sends If-Match, which is what
	// makes a second tab editing the same file fail loudly instead of winning
	// silently.
	node, err := s.publishFile(r.Context(), r, actor, parent.ID, name, rel, mime, res,
		preconditionFromHeader(r))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, toFileJSON(node, rel))
}

// preconditionFromHeader reads the conditional headers a browser or client sent.
//
// It exists so the three write paths share one interpretation of If-Match and
// If-None-Match instead of each deciding for itself what those mean.
func preconditionFromHeader(r *http.Request) *store.Precondition {
	im := strings.TrimSpace(r.Header.Get("If-Match"))
	inm := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if im == "" && inm == "" {
		return nil
	}
	return &store.Precondition{IfMatch: im, IfNoneMatch: inm}
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
	// Streaming the bytes exercises the `download` primitive, not `read`.
	//
	// §4.2 lists them separately and the ACL layer stores them separately, so a grant can
	// say "you may see that this file exists and read its metadata, but you may not take a
	// copy". Checking PermRead here made that grant indistinguishable from full access: the
	// fine-grained bit was expressible, storable, listable, and unenforced.
	if !cap.Can(authz.PermDownload) {
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

	// Inline rendering is opt-in. The default stays a download, so a client that predates
	// this feature keeps the behaviour it had.
	inline := previewFromHeader(r)
	kind := previewNone
	if inline {
		kind = previewKindForNode(node.Name, node.Mime)

		// The headers are claims. Read the first bytes and check what the file actually
		// starts with: a client may declare image/png, name it .png, and send HTML, and no
		// header can tell you otherwise.
		if kind != previewNone {
			head := make([]byte, sniffableBytes)
			n, rerr := io.ReadFull(rc, head)
			if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
				s.writeStoreError(w, rerr)
				return
			}
			if looksLikeActiveContent(head[:n]) {
				kind = previewNone
			}
			// Rewind regardless: ServeContent re-reads from the start, and a reader left
			// mid-stream would serve the file truncated.
			if _, serr := rc.Seek(0, io.SeekStart); serr != nil {
				s.writeStoreError(w, serr)
				return
			}
		}

		if kind == previewNone {
			// Asked for a preview of something this server will not render in place.
			// Answering with an attachment is the honest outcome: the file arrives, and the
			// UI shows "no preview for this type".
			inline = false
		}
	}

	if !inline {
		w.Header().Set("Content-Type", contentTypeFor(node))
		w.Header().Set("Content-Disposition", contentDisposition(node.Name))
	} else {
		previewHeaders(w, node.Name, node.Mime, kind)
	}

	w.Header().Set("ETag", node.ETag)
	w.Header().Set("Accept-Ranges", "bytes")

	// ServeContent handles Range, If-Range, If-None-Match and If-Modified-Since against
	// the seekable reader. The previous code advertised Accept-Ranges but streamed
	// io.Copy, so a video could not seek and a resumed download restarted from zero.
	//
	// The modtime is zero because the metadata layer's timestamps are not stable enough to
	// serve as validators here; the ETag, which is derived from the content address, does
	// that job correctly.
	http.ServeContent(w, r, node.Name, time.Time{}, rc)

	bytes := node.Size
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		meta := map[string]any{"name": node.Name}
		if inline {
			meta["inline"] = string(kind)
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.read", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Bytes: &bytes, Metadata: meta,
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
	// Renaming is the `rename` primitive (§4.2), not the composite `write`: an ACL can grant
	// "edit this file's content but do not reorganise it".
	if !cap.Can(authz.PermRename) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	// A rename moves the node, so a lock on it blocks the operation (§5.3).
	if !s.guardUnlocked(w, r, actor, node.ID) {
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
	//
	// The error used to be discarded, which is the same defect the WebDAV MOVE had: metadata
	// committed, bytes stayed where they were, and the client was told the rename succeeded. The
	// metadata is put back if the bytes cannot follow, so the two never disagree silently.
	if merr := s.moveContentByPath(r.Context(), actor.ID().String(), body.Path,
		path.Join(parentRel, body.Name), node.IsDir()); merr != nil {
		s.deps.Log.Error("rename: content move failed; restoring metadata",
			"from", body.Path, "to", body.Name, "error", merr)
		if cerr := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.Rename(ctx, node.ID, node.Name)
			return rerr
		}); cerr != nil {
			s.deps.Log.Error("rename: COMPENSATION FAILED", "node", node.ID, "error", cerr)
		}
		writeError(w, http.StatusInternalServerError, "content_move_failed",
			"the file could not be moved; nothing was changed")
		return
	}

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
	// A move consumes `delete` authority on the source, not merely write: the node leaves
	// the place it was. §4.2 wants the source's move/delete right and the target's create
	// right checked independently, which the two blocks below do.
	if !cap.Can(authz.PermDelete) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	// A lock on the source blocks moving it away from under the holder (§5.3).
	if !s.guardUnlocked(w, r, actor, node.ID) {
		return
	}

	// A directory move carries its whole subtree, so every inner node must permit it too.
	if node.IsDir() {
		if ok := s.checkSubtreePerm(r, actor, node.ID, authz.PermDelete); !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such file")
			return
		}
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
	// The target needs `create` (adding a name) plus `overwrite` when it replaces something;
	// PermWrite expands to both, so checking it covers the create side. Overwrite is
	// re-checked against the *existing* target below, where it is known whether there is one.
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

	// Same rule as rename: bytes that cannot follow the metadata put the metadata back.
	if merr := s.moveContentByPath(r.Context(), actor.ID().String(), body.From, body.To,
		node.IsDir()); merr != nil {
		s.deps.Log.Error("move: content move failed; restoring metadata",
			"from", body.From, "to", body.To, "error", merr)
		if cerr := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			// Only the parts that actually changed: a no-op rename is harmless now, but relying on
			// store semantics for a compensation path is how this failed the first time.
			if node.Name != name {
				if _, rerr := tx.Rename(ctx, node.ID, node.Name); rerr != nil {
					return rerr
				}
			}
			if node.ParentID != nil && *node.ParentID != parent.ID {
				if _, mverr := tx.Move(ctx, node.ID, *node.ParentID); mverr != nil {
					return mverr
				}
			}
			return nil
		}); cerr != nil {
			s.deps.Log.Error("move: COMPENSATION FAILED", "node", node.ID, "error", cerr)
		}
		writeError(w, http.StatusInternalServerError, "content_move_failed",
			"the file could not be moved; nothing was changed")
		return
	}

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

	// A lock blocks deletion through either interface (§5.3).
	if !s.guardUnlocked(w, r, actor, node.ID) {
		return
	}

	// Deleting a directory deletes everything inside it, so the root's permission is not
	// the whole question (§4.2: 目录递归操作不能只检查根目录). An inner file may carry a
	// narrower grant, or belong to a subtree somebody else can see — trashing it because
	// the parent was deletable would be privilege escalation by nesting.
	if node.IsDir() {
		if ok := s.checkSubtreePerm(r, actor, node.ID, authz.PermDelete); !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such file")
			return
		}
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

	retention := s.deps.Config.Limits.TrashRetention()

	out := make([]fileJSON, 0, len(nodes))
	for _, n := range nodes {
		item := toFileJSON(n, path.Join("/", n.Name))
		item.ID = n.ID.String()
		item.Trashed = true
		item.TrashedAt = ""
		if n.TrashedAt != nil {
			item.TrashedAt = n.TrashedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
			if retention > 0 {
				item.PurgeAt = n.TrashedAt.Add(retention).
					UTC().Format("2006-01-02T15:04:05Z07:00")
			}
		}
		out = append(out, item)
	}

	body := map[string]any{"items": out}
	// The window itself, so a client can explain the deadline without hardcoding a number that
	// the operator may have changed.
	if retention > 0 {
		body["retention_days"] = int(retention.Hours() / 24)
	}
	writeJSON(w, http.StatusOK, body)
}

// handleRestore un-deletes a node and its subtree.
//
// The request carries only a node id, which is exactly the shape that invites a
// cross-tenant bug: an id is not a capability, and a trashed node is not visible
// to its owner's authorizer (trash is reachable only through this flow). So the
// ownership check has to happen here, against the stored owner, and the restore
// must re-check that the user may write into the directory it returns to.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id := parseUUID(body.ID)
	if id == uuid.Nil {
		writeError(w, http.StatusBadRequest, "invalid_target", "not a valid file id")
		return
	}

	// Read the trashed node and decide whether this actor may see it at all.
	// A node belongs to exactly one owner; there is no legitimate cross-account
	// restore, so anything else answers the same 404 as a nonexistent id.
	var trashed store.Node
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.NodeIncludingTrashed(ctx, id)
		trashed = n
		return nerr
	})
	if err != nil || trashed.TrashedAt == nil || trashed.OwnerID != actor.ID() {
		writeError(w, http.StatusNotFound, "not_found", "no such item in the trash")
		return
	}

	// Restoring puts the subtree back where it came from, so the caller must
	// still be allowed to write there. Without this, an account whose access to a
	// directory was revoked could get its files back into it.
	if trashed.ParentID != nil {
		cap, cerr := s.deps.Authz.Authorize(actor, *trashed.ParentID)
		if cerr != nil || !cap.Can(authz.PermWrite) {
			writeError(w, http.StatusNotFound, "not_found", "no such item in the trash")
			return
		}
	}

	var restored store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
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

	// ?id= as well as ?path=, for the same reason the version-content route takes it: a path can
	// only ever name the caller's own files, so a shared file's history was unlistable even though
	// its bytes became readable. A caller who is granted version history needs to know WHICH
	// versions exist before asking for one.
	node, _, err := s.nodeForVersionContent(r, actor, rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

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
		entry := map[string]any{
			"version":    v.VersionNo,
			"size":       v.Size,
			"created_at": v.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
		// §8.1 requires every version to carry 内容校验信息. Without it a client downloading an
		// old version has no way to verify the bytes, and an operator has no way to tell which
		// stored object a version refers to. Empty when the blob row is gone, which the LEFT JOIN
		// allows on purpose: an id and a date are still worth showing.
		if len(v.SHA256) == sha256.Size {
			entry["sha256"] = hex.EncodeToString(v.SHA256)
		}
		out = append(out, entry)
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
		n, rerr := tx.RestoreVersion(ctx, node.ID, body.Version, actor.Ptr())
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
//
// The policy itself lives in config so an operator can set it; this is only the
// adapter to the active storage layout.
func (s *Server) maxVersions() int {
	return s.deps.Config.Limits.MaxVersionsFor(s.deps.Content.Capabilities().CheapVersions)
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
	case errors.Is(err, store.ErrPrecondition):
		// 412 rather than 409: the name did not clash, the file changed under the
		// caller. A client that gets 409 retries with a different name; one that
		// gets 412 re-reads and merges, which is the correct behaviour.
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"the file changed since you last read it")
	case errors.Is(err, store.ErrQuota):
		writeError(w, http.StatusInsufficientStorage, "quota_exceeded", "your storage quota is full")
	case errors.Is(err, store.ErrTooLarge), errors.Is(err, layout.ErrSizeMismatch):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the file is larger than allowed")
	case errors.Is(err, store.ErrInvalidName), errors.Is(err, layout.ErrUnsafePath),
		errors.Is(err, layout.ErrInvalidNode):
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, store.ErrCycle):
		writeError(w, http.StatusConflict, "cycle", "cannot move a directory into itself")
	case errors.Is(err, store.ErrAuditUnavailable):
		// 503, not 500: the request is well-formed and the data is fine — the recorder is down.
		// Retrying after the audit store recovers will succeed, and saying so is the difference
		// between an operator checking the data (wrong) and checking the audit table and disk
		// (right). This is the T-16 fail-closed policy surfacing to the client.
		s.deps.Log.Error("refused a write because the audit record could not be written",
			"error", err)
		writeError(w, http.StatusServiceUnavailable, "audit_unavailable",
			"the audit log is unavailable, so the change was refused and nothing was modified")
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

// existingFile reports whether a live file already occupies rel.
//
// A miss is not an error: the common case is a new file, and an upload must not fail because
// the caller has permission to create but the target happens not to exist yet.
func (s *Server) existingFile(ctx context.Context, ownerID uuid.UUID, rel string) (bool, store.Node, error) {
	var node store.Node
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		nodes, rerr := s.deps.Meta.Resolve(ctx, ownerID, rel)
		if rerr != nil {
			return rerr
		}
		node = nodes[len(nodes)-1]
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, store.Node{}, nil
		}
		return false, store.Node{}, err
	}
	return !node.IsDir(), node, nil
}

// formatRetainUntil renders the retention deadline, or "" when none applies.
func formatRetainUntil(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// moveContentByPath moves the bytes for a layout that keeps them at a path.
//
// Layouts with EfficientRename keep the bytes where they are and follow the metadata: their
// path-to-hash resolver IS the metadata, so by the time this runs the source path no longer exists
// there and the call could only fail. That is not a hypothetical — the call always failed for the
// default layout, and because its error was discarded, the failure was invisible and every rename
// and move looked successful.
//
// Returning nil for those layouts is the correct thing, not an optimisation: there is nothing to
// move and nothing to verify.
func (s *Server) moveContentByPath(ctx context.Context, owner, from, to string, isDir bool) error {
	if s.deps.Content.Capabilities().EfficientRename {
		return nil
	}
	return s.deps.Content.Move(ctx,
		layout.Node{Owner: owner, Path: from, IsDir: isDir},
		layout.Node{Owner: owner, Path: to, IsDir: isDir})
}

// handleVersionContent serves the bytes of one historical version.
//
// §8.1 lists 版本下载 among the entry points that must share the permission model, and it did not
// exist: the API could list a version and restore it, but not hand back the bytes. That made "the
// version history" a list of promises — an auditor could see that v3 existed and could not check
// what it contained.
//
// The read is by CONTENT ADDRESS, not by path: a path resolves to the node's CURRENT bytes, so
// serving a path would quietly return the wrong content under the right label. That is why this needs
// the blob store and refuses when the layout does not keep addressable history.
func (s *Server) handleVersionContent(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")
	no, err := strconv.Atoi(r.URL.Query().Get("version"))
	if err != nil || no <= 0 {
		writeError(w, http.StatusBadRequest, "bad_version", "a positive version number is required")
		return
	}

	// Two ways to name the file, exactly as the thumbnail endpoint has: `path` for the caller's own
	// namespace, `id` for a node somebody shared through an ACL. Without `id` the permission check
	// below is decorative — resolving by path can only ever find the caller's own files, where they
	// hold every right — and version history for a shared file would be unreachable while the
	// listing, download and WebDAV paths all honoured the grant.
	node, rel, err := s.nodeForVersionContent(r, actor, rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if node.IsDir() {
		writeError(w, http.StatusBadRequest, "is_directory", "a directory has no versions")
		return
	}

	// The same capability the version LIST requires: reading a version's bytes is a strictly
	// stronger act than seeing that it exists.
	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermReadVersions) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	var version store.Version
	found := false
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		// A generous window rather than a new store method: history is bounded by the version
		// limit the deployment sets, and the alternative is a new Tx method on both backends.
		versions, verr := tx.Versions(ctx, node.ID, 1000)
		if verr != nil {
			return verr
		}
		for _, v := range versions {
			if v.VersionNo == no {
				version = v
				found = true
				break
			}
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such version")
		return
	}

	if s.deps.Blobs == nil {
		// The tree layout stores one copy per version at a path, not an address, so there is
		// nothing to open here. Saying so is better than streaming the current bytes.
		writeError(w, http.StatusConflict, "no_version_content",
			"this storage layout does not keep addressable version history")
		return
	}
	if len(version.SHA256) != sha256.Size {
		writeError(w, http.StatusConflict, "no_version_content",
			"this version has no content address, so its bytes cannot be located")
		return
	}

	var hash blob.Hash
	copy(hash[:], version.SHA256)
	rc, err := s.deps.Blobs.Open(r.Context(), hash)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer rc.Close()

	// §10.1: reading a historical version is access to file content, and it is audited like any
	// other read. After the reader is open, so a missing object is not recorded as a read.
	_ = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.version.read", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok",
			Metadata: map[string]any{
				"name":    node.Name,
				"version": no,
				"sha256":  hex.EncodeToString(version.SHA256),
			},
		})
	})

	w.Header().Set("Content-Type", contentTypeFor(node))
	w.Header().Set("Content-Disposition", contentDisposition(node.Name))
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	// An old version never changes, so it can be cached for as long as anything can.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, node.Name, version.CreatedAt, rc)
}

// nodeForVersionContent resolves the file a version read is about, and its path under the OWNER.
func (s *Server) nodeForVersionContent(r *http.Request, actor authz.Actor,
	rel string) (store.Node, string, error) {
	if id := r.URL.Query().Get("id"); id != "" {
		nodeID, err := uuid.Parse(id)
		if err != nil {
			return store.Node{}, "", store.ErrNotFound
		}
		var node store.Node
		if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			n, nerr := tx.Node(ctx, nodeID)
			node = n
			return nerr
		}); err != nil {
			return store.Node{}, "", err
		}
		if node.TrashedAt != nil {
			return store.Node{}, "", store.ErrNotFound
		}
		path, perr := s.pathOfNode(r.Context(), node.OwnerID, node.ID)
		if perr != nil {
			return store.Node{}, "", perr
		}
		return node, path, nil
	}
	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		return store.Node{}, "", err
	}
	return nodes[len(nodes)-1], rel, nil
}
