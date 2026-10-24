package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// sharePageTimeout bounds the share-access cookie. It is short because the cookie
// is the only thing standing between a verified password and unlimited downloads:
// a long-lived one turns a one-time password check into a permanent grant.
const sharePageTimeout = time.Hour

// handleSharePage shows a share's landing page.
//
// Anonymous by design. Every failure ??unknown token, revoked, expired, exhausted,
// wrong password ??renders the same page, so a visitor cannot use it to discover
// whether a token ever existed.
//
// Two consumers share this URL:
//
//	a browser     navigates here and needs the HTML shell, which then fetches JSON
//	a program     calls it directly and wants the JSON
//
// The Accept header distinguishes them. Without that split, opening a share link in a
// browser showed raw JSON, because the shell is only served as the router's fallback for
// paths no handler claims ??and this path is claimed.
func (s *Server) handleSharePage(w http.ResponseWriter, r *http.Request) {
	// A browser navigation sends Accept: text/html; fetch() and API clients do not.
	if wantsHTML(r) {
		s.serveAppShell(w, r)
		return
	}

	token := chiURLParam(r, "token")

	sh, err := s.deps.Shares.Resolve(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !s.shareOwnerActive(r.Context(), sh) {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	// Access gate: a password, a verified address, or both.
	if g := s.gate(r, sh); !g.allowed() {
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":              string(sh.Kind),
			"requires_password": g.RequiresPassword,
			"requires_email":    g.RequiresEmail,
			"label":             sh.Label,
			"hide_contents":     sh.HideContents,
		})
		return
	}

	// §10.1 分享访问: a visitor who passed the gate has opened the link, and that is the event the
	// owner wants to see — not only the downloads. Recorded once per page view: the listing is
	// fetched once per visit, unlike a media player's range requests.
	s.auditShareView(r, sh)

	// A request share never lists its contents: the server decides, not the UI.
	//
	// SHARE-04: a drop box is upload-only. It cannot list, read another uploader's file, or reveal what
	// else arrived — enforced here and not by hiding buttons in the client.
	if sh.Kind == store.ShareRequest {
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":          string(sh.Kind),
			"hide_contents": true,
			"label":         sh.Label,
			"upload_count":  sh.UploadCount,
			"max_uploads":   sh.MaxUploads,
			// The recipient sees the rules before uploading, not after failing.
			"max_bytes_per_file": sh.MaxBytesPerFile,
			"allowed_mime":       sh.AllowedMIME,
		})
		return
	}

	if sh.HideContents {
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": string(sh.Kind), "hide_contents": true, "label": sh.Label,
		})
		return
	}

	// A share whose grant stops at uploading must not list either.
	//
	// The listing names every entry, its size and its ETag — and the download route
	// already refuses exactly this grant. Serving the inventory to a create-only
	// visitor disclosed what the owner's permission split had withheld, which is the
	// same class of leak as serving the bytes: metadata about content is content
	// information (2026-10-24 review, H10). The response keeps the fields an
	// uploader's landing page needs and hides the tree, exactly as HideContents does.
	if !sh.CanRead() {
		writeJSON(w, http.StatusOK, map[string]any{
			"kind":          string(sh.Kind),
			"hide_contents": true,
			"label":         sh.Label,
			"can_upload":    sh.CanCreate(),
			"can_read":      false,
		})
		return
	}

	// Listing a shared directory.
	//
	// The lookup error is handled rather than discarded. It was previously dropped with `_ =`,
	// which meant a link whose file had been trashed answered 200 with an empty `root_name` and
	// no children — the recipient saw a blank page instead of "this is no longer available", and
	// nothing in the response said why. SHARE-06 asks for the effect of a deleted or moved file
	// to be defined; a silent empty listing is not a definition.
	var node store.Node
	var children []store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, sh.NodeID)
		if nerr != nil {
			return nerr
		}
		node = n
		c, cerr := tx.Children(ctx, n.ID, s.deps.Config.Limits.MaxProfindChildren)
		children = c
		return cerr
	})
	if err != nil {
		// A trashed or purged node is reported as gone, not as an internal failure: the link is
		// valid, the file is not.
		if errors.Is(err, store.ErrNotFound) {
			s.renderShareError(w, r, store.ErrShareGone)
			return
		}
		s.renderShareError(w, r, err)
		return
	}

	items := make([]fileJSON, 0, len(children))
	for _, c := range children {
		items = append(items, toFileJSON(c, path.Join("/", c.Name)))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       string(sh.Kind),
		"label":      sh.Label,
		"root_name":  node.Name,
		"root_kind":  string(node.Kind),
		"children":   items,
		"can_upload": sh.CanCreate(),
		"can_read":   sh.CanRead(),
	})
}

