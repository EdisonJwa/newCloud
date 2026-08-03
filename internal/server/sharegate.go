package server

// Access gating for shares.
//
// Two independent gates exist and a share may require either, both, or neither:
//
//   password    a shared secret the owner chose
//   recipient   proof that the visitor owns an address the owner named
//
// They are deliberately separate. A password travels through whatever channel the
// owner used to send the link; recipient verification proves the visitor controls a
// particular mailbox. Conflating them into one "is the visitor allowed" boolean is how
// an email share ends up accepting the password as proof of identity.
//
// The check is centralised here because every entry point — page, download, upload —
// must reach the same verdict. Duplicating it is how one endpoint ends up weaker than
// the others, and the weaker one is the one an attacker uses.

import (
	"net/http"
	"time"

	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// gateResult says what the visitor must still do.
type gateResult struct {
	// RequiresPassword means a password must be supplied.
	RequiresPassword bool
	// RequiresEmail means an address must be verified.
	RequiresEmail bool
	// Email is the verified address, when one is present.
	Email string
}

// Gate decides whether this request may proceed.
//
// It returns the pending requirements rather than a bare bool so the landing page can
// ask for exactly the missing proof, and so a caller cannot accidentally treat "not
// allowed yet" as "allowed with no requirements".
func (s *Server) gate(r *http.Request, sh store.Share) gateResult {
	out := gateResult{}

	if sh.NeedsPassword() && !s.shareCookieValid(r, sh) {
		out.RequiresPassword = true
	}

	if share.RequiresRecipient(sh) {
		email := s.recipientCookieEmail(r, sh)
		if email == "" {
			out.RequiresEmail = true
		} else {
			// The cookie is signed, but the invitation may have been withdrawn since it
			// was issued. Re-checking is cheap and keeps revocation effective for
			// visitors who are already inside.
			if _, err := s.deps.Shares.ResolveRecipient(r.Context(), sh, email); err != nil {
				out.RequiresEmail = true
				out.Email = ""
			} else {
				out.Email = email
			}
		}
	}
	return out
}

// allowed reports whether the gate is satisfied.
func (g gateResult) allowed() bool {
	return !g.RequiresPassword && !g.RequiresEmail
}

// recipientCookieName scopes a verified-address grant to one share.
func recipientCookieName(shareID string) string {
	// Same reasoning as shareCookieName: a path-scoped cookie cannot carry the __Host- prefix.
	return "__Secure-wc_rcpt_" + shareID
}

// mintRecipientCookie issues the grant for a verified address.
func (s *Server) mintRecipientCookie(sh store.Share, email string) (string, error) {
	return s.deps.Tickets.Mint(ticket.PurposeShareAccess, ticket.Claims{
		Subject: sh.ID.String(),
		ID:      email,
		Expiry:  time.Now().Add(share.RecipientAccessTTL),
	})
}

// recipientCookieEmail returns the verified address for this share, or "".
func (s *Server) recipientCookieEmail(r *http.Request, sh store.Share) string {
	c, err := r.Cookie(recipientCookieName(sh.ID.String()))
	if err != nil || c.Value == "" {
		return ""
	}
	claims, err := s.deps.Tickets.Verify(ticket.PurposeShareAccess, c.Value)
	if err != nil {
		return ""
	}
	// The subject must be this share and the id must be an address: a grant minted for
	// something else (a password grant has the share id in both fields) must not be
	// accepted as proof of an email.
	if claims.Subject != sh.ID.String() || claims.ID == "" || claims.ID == sh.ID.String() {
		return ""
	}
	return claims.ID
}

// setRecipientCookie stores the grant.
func (s *Server) setRecipientCookie(w http.ResponseWriter, token string, sh store.Share, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     recipientCookieName(sh.ID.String()),
		Value:    value,
		Path:     "/s/" + token,
		MaxAge:   int(share.RecipientAccessTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
