package store

import (
	"github.com/google/uuid"
	"time"
)

// DisableUser blocks sign-in for an account and revokes its live credentials.
//
// The PostgreSQL implementation does the same, and the two must not diverge: this
// used to rely on "status is checked on every use", which was not true for browser
// sessions — a disabled user's cookie kept working in tests while production
// revoked it, so the suite proved the wrong thing.
func (m *Memory) DisableUser(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return
	}
	u.Status = "disabled"
	u.UpdatedAt = m.now()

	now := m.now()
	for _, s := range m.sessions {
		if s.UserID == id && s.RevokedAt == nil {
			at := now
			s.RevokedAt = &at
		}
	}
	for _, p := range m.appPasswords {
		if p.UserID == id && p.RevokedAt == nil {
			at := now
			p.RevokedAt = &at
		}
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

// broken makes every transaction fail. It exists so tests can prove that a
// dependency outage is *observed* — a readiness probe or a retry path that is only
// ever exercised against a working store is untested code.
//
// It is deliberately not guarded behind a build tag: the alternative is tests that
// cannot reach it, and the cost of carrying two unexported methods is smaller than the
// cost of a health check nobody has seen fail.
type brokenFlag struct{ on bool }

// BreakForTest makes the store unusable until UnbreakForTest is called.
func (m *Memory) BreakForTest() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broken = &brokenFlag{on: true}
}

// UnbreakForTest restores normal operation.
func (m *Memory) UnbreakForTest() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broken = nil
}

// isBroken reports whether the store is in the simulated-failure state.
func (m *Memory) isBroken() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.broken != nil
}

// offsetForTest shifts the store's clock forward for the rest of the test.
//
// Retry backoff is measured in tens of minutes. A test that proves a failed job is
// retried must not wait that long, and a test that sleeps through a backoff is one
// people delete the first time it is slow. Advancing the clock tests the same property
// in microseconds.
func (m *Memory) AdvanceForTest(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clockOffset == nil {
		m.clockOffset = &offsetFlag{}
	}
	m.clockOffset.by += d
}

// offsetFlag carries a test clock offset.
//
// It is a pointer so a transaction snapshot copies the reference rather than the value:
// advancing the clock must be visible to the next statement, the way a real clock would
// be.
type offsetFlag struct{ by time.Duration }

// AuditEntries returns the recorded audit rows, oldest first.
//
// It exposes the store's own record type rather than the sealed chain form, because the
// memory backend does not seal (see audit_read.go). Tests that assert "this action was
// audited" want the row, not a hash.
func (m *Memory) AuditEntries() []AuditEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]AuditEntry(nil), m.audit...)
}
