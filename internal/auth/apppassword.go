package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// AppPasswordPrefix marks a string as an app password in logs and in the UI, so
// a leaked one is recognisable by a secret scanner.
const AppPasswordPrefix = "wc_"

// AppPassword is a long-lived credential for non-interactive clients.
//
// Why not Argon2 here: the secret is 256 bits of crypto/rand, not a human-chosen
// password. There is no dictionary to attack, so a slow KDF buys nothing while
// costing ~60 ms on *every* WebDAV request. A desktop client issues one Basic
// auth per request, and a folder expand can be hundreds of requests, which would
// turn into seconds of pure hashing.
//
// The distinction that matters: human-typed secrets (break-glass password, share
// password) get Argon2id; machine-generated random tokens get a plain SHA-256
// index, which is the right tool for a 2^128 keyspace.
type AppPassword struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Label     string
	Prefix    string
	Scope     string // "ro" or "rw"
	CreatedAt time.Time
	ExpiresAt *time.Time
	LastUsed  *time.Time
	LastIP    string
	RevokedAt *time.Time
}

// AppPasswords issues and verifies app passwords.
type AppPasswords struct {
	meta store.Store
	now  func() time.Time
}

// NewAppPasswords wires app-password handling.
func NewAppPasswords(meta store.Store) *AppPasswords {
	return &AppPasswords{meta: meta, now: time.Now}
}

// Issue creates a new app password and returns the plaintext exactly once.
//
// The plaintext is not recoverable afterwards: only its SHA-256 is stored. That
// is deliberate — a database dump must not yield working credentials.
func (a *AppPasswords) Issue(ctx context.Context, userID uuid.UUID, label, scope string, ttl time.Duration) (string, AppPassword, error) {
	return a.issue(ctx, userID, label, scope, ttl, nil)
}

// IssueAudited creates the credential and records its audit event in ONE transaction.
//
// The event used to be written by the caller, best-effort, after Issue had already committed. If
// that second write failed the credential existed and nothing recorded who created it — a
// security-relevant event missing for an operation the client was told had succeeded, which is
// exactly what the audit failure policy forbids. Now a credential that cannot be recorded is not
// created.
func (a *AppPasswords) IssueAudited(ctx context.Context, userID uuid.UUID, label, scope string,
	ttl time.Duration, entry store.AuditEntry) (string, AppPassword, error) {
	return a.issue(ctx, userID, label, scope, ttl, &entry)
}

func (a *AppPasswords) issue(ctx context.Context, userID uuid.UUID, label, scope string,
	ttl time.Duration, entry *store.AuditEntry) (string, AppPassword, error) {
	if !ValidScope(scope) {
		return "", AppPassword{}, fmt.Errorf("auth: scope must be %q or %q, got %q",
			ScopeReadOnly, ScopeReadWrite, scope)
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "app password"
	}
	if len(label) > 100 {
		label = label[:100]
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", AppPassword{}, err
	}
	secret := AppPasswordPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	hash := sha256.Sum256([]byte(secret))

	rec := AppPassword{
		ID:        uuid.New(),
		UserID:    userID,
		Label:     label,
		Prefix:    secret[:min(len(secret), 12)],
		Scope:     scope,
		CreatedAt: a.now(),
	}
	if ttl > 0 {
		exp := a.now().Add(ttl)
		rec.ExpiresAt = &exp
	}

	if err := a.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.CreateAppPassword(ctx, store.AppPassword{
			ID: rec.ID, UserID: userID, Label: label, Prefix: rec.Prefix,
			TokenHash: hash[:], Scope: scope, ExpiresAt: rec.ExpiresAt,
		}); err != nil {
			return err
		}
		if entry != nil {
			e := *entry
			e.TargetKind, e.TargetID, e.Result = "user", userID.String(), "ok"
			return tx.Audit(ctx, e)
		}
		return nil
	}); err != nil {
		return "", AppPassword{}, err
	}
	return secret, rec, nil
}

// ErrInvalidAppPassword is returned for unknown, expired or revoked credentials.
// Callers must not distinguish these.
var ErrInvalidAppPassword = errors.New("auth: invalid app password")

// Verify resolves a Basic-auth password to an account.
//
// The lookup is a single indexed read on the hash, which is O(1) regardless of
// how many credentials exist. Updating last_used_at happens asynchronously so it
// does not add a write to every request.
func (a *AppPasswords) Verify(ctx context.Context, secret, ip string) (store.User, store.AppPassword, error) {
	if !strings.HasPrefix(secret, AppPasswordPrefix) {
		return store.User{}, store.AppPassword{}, ErrInvalidAppPassword
	}
	hash := sha256.Sum256([]byte(secret))

	var (
		cred store.AppPassword
		user store.User
	)
	err := a.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		c, err := tx.AppPasswordByHash(ctx, hash[:])
		if err != nil {
			return err
		}
		cred = c
		u, err := tx.UserByID(ctx, c.UserID)
		if err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		// Every failure looks identical from the outside: unknown credential,
		// revoked, expired. Telling them apart would let an attacker confirm a
		// stolen prefix.
		return store.User{}, store.AppPassword{}, ErrInvalidAppPassword
	}

	if cred.RevokedAt != nil {
		return store.User{}, store.AppPassword{}, ErrInvalidAppPassword
	}
	if cred.ExpiresAt != nil && !cred.ExpiresAt.After(a.now()) {
		return store.User{}, store.AppPassword{}, ErrInvalidAppPassword
	}
	if !user.IsActive() {
		return store.User{}, store.AppPassword{}, ErrInvalidAppPassword
	}

	// Constant-time comparison of the hash is belt-and-braces: the query already
	// matched it, but doing it again costs nothing and documents the intent.
	if subtle.ConstantTimeCompare(cred.TokenHash, hash[:]) != 1 {
		return store.User{}, store.AppPassword{}, ErrInvalidAppPassword
	}

	a.touch(ctx, cred.ID, ip, user, cred.Scope)
	return user, cred, nil
}

