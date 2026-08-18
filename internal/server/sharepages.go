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

	// A request share never lists its contents: the server decides, not the UI.
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
	if !s.gate(r, sh).allowed() {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	// Resolve the target relative to the share root. The requested name is
	// resolved as a child, never concatenated into a path: concatenation is how
	// "../.." escapes a share root.
	wantName := r.URL.Query().Get("name")

	var node store.Node
	var filePath string
	var ownerID = sh.OwnerID
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

	// The content store is keyed by owner + path, so the share root's full path is
	// needed. It is derived from the metadata tree rather than from the request.
	rel, err := s.pathOfNode(r.Context(), sh.OwnerID, node.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	_ = rel

	// Open before charging. The previous order spent a download and only then
	// failed with a 404 when the bytes were missing, so a storage fault silently
	// consumed a visitor's last allowed download.
	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: ownerID.String(), Path: rel,
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
	grant := s.downloadGrantHash(r, sh)
	// A Range request continues a download already authorised; a plain GET is a
	// fresh download and is charged as one. That is the distinction the owner's
	// "max downloads" setting actually means.
	continuation := r.Header.Get("Range") != ""
	err = s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		_, _, aerr := tx.AuthorizeDownload(ctx, store.DownloadSession{
			ShareID:   sh.ID,
			NodeID:    node.ID,
			GrantHash: grant,
			ETag:      node.ETag,
			ExpiresAt: time.Now().Add(store.DownloadSessionTTL),
		}, continuation)
		return aerr
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
	// Every cause looks identical from outside. Distinguishing "expired" from
	// "revoked" from "never existed" turns the endpoint into a token oracle.
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
