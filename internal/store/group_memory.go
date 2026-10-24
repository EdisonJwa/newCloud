package store

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// In-memory implementation of GroupTx.
//
// The group list is a slice rather than a map keyed by id so ordering matches the SQL
// (ORDER BY name), which keeps a test of "the groups are listed alphabetically" from
// passing on one backend and failing on the other.

func (t *memTx) CreateGroup(ctx context.Context, g Group) (Group, error) {
	if g.Name == "" {
		return Group{}, ErrInvalidName
	}
	for i := range t.m.groups {
		if strings.EqualFold(t.m.groups[i].Name, g.Name) {
			if g.DisplayName != "" {
				t.m.groups[i].DisplayName = g.DisplayName
			}
			return t.m.groups[i], nil
		}
	}
	if g.ID == uuid.Nil {
		g.ID = newID()
	}
	t.m.groups = append(t.m.groups, g)
	return g, nil
}

func (t *memTx) GroupByName(ctx context.Context, name string) (Group, error) {
	for _, g := range t.m.groups {
		if strings.EqualFold(g.Name, name) {
			return g, nil
		}
	}
	return Group{}, ErrNotFound
}

func (t *memTx) AddGroupMember(ctx context.Context, groupID, userID uuid.UUID, role string) error {
	if role == "" {
		role = "viewer"
	}
	if _, ok := t.m.users[userID]; !ok {
		return ErrNotFound
	}
	for i := range t.m.groupMembers {
		if t.m.groupMembers[i].GroupID == groupID && t.m.groupMembers[i].UserID == userID {
			t.m.groupMembers[i].Role = role
			return nil
		}
	}
	t.m.groupMembers = append(t.m.groupMembers, GroupMember{GroupID: groupID, UserID: userID, Role: role})
	return nil
}

func (t *memTx) RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error {
	for i, m := range t.m.groupMembers {
		if m.GroupID == groupID && m.UserID == userID {
			t.m.groupMembers = append(t.m.groupMembers[:i], t.m.groupMembers[i+1:]...)
			return nil
		}
	}
	return nil
}

func (t *memTx) GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error) {
	var out []Group
	for _, m := range t.m.groupMembers {
		if m.UserID != userID {
			continue
		}
		for _, g := range t.m.groups {
			if g.ID == m.GroupID {
				out = append(out, g)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Groups lists every group, ordered by name.
//
// Sorted here rather than left in insertion order so it matches the SQL's ORDER BY name: an
// ordering that differs between backends makes "the admin screen lists groups alphabetically" pass
// on one and fail on the other.
func (t *memTx) Groups(ctx context.Context) ([]Group, error) {
	out := make([]Group, len(t.m.groups))
	copy(out, t.m.groups)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (t *memTx) GroupMembers(ctx context.Context, groupID uuid.UUID) ([]GroupMember, error) {
	var out []GroupMember
	for _, m := range t.m.groupMembers {
		if m.GroupID == groupID {
			out = append(out, m)
		}
	}
	return out, nil
}

var _ GroupTx = (*memTx)(nil)

// GroupByID resolves a group by id.
func (t *memTx) GroupByID(ctx context.Context, id uuid.UUID) (Group, error) {
	for _, g := range t.m.groups {
		if g.ID == id {
			return g, nil
		}
	}
	return Group{}, ErrNotFound
}
