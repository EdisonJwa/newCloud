package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// In-memory implementation of ACLTx.
//
// It applies the identical inheritance rule as the SQL: a grant covers its own node
// and every descendant while inherit is true, and only its own node when it is not.
// The shared conformance suite runs both, so a grant that behaves one way in tests
// and another in production is caught rather than discovered.

func (t *memTx) Grant(ctx context.Context, e ACLEntry) (ACLEntry, error) {
	if (e.UserID == nil) == (e.GroupID == nil) {
		return ACLEntry{}, ErrInvalidName
	}
	if e.Perms == 0 {
		return ACLEntry{}, ErrInvalidName
	}
	if _, ok := t.m.nodes[e.NodeID]; !ok {
		return ACLEntry{}, ErrNotFound
	}
	if t.m.acl == nil {
		t.m.acl = map[uuid.UUID][]ACLEntry{}
	}

	// Replace an existing grant for the same (node, subject): see the PostgreSQL
	// comment for why duplicates are not allowed to accumulate.
	for i, existing := range t.m.acl[e.NodeID] {
		sameUser := e.UserID != nil && existing.UserID != nil && *existing.UserID == *e.UserID
		sameGroup := e.GroupID != nil && existing.GroupID != nil && *existing.GroupID == *e.GroupID
		if sameUser || sameGroup {
			e.ID = existing.ID
			e.CreatedAt = existing.CreatedAt
			t.m.acl[e.NodeID][i] = e
			return e, nil
		}
	}

	if e.ID == uuid.Nil {
		e.ID = newID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = t.m.now()
	}
	t.m.acl[e.NodeID] = append(t.m.acl[e.NodeID], e)
	return e, nil
}

func (t *memTx) Revoke(ctx context.Context, id uuid.UUID) error {
	for nodeID, entries := range t.m.acl {
		for i, e := range entries {
			if e.ID == id {
				t.m.acl[nodeID] = append(entries[:i], entries[i+1:]...)
				return nil
			}
		}
	}
	return ErrNotFound
}

func (t *memTx) ACLForNode(ctx context.Context, nodeID uuid.UUID) ([]ACLEntry, error) {
	out := append([]ACLEntry(nil), t.m.acl[nodeID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// EffectivePermsFor walks the ancestor chain and unions the grants that reach the
// actor.
func (t *memTx) EffectivePermsFor(ctx context.Context, actorID, nodeID uuid.UUID) (Perm, bool, error) {
	groups := t.groupsOf(actorID)

	var (
		out Perm
		any bool
	)
	distance := 0
	for cur := nodeID; ; {
		for _, e := range t.m.acl[cur] {
			// A grant at the target always applies; one further up applies only if
			// it inherits.
			if distance > 0 && !e.Inherit {
				continue
			}
			matchesUser := e.UserID != nil && *e.UserID == actorID
			matchesGroup := e.GroupID != nil && groups[*e.GroupID]
			if matchesUser || matchesGroup {
				out |= e.Perms
				any = true
			}
		}

		node, ok := t.m.nodes[cur]
		if !ok || node.node.ParentID == nil {
			// The namespace root: nothing above it belongs to this tree.
			break
		}
		cur = *node.node.ParentID
		distance++
	}
	return out, any, nil
}

// groupsOf returns the actor's group memberships as a set.
func (t *memTx) groupsOf(actorID uuid.UUID) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, m := range t.m.groupMembers {
		if m.UserID == actorID {
			out[m.GroupID] = true
		}
	}
	return out
}

var _ ACLTx = (*memTx)(nil)
var _ = time.Now

// UserByUsername resolves a grant subject named in a manage request.
func (t *memTx) UserByUsername(ctx context.Context, username string) (User, error) {
	for _, u := range t.m.users {
		if strings.EqualFold(u.Username, username) {
			return t.hydrateUser(*u), nil
		}
	}
	return User{}, ErrNotFound
}

// UserByEmail resolves an account by address.
func (t *memTx) UserByEmail(ctx context.Context, email string) (User, error) {
	for _, u := range t.m.users {
		if strings.EqualFold(u.Email, email) {
			return t.hydrateUser(*u), nil
		}
	}
	return User{}, ErrNotFound
}
