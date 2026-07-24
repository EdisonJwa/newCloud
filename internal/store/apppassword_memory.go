package store

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
)

// In-memory implementations of AppPasswordTx.

func (t *memTx) CreateAppPassword(ctx context.Context, p AppPassword) error {
	if _, ok := t.m.users[p.UserID]; !ok {
		return ErrNotFound
	}
	cp := p
	cp.TokenHash = append([]byte(nil), p.TokenHash...)
	t.m.appPasswords[p.ID] = &cp
	return nil
}

func (t *memTx) AppPasswordByHash(ctx context.Context, hash []byte) (AppPassword, error) {
	for _, p := range t.m.appPasswords {
		if len(p.TokenHash) == len(hash) {
			same := true
			for i := range hash {
				if p.TokenHash[i] != hash[i] {
					same = false
					break
				}
			}
			if same {
				return *p, nil
			}
		}
	}
	return AppPassword{}, ErrNotFound
}

func (t *memTx) TouchAppPassword(ctx context.Context, id uuid.UUID, ip string) error {
	p, ok := t.m.appPasswords[id]
	if !ok {
		return ErrNotFound
	}
	now := t.m.now()
	p.LastUsed = &now
	p.LastIP = ip
	return nil
}

func (t *memTx) RevokeAppPassword(ctx context.Context, id uuid.UUID) error {
	p, ok := t.m.appPasswords[id]
	if !ok {
		return nil
	}
	if p.RevokedAt == nil {
		now := t.m.now()
		p.RevokedAt = &now
	}
	return nil
}

func (t *memTx) ListAppPasswords(ctx context.Context, userID uuid.UUID) ([]AppPassword, error) {
	var out []AppPassword
	for _, p := range t.m.appPasswords {
		if p.UserID == userID {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

var errUnused = errors.New("unused")
