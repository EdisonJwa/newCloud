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
	return *u, nil
}

func (t *memTx) UserByIdPSubject(ctx context.Context, issuer, subject string) (User, error) {
	for _, u := range t.m.users {
		if u.IdPIssuer == issuer && u.IdPSubject == subject {
			return *u, nil
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
		byIdentity.Role = orDefault(in.Role, "guest")
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
		byName.Role = orDefault(in.Role, "guest")
		byName.UpdatedAt = t.m.now()
		return *byName, nil
	}

	now := t.m.now()
	u := &User{
		ID: newID(), Username: username, Email: in.Email, DisplayName: in.DisplayName,
		Role: orDefault(in.Role, "guest"), Status: "active",
		IdPIssuer: in.Issuer, IdPSubject: in.Subject,
		CreatedAt: now, UpdatedAt: now,
	}
	t.m.users[u.ID] = u
	return *u, nil
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