// handleShareDownload streams a file through a share.
//
// The bytes go through this process rather than a signed storage URL because that
// is what makes the download recordable and the counter exact.
func (s *Server) handleShareDownload(w http.ResponseWriter, r *http.Request) {
	token := chiURLParam(r, "token")

	// ResolveForDownload, not Resolve: the download cap is enforced by the session logic
	// below, which can tell a continuation from a new download. Applying it here would
	// refuse a Range request that belongs to a download already paid for — and a media
	// player seeking through a shared video issues one of those per seek.
	sh, err := s.deps.Shares.ResolveForDownload(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !sh.CanRead() {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}
	if !s.shareOwnerActive(r.Context(), sh) {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}
	// The access gate, or the capability that stands in for it on the content origin.
	//
	// The gate reads host-only cookies, which by design never reach this origin; without the
	// capability branch, a split deployment would 404 every password-protected and
	// recipient-verified download even though the visitor had just satisfied the gate. The two
	// paths are mutually exclusive and the capability is only consulted when present and valid.
	verdict := s.gate(r, sh)
	grant := s.downloadGrantHash(r, sh)
	actorKind := s.shareActorKind(r, sh)
	actorEmail := s.shareRecipientEmail(r, sh)
	if g, ok := s.shareContentCapability(r, sh.ID, "download"); ok {
		verdict = gateResult{Email: g.Email}
		grant = g.Grant
		actorKind = g.Actor
		actorEmail = g.Email
	}
	if !verdict.allowed() {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	// Record that a verified recipient actually reached the content.
	//
	// MarkRecipientAccess existed, was implemented on both backends, and had NO production
	// caller — so first_access_at and last_access_at were never written, and the columns that
	// answer "did the recipient ever open this" were permanently NULL. The same defect class as
	// a retention column that is stored and never enforced: the schema promises an audit trail
	// nothing produces.
	//
	// Failure to record is logged but does not fail the download: the recipient is authorised,
	// and refusing to serve a file because an audit column could not be updated would trade a
	// real user's access for a bookkeeping row.
	if verdict.Email != "" {
		err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			return tx.MarkRecipientAccess(ctx, sh.ID, verdict.Email)
		})
		if err != nil {
			s.deps.Log.Warn("could not record recipient access",
				"share", sh.ID.String(), "error", err)
		}
	}

	// Resolve the target relative to the share root. The requested name is
	// resolved as a child, never concatenated into a path: concatenation is how
	// "../.." escapes a share root.
	wantName := r.URL.Query().Get("name")

	var node store.Node
	var filePath string
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		root, rerr := tx.Node(ctx, sh.NodeID)
		if rerr != nil {
			return rerr
		}
		if wantName == "" {
			node = root
			filePath = root.Name
			return nil
		}
		child, cerr := tx.ChildByName(ctx, root.ID, wantName)
		if cerr != nil {
			return cerr
		}
		node = child
		filePath = wantName
		return nil
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such file in this share")
		return
	}
	if node.IsDir() {
		writeError(w, http.StatusBadRequest, "is_directory", "this endpoint serves files")
		return
	}

	// The content store is keyed by OWNER + path, and the owner here is the NODE's, not the share
	// creator's. An owner-created share has the same value for both; a DELEGATED share — a grantee
	// holding `share` on somebody else's file — has bytes that only exist under the node's owner, so
	// using the creator looked in the wrong namespace and 404'd every delegated download.
	contentOwner := node.OwnerID
	if contentOwner == uuid.Nil {
		contentOwner = sh.OwnerID
	}
	rel, err := s.pathOfNode(r.Context(), contentOwner, node.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	_ = rel

	// Open before charging. The previous order spent a download and only then
	// failed with a 404 when the bytes were missing, so a storage fault silently
	// consumed a visitor's last allowed download.
	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: contentOwner.String(), Path: rel,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer rc.Close()

	// Authorize this download as a *session*, not as a request.
	//
	// A media player seeking through a shared video issues a Range request per
	// seek, and a resumed download is another request for the same bytes. Charging
	// each one consumed a limited share almost immediately. The session is keyed by
	// (share, node, visitor grant) and carries the node's ETag, so:
	//   - the first request pays one download and stores the session,
	//   - later Range requests and retries join it and pay nothing,
	//   - a different visitor, or the same visitor downloading another file, pays,
	//   - a session minted for one version never serves another.
	// grant, actorKind and actorEmail were resolved above, because for a capability-authenticated
	// request they come from the capability rather than from cookies.
	//
	// A Range request is a continuation only when it asks for less than the whole
	// entity. `Range: bytes=0-` names every byte from the start: it is a fresh
	// download wearing a Range header, and treating it as a continuation let a
	// visitor re-fetch the COMPLETE file, for free, for the session's TTL after a
	// link's download limit had been exhausted (2026-10-24 review, H9). A seek or a
	// resume names a non-zero start or a bounded window and rides the session the
	// first request paid for.
	continuation := isPartialRange(r.Header.Get("Range"), node.Size)
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		_, charged, aerr := tx.AuthorizeDownload(ctx, store.DownloadSession{
			ShareID:   sh.ID,
			NodeID:    node.ID,
			GrantHash: grant,
			ETag:      node.ETag,
			ExpiresAt: time.Now().Add(store.DownloadSessionTTL),
		}, continuation)
		if aerr != nil {
			return aerr
		}
		if !charged {
			// A Range request or a retry joining a session already paid for. Recording it would
			// inflate the log with one row per byte-range of a video and bury the real events.
			return nil
		}
		// §10.1: a public share download is anonymous access to file content, and it was not
		// audited at all. share.download was declared, had an emitter (RecordDownload), had tests —
		// and that emitter had no caller, so nothing ever wrote the event while the vocabulary
		// suggested otherwise. It is written here, inside the transaction that CHARGES the
		// download: if the row cannot be written, the download is not authorised either, because
		// "nobody knows this link was used" is not a state this should be able to reach silently.
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind:  actorKind,
			ActorEmail: actorEmail,
			ActorIP:    clientIP(r),
			UserAgent:  r.UserAgent(),
			Action:     "share.download",
			TargetKind: "share",
			TargetID:   sh.ID.String(),
			ShareID:    &sh.ID,
			Result:     "ok",
			Metadata: map[string]any{
				"name":       node.Name,
				"node_id":    node.ID.String(),
				"share_kind": string(sh.Kind),
			},
		})
	})
	if err != nil {
		// Out of allowance, or the share went away between the gate and here.
		s.renderShareError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", contentTypeFor(node))
	w.Header().Set("Content-Disposition", contentDisposition(node.Name))
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("ETag", node.ETag)

	// ServeContent supplies Range, If-Range and If-None-Match against the seekable
	// reader: seeking inside a shared video works, and a resumed download continues
	// instead of restarting. The previous code set Content-Length and copied the
	// whole entity, so every Range request received the full body.
	http.ServeContent(w, r, node.Name, node.ModifiedAt, rc)
	_ = filePath
}

