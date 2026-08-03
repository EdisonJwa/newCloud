package store

// In-memory implementation of account administration.
//
// It mirrors the SQL, including the part that matters most: a status change that removes
// access revokes credentials in the same transaction. A memory store that only flipped the
// status column would let a test "prove" disabling works while the sessions kept running.

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

// ListUsers returns accounts, oldest first.
func (t *memTx) ListUsers(ctx context.Context, status string, limit int) ([]User, error) {
	st, err := validateStatusArg(status, false)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	out := make([]User, 0, len(t.m.users))
	for _, u := range t.m.users {
		if st != "" && u.Status != st {
			continue
		}
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetUserStatus changes an account's status and revokes credentials when access is removed.
func (t *memTx) SetUserStatus(ctx context.Context, id uuid.UUID, status string) (UserStatusChange, error) {
	st, err := validateStatusArg(status, true)
	if err != nil {
		return UserStatusChange{}, err
	}

	u, ok := t.m.users[id]
	if !ok {
		return UserStatusChange{}, ErrNotFound
	}

	change := UserStatusChange{From: u.Status, To: st}
	if u.Status == st {
		return change, nil
	}

	u.Status = st
	u.UpdatedAt = t.m.now()
	change.Changed = true

	if !revokesAccess(st) {
		return change, nil
	}

	// Revoke every live session and app password, exactly as the SQL does.
	now := t.m.now()
	for token, s := range t.m.sessions {
		if s.UserID != id || s.RevokedAt != nil {
			continue
		}
		revoked := now
		s.RevokedAt = &revoked
		t.m.sessions[token] = s
		change.SessionsRevoked++
	}
	for _, ap := range t.m.appPasswords {
		if ap.UserID != id || ap.RevokedAt != nil {
			continue
		}
		revoked := now
		ap.RevokedAt = &revoked
		change.AppPasswordsRevoked++
	}

	return change, nil
}

var _ UserAdminTx = (*memTx)(nil)
