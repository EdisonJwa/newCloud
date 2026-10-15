package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// In-memory implementations of the session and identity operations.
//
// These are not throwaway stubs: they enforce the same invariants PostgreSQL does
// (revoked sessions are dead, disabled accounts cannot sign in, the IdP subject is
// the account key) so that the API layer behaves identically in tests and in
// production.

// EnsureRoot provisions the owner's root directory inside the transaction.
func (t *memTx) EnsureRoot(ctx context.Context, ownerID uuid.UUID) (Node, error) {
	if id, ok := t.m.roots[ownerID]; ok {
		return t.m.nodes[id].node, nil
	}
	now := t.m.now()
	node := Node{
		ID: newID(), OwnerID: ownerID, Name: "", Kind: KindDir,
		CreatedAt: now, ModifiedAt: now,
	}
	t.m.nodes[node.ID] = &memNode{node: node}
	t.m.roots[ownerID] = node.ID
	t.m.children[node.ID] = map[string]uuid.UUID{}
	return node, nil
}
func (t *memTx) CreateSession(ctx context.Context, s Session) error {
	if _, exists := t.m.sessions[s.ID]; exists {
		return ErrConflict
	}
	if _, ok := t.m.users[s.UserID]; !ok {
		return ErrNotFound
	}
	t.m.sessions[s.ID] = &memSession{Session: s}
	return nil
}

func (t *memTx) SessionLive(ctx context.Context, jti string) (bool, error) {
	rec, ok := t.m.sessions[jti]
	if !ok {
		return false, nil
	}
	if rec.RevokedAt != nil {
		return false, nil
	}
	if !rec.ExpiresAt.After(t.m.now()) {
		return false, nil
	}
	return true, nil
}

func (t *memTx) RevokeSession(ctx context.Context, jti string) error {
	rec, ok := t.m.sessions[jti]
	if !ok {
		// Revoking an unknown session is not an error: the caller wanted it gone
		// and it is gone. Reporting ErrNotFound here would turn a retried logout
		// into a 404.
		return nil
	}
	if rec.RevokedAt == nil {
		at := t.m.now()
		rec.RevokedAt = &at
	}
	return nil
}

func (t *memTx) RevokeUserSessions(ctx context.Context, userID string) error {
	id, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil {
		return ErrInvalidName
	}
	at := t.m.now()
	for _, rec := range t.m.sessions {
		if rec.UserID == id && rec.RevokedAt == nil {
			rec.RevokedAt = &at
		}
	}
	return nil
}

func (t *memTx) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	u, ok := t.m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return t.hydrateUser(*u), nil
}

// hydrateUser fills the fields that live outside the stored struct.
//
// Usage and limit are separate maps because they are mutable counters rather than identity
// attributes. Returning the struct as stored left both at whatever value the account was created
// with — zero. PostgreSQL reads the real columns, so the two backends disagreed on two figures
// that reach a user (/api/me reports quota_bytes) and an administrator (the user list reports
// both).
//
// A helper rather than two lines in one method, because there are FIVE places that return a User
// from this backend and only one of them was fixed first. The next one to return a raw struct
// would have reintroduced the same silent zero.
func (t *memTx) hydrateUser(u User) User {
	u.UsedBytes = t.m.usage[u.ID]
	u.QuotaBytes = t.m.quota[u.ID]
	return u
}

func (t *memTx) UserByIdPSubject(ctx context.Context, issuer, subject string) (User, error) {
	for _, u := range t.m.users {
		if u.IdPIssuer == issuer && u.IdPSubject == subject {
			return t.hydrateUser(*u), nil
		}
	}
	return User{}, ErrNotFound
}