// downloadGrantHash identifies the visitor's access grant for session binding.
//
// It hashes whichever grant the visitor actually proved ??the share password cookie
// or the recipient-email cookie ??so two visitors cannot share one paid download.
// A visitor who proved nothing (an open link) gets a stable anonymous key per
// client address, which is the only identity available and is enough to keep one
// client's Range requests on one session.
func (s *Server) downloadGrantHash(r *http.Request, sh store.Share) []byte {
	grant := ""
	if c, err := r.Cookie(shareCookieName(sh.ID.String())); err == nil && c.Value != "" {
		grant = "pw:" + c.Value
	} else if c, err := r.Cookie(recipientCookieName(sh.ID.String())); err == nil && c.Value != "" {
		grant = "rcpt:" + c.Value
	} else {
		grant = "anon:" + clientIP(r) + ":" + r.UserAgent()
	}
	sum := sha256.Sum256([]byte(grant))
	return sum[:]
}

// handleShareUpload accepts a file into a request share.
//
// The uploader is anonymous, so the only guardrails are the share's own limits:
// count, per-file size, and MIME allow-list. All three are enforced server-side.
func (s *Server) handleShareUpload(w http.ResponseWriter, r *http.Request) {
	token := chiURLParam(r, "token")

	sh, err := s.deps.Shares.Resolve(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !sh.CanCreate() {
		writeError(w, http.StatusNotFound, "not_found", "this link does not accept uploads")
		return
	}
	if !s.shareOwnerActive(r.Context(), sh) {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}
	if !s.gate(r, sh).allowed() {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	// Refuse the body before reading it when the share declares a per-file cap.'
	limit := s.deps.Config.Limits.MaxFileBytes
	if sh.MaxBytesPerFile != nil && *sh.MaxBytesPerFile > 0 && *sh.MaxBytesPerFile < limit {
		limit = *sh.MaxBytesPerFile
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the upload exceeds the limit for this link")
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

	// MIME allow-list is checked against the client-declared type and the
	// extension: a single check is trivially bypassed by lying about one of them.
	if len(sh.AllowedMIME) > 0 && !s.mimeAllowed(sh.AllowedMIME, header) {
		writeError(w, http.StatusUnsupportedMediaType, "mime_not_allowed", "this file type is not accepted")
		return
	}

	var root store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, sh.NodeID)
		root = n
		return nerr
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	rootPath, err := s.pathOfNode(r.Context(), sh.OwnerID, root.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not resolve the target")
		return
	}

	// Names are uniquified rather than overwritten: an anonymous uploader must not
	// be able to clobber a file another visitor already delivered.
	finalName := s.uniqueName(r.Context(), root.ID, name)
	rel := path.Join(rootPath, finalName)

	// Refuse an over-cap delivery BEFORE writing its bytes (§6.1 保护公共投递箱不耗尽磁盘).
	//
	// The authoritative check is RecordUpload below, which increments and refuses under one guarded
	// statement — but it runs AFTER Content.Put, so an oversized request still wrote its content to
	// storage first. An anonymous visitor cannot see that cost, and the rejected bytes then sit in
	// the store until the collector's grace period expires.
	//
	// This pre-check is advisory, not authoritative: two concurrent requests can both pass it. It
	// exists to stop the ordinary case (one visitor sending a file larger than what is left of the
	// allowance) from touching storage at all. The guarded UPDATE remains the thing that is correct
	// under concurrency, and it is what the error and the status come from.
	if sh.MaxTotalBytes != nil {
		if remaining := *sh.MaxTotalBytes - sh.ReceivedBytes; header.Size > remaining {
			s.renderShareError(w, r, store.ErrShareExhausted)
			return
		}
	}
	// The rate limit gets the same treatment, and for the same reason: RecordUpload checks it after
	// the content is stored, so a visitor who has exhausted their allowance could keep writing
	// bytes — each request refused, each refusal still costing storage. The authoritative check
	// stays where it is; this one just makes the common case free.
	if exceeded, rerr := s.deps.Shares.UploadRateExceeded(r.Context(), sh); rerr == nil && exceeded {
		s.renderShareError(w, r, store.ErrShareRateLimited)
		return
	}

	res, err := s.deps.Content.Put(r.Context(), layout.Node{
		Owner: sh.OwnerID.String(), Path: rel,
	}, file, header.Size)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	// Count before committing the metadata row: if the share is already at its
	// upload limit, no node should be created.
	if err := s.deps.Shares.RecordUpload(r.Context(), sh, share.DownloadMeta{
		ActorKind: "anonymous", ActorIP: clientIP(r), UserAgent: r.UserAgent(),
		Name: finalName, Bytes: res.Size,
	}); err != nil {
		s.renderShareError(w, r, err)
		return
	}

	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		// Stamp the retention in force NOW, not a reference to the share's current policy.
		// A link whose retention is shortened later must not retroactively delete deliveries
		// made under the old one, and lengthening it must not extend them either.
		var retainUntil *time.Time
		if sh.RetentionSeconds != nil && *sh.RetentionSeconds > 0 {
			t := time.Now().Add(time.Duration(*sh.RetentionSeconds) * time.Second)
			retainUntil = &t
		}
		n, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: sh.OwnerID, ParentID: root.ID, Name: finalName,
			Size: res.Size, Mime: header.Header.Get("Content-Type"),
			ETag: etagFor(res.Hash), BlobID: s.deps.Meta.BlobIDFor(ctx, res.Hash, res.Size),
			BlobHash: res.Hash, RetainUntil: retainUntil,
		})
		if cerr != nil {
			return cerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "share.upload", TargetKind: "node", TargetID: n.ID.String(),
			ShareID: &sh.ID, Result: "ok",
			Metadata: map[string]any{"name": finalName, "share_kind": string(sh.Kind)},
		})
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"name": finalName, "size": res.Size, "accepted": true,
	})
}

