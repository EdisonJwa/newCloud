package server

// Share verification endpoints.
//
// A share may require a password, a verified recipient address, or both. Each is
// proven by its own endpoint and its own cookie; the landing page asks for whichever
// is still missing. Keeping them separate matters: a single "verify" action taking
// either credential invites an implementation where a password satisfies the recipient
// requirement, which would defeat the reason email shares exist.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// handleShareVerify checks a share password and sets an access cookie.
//
// The cookie is scoped to the share id, not just "some password was entered": a
// visitor who verified share A must not thereby get into share B.
func (s *Server) handleShareVerify(w http.ResponseWriter, r *http.Request) {
	token := chiURLParam(r, "token")

	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	sh, err := s.deps.Shares.Resolve(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}

	if err := s.deps.Shares.VerifyPassword(r.Context(), sh, body.Password); err != nil {
		// Deliberately the same status and shape as a bad token: an attacker probing
		// passwords should not learn that the token is real.
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	cookie, err := s.mintShareCookie(sh)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not create an access grant")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     shareCookieName(sh.ID.String()),
		Value:    cookie,
		Path:     "/s/" + token,
		MaxAge:   int(sharePageTimeout.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"verified": true})
}

// handleShareRecipientRequest sends a verification code to an invited address.
//
// It always answers 202 with no detail. Whether an address was invited, already
// verified, or never heard of must not be observable here: this endpoint accepts
// unauthenticated input of arbitrary addresses, so any distinction turns it into a way
// to test whether a given person was invited to a given share.
func (s *Server) handleShareRecipientRequest(w http.ResponseWriter, r *http.Request) {
	token := chiURLParam(r, "token")

	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	sh, err := s.deps.Shares.Resolve(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !share.RequiresRecipient(sh) {
		// Not an email share: telling the caller is harmless and saves a confusing
		// "check your inbox" for a link that never sends anything.
		writeError(w, http.StatusBadRequest, "not_email_share", "this link does not use email verification")
		return
	}

	// The outcome is deliberately discarded. The code is delivered out of band; the
	// response says only that the request was accepted.
	if code, cerr := s.deps.Shares.IssueRecipientCode(r.Context(), sh, body.Email); cerr == nil {
		s.deliverRecipientCode(r, sh, body.Email, code)
	}

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, http.StatusAccepted, map[string]any{"sent": true})
}

// handleShareRecipientVerify exchanges a code for a verified-address grant.
func (s *Server) handleShareRecipientVerify(w http.ResponseWriter, r *http.Request) {
	token := chiURLParam(r, "token")

	var body struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	sh, err := s.deps.Shares.Resolve(r.Context(), token)
	if err != nil {
		s.renderShareError(w, r, err)
		return
	}
	if !share.RequiresRecipient(sh) {
		writeError(w, http.StatusBadRequest, "not_email_share", "this link does not use email verification")
		return
	}

	if err := s.deps.Shares.VerifyRecipientCode(r.Context(), sh, body.Email, body.Code); err != nil {
		// Same shape as a bad password: a wrong code and an uninvited address must be
		// indistinguishable, or this becomes an oracle for who was invited.
		writeError(w, http.StatusNotFound, "not_found", "this link is not available")
		return
	}

	cookie, err := s.mintRecipientCookie(sh, body.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not create an access grant")
		return
	}
	s.setRecipientCookie(w, token, sh, cookie)

	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "email": body.Email})
}

// deliverRecipientCode sends the code to the address.
//
// No mail transport is configured in this build, so the code is written to the log at
// WARN when the deployment explicitly allows it and discarded otherwise. This is a
// deliberate, visible gap rather than a silent one: an operator who enables email
// shares must also configure delivery, and the startup check says so.
func (s *Server) deliverRecipientCode(r *http.Request, sh store.Share, email, code string) {
	if s.deps.Mailer != nil {
		s.deps.Mailer.SendRecipientCode(email, sh.Label, code, humanTTL(share.RecipientCodeTTL))
		return
	}
	s.deps.Log.Warn("recipient verification code generated but no mail transport is configured",
		"share", sh.ID.String(), "email", email,
		"hint", "email shares require mail.delivery in the configuration")
	_ = r
}

// humanTTL renders a duration the way a person reads it in an email.
func humanTTL(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	if d == time.Hour {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", int(d.Hours()))
}