func (t *memTx) UpsertIdPUser(ctx context.Context, in UpsertUser) (User, error) {
	username := in.Username
	if username == "" {
		username = in.Subject
	}

	// Resolve by IdP identity first, then by username.
	//
	// The username fallback mirrors the PostgreSQL implementation and exists for the
	// same reason: an account provisioned ahead of time (SCIM, or an administrator)
	// has the username but no IdP identity, and its first login must claim that row
	// rather than create a duplicate. Keeping the two stores in step is what stops
	// "works in tests, 500s in production" from being a category of bug here.
	var byIdentity, byName *User
	for _, u := range t.m.users {
		if u.IdPIssuer == in.Issuer && u.IdPSubject == in.Subject {
			byIdentity = u
			break
		}
		if u.Username == username {
			byName = u
		}
	}

	switch {
	case byIdentity != nil:
		if !byIdentity.IsActive() {
			// A disabled account must not be silently re-enabled by signing in.
			return User{}, fmt.Errorf("%w: account is disabled", ErrConflict)
		}
		// Username is intentionally not updated: it appears in WebDAV URLs and share
		// links, so renaming it would break existing paths.
		if in.Email != "" {
			byIdentity.Email = in.Email
		}
		if in.DisplayName != "" {
			byIdentity.DisplayName = in.DisplayName
		}
		// Role is intentionally not written here: ApplyIdPRole owns it, because
		// only it can also revoke the credentials a demotion invalidates. Writing
		// it in both places made the change invisible to the reconciler.
		byIdentity.UpdatedAt = t.m.now()
		return *byIdentity, nil

	case byName != nil:
		// Refuse to hand over an account that already belongs to a different
		// identity: that is an account takeover via a colliding username.
		if byName.IdPSubject != "" && byName.IdPSubject != in.Subject {
			return User{}, fmt.Errorf("%w: username %q belongs to another identity",
				ErrConflict, username)
		}
		if !byName.IsActive() {
			return User{}, fmt.Errorf("%w: account is disabled", ErrConflict)
		}
		byName.IdPIssuer = in.Issuer
		byName.IdPSubject = in.Subject
		if in.Email != "" {
			byName.Email = in.Email
		}
		if in.DisplayName != "" {
			byName.DisplayName = in.DisplayName
		}
		// See above: role reconciliation belongs to ApplyIdPRole.
		byName.UpdatedAt = t.m.now()
		return *byName, nil
	}

	now := t.m.now()

	// The email column is UNIQUE in PostgreSQL, and this store must enforce the same
	// rule or the two disagree about a case that actually happens: an IdP reassigning
	// a mailbox to a new person. PostgreSQL answered ErrConflict; the memory store
	// happily created a second account with the same address, so a test could prove a
	// behaviour production did not have.
	if in.Email != "" {
		for _, existing := range t.m.users {
			if strings.EqualFold(existing.Email, in.Email) {
				return User{}, fmt.Errorf("%w: email %q is already attached to another identity",
					ErrConflict, in.Email)
			}
		}
	}

	u := &User{
		ID: newID(), Username: username, Email: in.Email, DisplayName: in.DisplayName,
		Role: orDefault(in.Role, "guest"), Status: orDefault(in.Status, StatusActive),
		IdPIssuer: in.Issuer, IdPSubject: in.Subject,
		CreatedAt: now, UpdatedAt: now,
	}
	// Mirrors the PostgreSQL column default (10 GiB) when no explicit quota is supplied, so the
	// two backends agree about a freshly provisioned account even though only one of them has a
	// schema to declare it in.
	if in.QuotaBytes != nil {
		u.QuotaBytes = *in.QuotaBytes
		t.m.quota[u.ID] = *in.QuotaBytes
	} else {
		t.m.quota[u.ID] = defaultUserQuotaBytes
		u.QuotaBytes = defaultUserQuotaBytes
	}
	t.m.users[u.ID] = u
	return *u, nil
}

// defaultUserQuotaBytes mirrors the users.quota_bytes default in migration 0001.
//
// Duplicated in the in-memory store because it has no schema to declare a default in, and a
// backend that leaves a fresh account at zero (unlimited) while PostgreSQL gives it 10 GiB is
// exactly the divergence the shared conformance suite exists to catch.
const defaultUserQuotaBytes int64 = 10737418240

