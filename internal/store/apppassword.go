package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AppPassword is a long-lived credential for non-interactive clients.
//
// TokenHash is the SHA-256 of the secret, never the secret itself: a database
// dump must not yield working credentials. A plain hash (not Argon2) is correct
// here because the secret is 256 bits of crypto/rand — there is no dictionary to
// attack, and a slow KDF on every WebDAV request would make sync clients unusable.
type AppPassword struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Label     string
	Prefix    string
	TokenHash []byte
	Scope     string
	CreatedAt time.Time
	ExpiresAt *time.Time
	LastUsed  *time.Time
	LastIP    string
	RevokedAt *time.Time
}

// AppPasswordTTLData is the audit payload for credential creation.
type AppPasswordTTLData struct{}

// App password operations, on the transaction so that issuing a credential and
// recording the audit event commit together.
type AppPasswordTx interface {
	CreateAppPassword(ctx context.Context, p AppPassword) error
	AppPasswordByHash(ctx context.Context, hash []byte) (AppPassword, error)
	TouchAppPassword(ctx context.Context, id uuid.UUID, ip string) error
	RevokeAppPassword(ctx context.Context, id uuid.UUID) error
	// RevokeUserAppPasswords kills every live credential of an account. It exists
	// because disabling an account must not leave a 90-day WebDAV credential
	// working: revoking the sessions alone covers the browser and nothing else.
	RevokeUserAppPasswords(ctx context.Context, userID uuid.UUID) (int, error)
	ListAppPasswords(ctx context.Context, userID uuid.UUID) ([]AppPassword, error)
}