// renderShareError produces the single response shape every share failure uses.
func (s *Server) renderShareError(w http.ResponseWriter, r *http.Request, err error) {
	// Rate limiting is the one cause that is reported distinctly, and deliberately so.
	//
	// Every other cause looks identical from outside, because distinguishing "expired" from
	// "revoked" from "never existed" turns the endpoint into a token oracle. A rate limit is
	// different in kind: the caller already proved they hold a working link by triggering it, so
	// nothing is leaked, and a 404 with "this link is not available" would tell a legitimate
	// visitor their link is dead when the correct instruction is "try again shortly". Answering 429
	// also lets a client back off instead of retrying into the limit.
	if errors.Is(err, store.ErrSharePasswordThrottled) {
		w.Header().Set("Retry-After", "900")
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many password attempts on this link; try again later")
		return
	}
	if errors.Is(err, store.ErrShareRateLimited) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many uploads through this link; try again shortly")
		return
	}
	// The cause is hidden from the VISITOR and recorded for the OWNER.
	//
	// Indistinguishable responses are what keep this endpoint from being a token oracle, but the
	// distinction is exactly what the person who published the link needs: "my link stopped
	// working" has two very different answers — it expired, or somebody revoked it. share.expired
	// was declared with no emitter, so the owner had no way to tell. The audit log is not a public
	// answer surface, so recording the reason there leaks nothing.
	if errors.Is(err, store.ErrShareExpired) {
		s.auditShareRefusal(r, "share.expired", "anonymous")
	}
	// The other half of "my link stopped working": the allowance ran out. Same reasoning — hidden
	// from the visitor, recorded for the owner, who is the only one who can raise the limit.
	if errors.Is(err, store.ErrShareExhausted) {
		s.auditShareRefusal(r, "share.limit_reached", "anonymous")
	}
	writeError(w, http.StatusNotFound, "not_found", "this link is not available")
}

