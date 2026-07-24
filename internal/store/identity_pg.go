package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Implementations of SessionTx and IdentityTx for the PostgreSQL store.

// EnsureRoot provisions the owner's root inside the transaction.
func (t *pgTx) EnsureRoot(ctx context.Context, ownerID uuid.UUID) (Node, error) {
	const q = `INSERT INTO nodes (parent_id, owner_id, name, kind, size, mime, etag)
		VALUES (NULL, $1, '', 'dir', 0, '', '')
		ON CONFLICT (owner_id) WHERE parent_id IS NULL AND trashed_at IS NULL DO NOTHING
		RETURNING ` + nodeColumns

	node, err := scanNode(t.tx.QueryRow(ctx, q, ownerID))
	if err == nil {
		return node, nil
	}
	// Lost the race (or ON CONFLICT DO NOTHING returned no row): read the winner.
	const sel = `SELECT ` + nodeColumns + ` FROM nodes
		WHERE owner_id = $1 AND parent_id IS NULL AND trashed_at IS NULL LIMIT 1`
	return scanNode(t.tx.QueryRow(ctx, sel, ownerID))
}
func (t *pgTx) CreateSession(ctx context.Context, s Session) error {
	const q = `INSERT INTO sessions (id, user_id, expires_at, ip, user_agent, auth_method)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := t.tx.Exec(ctx, q, s.ID, s.UserID, s.ExpiresAt,
		nullableString(s.IP), s.UserAgent, orDefault(s.AuthMethod, "oidc"))
	return translate(err)
}

func (t *pgTx) SessionLive(ctx context.Context, jti string) (bool, error) {
	// The expiry check is in SQL rather than in Go so a clock skew between the
	// application servers cannot extend a session past its row's expiry.
	const q = `SELECT 1 FROM sessions
		WHERE id = $1 AND revoked_at IS NULL AND expires_at > now()`
	var one int
	err := t.tx.QueryRow(ctx, q, jti).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, translate(err)
	}
	return true, nil
}

func (t *pgTx) RevokeSession(ctx context.Context, jti string) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	_, err := t.tx.Exec(ctx, q, jti)
	return translate(err)
}

func (t *pgTx) RevokeUserSessions(ctx context.Context, userID string) error {
	id, err := uuid.Parse(userID)
	if err != nil {
		return ErrInvalidName
	}
	const q = `UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`
	_, err = t.tx.Exec(ctx, q, id)
	return translate(err)
}

const userColumns = `id, username, COALESCE(email::text,''), display_name, role, status,
	quota_bytes, used_bytes, COALESCE(idp_issuer,''), COALESCE(idp_subject,''),
	is_breakglass, created_at, updated_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.Role, &u.Status,
		&u.QuotaBytes, &u.UsedBytes, &u.IdPIssuer, &u.IdPSubject,
		&u.IsBreakglass, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return User{}, translate(err)
	}
	return u, nil
}

func (t *pgTx) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return scanUser(t.tx.QueryRow(ctx, q, id))
}

func (t *pgTx) UserByIdPSubject(ctx context.Context, issuer, subject string) (User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE idp_issuer = $1 AND idp_subject = $2`
	return scanUser(t.tx.QueryRow(ctx, q, issuer, subject))
}

// UpsertIdPUser provisions or refreshes an account from IdP claims.
//
// ON CONFLICT targets (idp_issuer, idp_subject) so a second login updates the
// existing row instead of creating a duplicate. The username is left alone after
// creation: it appears in WebDAV URLs and share links, and silently renaming it
// would break those for everyone except the user.
func (t *pgTx) UpsertIdPUser(ctx context.Context, u UpsertUser) (User, error) {
	role := orDefault(u.Role, "guest")
	username := u.Username
	if username == "" {
		username = u.Subject
	}

	// Look first, then write; do not rely on ON CONFLICT here.
	//
	// Two different uniqueness rules apply — (idp_issuer, idp_subject) for a
	// returning user, and username for an account that SCIM or an administrator
	// created ahead of time. A failed statement aborts the whole PostgreSQL
	// transaction, so "try the insert, catch the conflict, try something else" is
	// not available: after the first failure every later query returns 25P02.
	existing, err := t.lookupIdPUser(ctx, u.Issuer, u.Subject, username)
	if err != nil {
		return User{}, err
	}
	if existing != nil {
		if !existing.IsActive() {
			// A disabled account must not be silently re-enabled by signing in; that
			// would make "disable" meaningless.
			return User{}, errors.New("store: account is disabled")
		}
		return t.refreshIdPUser(ctx, *existing, u, role)
	}

	const q = `INSERT INTO users (username, email, display_name, idp_issuer, idp_subject, role)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + userColumns
	user, err := scanUser(t.tx.QueryRow(ctx, q, username, nullableString(u.Email),
		u.DisplayName, u.Issuer, u.Subject, role))
	if err != nil {
		return User{}, err
	}
	if !user.IsActive() {
		return User{}, errors.New("store: account is disabled")
	}
	return user, nil
}

