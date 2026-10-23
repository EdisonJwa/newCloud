package server

// Share content delivery across the origin split (PRG-002).
//
// The problem this file solves, stated precisely, because the first attempt got it wrong:
//
//	The share landing page must be served by the APPLICATION origin. It renders the HTML shell and
//	it runs the access gate, and the grants that gate produces — a password cookie, a verified
//	recipient cookie — are deliberately HOST-ONLY, so they are never sent to the host that serves
//	user bytes. That is not an oversight to be fixed by widening the cookie: widening it would hand
//	the content origin the ability to authenticate a visitor as that share, which is exactly the
//	isolation the split exists to provide.
//
//	The bytes, meanwhile, must only be served by the CONTENT origin.
//
// So a relative `/s/{token}/download` link from the landing page cannot work: the browser stays on
// the application origin, which refuses content routes by design. An absolute link to the content
// origin does not work either, because that request arrives without the gate cookies.
//
// The resolution is a short-lived, narrowly bound capability minted by the application origin at the
// moment the gate passes. It is not a session and not a grant: it names one share, one file, one
// purpose, and expires. The content origin accepts it in place of the cookies, re-validates the
// share's own lifecycle on every request, and never stores it.
//
// What the capability deliberately does NOT do:
//
//   - It cannot skip the gate on the application origin. It is only minted after the gate passed,
//     and it is only accepted on the content origin's two content routes.
//   - It cannot be replayed as a cookie (different purpose, different verification path), so it
//     cannot be used to reach the landing page's gated JSON or the upload route.
//   - It cannot be widened to another file or another share: the share id is the token subject and
//     the requested child name is bound into the claims.
//
// Repeated use IS allowed within its lifetime, and that is a requirement rather than a concession:
// a media player seeking through a shared video issues a Range request per seek, and a resumed
// download is another request for bytes the visitor already paid for. Revocation does not depend on
// single use — every request re-resolves the share, so revoking, expiring or exhausting it stops the
// next request even inside the window.

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// shareContentTicketTTL bounds how long a capability stays usable.
//
// Thirty minutes. The trade is: long enough that a large download and its Range retries survive a
// slow link, short enough that a URL copied out of a chat stops working while the share itself is
// still the thing an owner reasons about. It is NOT the revocation window — the share is re-resolved
// on every request, so revocation, expiry, exhaustion and owner deactivation all take effect
// immediately.
const shareContentTicketTTL = 30 * time.Minute

// shareContentGrant is the verdict a capability carries: enough to authorise and account for one
// request without any cookie.
type shareContentGrant struct {
	// Name is the child requested inside a shared directory ("" for the share root itself).
	Name string
	// Grant binds the request to the visitor's download session, so a capability-authenticated
	// Range request joins the session the first request paid for instead of being charged again.
	Grant []byte
	// Actor is the audit actor kind ("anonymous", "recipient", ...), decided at the gate.
	Actor string
	// Email is the verified recipient address, when the gate verified one.
	Email string
}

// shareContentCapability verifies a capability carried in the `ticket` query parameter.
//
// It returns ok=false for anything that is missing, expired, minted for another share, minted for
// another purpose, or minted for a different child of this share. A caller must treat ok=false as
// "no capability" and fall back to the cookie gate rather than as an error, so that a single content
// route can serve both topologies.
func (s *Server) shareContentCapability(r *http.Request, sh uuid.UUID, kind string) (shareContentGrant, bool) {
	raw := r.URL.Query().Get("ticket")
	if raw == "" {
		return shareContentGrant{}, false
	}
	claims, err := s.deps.Tickets.Verify(ticket.PurposeShareContent, raw)
	if err != nil {
		return shareContentGrant{}, false
	}
	// The subject is the share id, so a capability minted for share A is inert on share B's URL
	// even though both are reachable with only a token.
	if claims.Subject != sh.String() {
		return shareContentGrant{}, false
	}
	// The purpose (download vs thumbnail) is bound in, so a preview capability cannot fetch the
	// original bytes and a download capability cannot be used to enumerate previews.
	if claims.Extra["kind"] != kind {
		return shareContentGrant{}, false
	}
	// The file is bound in. Without this a capability for one child of a shared folder would fetch
	// every other child, which is a wider grant than the visitor proved.
	want := r.URL.Query().Get("name")
	if claims.Extra["name"] != want {
		return shareContentGrant{}, false
	}
	grant, err := hex.DecodeString(claims.Extra["grant"])
	if err != nil || len(grant) == 0 {
		return shareContentGrant{}, false
	}
	return shareContentGrant{
		Name:  claims.Extra["name"],
		Grant: grant,
		Actor: claims.Extra["actor"],
		Email: claims.Extra["email"],
	}, true
}

