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

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/dircommit"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/swap"
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

	// SHA256 is the file's content address (core-scope §2), hex-encoded.
	//
	// It already existed in the database — the blob address IS the content hash, and etagFor
	// truncates it — but it was only ever surfaced on the version listing. A client could verify a
	// historical version and had no way to verify the file it was actually looking at.
	//
	// Absent for directories (no content), and absent when the address is unknown, which is the
	// honest answer rather than a row of zeroes a client might compare against.
	SHA256 string `json:"sha256,omitempty"`
	// Starred is the VIEWER's own star (core-scope §4).
	//
	// Per user, not per node: two people sharing a folder star independently, which is why the
	// favorites table is keyed (user_id, node_id) rather than being a flag on the node.
	Starred bool `json:"starred,omitempty"`
	// FavoriteAt is when the viewer starred it, so a UI can sort or label by that. Only set on the
	// favorites listing, where it is the ordering key; the ordinary listing omits it.
	FavoriteAt string `json:"favorite_at,omitempty"`
	// Tags are the labels on this node that the viewer may see: the deployment's system tags plus
	// their own (core-scope §5). Omitted when there are none.
	Tags []tagJSON `json:"tags,omitempty"`
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

	// A path below a mount point has no rows to resolve against: the listing comes from
	// the remote, live (ADR-0014).
	if tgt, external := s.externalPath(r, rel); external {
		s.handleExternalList(w, r, tgt.Rel)
		return
	}

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

	// A directory below a mount point is created on the remote (ADR-0014). The fork
	// resolves the PARENT: the new name does not exist yet, on either side.
	if _, external := s.externalPath(r, parentRel); external {
		s.handleExternalMkdir(w, r, parentRel, name)
		return
	}

	parentNodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), parentRel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	parent := parentNodes[len(parentNodes)-1]
	// Canonical content path of the parent, from the resolved identity rather than the request text:
	// "path" arrives in the listing's own "/docs/new" form, and a path-backed store refuses the
	// absolute spelling. The new name is validated by the store like any other name.
	parentRel = relOfChain(parentNodes)
	newRel := path.Join(parentRel, name)

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
	// The directory is created in the content store before its metadata row, and neither
	// failure is ignored (NC-R6-001). The old order committed the node and the "ok" audit
	// row, then discarded a failed Mkdir: the tree layout answered 201 with a directory that
	// did not exist on disk.
	err = dircommit.Create(r.Context(), s.deps.Content,
		layout.Node{Owner: actor.ID().String(), Path: newRel, IsDir: true},
		func(ctx context.Context) error {
			return s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
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
		})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, toFileJSON(created, newRel))
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

	// A file below a mount point is stored on the remote, never locally (ADR-0014). The
	// fork sits before the local path's lock guards on purpose: an external write has no
	// local node to fence, and the remote's own concurrency rules are the truth there.
	if _, external := s.externalPath(r, dirRel); external {
		s.handleExternalUpload(w, r, dirRel)
		return
	}

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
	// The store's own segment validator, applied BEFORE any storage write.
	//
	// ".." passes the three-way check above (path.Base keeps it), and everything else the
	// validator refuses — traversal, separators, reserved device names, control characters —
	// would only be rejected later by CreateFile, which on the tree layout is after the bytes
	// have already been published at that path. One validator, the same one the store applies,
	// so the handler cannot drift from what the store will eventually decide.
	if verr := layout.ValidateSegment(name); verr != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "the file needs a usable name")
		return
	}

	parent, parentChain, err := s.writableParent(r.Context(), actor, dirRel, authz.PermCreate)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// The destination directory's content path comes from the resolved chain, so a caller that sends
	// the listing's own "/docs" writes into "docs" on a path-backed layout instead of being refused.
	rel := path.Join(relOfChain(parentChain), name)

	// One resolution of the folded sibling, used for the fold check, the lock guards and the
	// overwrite decision. The lookup used to be Resolve over the whole rel path, run twice and
	// reporting "no such file" for a new name, so a guard written after it never ran for exactly
	// the case it was there for.
	sibling, xerr := s.childAtTarget(r.Context(), parent.ID, name)
	if xerr != nil {
		s.writeStoreError(w, xerr)
		return
	}

	// A sibling that only FOLDS to this name is a conflict, decided before anything is written.
	//
	// The metadata layer keeps "README.md" and "Readme.md" as one name (T-12), so this upload would
	// be refused at CreateFile with 409 — but on the tree layout the Put below runs first and
	// publishes at this path, which on a case-insensitive filesystem destroys the live sibling's
	// bytes for an upload that was never going to commit. The same defect class was fixed in the
	// WebDAV PUT, where preconditions are decided before a single byte is written.
	if sibling != nil && sibling.Name != name {
		writeError(w, http.StatusConflict, "conflict",
			"something whose name differs only in case or Unicode form already exists there")
		return
	}

	if sibling == nil || sibling.IsDir() {
		// Creating a new name is a membership change on the parent, so a Depth: 0 parent lock
		// blocks it (RFC 4918 §7.4). A name that already belongs to a DIRECTORY is the same
		// question: the upload cannot replace it, so what happens next is a store-level refusal,
		// not a content overwrite.
		if !s.guardParentUnlocked(w, r, actor, parent.ID) {
			return
		}
	}
	if sibling != nil && !sibling.IsDir() {
		// Replacing an existing file needs `overwrite`, which `create` alone does not imply.
		//
		// §4.2 separates them, and the distinction is the difference between "add files here" and
		// "may replace my work". The existence check is a read; the authoritative guard is the
		// precondition inside WriteFile, so a file created between here and the write still cannot
		// be clobbered by a caller who lacks the bit — it fails the precondition instead.
		//
		// A WebDAV lock on the target blocks an HTTP overwrite (§5.3). Without this the two write
		// paths disagree and the lock is advisory on one of them.
		if !s.guardUnlocked(w, r, actor, sibling.ID) {
			return
		}

		// A Depth: infinity lock on an ancestor also covers this child content (RFC 4918
		// 7.4), which is the half that makes a subtree lock worth taking.
		if !s.guardAncestorContentUnlocked(w, r, actor, sibling.ID) {
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

	// Preconditions and quota are decided BEFORE a byte is written, for the same reason the
	// WebDAV PUT decided them there: the tree layout publishes at the live path, so a refusal
	// that arrives after the Put has already replaced the bytes it was refusing to replace.
	// Both are fast paths — the authoritative precondition re-runs inside WriteFile and the
	// authoritative reservation inside CreateFile — so a writer committing in between still
	// fails safely; these exist so a DOOMED upload never reaches the content store at all.
	if !s.preconditionHoldsNow(r, sibling) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed",
			"the file changed since you last read it")
		return
	}
	if !s.quotaHasRoomFor(r.Context(), actor.ID(), header.Size, sibling) {
		writeError(w, http.StatusInsufficientStorage, "quota_exceeded", "your storage quota is full")
		return
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

// childAtTarget resolves the sibling that occupies (parentID, name), or nil when the name is free.
//
// The lookup is the store's own folded comparison (T-12), so "readme.md" finds "README.md" —
// which is what makes the fold-conflict pre-check in the upload handlers possible without a
// second, weaker spelling of name identity.
func (s *Server) childAtTarget(ctx context.Context, parentID uuid.UUID, name string) (*store.Node, error) {
	var found *store.Node
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, parentID, name)
		if cerr == nil {
			found = &n
			return nil
		}
		if errors.Is(cerr, store.ErrNotFound) {
			return nil
		}
		return cerr
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// preconditionHoldsNow evaluates the request's conditional headers against the node that
// currently occupies the target name, BEFORE any byte is written.
//
// The tree layout publishes at the live path, so a 412 decided after the Put — which is where
// WriteFile's Expect evaluates it — has already replaced the bytes it refused to replace. The
// WebDAV PUT made this exact move for this exact reason; this is the JSON API's half. It is a
// fast path only: publishFile passes the same headers into WriteFile, whose Expect re-runs the
// decision inside the UPDATE under the row lock, so a writer that commits between the two
// checks still fails safely with 412.
//
// The comparison rules are the ones the DAV handler applies (RFC 7232 as they apply to a
// state-changing request): If-Match with the strong comparison, failing outright on a target
// that does not exist; If-None-Match with the weak comparison, where a match on a write is a
// refusal; both must hold when both are present.
func (s *Server) preconditionHoldsNow(r *http.Request, existing *store.Node) bool {
	expect := preconditionFromHeader(r)
	if expect == nil {
		return true
	}
	exists := existing != nil && !existing.IsDir()
	var etag string
	if exists {
		etag = existing.ETag
	}
	if expect.IfMatch != "" {
		if !exists || !strongEtagMatches(expect.IfMatch, etag) {
			return false
		}
	}
	if expect.IfNoneMatch != "" && exists && (expect.IfNoneMatch == "*" || weakEtagMatches(expect.IfNoneMatch, etag)) {
		return false
	}
	return true
}

// strongEtagMatches evaluates an If-Match list with the strong comparison: a member matches
// when it is "*" or the current entity-tag, and a weakly-tagged member never matches.
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

// weakEtagMatches evaluates an If-None-Match list with the weak comparison: a weak spelling
// of the current tag counts as a match.
func weakEtagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || part == etag || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// quotaHasRoomFor is the best-effort quota fast-path for the upload handlers.
//
// The authoritative reservation happens inside WriteFile/CreateFile, under the same row lock
// that commits the row — that check is the one that is correct under concurrency, and this one
// is deliberately not a second copy of it. It exists because the content Put runs BEFORE that
// reservation, so an upload that could never commit used to write its bytes first and only then
// hear "507": on the tree layout those bytes replaced the live file at the target path.
//
// Racy by design: two concurrent requests can both pass this and still be settled correctly by
// the authoritative check. A read error fails OPEN for the same reason — the slow path still
// refuses. The delta accounts for a replacement, whose old bytes the write frees; without that,
// re-uploading a large file at a full quota would be refused here although the authoritative
// delta check would have admitted it.
func (s *Server) quotaHasRoomFor(ctx context.Context, ownerID uuid.UUID, size int64, existing *store.Node) bool {
	need := size
	if existing != nil && !existing.IsDir() {
		need = size - existing.Size
		if need < 0 {
			need = 0
		}
	}
	// Two reads, deliberately not one transaction: Usage is a Store-level counter (a nested
	// WithTx here would be refused on the in-memory backend and deadlocks on it), and the fast
	// path gains nothing from a snapshot both halves agree on — it is racy by design, and the
	// authoritative reservation inside WriteFile/CreateFile is what is exact.
	used, uerr := s.deps.Meta.Usage(ctx, ownerID)
	if uerr != nil {
		return true
	}
	var quota int64
	if err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, gerr := tx.UserByID(ctx, ownerID)
		if gerr != nil {
			return gerr
		}
		quota = u.QuotaBytes
		return nil
	}); err != nil {
		return true
	}
	if quota <= 0 {
		return true
	}
	return used+need <= quota
}