// shareCookieName scopes the access grant to one share.
//
// The __Secure- prefix, not __Host-: the cookie is deliberately scoped to "/s/<token>", and
// __Host- requires Path=/ ??a browser silently drops such a cookie, which looks exactly like a
// wrong password. The per-share scoping therefore lives in the name and in the signed value,
// both of which the server checks, rather than in the prefix.
func shareCookieName(shareID string) string {
	return "__Secure-wc_share_" + shareID
}

// mintShareCookie issues the access grant.
func (s *Server) mintShareCookie(sh store.Share) (string, error) {
	return s.deps.Tickets.Mint(ticket.PurposeShareAccess, ticket.Claims{
		Subject: sh.ID.String(),
		ID:      sh.ID.String(),
		Expiry:  time.Now().Add(sharePageTimeout),
	})
}

// shareCookieValid reports whether the visitor has proven access to this share.
func (s *Server) shareCookieValid(r *http.Request, sh store.Share) bool {
	c, err := r.Cookie(shareCookieName(sh.ID.String()))
	if err != nil || c.Value == "" {
		return false
	}
	claims, err := s.deps.Tickets.Verify(ticket.PurposeShareAccess, c.Value)
	if err != nil {
		return false
	}
	// The subject must be this share: a grant for another share is not a grant
	// for this one.
	return claims.Subject == sh.ID.String()
}

