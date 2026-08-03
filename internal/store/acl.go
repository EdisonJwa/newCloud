package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Resource ACL (§4.1 layer 2).
//
// Before this existed, acl_entries was dead SQL: the table had been created by the
// initial migration and no Go code ever read or wrote it, so the documented
// three-layer permission model was really two — a platform role plus "owner_id
// matches". That left no way to express "this group may read the Finance folder",
// which is the one thing a team file server is actually for.
//
// The rules this implements, and the reasoning behind each:
//
//   - A grant names either a user or a group, never both (enforced by the schema's
//     CHECK as well as here).
//   - Grants inherit down the tree, and `inherit = false` stops that. A non-inheriting
//     grant applies to the node it names and nothing below it, which is how a
//     protected file inside a shared folder keeps its boundary.
//   - Inherited grants accumulate (union). There is no "deny" bit: two grants that
//     both allow produce the union, and removing access means removing the grant.
//     A deny bit sounds more expressive but makes the answer depend on evaluation
//     order, which is how ACL systems become impossible to reason about.
//   - The walk stops at the owner's namespace root. Walking past it would let a
//     grant on one user's root reach another user's tree.

// ACLSubjectKind distinguishes the two things a grant can name.
type ACLSubjectKind string

const (
	ACLSubjectUser  ACLSubjectKind = "user"
	ACLSubjectGroup ACLSubjectKind = "group"
)

// ACLEntry is one grant: a subject, a node, a permission set, and whether it
// inherits downward.
type ACLEntry struct {
	ID      uuid.UUID
	NodeID  uuid.UUID
	UserID  *uuid.UUID
	GroupID *uuid.UUID
	// Perms is the raw bitmap stored as a string list ("read,write"). Keeping the
	// wire format human-readable means an operator inspecting the table can tell
	// what a grant does without decoding a bitmap.
	Perms     Perm
	Inherit   bool
	CreatedAt time.Time
	CreatedBy *uuid.UUID
}

// SubjectKind reports which column is set.
func (e ACLEntry) SubjectKind() ACLSubjectKind {
	if e.GroupID != nil {
		return ACLSubjectGroup
	}
	return ACLSubjectUser
}

// ACLTx is the ACL surface.
//
// It lives on Tx so that changing a grant and auditing the change commit together.
// An ACL edit that was not recorded would be the one change a user could not explain
// afterwards — "who gave them access to my folder" has to be answerable.
type ACLTx interface {
	// Grant adds or replaces a grant for one subject on one node.
	Grant(ctx context.Context, e ACLEntry) (ACLEntry, error)
	// Revoke removes a grant by id.
	Revoke(ctx context.Context, id uuid.UUID) error
	// ACLForNode lists the grants attached directly to a node.
	ACLForNode(ctx context.Context, nodeID uuid.UUID) ([]ACLEntry, error)
	// EffectivePermsFor resolves the permissions an actor holds on a node by
	// walking its ancestor chain and honouring inheritance.
	//
	// It returns the union of every live grant that reaches the node for this
	// actor, either directly or through a group they belong to. A nil result with
	// no error means "no grant": the caller must treat that as denied.
	EffectivePermsFor(ctx context.Context, actorID, nodeID uuid.UUID) (Perm, bool, error)

	// UserByUsername resolves a grant subject named in a request.
	//
	// The management API takes a username rather than a UUID because that is what an
	// owner knows: they were told "share this with alice", not given her account id.
	UserByUsername(ctx context.Context, username string) (User, error)
	// UserByEmail resolves an account by address, for the same reason.
	UserByEmail(ctx context.Context, email string) (User, error)
}

// ACLSubject is a grant subject resolved for display: the name an operator would
// recognise rather than the raw column.
type ACLSubject struct {
	Kind      ACLSubjectKind
	UserID    *uuid.UUID
	GroupID   *uuid.UUID
	Name      string // username or group name
	Display   string // display name or email, when known
	Perms     Perm
	Inherit   bool
	EntryID   uuid.UUID
	CreatedAt time.Time
}

// Perm is a permission bitmap, duplicated from internal/authz on purpose.
//
// The store must not import authz: authz's Accessor interface is implemented by the
// store, so an import in that direction would be a cycle. Rather than force one
// package to own a type the other needs, the two declare the same bits and the
// adapter at the edge converts. The bit values are load-bearing and must stay in
// lockstep — the conformance suite asserts it, because a silent divergence would mean
// a grant stored with one meaning and checked with another.
type Perm uint16

// The permission bits. The order is the storage order and must not change without a
// migration: a stored list decodes by name, but a reordering here would change what
// an existing grant means.
const (
	PermRead Perm = 1 << iota
	PermWrite
	PermDelete
	PermShare
	PermListChildren
	PermReadVersions
	PermRestoreVersion
	PermUploadOnly

	PermDownload
	PermCreate
	PermOverwrite
	PermRename
	PermMove
	PermManagePermissions
	PermViewHistory
)

// AllPerms enumerates every bit.
const AllPerms = PermRead | PermWrite | PermDelete | PermShare |
	PermListChildren | PermReadVersions | PermRestoreVersion | PermUploadOnly |
	PermDownload | PermCreate | PermOverwrite | PermRename | PermMove |
	PermManagePermissions | PermViewHistory

// ExpandComposite turns the coarse bits into the fine ones they imply, matching
// authz.expandComposite. Keeping the rule in both places is unavoidable without a
// shared package; the conformance test cross-checks it.
func ExpandComposite(p Perm) Perm {
	if p&PermWrite != 0 {
		p |= PermCreate | PermOverwrite
	}
	if p&PermRead != 0 {
		p |= PermDownload
	}
	if p&PermReadVersions != 0 {
		p |= PermViewHistory
	}
	return p
}

// aclPermNames maps each bit to its stored name.
//
// Order matters only for readability of the stored list; decoding is by name, so
// reordering this table is safe while renumbering the bits is not.
var aclPermNames = []struct {
	bit  Perm
	name string
}{
	{PermListChildren, "list"},
	{PermRead, "read"},
	{PermDownload, "download"},
	// "write" is kept alongside create/overwrite rather than replaced by them: the
	// existing API and every legacy grant say "read,write", and decoding must keep
	// giving those grants the meaning they had.
	{PermWrite, "write"},
	{PermCreate, "create"},
	{PermOverwrite, "overwrite"},
	{PermRename, "rename"},
	{PermMove, "move"},
	{PermDelete, "delete"},
	{PermShare, "share"},
	{PermManagePermissions, "manage_permissions"},
	{PermViewHistory, "view_history"},
	{PermReadVersions, "read_versions"},
	{PermRestoreVersion, "restore_version"},
	{PermUploadOnly, "upload_only"},
}

// EncodePerms renders a permission set as the stored name list.
//
// The names are the API vocabulary from §4.1, so an operator reading the table or an
// audit row sees the same words the requirement uses rather than a bitmap.
func EncodePerms(p Perm) []string {
	out := make([]string, 0, len(aclPermNames))
	for _, e := range aclPermNames {
		if p&e.bit != 0 {
			out = append(out, e.name)
		}
	}
	return out
}

// DecodePerms parses a stored name list into a bitmap.
//
// An unrecognised name is ignored rather than fatal: a newer version may have written
// a permission this build does not know, and refusing to read the whole grant would
// lock a user out of a folder because of a word. The unknown name grants nothing,
// which is the fail-closed reading.
func DecodePerms(names []string) Perm {
	var p Perm
	for _, n := range names {
		for _, e := range aclPermNames {
			if e.name == n {
				p |= e.bit
				break
			}
		}
	}
	return p
}
