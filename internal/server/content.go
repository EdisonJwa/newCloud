package server

import (
	"context"
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

	// WC-GAP-006 / INV-04: the ticket is a bearer capability, and a bearer capability that outlives
	// the account's authority is a revocation hole. Until this check existed, disabling an account
	// stopped NEW requests at requireSession while a ticket minted a second earlier kept serving
	// bytes for the rest of its five-minute life — so "disable takes effect on the next request" was
	// true of the API and false of the content origin.
	//
	// The account row is re-read here rather than trusted from the ticket because the ticket carries
	// no status: it is a signature over (subject, path, expiry), and a status baked into it would be
	// the status at MINT time, which is the value that is now stale. This is one indexed read on a
	// path that has already done a metadata resolve and is about to stream a file.
	//
	// The consequence is deliberate and worth stating: account disable now beats ticket expiry, so an
	// outstanding URL stops working immediately. That is the stricter of the two policies the review
	// asked to choose between, and the cheaper one to prove.
	if revoked, rerr := s.contentSubjectRevoked(r, ownerID); rerr != nil {
		// A failure to determine status must not serve content. Refusing is the safe direction: the
		// client can retry with a fresh ticket, and a disabled account cannot ride out a metadata
		// outage. Compare share.password.verify, which fails closed for the same reason.
		s.deps.Log.Error("content ticket: could not check account status", "error", rerr)
		writeError(w, http.StatusNotFound, "not_found", "no such content")
		return
	} else if revoked {
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

// contentSubjectRevoked reports whether the account behind a content ticket may no longer have its
// bytes served (WC-GAP-006).
//
// "Revoked" is the account being anything other than active: disabled by an administrator, deleted,
// or left pending by the JIT policy. It is the same predicate requireSession applies to a browser
// request, deliberately — the content origin and the API must not disagree about whether an account
// is live, or one of them becomes the way in.
func (s *Server) contentSubjectRevoked(r *http.Request, userID uuid.UUID) (bool, error) {
	var user store.User
	err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UserByID(ctx, userID)
		user = u
		return uerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A deleted account: there is no owner left to serve bytes for.
			return true, nil
		}
		return false, err
	}
	return !user.IsActive(), nil
}

// contentTicketTTL is how long a content URL stays valid.
//
// Five minutes: long enough to start a download from a listing, short enough that a URL copied into a
// chat has stopped working before anyone can be tempted to rely on it.
//
// WC-GAP-006 note: this is now the OUTER bound, not the revocation window. The account status is
// rechecked on every fetch, so disabling an account stops an outstanding ticket immediately and the
// five minutes applies only to an account that is still active.
func (s *Server) contentTicketTTL() time.Duration { return 5 * time.Minute }

// MintExpiredContentTicketForTest mints a content ticket that is already expired.
//
// Exported for the expiry test and for nothing else: the property under test is that the handler
// rejects an EXPIRED ticket, and the only honest way to produce one is with the real key and a real
// past expiry. Handing the test a forged token would prove that a bad MAC is rejected instead, which
// is a different and much weaker claim.
func (s *Server) MintExpiredContentTicketForTest(userID uuid.UUID, rel string) (string, error) {
	return s.deps.Tickets.Mint(ticket.PurposeDownload, ticket.Claims{
		Subject: userID.String(),
		Expiry:  time.Now().Add(-time.Minute),
		Extra:   map[string]string{"path": rel},
	})
}

// escapePathForURL percent-encodes each segment but keeps the separators.
func escapePathForURL(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// refuseContentRoute answers a content-delivery path on the APPLICATION origin when a content origin
// exists (NC-RR-006).
//
// It answers exactly what every other anonymous miss on that origin answers. A 404 is chosen over a
// redirect or a 400 for two reasons: the app origin must not tell a caller that a content origin
// exists or where, and a share token that does not resolve must be indistinguishable from one whose
// route happens to live elsewhere. Anything more specific is a discovery oracle.
//
// The headers are deliberately the plain application ones. Setting an attachment disposition here
// would suggest bytes could follow; not setting it means a client that somehow constructs this URL
// sees a normal not-found page.
func (s *Server) refuseContentRoute(w http.ResponseWriter, r *http.Request) {
	s.deps.Log.Debug("content route refused on the application origin",
		"path", r.URL.Path, "host", r.Host,
		"hint", "user content is served by server.usercontent_addr; check the reverse proxy routes")
	writeError(w, http.StatusNotFound, "not_found", "no such content")
}

// ContentHandler is the listener for the content origin (§7.1, §12; NC-RR-006).
//
// Only content routes exist here. The content origin holds a signed-in user's files, so a smaller
// surface is the point: no API, no listing, no mutation, no session middleware reading a cookie the
// origin does not need. What it does serve is gated by tickets or share tokens, never by a session.
//
// The route set is the COMPLETE inventory of everything that returns user bytes:
//
//	/content/{path}?ticket=...   an account's file, by ticket
//	/s/{token}/download          a share's file
//	/s/{token}/thumbnail         a share's preview
//
// Anything else — including the share landing page, the API and the SPA — is not an address on this
// origin, and the catch-all says so regardless of the Host header. That is deliberate: the boundary
// is enforced by the route table rather than by trusting the proxy to have routed the right vhost,
// so a proxy that forwards the application hostname here (or this hostname to the application) gets a
// 404 from whichever side it reaches instead of a working cross-origin path.
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
