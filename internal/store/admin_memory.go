package store

import (
	"github.com/google/uuid"
)

// DisableUser blocks sign-in for an account.
//
// Sessions and app passwords are checked against the account status on every use,
// so disabling does not need to enumerate and delete credentials: an existing
// WebDAV client stops working on its next request. Revoking them eagerly would
// also work, but it would lose the reason for revocation.
func (m *Memory) DisableUser(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.Status = "disabled"
		u.UpdatedAt = m.now()
	}
}

// EnableUser clears the disabled state.
func (m *Memory) EnableUser(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.Status = "active"
		u.UpdatedAt = m.now()
	}
}

// SetRole changes an account's role. Used by group sync and by tests.
func (m *Memory) SetRole(id uuid.UUID, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.Role = role
		u.UpdatedAt = m.now()
	}
}

// Users returns every account, for tests and admin screens.
func (m *Memory) Users() []User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, *u)
	}
	return out
}
