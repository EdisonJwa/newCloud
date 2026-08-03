package server

// Just-in-time provisioning policy (ID-04).
//
// The users table has had a `status` column since the first migration with a "pending"
// value described in the schema comment, and nothing ever set it. Every unknown identity
// the IdP asserts was created active immediately, in whatever role its groups mapped to.
// For a deployment trusted to hold one organisation's documents that is reasonable; for
// one where the IdP tenant is wider than the set of people who should have an account, it
// means "whoever can authenticate, can store files".
//
// Three modes, decided at login:
//
//	open    any asserted identity is provisioned (the historical behaviour, the default)
//	pending the account exists but is disabled until an administrator approves it
//	deny    the identity is refused outright; accounts must be pre-created
//
// The domain allow-list applies in every mode. It is not a replacement for the IdP as the
// gate — it is a second lock for the case where the IdP's tenant is broader than the set
// of people who should reach this deployment.

import (
	"net/http"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/config"
)

// jitDecision is the outcome of applying the policy to one login.
type jitDecision struct {
	// Existing is true when the account already exists, in which case the policy does
	// not apply: a provisioned user does not need re-approval on every login.
	Existing bool
	// Provision reports whether a new account may be created.
	Provision bool
	// Status is the status to store for a newly created account.
	Status string
	// Refusal explains a refusal, for the audit row and the user-facing error.
	Refusal string
}

// Allowed reports whether the login may proceed.
func (d jitDecision) Allowed() bool { return d.Existing || d.Provision }

// decideJIT applies the configured policy to an asserted identity.
//
// It is a pure function of the config and the claims so it can be tested directly: the
// interesting cases are all "what happens to this identity", not "what does the HTTP
// handler do with it".
func (s *Server) decideJIT(existing bool, email string) jitDecision {
	cfg := s.deps.Config.Auth.JIT

	// Domain gate first: it applies even to a first-time account in any mode, and it is
	// cheaper to reason about than the mode interaction.
	if !emailDomainAllowed(email, cfg.AllowedDomains) {
		return jitDecision{
			Existing: existing,
			Refusal:  "email domain is not permitted for this deployment",
		}
	}

	// An existing account is never re-evaluated. Approval is a one-time event, and making
	// it per-login would mean an outage the moment the config changed.
	if existing {
		return jitDecision{Existing: true}
	}

	switch cfg.Mode {
	case config.JITDeny:
		return jitDecision{Refusal: "this deployment does not provision accounts automatically"}

	case config.JITPending:
		// Create it, but disabled, so an administrator can find it in the list and
		// approve it. Refusing to create it at all would leave the admin with no record to
		// approve and the user with no explanation.
		return jitDecision{Provision: true, Status: "pending"}

	default: // JITOpen
		return jitDecision{Provision: true, Status: "active"}
	}
}

// emailDomainAllowed checks an address against the allow-list.
//
// An empty list allows everything; a missing or malformed address is refused when a list
// is configured, because an address whose domain cannot be determined cannot be shown to
// be permitted.
func emailDomainAllowed(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, want := range allowed {
		if strings.ToLower(strings.TrimSpace(want)) == domain {
			return true
		}
	}
	return false
}

// refuseJIT answers a login the policy declined.
//
// 403 rather than 401: the credential was fine, the deployment's policy said no. The
// distinction matters to an operator reading the audit trail, and to a user who should not
// be told to "check your password".
func (s *Server) refuseJIT(w http.ResponseWriter, reason string) {
	writeError(w, http.StatusForbidden, "provisioning_denied", reason)
}

// pendingMessage is what a not-yet-approved user is told.
//
// It says what to do rather than only what happened: "contact your administrator" is
// actionable, "access denied" is not.
const pendingMessage = "your account is awaiting approval by an administrator"