// lookupIdPUser finds a row by IdP identity, or failing that by username.
//
// The username fallback is what makes a pre-provisioned account claimable on first
// login. It returns the row rather than updating it so the caller decides whether
// claiming is safe.
func (t *pgTx) lookupIdPUser(ctx context.Context, issuer, subject, username string) (*User, error) {
	const byIdentity = `SELECT ` + userColumns + ` FROM users
		WHERE idp_issuer = $1 AND idp_subject = $2`
	u, err := scanUser(t.tx.QueryRow(ctx, byIdentity, issuer, subject))
	if err == nil {
		return &u, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	const byName = `SELECT ` + userColumns + ` FROM users WHERE username = $1`
	byNameUser, err := scanUser(t.tx.QueryRow(ctx, byName, username))
	if err == nil {
		// Refuse to hand over an account that already belongs to another identity:
		// that would be an account takeover via a colliding username.
		if byNameUser.IdPSubject != "" && byNameUser.IdPSubject != subject {
			return nil, fmt.Errorf("%w: username %q belongs to another identity", ErrConflict, username)
		}
		return &byNameUser, nil
	}
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return nil, err
}

// refreshIdPUser updates the mutable profile fields and binds the identity when the
// account was pre-provisioned.
func (t *pgTx) refreshIdPUser(ctx context.Context, cur User, u UpsertUser, role string) (User, error) {
	// The username is intentionally not updated: it appears in WebDAV URLs and share
	// links, so renaming it would break existing paths for everyone else.
	const q = `UPDATE users
		SET idp_issuer = $2, idp_subject = $3,
		    email = COALESCE($4, email),
		    display_name = COALESCE(NULLIF($5, ''), display_name),
		    role = $6, updated_at = now()
		WHERE id = $1
		RETURNING ` + userColumns
	return scanUser(t.tx.QueryRow(ctx, q, cur.ID, u.Issuer, u.Subject,
		nullableString(u.Email), u.DisplayName, role))
}

// Users is the non-transactional read path used by admin screens and the
// authorization layer's group lookups.
func (s *PG) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return scanUser(s.pool.QueryRow(ctx, q, id))
}

// SetUserQuota updates a limit. Used by the admin API and by tests.
func (s *PG) SetUserQuota(ctx context.Context, id uuid.UUID, limit int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET quota_bytes = $2, updated_at = now() WHERE id = $1`, id, limit)
	return translate(err)
}

// DisableUser blocks sign-in and revokes every live session.
//
// Revoking sessions in the same call is the point: disabling an account while
// leaving its sessions alive means the person you just disabled keeps working
// until their token expires, which for a WebDAV app password is up to 90 days.
func (s *PG) DisableUser(ctx context.Context, id uuid.UUID) error {
	return s.WithTx(ctx, func(ctx context.Context, tx Tx) error {
		full, ok := tx.(FullTx)
		if !ok {
			return errors.New("store: transaction missing identity operations")
		}
		ptx, ok := tx.(*pgTx)
		if !ok {
			return errors.New("store: disable requires a PostgreSQL transaction")
		}
		if _, err := ptx.Exec(ctx, `UPDATE users SET status = 'disabled', disabled_at = now(),
			updated_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		return full.RevokeUserSessions(ctx, id.String())
	})
}

// EnableUser clears the disabled state.
func (s *PG) EnableUser(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET status = 'active', disabled_at = NULL, updated_at = now() WHERE id = $1`, id)
	return translate(err)
}

var _ FullTx = (*pgTx)(nil)
var _ = time.Now