// pathOfNode reconstructs a node's path from its ancestors.
//
// The content store is keyed by owner + path, so the path is needed even though
// the application addresses nodes by id everywhere else.
func (s *Server) pathOfNode(ctx context.Context, ownerID [16]byte, nodeID [16]byte) (string, error) {
	uid := uuidFrom(nodeID)

	var segs []string
	for depth := 0; depth < 256; depth++ {
		var n store.Node
		err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, gerr := tx.Node(ctx, uid)
			n = got
			return gerr
		})
		if err != nil {
			return "", err
		}
		if n.ParentID == nil {
			break
		}
		segs = append(segs, n.Name)
		uid = *n.ParentID
	}
	// Reverse: the loop collected leaf-first.
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return strings.Join(segs, "/"), nil
}

// uniqueName returns name, or "name (2)" and so on if it is taken.
func (s *Server) uniqueName(ctx context.Context, parentID [16]byte, name string) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)

	for i := 1; i < 100; i++ {
		candidate := name
		if i > 1 {
			candidate = base + " (" + strconv.Itoa(i) + ")" + ext
		}
		taken := false
		_ = s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.ChildByName(ctx, parentID, candidate); err == nil {
				taken = true
			}
			return nil
		})
		if !taken {
			return candidate
		}
	}
	// Deterministic suffix of last resort rather than an unbounded loop.
	return base + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ext
}

// mimeAllowed checks both the declared type and the extension.
func (s *Server) mimeAllowed(allowed []string, header *multipart.FileHeader) bool {
	declared := strings.ToLower(strings.TrimSpace(header.Header.Get("Content-Type")))
	if semicolon := strings.IndexByte(declared, ';'); semicolon >= 0 {
		declared = strings.TrimSpace(declared[:semicolon])
	}
	ext := strings.ToLower(path.Ext(header.Filename))
	guessed := strings.ToLower(guessType(header.Filename))

	for _, want := range allowed {
		want = strings.ToLower(strings.TrimSpace(want))
		switch {
		case want == declared, want == guessed, want == ext:
			return true
		}
	}
	return false
}

func uuidFrom(b [16]byte) [16]byte { return b }

var (
	_ = errors.Is
	_ = io.Copy
)

// shareOwnerActive reports whether the account that published a link is still able to use it.
//
// ID-08 asks for a determined policy after an account is disabled, and this is the sharing half of
// it. Without the check, disabling somebody closed their own sessions — the middleware refuses them
// on every request — while every link they had already published went on serving files to anyone
// holding the URL. The account was blocked; the access it had granted was not.
//
// The policy is suspension, not deletion: the links come back if the account is re-enabled, because
// revoking them would destroy a sharing decision an administrator may only have meant to pause, and
// the files themselves stay owned and untouched. A disabled or pending owner is treated exactly like
// an expired link — 404, the same answer every other unavailable share gets, so the response does not
// reveal that the account exists.
// shareOwnerActive reports whether the share's creator may still have a public link.
//
// Two conditions, and the second was missing:
//
//  1. The creator's account is ACTIVE — a disabled account must not keep a link alive.
//  2. The node is still live, and the creator STILL HOLDS `share` on it.
//
// The second is what a DELEGATED link needs. A grantee can be given `share` on somebody else's file,
// mint a link, and have that grant revoked — and checking only the account left the public link serving
// the owner's content afterwards. Measured, not assumed: with no creator check the revoked link answers
// 200 with the file's bytes, which makes a grant a way to turn somebody's file into a permanent public
// URL.
//
// Fail closed on every error: the visitor is anonymous, and there is nobody to ask.
//
// The reads and the authorization are SEPARATE transactions. Authorize opens its own, and calling it
// inside WithTx deadlocks the in-memory store (bounded at ten seconds, so it looks like a hang) and is
// refused by the PostgreSQL store.
func (s *Server) shareOwnerActive(ctx context.Context, sh store.Share) bool {
	var (
		owner store.User
		node  store.Node
	)
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UserByID(ctx, sh.OwnerID)
		if uerr != nil {
			return uerr
		}
		owner = u
		// Node() refuses a trashed or deleted node, which is the other way a share goes stale.
		n, nerr := tx.Node(ctx, sh.NodeID)
		if nerr != nil {
			return nerr
		}
		node = n
		return nil
	})
	if err != nil {
		s.deps.Log.Warn("could not read the share's owner or node",
			"share", sh.ID.String(), "error", err)
		return false
	}
	if !owner.IsActive() {
		return false
	}

	role := authz.RoleUser
	switch owner.Role {
	case "admin":
		role = authz.RoleAdmin
	case "guest":
		role = authz.RoleGuest
	}
	actor := authz.NewUserActor(owner.ID, owner.Email, role,
		authz.ScopeRead|authz.ScopeWrite).WithUsername(owner.Username)
	cap, cerr := s.deps.Authz.Authorize(actor, node.ID)
	if cerr != nil {
		s.deps.Log.Warn("could not authorize the share's creator",
			"share", sh.ID.String(), "error", cerr)
		return false
	}
	return cap.Can(authz.PermShare)
}

