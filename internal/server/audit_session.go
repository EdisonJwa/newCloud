package server

// Emitting session revocations as their own audit event (§10.1 会话撤销).
//
// auth.session.revoke was declared in the vocabulary and emitted by nothing. The information was
// not missing — a status change and a role downgrade both record `sessions_revoked` in the metadata
// of their own event — but an operator asking "was this session revoked, and why" had to know which
// of several unrelated causes to filter for, and would get an empty result from the one event name
// the system advertises. An audit vocabulary that names an event nobody emits is worse than one that
// omits it: the absence reads as "it never happened".
//
// One helper because the causes must agree. Two call sites writing this event by hand is how they
// drift into different field names for the same fact.

import (
	"context"
	"net/http"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// auditSessionRevocation records that a user's sessions were cut off.
//
// It writes nothing when count is zero. A sweep every login would otherwise produce an
// "revoked 0 sessions" row on every request that changed nothing, and an audit log full of no-ops
// is one that gets ignored.
func auditSessionRevocation(ctx context.Context, tx store.Tx, r *http.Request,
	userID uuid.UUID, username string, count int, cause, detail string) error {

	if count <= 0 {
		return nil
	}
	return tx.Audit(ctx, store.AuditEntry{
		ActorKind: "system", ActorUserID: &userID, ActorEmail: username,
		ActorIP: clientIP(r), UserAgent: r.UserAgent(),
		Action: string(audit.ActionSessionRevoke), TargetKind: "user", TargetID: userID.String(),
		Result: string(audit.ResultOK),
		// The cause is what makes the event useful: "12 sessions revoked" without "because the
		// account was disabled" is a fact with no story, and the two most common causes look
		// identical from the count alone.
		Metadata: map[string]any{"sessions": count, "cause": cause, "detail": detail},
	})
}
