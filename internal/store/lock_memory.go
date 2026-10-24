package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// In-memory lock implementation.
//
// It enforces the same rules as the PostgreSQL version — expired locks do not
// block, only the holder can refresh or release — so behaviour in tests matches
// production.

func (t *memTx) CreateLock(ctx context.Context, l Lock) error {
	if _, ok := t.m.nodes[l.NodeID]; !ok {
		return ErrNotFound
	}
	if existing, ok := t.m.locks[l.NodeID]; ok && existing.ExpiresAt.After(t.m.now()) {
		return ErrConflict
	}
	cp := l
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = t.m.now()
	}
	t.m.locks[l.NodeID] = &cp
	return nil
}

func (t *memTx) ActiveLock(ctx context.Context, nodeID uuid.UUID) (Lock, error) {
	l, ok := t.m.locks[nodeID]
	if !ok || !l.ExpiresAt.After(t.m.now()) {
		return Lock{}, ErrNotFound
	}
	return *l, nil
}

func (t *memTx) RefreshLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID, expires time.Time) (Lock, error) {
	l, ok := t.m.locks[nodeID]
	if !ok || l.Token != token || l.ExpiresAt.Before(t.m.now()) {
		return Lock{}, ErrNotFound
	}
	if l.UserID == nil || *l.UserID != userID {
		return Lock{}, ErrNotFound
	}
	l.ExpiresAt = expires
	return *l, nil
}

func (t *memTx) DeleteLock(ctx context.Context, nodeID uuid.UUID, token string, userID uuid.UUID) (bool, error) {
	l, ok := t.m.locks[nodeID]
	if !ok || l.Token != token {
		return false, nil
	}
	// A lock with no owner (created by the system) can be released by anyone with
	// write access; an owned lock only by its holder.
	if l.UserID != nil && *l.UserID != userID {
		return false, nil
	}
	delete(t.m.locks, nodeID)
	return true, nil
}

func (t *memTx) ExpireLocks(ctx context.Context) (int, error) {
	now := t.m.now()
	n := 0
	for nodeID, l := range t.m.locks {
		if !l.ExpiresAt.After(now) {
			delete(t.m.locks, nodeID)
			n++
		}
	}
	return n, nil
}

func (t *memTx) LocksForUser(ctx context.Context, userID uuid.UUID) ([]Lock, error) {
	var out []Lock
	for _, l := range t.m.locks {
		if l.UserID != nil && *l.UserID == userID && l.ExpiresAt.After(t.m.now()) {
			out = append(out, *l)
		}
	}
	return out, nil
}

// LocksInSubtree lists every live lock on the node or below it.
//
// The memory backend walks the node map instead of the lock map: locks are few and nodes
// are many, but the subtree is what bounds the answer, and iterating nodes here keeps the
// same meaning as the PostgreSQL recursive query without a second index to maintain.
func (t *memTx) LocksInSubtree(ctx context.Context, rootID uuid.UUID) ([]Lock, error) {
	now := t.m.now()
	var out []Lock
	for id, rec := range t.m.nodes {
		if rec.node.TrashedAt != nil {
			continue
		}
		if !t.isDescendantOrSelf(rootID, id) {
			continue
		}
		if l, ok := t.m.locks[id]; ok && l.ExpiresAt.After(now) {
			out = append(out, *l)
		}
	}
	return out, nil
}

// isDescendantOrSelf reports whether id is rootID or has it as an ancestor.
//
// The depth bound is a safety valve, not a product limit: a cycle in the node map would
// otherwise hang the request that is trying to delete it.
func (t *memTx) isDescendantOrSelf(rootID, id uuid.UUID) bool {
	if id == rootID {
		return true
	}
	cur, ok := t.m.nodes[id]
	for depth := 0; ok && depth < 256; depth++ {
		if cur.node.ParentID == nil {
			return false
		}
		if *cur.node.ParentID == rootID {
			return true
		}
		cur, ok = t.m.nodes[*cur.node.ParentID]
	}
	return false
}

// ActiveLocks returns every live lock, for the cleanup worker in tests.
func (m *Memory) ActiveLocks() []Lock {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Lock
	for _, l := range m.locks {
		if l.ExpiresAt.After(m.now()) {
			out = append(out, *l)
		}
	}
	return out
}