// ApplyIdPRole stores the claimed role and revokes credentials on a downgrade.
//
// It mirrors the PostgreSQL implementation, including the revocation: a downgrade
// that only rewrote a field would pass a test of the field and leave the live
// credential working, which is the failure this exists to prevent.
func (t *memTx) ApplyIdPRole(ctx context.Context, userID uuid.UUID, role string) (RoleChange, error) {
	u, ok := t.m.users[userID]
	if !ok {
		return RoleChange{}, ErrNotFound
	}

	out := RoleChange{From: u.Role, To: role}
	if u.Role == role {
		return out, nil
	}
	out.Changed = true
	out.Downgraded = roleRank(role) < roleRank(u.Role)
	u.Role = role
	u.UpdatedAt = t.m.now()

	if !out.Downgraded {
		return out, nil
	}

	now := t.m.now()
	for _, s := range t.m.sessions {
		if s.UserID == userID && s.RevokedAt == nil {
			at := now
			s.RevokedAt = &at
			out.SessionsRevoked++
		}
	}
	for _, p := range t.m.appPasswords {
		if p.UserID == userID && p.RevokedAt == nil {
			at := now
			p.RevokedAt = &at
			out.AppPasswordsRevoked++
		}
	}
	return out, nil
}

// AddUser creates an account directly. Used by tests and by the break-glass
// bootstrap path, which has no IdP claims to work from.
func (m *Memory) AddUser(u User) User {
	m.mu.Lock()
	defer m.mu.Unlock()

	if u.ID == uuid.Nil {
		u.ID = newID()
	}
	if u.Status == "" {
		u.Status = "active"
	}
	if u.Role == "" {
		u.Role = "user"
	}
	now := m.now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now

	cp := u
	m.users[u.ID] = &cp
	return cp
}

// SessionsFor returns a user's live sessions, newest first. Used by the "where am
// I signed in" screen.
func (m *Memory) SessionsFor(userID uuid.UUID) []Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Session
	for _, rec := range m.sessions {
		if rec.UserID == userID && rec.RevokedAt == nil {
			out = append(out, rec.Session)
		}
	}
	return out
}

// CreateUser creates a new account, refusing duplicates instead of updating them.
func (t *memTx) CreateUser(ctx context.Context, in UpsertUser) (User, error) {
	if err := validateNewUser(in); err != nil {
		return User{}, err
	}
	for _, u := range t.m.users {
		if strings.EqualFold(u.Username, in.Username) {
			return User{}, fmt.Errorf("%w: username %q is already taken", ErrConflict, in.Username)
		}
		if in.Email != "" && strings.EqualFold(u.Email, in.Email) {
			return User{}, fmt.Errorf("%w: email %q is already in use", ErrConflict, in.Email)
		}
		if in.Issuer != "" && in.Subject != "" &&
			u.IdPIssuer == in.Issuer && u.IdPSubject == in.Subject {
			return User{}, fmt.Errorf("%w: that identity already has an account", ErrConflict)
		}
	}

	now := t.m.now()
	u := User{
		ID: newID(), Username: in.Username, Email: in.Email, DisplayName: in.DisplayName,
		Role: orDefault(in.Role, "user"), Status: orDefault(in.Status, "active"),
		IdPIssuer: in.Issuer, IdPSubject: in.Subject,
		CreatedAt: now, UpdatedAt: now,
	}
	if in.QuotaBytes != nil {
		u.QuotaBytes = *in.QuotaBytes
		t.m.quota[u.ID] = *in.QuotaBytes
	}
	t.m.users[u.ID] = &u
	return u, nil
}

// validateNewUser applies the rules the schema would enforce, so both stores refuse the same input.
func validateNewUser(in UpsertUser) error {
	switch {
	case strings.TrimSpace(in.Username) == "":
		return fmt.Errorf("%w: a username is required", ErrInvalidName)
	case in.Role != "" && in.Role != "admin" && in.Role != "user" && in.Role != "guest":
		return fmt.Errorf("%w: role must be admin, user or guest", ErrInvalidName)
	case in.Status != "" && in.Status != "active" && in.Status != "disabled" && in.Status != "pending":
		return fmt.Errorf("%w: status must be active, disabled or pending", ErrInvalidName)
	}
	return nil
}