// mintShareContentCapability issues the capability after the gate has passed on the application
// origin.
func (s *Server) mintShareContentCapability(sh uuid.UUID, kind, name, actor, email string,
	grant []byte) (string, error) {

	return s.deps.Tickets.Mint(ticket.PurposeShareContent, ticket.Claims{
		Subject: sh.String(),
		Expiry:  time.Now().Add(shareContentTicketTTL),
		Extra: map[string]string{
			"kind":  kind,
			"name":  name,
			"actor": actor,
			"email": email,
			"grant": hex.EncodeToString(grant),
		},
	})
}

// shareContentRedirectURL builds the absolute content-origin URL a gated download is handed to.
//
// Absolute, not relative: a relative link would keep the browser on the application origin, which
// refuses content routes — the exact defect this replaces. The base comes from
// server.usercontent_url, which the configuration validator already requires to be a different
// hostname from public_url.
func (s *Server) shareContentRedirectURL(token, kind, name, capability string) string {
	base := strings.TrimRight(s.deps.Config.Server.UserContentURL, "/")
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("/s/")
	b.WriteString(url.PathEscape(token))
	b.WriteString("/")
	b.WriteString(kind)
	b.WriteString("?ticket=")
	b.WriteString(url.QueryEscape(capability))
	if name != "" {
		b.WriteString("&name=")
		b.WriteString(url.QueryEscape(name))
	}
	return b.String()
}

// handleShareDownloadRedirect is the split-origin entry point for a share download.
//
// It does the part only the application origin can do — resolve the share and satisfy the gate
// against host-only cookies — and then hands the visitor to the content origin with a capability.
// It deliberately does NOT stream the bytes: that is the content origin's job, and streaming here
// would put user content back on the application origin.
func (s *Server) handleShareDownloadRedirect(w http.ResponseWriter, r *http.Request) {
	s.shareContentRedirect(w, r, "download")
}

// handleShareThumbnailRedirect is the same hand-off for a share preview.
func (s *Server) handleShareThumbnailRedirect(w http.ResponseWriter, r *http.Request) {
	s.shareContentRedirect(w, r, "thumbnail")
}

func (s *Server) shareContentRedirect(w http.ResponseWriter, r *http.Request, kind string) {
	token := chi.URLParam(r, "token")

	// The same resolution and the same checks the direct path performs, in the same order, so the
	// two topologies cannot disagree about whether a link is live. ResolveForDownload is used for
	// downloads because the remaining-allowance check belongs to the session logic that can tell a
	// continuation from a fresh download.
	var (
		sh  store.Share
		err error
	)
	if kind == "download" {
		sh, err = s.deps.Shares.ResolveForDownload(r.Context(), token)
	} else {
		sh, err = s.deps.Shares.Resolve(r.Context(), token)
	}
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !sh.CanRead() || !s.shareOwnerActive(r.Context(), sh) {
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}
	verdict := s.gate(r, sh)
	if !verdict.allowed() {
		// Not a redirect to the gate: the gate IS this origin. A visitor who has not proved the
		// password or the address is told the link is unavailable, exactly as the direct path does,
		// rather than being bounced somewhere that will reject them too.
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	// Recording recipient access happens here rather than on the content origin because this is
	// where the verified address is known without trusting a capability.
	if verdict.Email != "" {
		if merr := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
			return tx.MarkRecipientAccess(ctx, sh.ID, verdict.Email)
		}); merr != nil {
			s.deps.Log.Warn("could not record recipient access", "share", sh.ID.String(), "error", merr)
		}
	}

	name := r.URL.Query().Get("name")
	grant := s.downloadGrantHash(r, sh)
	actor := s.shareActorKind(r, sh)

	capability, err := s.mintShareContentCapability(sh.ID, kind, name, actor, verdict.Email, grant)
	if err != nil {
		s.deps.Log.Error("mint share content capability", "share", sh.ID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}

	// 302 rather than 307: the redirect is a GET and must stay one. The ticket must not be cached
	// by anything between the browser and us, and it must not outlive the response in a history
	// entry that a later request replays against a revoked share.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, s.shareContentRedirectURL(token, kind, name, capability),
		http.StatusFound)
}