// handleShareThumbnail renders a preview for a visitor of a public share.
//
// §6.3 asks for preview derivatives to be protected resources reached through the same authorization
// as the content, not through a guessable permanent URL. The credential here is the share token plus
// whatever gate the share declares — a password, a verified recipient — checked in the same order as
// the download route. The node is resolved as a CHILD of the share root, never by the requested name
// being concatenated into a path, so "..%2f" cannot escape the shared subtree.
//
// Without this route a shared folder showed type badges where it could show pictures, which made the
// share page the one place previews were unavailable rather than the one place they were unsafe.
func (s *Server) handleShareThumbnail(w http.ResponseWriter, r *http.Request) {
	token := chiURLParam(r, "token")

	sh, err := s.deps.Shares.Resolve(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !sh.CanRead() {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}
	if !s.shareOwnerActive(r.Context(), sh) {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}
	// Same two-path gate as the download: the cookie gate on a single-origin deployment, or the
	// capability the application origin minted after that gate passed (PRG-002). Previews are
	// content too, so they moved to the content origin with the download and need the same
	// accommodation for host-only cookies.
	if _, ok := s.shareContentCapability(r, sh.ID, "thumbnail"); !ok {
		if !s.gate(r, sh).allowed() {
			writeError(w, http.StatusNotFound, "not_found", "this link is not available")
			return
		}
	}

	wantName := r.URL.Query().Get("name")
	var node store.Node
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		root, rerr := tx.Node(ctx, sh.NodeID)
		if rerr != nil {
			return rerr
		}
		if wantName == "" {
			node = root
			return nil
		}
		child, cerr := tx.ChildByName(ctx, root.ID, wantName)
		if cerr != nil {
			return cerr
		}
		if child.TrashedAt != nil {
			return store.ErrNotFound
		}
		node = child
		return nil
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if node.IsDir() {
		writeError(w, http.StatusBadRequest, "is_directory", "a directory has no thumbnail")
		return
	}
	key := contentKey(node)
	if key == ([32]byte{}) {
		writeError(w, http.StatusUnsupportedMediaType, "not_an_image", "this file has no content address")
		return
	}
	// Classified from the stored type before the bytes are opened, exactly as the authenticated
	// route does: a share full of PDFs must not cost a decode attempt each.
	if kind := previewKindForNode(node.Name, node.Mime); kind != previewImage {
		s.observeThumbnail("not_an_image")
		writeError(w, http.StatusUnsupportedMediaType, "not_an_image", "this file has no thumbnail")
		return
	}

	rel, perr := s.pathOfNode(r.Context(), node.OwnerID, node.ID)
	if perr != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	rc, oerr := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: node.OwnerID.String(), Path: rel,
	})
	if oerr != nil {
		s.writeStoreError(w, oerr)
		return
	}
	defer rc.Close()

	s.serveThumbnailBytes(w, r, node, rel, key, rc)
}

// shareActorKind classifies the visitor of a public link for the audit row.
//
// A share visitor is usually not authenticated at all, and calling them a "user" would be a lie that
// makes an anonymous download look like an account action. A visitor who proved a recipient address
// is recorded as that address, which is the strongest identity a link can carry.
func (s *Server) shareActorKind(r *http.Request, sh store.Share) string {
	if s.shareRecipientEmail(r, sh) != "" {
		return "share_recipient"
	}
	return "anonymous"
}

