package server

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// handleContent serves user-uploaded bytes from the content origin.
//
// Every response here is forced to download. The uploaded file may itself be a
// perfectly valid HTML document with a script tag; serving it as text/html from
// an origin that shares a cookie jar with the app would be stored XSS with extra
// steps. Attachment + nosniff + the sandbox CSP set in Caddy makes the browser
// treat it as inert bytes.
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	// Every content response is forced to download, before anything can fail or redirect. The
	// uploaded file may be a valid HTML document with a script tag; the point of the separate origin
	// is that the browser never treats it as part of the application, and these headers say so even
	// if a proxy strips the ones the deployment adds.
	w.Header().Set("Content-Disposition", `attachment; filename="download"`)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// The ticket, not the cookie, is the credential.
	//
	// A cookie for the application's origin is not sent to the content origin (that is what makes
	// the separation worth having), so the app mints a short-lived ticket and puts it in the URL.
	// The ticket names the path it is good for: taking the path from the request instead would make
	// the ticket a bearer credential for the whole account, which is exactly the over-grant the
	// download purpose exists to avoid.
	tok := r.URL.Query().Get("ticket")
	if tok == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}
	claims, err := s.deps.Tickets.Verify(ticket.PurposeDownload, tok)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}
	rel := claims.Extra["path"]
	if rel == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}
	// The URL path has to agree with the ticket's. Serving the ticket's file under a different URL
	// would make the URL a lie: caches and proxies key on it, and a URL that says "other.txt" while
	// returning file.txt is a confusion the next feature inherits.
	if asked := strings.TrimPrefix(path.Clean("/"+chi.URLParam(r, "*")), "/"); asked != rel {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}
	ownerID, err := uuid.Parse(claims.Subject)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}

	// Authorization is the ticket's scope, but the node is resolved again so a file deleted (or
	// moved) after the ticket was minted is not served from a stale path.
	nodes, err := s.deps.Meta.Resolve(r.Context(), ownerID, rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}
	node := nodes[len(nodes)-1]
	if node.IsDir() {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	}

	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: ownerID.String(), Path: rel,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer rc.Close()

	// Always inline=false: the content origin never renders, whatever the client asks for.
	http.ServeContent(w, r, node.Name, node.ModifiedAt, rc)
}

// ErrUnsafePath is returned for names that must never reach the filesystem or a
// URL, regardless of what the client claims.
var ErrUnsafePath = errors.New("unsafe path component")

// windowsReserved are device names Windows refuses, case-insensitively and with
// or without an extension. A file named "CON.txt" syncs to a Windows client and
// then cannot be opened, which users report as data loss.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// windowsIllegal are characters Windows forbids in a filename. The colon is the
// dangerous one: "C:evil" is a drive-relative path and "file:stream" addresses
// an NTFS alternate data stream, so a name containing it is never just a name.
const windowsIllegal = `<>:"|?*`

// SanitizeSegment validates a single path component.
//
// A name is a name, not a path: separators and traversal sequences are rejected
// rather than silently cleaned, because "cleaned" input that differs from what
// the user submitted is how two files end up with one name.
func SanitizeSegment(seg string) (string, error) {
	if seg == "" || seg == "." || seg == ".." {
		return "", ErrUnsafePath
	}
	if !utf8.ValidString(seg) {
		return "", ErrUnsafePath
	}
	if len(seg) > 255 {
		return "", ErrUnsafePath
	}
	for _, r := range seg {
		if r == 0 || r == '/' || r == '\\' {
			return "", ErrUnsafePath
		}
		if r < 0x20 || r == 0x7f {
			return "", ErrUnsafePath
		}
		if strings.ContainsRune(windowsIllegal, r) {
			return "", ErrUnsafePath
		}
		if unicode.IsControl(r) {
			return "", ErrUnsafePath
		}
	}
	// Windows strips trailing dots and spaces, so "report." and "report" would
	// collide on a Windows client.
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return "", ErrUnsafePath
	}
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return "", ErrUnsafePath
	}
	return seg, nil
}

// SplitPath validates a URL path into components.
func SplitPath(p string) ([]string, error) {
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "" {
		return nil, nil
	}
	parts := strings.Split(p, "/")
	for _, seg := range parts {
		if _, err := SanitizeSegment(seg); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

var _ io.Reader

// handleContentTicket mints a URL for the content origin.
//
// The application origin answers this, because it is where the session cookie is sent. The URL it
// returns points at the CONTENT origin, which is the whole design: the app never serves the bytes, so
// a file that is really HTML cannot run with the app's origin, cookies or CSP.
//
// The ticket is short-lived and names exactly one path. It is not a session: it cannot list, upload,
// delete, or be replayed for a different file, and it stops working when it expires whether or not
// anybody revokes it.
func (s *Server) handleContentTicket(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+body.Path), "/")
	if rel == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "a path is required")
		return
	}

	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	node := nodes[len(nodes)-1]
	if node.IsDir() {
		// Directories are archived by the app, not streamed as one blob.
		writeError(w, http.StatusBadRequest, "is_directory",
			"a directory has no single content URL; use the archive endpoint")
		return
	}
	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermDownload) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	ttl := s.contentTicketTTL()
	tok, err := s.deps.Tickets.Mint(ticket.PurposeDownload, ticket.Claims{
		Subject: actor.ID().String(),
		Expiry:  time.Now().Add(ttl),
		Extra:   map[string]string{"path": rel},
	})
	if err != nil {
		s.deps.Log.Error("mint content ticket", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}

	base := strings.TrimRight(s.deps.Config.Server.UserContentURL, "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        base + "/content/" + escapePathForURL(rel) + "?ticket=" + url.QueryEscape(tok),
		"expires_in": int(ttl.Seconds()),
	})
}

// contentTicketTTL is how long a content URL stays valid.
//
// Five minutes: long enough to start a download from a listing, short enough that a URL copied into a
// chat has stopped working before anyone can be tempted to rely on it.
func (s *Server) contentTicketTTL() time.Duration { return 5 * time.Minute }

// escapePathForURL percent-encodes each segment but keeps the separators.
func escapePathForURL(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ContentHandler is the listener for the content origin (§7.1, §12).
//
// Only content routes exist here. The content origin holds a signed-in user's files, so a smaller
// surface is the point: no API, no listing, no mutation, no session middleware reading a cookie the
// origin does not need. What it does serve is gated by tickets or share tokens, never by a session.
func (s *Server) ContentHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := middleware.GetReqID(r.Context())
			if id != "" {
				w.Header().Set("X-Request-Id", id)
			}
			next.ServeHTTP(w, r.WithContext(store.WithRequestID(r.Context(), id)))
		})
	})
	r.Use(s.clientAddress)
	r.Use(s.requestLogger)
	r.Use(s.securityHeaders)
	r.Use(middleware.Recoverer)

	r.Get("/content/*", s.handleContent)
	// Share downloads and previews are content too: a recipient's browser fetches them from the
	// origin that serves user bytes, not from the application.
	r.Get("/s/{token}/download", s.handleShareDownload)
	r.Get("/s/{token}/thumbnail", s.handleShareThumbnail)

	// Everything else is not an address on this origin. A 404 rather than the app's 405 table: the
	// content origin should look like a file host, not like the API.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such content")
	})
	return r
}