// handleDownload streams a file for the web UI.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	// Bytes below a mount point stream from the remote (ADR-0014).
	if tgt, external := s.externalPath(r, rel); external {
		s.handleExternalDownload(w, r, tgt.Rel)
		return
	}

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

	// The content path is derived from the node the caller was authorized for, not from the query
	// string: the file manager sends back "/docs/a.txt" (the form the listing renders) and a
	// path-backed store refuses an absolute path outright. Inline preview and media playback are
	// this same handler, so both are covered.
	rel = relOfChain(nodes)

	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: actor.ID().String(), Path: rel,
	})
	if err != nil {
		// "The row is there and you may read it, but its bytes are not where the metadata says" is
		// storage damage, not a missing file. It used to reach the client as a bare 500 with an
		// empty body and leave no trace in the log: the user cannot report anything useful and the
		// operator cannot tell it apart from any other fault.
		//
		// The three states are different and answered differently on purpose:
		//   404 — the file does not exist (Resolve failed earlier)
		//   403 — the caller may not read it (Authorize failed earlier)
		//   500 — the file exists, the caller may read it, and its bytes are gone
		if errors.Is(err, layout.ErrNotFound) {
			if s.deps.Metrics != nil {
				s.deps.Metrics.ObserveContentMissing()
			}
			s.deps.Log.Error("content missing for an existing file: the metadata row says the "+
				"bytes are on the storage backend and they are not",
				"node", node.ID.String(), "name", node.Name, "path", rel,
				"owner", actor.ID().String())
			writeError(w, http.StatusInternalServerError, "content_missing",
				"this file's metadata exists but its content is missing on the storage backend; "+
					"it has been reported and an operator needs to check the storage")
			return
		}
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
// handleRename changes a node's NAME.
//
// INV-05: the node id, its versions and every share reference are untouched — a path is not an
// identity, so renaming (like moving) cannot orphan history or break an existing link.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// Renaming below a mount point is a remote rename. The mount point itself keeps its
	// name: it is the mount's identity in listings and audit rows (ADR-0014).
	if tgt, external := s.externalPath(r, body.Path); external {
		if tgt.Rel == "" {
			writeError(w, http.StatusConflict, "is_mount",
				"the mount point's name is the mount's identity; unmount and remount under the new name")
			return
		}
		s.handleExternalRename(w, r, tgt, body.Name)
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

	// Both paths are content paths, built from the resolved identity: the caller sent the listing's
	// "/docs/a.txt", which a path-backed store would refuse as absolute.
	fromRel := relOfChain(nodes)
	parentRel, _ := splitParent(fromRel)
	toRel := path.Join(parentRel, body.Name)

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

	// The tree layout mirrors names on disk; blob layout treats this as metadata.
	//
	// The error used to be discarded, which is the same defect the WebDAV MOVE had: metadata
	// committed, bytes stayed where they were, and the client was told the rename succeeded. The
	// metadata is put back if the bytes cannot follow, so the two never disagree silently.
	if merr := s.moveContentByPath(r.Context(), actor.ID().String(), fromRel, toRel, node.IsDir()); merr != nil {
		s.deps.Log.Error("rename: content move failed; restoring metadata",
			"from", fromRel, "to", toRel, "error", merr)
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

	writeJSON(w, http.StatusOK, toFileJSON(renamed, toRel))
}

// handleMove reparents a file or directory.
//
// The semantics live in moveOne, which the batch endpoint shares: one implementation, so a
// multi-file move and a single-file move cannot end up with different authorization or a different
// compensation path.
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	// A move that touches a mount at either end is decided here: within one mount it is
	// a remote rename; across a boundary it is a copy wearing a move's name, and the
	// refusal says so rather than silently duplicating the bytes (ADR-0014).
	if srcTgt, external := s.externalPath(r, body.From); external {
		_ = srcTgt
		s.handleExternalMove(w, r, body.From, body.To)
		return
	}
	if _, external := s.externalPath(r, body.To); external {
		s.handleExternalMove(w, r, body.From, body.To)
		return
	}

	moved, err := s.moveOne(r.Context(), r, actor, body.From, body.To, false)
	if err != nil {
		s.writeTransferError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toFileJSON(moved, body.To))
}

// handleDelete moves a node to the trash.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	// Deleting below a mount point deletes on the remote, immediately, with no trash —
	// the response says so. The mount point itself is not deletable here: unmounting is
	// a mount operation, and it never touches the remote's data (ADR-0014).
	if tgt, external := s.externalPath(r, rel); external {
		if tgt.Rel == "" {
			writeError(w, http.StatusConflict, "is_mount",
				"this folder is a mount point; DELETE /api/mounts/{id} removes it without touching the remote")
			return
		}
		s.handleExternalDelete(w, r, tgt.Rel)
		return
	}

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

	// Move the content out of the live path, then trash the row — the same sequence WebDAV's
	// DELETE uses, through the same primitive, so the two protocols cannot drift.
	//
	// Trashing is metadata-only: on the tree layout a deleted file used to keep occupying its path
	// until the retention sweep, which left the name unavailable and the retained copy sitting at a
	// path derived from a name that no longer existed in the namespace (2026-10-24 review, phase 0).
	parentRel := path.Dir(strings.TrimSuffix(rel, "/"))
	if parentRel == "." {
		parentRel = ""
	}
	err = swap.Retire(r.Context(),
		swap.Deps{Meta: s.deps.Meta, Content: s.deps.Content, Log: s.deps.Log,
			TrashCap: s.maxTrashBytes(r.Context())},
		node, parentRel,
		func(ctx context.Context, tx store.Tx, retired store.Node) error {
			// Same guard as WebDAV's DELETE: the validator comes from before the retirement, so a
			// writer that changed this file between the request resolving it and this transaction
			// makes the trash refuse rather than taking the writer's update with it.
			var expect *store.Precondition
			if !retired.IsDir() && node.ETag != "" {
				expect = &store.Precondition{IfMatch: node.ETag}
			}
			if terr := tx.TrashCappedExpect(ctx, retired.ID, s.maxTrashBytes(ctx), expect); terr != nil {
				return terr
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
				ActorIP: clientIP(r), UserAgent: r.UserAgent(),
				Action: "file.delete", TargetKind: "node", TargetID: retired.ID.String(),
				// The name the user deleted, not the reserved one the row carries in the trash.
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

	// Paged by a keyset cursor over (trashed_at, id). One extra row answers "is there more?"
	// without a second query.
	//
	// Before this the trash had neither a cursor nor a truncation flag, so a trash larger than the
	// cap was returned as if it were the whole thing: a client offering "restore everything"
	// restored a subset and reported success (2026-10-24 review, M8).
	limit := s.deps.Config.Limits.MaxProfindChildren
	cursor := r.URL.Query().Get("cursor")
	truncated := false
	nextCursor := ""
	var nodes []store.Node
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		list, lerr := tx.TrashedAfter(ctx, actor.ID(), cursor, limit+1)
		if lerr != nil {
			return lerr
		}
		if len(list) > limit {
			truncated = true
			list = list[:limit]
		}
		nodes = list
		if truncated && len(list) > 0 {
			nextCursor = store.FormatTrashCursor(list[len(list)-1])
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			// A cursor this server did not issue is a bad request. Starting over instead would make
			// a client with a stale cursor silently re-read page one forever.
			writeError(w, http.StatusBadRequest, "invalid_cursor",
				"the cursor is not one this server issued; drop it to start from the newest entry")
			return
		}
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

	body := map[string]any{
		"items": out,
		// True when there are more entries after this page; pass `cursor` to fetch them.
		"truncated": truncated,
		"cursor":    nextCursor,
	}
	// The window itself, so a client can explain the deadline without hardcoding a number that
	// the operator may have changed.
	if retention > 0 {
		body["retention_days"] = int(retention.Hours() / 24)
	}
	writeJSON(w, http.StatusOK, body)
}

// emptyTrashBatch bounds one purge call.
//
// The handler runs the purge in bounded batches and may be asked again (§3): a bin with a million
// entries must not hold one request open for minutes, and a client that gets a timeout has to be
// told "call me again" rather than left guessing. 500 roots is a few thousand rows in a subtree
// delete, which completes well inside a proxy's idle timeout.
const emptyTrashBatch = 500

// handleEmptyTrash permanently deletes the caller's own trash (core-scope §3).
//
// Three properties, each of which is the reason for a line below:
//
//  1. ONE implementation. It calls PurgeTrashFor, which is the same accounting and the same
//     recursive delete the worker's retention sweep uses (PurgeTrash). The acceptance criterion is
//     that a concurrent purge and delete cannot double-release a blob refcount, and that is only
//     true if the release exists once.
//
//  2. The caller's OWN trash. The owner id is a parameter of the statement that selects the roots,
//     so no handler mistake can reach another account's rows.
//
//  3. Idempotent and batched. A timeout means "call me again", never "unknown state": the response
//     says how much went and whether more remains.
func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var res store.PurgeResult
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, perr := tx.PurgeTrashFor(ctx, actor.ID(), emptyTrashBatch)
		res = got
		if perr != nil {
			return perr
		}
		// Audited as trash.purge with actor attribution (§3). The event vocabulary already
		// existed — only the worker emitted it — and the counts-style single-event pattern is
		// reused rather than one event per node: a thousand-node bin must not write a thousand
		// audit rows, or the log becomes the noisiest thing in the database.
		//
		// `via=user` is what tells an operator reading the log whether a user emptied their bin or
		// a retention policy did it. The two have different explanations to give.
		if got.Nodes == 0 {
			// Nothing was deleted. A sweep every few minutes would otherwise fill the log with
			// "deleted 0 items"; the same reasoning as the worker's purge.
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: string(audit.ActionTrashPurge), TargetKind: "trash", TargetID: "own",
			Result: "ok",
			Metadata: map[string]any{
				"nodes": got.Nodes, "roots": got.Roots, "bytes": got.Bytes,
				"via": "user", "more": got.More,
			},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"purged":      res.Roots,
		"nodes":       res.Nodes,
		"bytes_freed": res.Bytes,
		// `more` is the whole contract for a client: true means the bin may still hold items and
		// this call should be repeated. False means it is empty.
		"more": res.More,
	})
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

	// A node displaced by a MOVE overwrite is trashed as "<rand>.newcloud-replaced-<name>", and
	// tx.Restore brings it back with that name. Handing the user a name they never chose is bad
	// on its own, and it also blocks the fix for those reserved names being addressable: that
	// fix is only safe once restore stops producing them.
	//
	// The rename has to move the bytes with the metadata. On the tree layout the content path is
	// derived from parent and name, so a metadata-only rename leaves the bytes behind — which is
	// what swap.Relocate is for, and it is the same primitive the rename route uses.
	//
	// The restore commits first and the relocation second, on purpose: if the relocation fails
	// the node is live under the reserved name, which is no worse than before this change and is
	// still recoverable, and the caller gets the error instead of a 200 with a useless name.
	if restored.ParentID != nil && strings.Contains(restored.Name, ".newcloud-") {
		moved, merr := s.restoreArtifactName(r, restored)
		if merr != nil {
			s.writeStoreError(w, merr)
			return
		}
		restored = moved
	}

	writeJSON(w, http.StatusOK, toFileJSON(restored, restored.Name))
}

// restoreArtifactName recovers a usable name for a node restored out of a swap's trash row.
func (s *Server) restoreArtifactName(r *http.Request, n store.Node) (store.Node, error) {
	ctx := r.Context()
	fromRel, err := s.pathOfNode(ctx, n.OwnerID, n.ID)
	if err != nil {
		return store.Node{}, err
	}
	parentRel, err := s.pathOfNode(ctx, n.OwnerID, *n.ParentID)
	if err != nil {
		return store.Node{}, err
	}
	target := s.uniqueName(ctx, *n.ParentID, artifactName(n.Name))
	return s.relocate(ctx, n, fromRel, *n.ParentID, target, path.Join(parentRel, target), nil)
}

// artifactName strips the reserved marker a swap prefixes onto a sibling name.
//
// "<rand>.newcloud-replaced-budget.xlsx" is not a name anybody chose; "budget.xlsx" is the name
// the file had before it was overwritten.
func artifactName(name string) string {
	for _, marker := range []string{".newcloud-replaced-", ".newcloud-staged-"} {
		if i := strings.Index(name, marker); i >= 0 {
			if rest := name[i+len(marker):]; rest != "" {
				return rest
			}
		}
	}
	return "restored"
}
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
		// The layout decides how much history is affordable: the tree layout stores no per-version
		// content at all, while the blob layout keeps only the differences.
		"cheap_versions": s.deps.Content.Capabilities().CheapVersions,
		// A client has to know whether the history it is looking at can be RESTORED before offering
		// the action. On the tree layout the rows are a record of changes and the superseded bytes are
		// gone, so a Restore button there is a promise the deployment cannot keep — it would answer
		// 409 every time. This is a capability, not a hint: the UI disables the action on it, and the
		// handler refuses for the same reason.
		"version_content_available": s.deps.Blobs != nil,
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

	// A version can only be restored where its BYTES can be restored. The version's content lives at
	// a content address, which the tree layout does not keep: its bytes are at the path, and the
	// overwrite that created the version replaced them there. Restoring would flip the node's ETag
	// and size to the old version while the path still serves the current bytes — a success that did
	// not happen, which is the shape of failure this project keeps removing. handleVersionContent
	// refuses for the same reason; this is the write half of that refusal.
	if s.deps.Blobs == nil {
		writeError(w, http.StatusConflict, "no_version_content",
			"this storage layout does not keep addressable version history, so a version cannot be restored")
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
	writeJSON(w, http.StatusOK, toFileJSON(restored, relOfChain(nodes)))
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
		errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
	case errors.Is(err, layout.ErrNotFound):
		// The storage layer was asked for bytes that the metadata says exist. That is damage, not an
		// absence, and answering 404 is the confusion this project's own failure table forbids:
		// "blob missing but row present → error, never 404". It was mapped to 404 here, so every
		// route that opens content without its own handling — archive, thumbnail, share download,
		// content ticket — reported a file people can see in a listing as a file that does not exist.
		if s.deps.Metrics != nil {
			s.deps.Metrics.ObserveContentMissing()
		}
		s.deps.Log.Error("content missing for a file whose metadata exists", "error", err)
		writeError(w, http.StatusInternalServerError, "content_missing",
			"this file's metadata exists but its content is missing on the storage backend; an "+
				"operator needs to check the storage")
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
	case errors.Is(err, store.ErrTrashFull):
		// The trash is a separate resource from the quota (§4.3), so the answer names the trash:
		// "your storage quota is full" would send the user looking at a bar that has room on it.
		writeError(w, http.StatusInsufficientStorage, "trash_full",
			"the trash is full; empty it before deleting more")
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
		// The tree layout keeps its bytes at the path, and an overwrite replaces them there: the
		// superseded version's content is not retained anywhere, so there is nothing to open. (The
		// version ROW exists, because the metadata layer records the change.) Saying so is better than
		// streaming the current bytes under the label of an older version.
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
	// The returned path is the OWNER's content path, from the resolved chain: the caller passed the
	// listing's "/docs/a.txt" and a path-backed store will not accept that spelling.
	return nodes[len(nodes)-1], relOfChain(nodes), nil
}