// touch records a successful use: the last-used column, and an audit event.
//
// Failures are ignored on purpose: a bookkeeping update must never fail a
// legitimate request. The cost of a missed touch is a stale "last used" column,
// which is cosmetic; the cost of failing the request is a broken sync client.
//
// The audit row is written here rather than before the return, and in this detached
// goroutine, for two reasons that pull in the same direction. It must not fail the request —
// an audit write that fails is not a reason to break a sync client. And it must survive the
// response: a request context is cancelled as soon as the response is written, so an audit
// row tied to it would be lost precisely for the fast requests that make up most traffic.
//
// Volume: this fires on every request that authenticates with an app password, which for a
// WebDAV client is every request. That is a real cost and it is accepted deliberately —
// §10.1 asks for 使用, and a credential that works without a session is exactly the one whose
// every use must be attributable. An operator who finds it too loud can raise the audit
// retention policy; they cannot reconstruct uses that were never recorded.
func (a *AppPasswords) touch(ctx context.Context, id uuid.UUID, ip string, user store.User, scope string) {
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.meta.WithTx(bg, func(ctx context.Context, tx store.Tx) error {
			if err := tx.TouchAppPassword(ctx, id, ip); err != nil {
				return err
			}
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: &user.ID, ActorEmail: user.Email,
				ActorIP: ip, Action: string(audit.ActionAppPasswordUse),
				TargetKind: "app_password", TargetID: id.String(),
				Result:   string(audit.ResultOK),
				Metadata: map[string]any{"scope": scope},
			})
		})
	}()
}

// Revoke kills a credential. It takes effect on the next request.
func (a *AppPasswords) Revoke(ctx context.Context, id uuid.UUID) error {
	return a.revoke(ctx, id, nil)
}

// RevokeAudited revokes the credential and records the event in ONE transaction.
//
// A revoked credential whose revocation went unrecorded is the same defect as an unrecorded
// creation: the client is told the secret is dead and the audit log has no evidence of who killed
// it. The same fix applies.
func (a *AppPasswords) RevokeAudited(ctx context.Context, id uuid.UUID,
	entry store.AuditEntry) error {
	return a.revoke(ctx, id, &entry)
}

func (a *AppPasswords) revoke(ctx context.Context, id uuid.UUID, entry *store.AuditEntry) error {
	return a.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.RevokeAppPassword(ctx, id); err != nil {
			return err
		}
		if entry != nil {
			e := *entry
			e.TargetKind, e.TargetID, e.Result = "user", id.String(), "ok"
			return tx.Audit(ctx, e)
		}
		return nil
	})
}

// RevokeAll kills every live credential of a user and reports how many.
//
// Disabling an account calls this: a session revocation covers the browser, but a
// WebDAV app password is a ninety-day credential that would otherwise keep working
// for a person who has been locked out.
func (a *AppPasswords) RevokeAll(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := a.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, rerr := tx.RevokeUserAppPasswords(ctx, userID)
		n = got
		return rerr
	})
	return n, err
}

// List returns the caller's credentials for the management screen.
func (a *AppPasswords) List(ctx context.Context, userID uuid.UUID) ([]AppPassword, error) {
	var out []AppPassword
	err := a.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		recs, err := tx.ListAppPasswords(ctx, userID)
		if err != nil {
			return err
		}
		for _, r := range recs {
			out = append(out, AppPassword{
				ID: r.ID, UserID: r.UserID, Label: r.Label, Prefix: r.Prefix,
				Scope: r.Scope, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
				LastUsed: r.LastUsed, LastIP: r.LastIP, RevokedAt: r.RevokedAt,
			})
		}
		return nil
	})
	return out, err
}

// ScopeForActor translates a credential scope into the authz scope bits.
//
// It is the single translation between the stored string and the permission
// model. Both entry points that authenticate a credential — the WebDAV handler and
// the automation path — must use it, or one of them silently grants more than the
// credential says.
func ScopeForActor(scope string) (read, write bool) {
	switch scope {
	case ScopeReadOnly:
		return true, false
	default:
		return true, true
	}
}

// Credential scopes.
//
// The requirement is that a credential carries the least authority that makes it
// useful, so an automation token meant to upload cannot delete. Two scopes are
// implemented; anything else is rejected at issuance rather than silently treated
// as read-write.
const (
	// ScopeReadOnly permits reading and listing, never writing.
	ScopeReadOnly = "ro"
	// ScopeReadWrite permits everything the owner may do.
	ScopeReadWrite = "rw"
)

// ValidScope reports whether a scope string is accepted.
func ValidScope(scope string) bool {
	return scope == ScopeReadOnly || scope == ScopeReadWrite
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
