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
}

// UpsertUser is the input for JIT provisioning.
type UpsertUser struct {
	Issuer      string
	Subject     string
	Username    string
	Email       string
	DisplayName string
	Role        string
}

// Tx combines every operation available inside a transaction.
//
// It is split into embeddable interfaces so that a component can depend on just
// the part it needs (the auth package only needs sessions and identity) while
// there is still exactly one transaction type.
type FullTx interface {
	Tx
	SessionTx
	IdentityTx
}

var (
	// ErrSessionRevoked is returned when a session was explicitly killed.
	ErrSessionRevoked = errors.New("store: session revoked")
	// ErrQuota is declared in store.go; keep a single source of truth.
)
