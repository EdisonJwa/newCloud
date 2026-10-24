package store

import (
	"context"

	"github.com/google/uuid"
)

// Groups and membership (§4.1).
//
// Without membership there is nothing for a group grant to resolve against, so the
// group half of the ACL model would be unexpressible even with the table in place.

// Group is a team whose members can be granted access collectively.
type Group struct {
	ID          uuid.UUID
	Name        string
	DisplayName string
	// QuotaBytes is optional; nil means the group has no separate quota.
	QuotaBytes *int64
}

// GroupMember links an account to a group.
type GroupMember struct {
	GroupID uuid.UUID
	UserID  uuid.UUID
	// Role within the group: viewer, editor or manager. It is stored for display
	// and future use; authorization uses the ACL grants, not this field.
	Role string
}

// GroupTx is the group surface.
type GroupTx interface {
	// CreateGroup creates or returns the group with this name.
	CreateGroup(ctx context.Context, g Group) (Group, error)
	// GroupByName resolves a group by its IdP-facing name.
	GroupByName(ctx context.Context, name string) (Group, error)
	// GroupByID resolves a group by id, for rendering a grant's subject.
	GroupByID(ctx context.Context, id uuid.UUID) (Group, error)
	// AddGroupMember adds an account to a group, idempotently.
	AddGroupMember(ctx context.Context, groupID, userID uuid.UUID, role string) error
	// RemoveGroupMember removes an account from a group.
	RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error
	// GroupsForUser lists the groups an account belongs to.
	GroupsForUser(ctx context.Context, userID uuid.UUID) ([]Group, error)
	// Groups lists every group, ordered by name.
	//
	// Added for the admin screen: an operator has to be able to see which groups exist before
	// deciding who belongs to one, and until this existed the only way to discover a group was to
	// already know its name.
	Groups(ctx context.Context) ([]Group, error)
	// GroupMembers lists an account's membership rows.
	GroupMembers(ctx context.Context, groupID uuid.UUID) ([]GroupMember, error)
}