// shareRecipientEmail returns the address a visitor proved for this share, if any.
func (s *Server) shareRecipientEmail(r *http.Request, sh store.Share) string {
	if !share.RequiresRecipient(sh) {
		return ""
	}
	email := s.recipientCookieEmail(r, sh)
	if email == "" {
		return ""
	}
	// Re-check the invitation: the cookie is signed, but the recipient may have been withdrawn
	// since it was issued.
	if _, err := s.deps.Shares.ResolveRecipient(r.Context(), sh, email); err != nil {
		return ""
	}
	return email
}

// auditShareRefusal records why a public link refused a visitor.
//
// Anonymous, and best-effort in a separate transaction: the refusal has already been decided, and a
// bookkeeping failure must not turn a 404 into a 500 the visitor can see. It is still recorded,
// because "nobody knows this link was used" and "this link was used and refused" are different
// states for the person who owns it.
//
// The path in the metadata is redacted for the same reason the access log's is: a refusal can carry
// a perfectly valid token (a wrong password is a refusal), and the raw token in an audit row is a
// capability stored in the one place that is supposed to outlive everything else.
func (s *Server) auditShareRefusal(r *http.Request, action, actorKind string) {
	ctx := store.WithAuthMethod(r.Context(), store.AuthShareLink)
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: actorKind, ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: action, TargetKind: "share", Result: "denied",
			Metadata: map[string]any{"path": redactLogPath(r.URL.Path)},
		})
	})
	if err != nil {
		s.deps.Log.Warn("could not audit a share refusal", "action", action, "error", err)
	}
}

// auditShareView records that somebody opened a share link.
//
// Best effort in its own transaction: the visitor has already been authorised, and a bookkeeping
// failure must not turn a working link into an error they cannot act on. The row names the share and
// the visitor class, which is all a link can honestly say about an anonymous person.
func (s *Server) auditShareView(r *http.Request, sh store.Share) {
	// §10.1 auth_method: a public link, not an account.
	ctx := store.WithAuthMethod(r.Context(), store.AuthShareLink)
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind:  s.shareActorKind(r, sh),
			ActorEmail: s.shareRecipientEmail(r, sh),
			ActorIP:    clientIP(r), UserAgent: r.UserAgent(),
			Action: "share.view", TargetKind: "share", TargetID: sh.ID.String(),
			ShareID: &sh.ID, Result: "ok",
			Metadata: map[string]any{"share_kind": string(sh.Kind), "label": sh.Label},
		})
	})
	if err != nil {
		s.deps.Log.Warn("could not audit a share view", "share", sh.ID.String(), "error", err)
	}
}

// isPartialRange reports whether a Range header names less than the whole entity,
// which is what makes a request a continuation of a download rather than a new one.
//
// The distinction exists because the download-session logic charges nothing for a
// continuation — that is what keeps a video seek from consuming a limited link — but
// "any Range header" was once the test, and `Range: bytes=0-` names the COMPLETE
// entity from the first byte. Treating that as a continuation meant a visitor could
// re-download the entire file, repeatedly, for the session's TTL after the share's
// download limit was exhausted.
//
// Continuations (free): a non-zero start, a bounded window that ends before the last
// byte, and suffix ranges shorter than the entity. Fresh downloads (charged): no
// Range at all, `bytes=0-`, `bytes=0-(size-1)`, a suffix as long as the entity,
// multi-range requests (conservatively — a player that asks for scattered windows is
// doing something a single session should not be asked to cover), and anything
// malformed (charge rather than guess).
func isPartialRange(header string, size int64) bool {
	if header == "" {
		return false
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return false
	}
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, ",") {
		return false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return false
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	if first == "" {
		// Suffix range: the last N bytes. It is a continuation only when it is
		// genuinely shorter than the entity.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return false
		}
		return n < size
	}

	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return false
	}
	if start > 0 {
		return true
	}
	// start == 0: the whole entity unless the window is bounded short of the end.
	if last == "" {
		return false
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil {
		return false
	}
	return end >= 0 && end < size-1
}
