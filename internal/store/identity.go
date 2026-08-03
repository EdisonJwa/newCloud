package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Session is a browser session record. The token itself is a stateless ticket;
// this row exists so a session can be revoked individually.
type Session struct {
	ID         string
	UserID     uuid.UUID
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
	AuthMethod string
}

// User is an account. The IdP subject is the identity; email and username are
// display data that the IdP may change.
type User struct {
	ID          uuid.UUID
	Username    string
	Email       string
	DisplayName string
	Role        string
	Status      string
	QuotaBytes  int64
	UsedBytes   int64
	// IdP identity. Subject is stable forever, so it is the account key.
	IdPIssuer    string
	IdPSubject   string
	IsBreakglass bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IsActive reports whether the account may sign in.
func (u User) IsActive() bool { return u.Status == "active" }

// RoleAdmin reports whether the account has the global admin role.
func (u User) RoleAdmin() bool { return u.Role == "admin" }

// Session and identity operations.
//
// These live on Tx so that creating a session can be audited in the same
// transaction as the login event: a session that exists without a corresponding
// audit record would be an unaccounted-for credential.
type SessionTx interface {
	CreateSession(ctx context.Context, s Session) error
	SessionLive(ctx context.Context, jti string) (bool, error)
	RevokeSession(ctx context.Context, jti string) error
	RevokeUserSessions(ctx context.Context, userID string) error
}

// IdentityTx is the user lookup and provisioning surface.
type IdentityTx interface {
	UserByID(ctx context.Context, id uuid.UUID) (User, error)
	UserByIdPSubject(ctx context.Context, issuer, subject string) (User, error)
	// UpsertIdPUser creates or updates the account for an IdP identity.
	//
	// The (issuer, subject) pair is the key: Pocket ID's sub never changes, so a
	// user who changes their email keeps their files. Keying on email would
	// silently create a second account and orphan the first one's data.
	UpsertIdPUser(ctx context.Context, u UpsertUser) (User, error)
	// ApplyIdPRole records the role the IdP now claims for an account, revoking
	// its live sessions when that is a downgrade.
	//
	// This exists because "the role changed in the IdP" and "the role changed in
	// NewCloud" are different events, and only the second one affects access. A
	// person removed from the admin group must lose admin rights for the session
	// they already hold — otherwise the removal is advisory until their token
	// expires, which for a WebDAV client can be months.
	ApplyIdPRole(ctx context.Context, userID uuid.UUID, role string) (RoleChange, error)
}

// RoleChange reports what ApplyIdPRole did.
type RoleChange struct {
	// From and To are the stored and the newly claimed role.
	From, To string
	// Changed reports whether the stored role moved.
	Changed bool
	// Downgraded reports a move to strictly less privilege. That is the case the
	// revocation exists for: everything else takes effect on the next request
	// because the middleware reads the role live.
	Downgraded bool
	// SessionsRevoked and AppPasswordsRevoked count what was killed.
	SessionsRevoked     int
	AppPasswordsRevoked int
}

// roleRank orders roles so "downgrade" is decidable without a second table.
//
// It mirrors the ordering in internal/config: guest < user < admin. An unknown
// role ranks 0, so a claim that stops matching the map is treated as a downgrade
// rather than silently preserved.
func roleRank(role string) int {
	switch role {
	case "admin":
		return 3
	case "user":
		return 2
	case "guest":
		return 1
	default:
		return 0
	}
}

// UpsertUser is the input for JIT provisioning.
type UpsertUser struct {
	Issuer      string
	Subject     string
	Username    string
	Email       string
	DisplayName string
	Role        string
	// Status is the initial status for a *new* account. Empty means active.
	//
	// It matters because the provisioning policy (ID-04) decides whether a first-time
	// identity is admitted or held for approval, and it can only express that by creating
	// the account in a non-active state. Without this field the policy computed a status
	// that nothing stored, so "pending" mode silently behaved like "open".
	//
	// It is ignored for an existing account: status is changed through SetUserStatus, so
	// that approval is a deliberate act with its own audit row.
	Status string
}

// FullTx combines every operation available inside a transaction.
//
// It is split into embeddable interfaces so that a component can depend on just
// the part it needs (the auth package only needs sessions and identity) while
// there is still exactly one transaction type.
type FullTx interface {
	Tx
	SessionTx
	IdentityTx
	AppPasswordTx
	ShareTx
	LockTx
	DeadPropTx
}

var (
	// ErrSessionRevoked is returned when a session was explicitly killed.
	ErrSessionRevoked = errors.New("store: session revoked")
	// ErrQuota is declared in store.go; keep a single source of truth.
)
