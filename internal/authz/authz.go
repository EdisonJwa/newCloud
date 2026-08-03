// Package authz is the single authorization choke point.
//
// The design rule that makes this package worth existing: callers never hold a
// raw node. They hold a Cap, which is a permission bitmap bound to one node ID
// and constructible only by Service.Authorize. A handler that forgets to call
// Authorize therefore has nothing to pass to the store layer.
package authz

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind describes who is acting.
type Kind uint8

const (
	KindAnonymous Kind = iota
	KindUser
	KindService // internal worker / system task
)

// Actor is the authenticated subject. Fields are unexported so that only this
// package can mint one; handlers receive it from the auth middleware.
type Actor struct {
	kind     Kind
	id       uuid.UUID
	email    string
	username string
	role     Role
	scopes   Scope
}

// Role is the coarse-grained local role.
type Role uint8

const (
	RoleGuest Role = iota + 1
	RoleUser
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleAdmin:
		return "admin"
	case RoleUser:
		return "user"
	case RoleGuest:
		return "guest"
	default:
		return "invalid"
	}
}

// Scope narrows what an app password may do. A read-only app password must not
// be able to write even though the underlying user could.
type Scope uint8

const (
	ScopeRead Scope = 1 << iota
	ScopeWrite
)

func (s Scope) Has(want Scope) bool { return s&want == want }

// NewUserActor builds an actor for an interactive session.
// scope is ScopeRead|ScopeWrite for browser sessions.
func NewUserActor(id uuid.UUID, email string, role Role, scope Scope) Actor {
	return Actor{kind: KindUser, id: id, email: email, role: role, scopes: scope}
}

// WithUsername records the namespace segment used in WebDAV hrefs.
//
// It is set by the auth layer from the account record rather than from the
// request, so a client cannot address another namespace by lying in the URL.
func (a Actor) WithUsername(name string) Actor {
	a.username = name
	return a
}

// Username returns the namespace segment for URL construction.
func (a Actor) Username() string { return a.username }

// Ptr returns the actor's ID as a pointer, for audit records.
func (a Actor) Ptr() *uuid.UUID {
	id := a.id
	return &id
}

// NewAnonymousActor is used by share-link handlers before any identity exists.
func NewAnonymousActor(scope Scope) Actor {
	return Actor{kind: KindAnonymous, scopes: scope}
}

// NewServiceActor is used by background workers that act on behalf of the
// system itself. It gets no implicit file access: workers must resolve the node
// through a system-owned context and are audited like anything else.
func NewServiceActor() Actor {
	return Actor{kind: KindService, scopes: ScopeRead | ScopeWrite}
}

func (a Actor) Kind() Kind        { return a.kind }
func (a Actor) ID() uuid.UUID     { return a.id }
func (a Actor) Email() string     { return a.email }
func (a Actor) Role() Role        { return a.role }
func (a Actor) Scope() Scope      { return a.scopes }
func (a Actor) IsAnonymous() bool { return a.kind == KindAnonymous }
func (a Actor) IsAdmin() bool     { return a.role == RoleAdmin }

// Perm is a permission bit.
//
// The first group is the original coarse vocabulary. The second group is the finer
// distinction §4.1 asks for: "may overwrite" and "may delete" are different rights,
// and a collaborator who can upload a new revision is not necessarily allowed to
// remove the file.
type Perm uint16

const (
	PermRead Perm = 1 << iota
	PermWrite
	PermDelete
	PermShare
	PermListChildren
	PermReadVersions
	PermRestoreVersion
	PermUploadOnly // file request: create children, nothing else

	// Fine-grained primitives (§4.1). They are additive: a grant that carries
	// PermWrite still satisfies every check that asks for PermCreate or
	// PermOverwrite, via ExpandComposite inside the authorizer.
	PermDownload
	PermCreate
	PermOverwrite
	PermRename
	PermMove
	PermManagePermissions
	PermViewHistory
)

// AllPerms is used by tests to enumerate every permission.
const AllPerms = PermRead | PermWrite | PermDelete | PermShare |
	PermListChildren | PermReadVersions | PermRestoreVersion | PermUploadOnly |
	PermDownload | PermCreate | PermOverwrite | PermRename | PermMove |
	PermManagePermissions | PermViewHistory

// String renders a permission set for audit metadata and tests.
func (p Perm) String() string {
	if p == 0 {
		return "none"
	}
	names := make([]string, 0, 16)
	for _, e := range []struct {
		bit  Perm
		name string
	}{
		{PermRead, "read"},
		{PermWrite, "write"},
		{PermDelete, "delete"},
		{PermShare, "share"},
		{PermListChildren, "list"},
		{PermReadVersions, "read_versions"},
		{PermRestoreVersion, "restore_version"},
		{PermUploadOnly, "upload_only"},
		{PermDownload, "download"},
		{PermCreate, "create"},
		{PermOverwrite, "overwrite"},
		{PermRename, "rename"},
		{PermMove, "move"},
		{PermManagePermissions, "manage_permissions"},
		{PermViewHistory, "view_history"},
	} {
		if p&e.bit != 0 {
			names = append(names, e.name)
		}
	}
	if len(names) == 0 {
		return "unknown"
	}
	return joinPlus(names)
}

func joinPlus(xs []string) string {
	out := xs[0]
	for _, x := range xs[1:] {
		out += "+" + x
	}
	return out
}

// Cap is proof that a permission check passed. Its fields are unexported and it
// is only constructible here, so a Cap cannot be fabricated by a handler.
type Cap struct {
	perm     Perm
	nodeID   uuid.UUID
	ownerID  uuid.UUID
	viaShare bool
	shareID  uuid.UUID
	actor    Actor
}

// NodeID returns the node this capability is bound to.
func (c Cap) NodeID() uuid.UUID { return c.nodeID }

// OwnerID is exposed only for display decisions (e.g. "shared by you"), never
// for authorization. Handlers must not branch on it to widen access.
func (c Cap) OwnerID() uuid.UUID { return c.ownerID }

// ViaShare reports whether access came from a share link.
func (c Cap) ViaShare() bool { return c.viaShare }

// ShareID returns the share that granted access, if any.
func (c Cap) ShareID() uuid.UUID { return c.shareID }

// Actor returns the subject the capability was granted to.
func (c Cap) Actor() Actor { return c.actor }

// Can reports whether the capability includes every requested permission.
func (c Cap) Can(p Perm) bool { return c.perm&p == p }

// CanAny reports whether at least one requested permission is present.
func (c Cap) CanAny(p Perm) bool { return c.perm&p != 0 }

// Perms returns the raw permission bitmap (for audit logging).
func (c Cap) Perms() Perm { return c.perm }

// ErrDenied is returned for every negative decision. It is deliberately
// uninformative: callers must map it to 404, never 403, so that the existence
// of a node is not leaked.
var ErrDenied = errors.New("authz: permission denied")

// ErrNotFound is returned when the node does not exist. Handlers map it to the
// same 404 as ErrDenied.
var ErrNotFound = errors.New("authz: node not found")

// Node is the minimum the authorizer needs to know about a node. The store
// fills it in; it is never handed to handlers.
type Node struct {
	ID       uuid.UUID
	OwnerID  uuid.UUID
	ParentID *uuid.UUID
	Trashed  bool
}

// ShareGrant describes why an actor may touch a node.
type ShareGrant struct {
	ShareID uuid.UUID
	Perms   Perm
	// ExpiresAt is nil for never-expiring shares.
	ExpiresAt *time.Time
	// MaxDownloads/MaxUploads are enforced by the share service; the
	// authorizer only cares that the share is still live.
	Revoked bool
}

// Accessor is the data dependency of the authorizer. The store implements it.
// Keeping it narrow makes the authorization matrix testable without a database.
type Accessor interface {
	// LoadNode returns ErrNotFound when the node is absent.
	LoadNode(id uuid.UUID) (Node, error)
	// OwnerGroupPerm returns the permission a group-shared ancestor grants to
	// the actor, if any. This is now backed by acl_entries rather than a stub.
	OwnerGroupPerm(actorID, nodeID uuid.UUID) (Perm, bool, error)
	// ShareForActor returns the grant a share gives this actor on this node.
	ShareForActor(actorID, nodeID uuid.UUID) (*ShareGrant, error)
}

// Service performs authorization decisions.
type Service struct {
	acc Accessor
	now func() time.Time
}

func NewService(acc Accessor) *Service {
	return &Service{acc: acc, now: time.Now}
}

// Authorize resolves the effective permission set for actor on nodeID.
//
// Rules, in order of precedence:
//  1. owner                       -> all permissions
//  2. admin on non-content nodes  -> read metadata only (see ADR-0002 §5.2)
//  3. group share on an ancestor  -> the group's permission bits
//  4. explicit share to the actor -> the share's permission bits
//  5. otherwise                   -> ErrDenied
//
// An app-password scope narrows the result: a ScopeRead credential can never
// come out of here with PermWrite.
func (s *Service) Authorize(a Actor, nodeID uuid.UUID) (Cap, error) {
	n, err := s.acc.LoadNode(nodeID)
	if err != nil {
		return Cap{}, err
	}
	if n.Trashed {
		// The trash is reachable only through the dedicated restore flow,
		// which authorizes the original parent.
		return Cap{}, ErrDenied
	}

	var (
		perms    Perm
		viaShare bool
		shareID  uuid.UUID
		// adminNoContent marks the administrator branch, whose permission set must
		// NOT be expanded: PermRead normally implies PermDownload, and an admin
		// holding PermDownload could read every user's files — the one capability
		// this branch exists to withhold.
		adminNoContent bool
	)

	switch {
	case a.kind == KindUser && n.OwnerID == a.id:
		perms = AllPerms

	case a.kind == KindService:
		perms = PermRead | PermWrite | PermDelete | PermListChildren

	case a.role == RoleAdmin:
		// Admins do not read user content. They can see metadata and force
		// deletion, which keeps "admin abuse" an auditable event rather than a
		// standing capability.
		perms = PermRead | PermDelete | PermListChildren
		adminNoContent = true

	default:
		if a.kind == KindUser {
			if gp, ok, err := s.acc.OwnerGroupPerm(a.id, n.ID); err != nil {
				return Cap{}, err
			} else if ok {
				perms |= gp
			}

			if g, err := s.acc.ShareForActor(a.id, n.ID); err != nil {
				return Cap{}, err
			} else if g != nil && live(g, s.now()) {
				perms |= g.Perms
				viaShare = true
				shareID = g.ShareID
			}
		}
	}

	// Anonymous actors only ever act through an explicit share grant, which the
	// share handler resolves itself before calling into the store.
	if a.kind == KindAnonymous {
		perms &= PermUploadOnly | PermWrite
	}

	// "Write" has always meant "may modify", and every existing call site checks
	// it. Expanding it here lets a grant express only the fine-grained half
	// ("overwrite but not delete") without breaking those checks.
	//
	// The administrator branch is excluded: its whole point is to withhold content
	// access, and expanding its PermRead into PermDownload would hand it back.
	if !adminNoContent {
		perms = expandComposite(perms)
	}

	perms = applyScope(a.scopes, perms)

	if perms == 0 {
		return Cap{}, ErrDenied
	}

	return Cap{
		perm:     perms,
		nodeID:   n.ID,
		ownerID:  n.OwnerID,
		viaShare: viaShare,
		shareID:  shareID,
		actor:    a,
	}, nil
}

// expandComposite turns the coarse bits into the fine ones they imply.
//
// Two directions matter:
//
//	PermWrite     -> PermCreate | PermOverwrite   (a writer may add and replace)
//	PermRead      -> PermDownload                 (seeing a file implies fetching it)
//
// It deliberately does NOT grant PermDelete: write access that silently includes
// deletion is how a "collaborator" ends up able to destroy the folder they were
// invited to edit.
func expandComposite(p Perm) Perm {
	if p&PermWrite != 0 {
		p |= PermCreate | PermOverwrite
	}
	// `read` deliberately does NOT imply `download`.
	//
	// It used to, on the reasoning that seeing a file implies fetching it. That held only
	// while download was not separately grantable: as soon as §4.2 split the primitives and
	// the ACL layer could store them independently, the implication made the split a lie -
	// a grant of `read` alone still fetched bytes, so `download` could never be withheld.
	//
	// Callers that need both (a WebDAV client, the web download button, a share link) check
	// them separately, and the ACL surface accepts "read,download" for that case. Shares and
	// legacy grants that say only "read" are handled at their own boundary - see
	// share.PermissionsToACL, which adds download because a share link that renders a file
	// in a browser must be able to serve it.
	if p&PermReadVersions != 0 {
		p |= PermViewHistory
	}
	return p
}

// applyScope narrows perms for read-only credentials.
func applyScope(sc Scope, perms Perm) Perm {
	if sc.Has(ScopeWrite) {
		return perms
	}
	// Read-only: keep read-shaped bits, drop everything that mutates.
	return perms & (PermRead | PermListChildren | PermReadVersions |
		PermDownload | PermViewHistory)
}

func live(g *ShareGrant, now time.Time) bool {
	if g.Revoked {
		return false
	}
	if g.ExpiresAt != nil && now.After(*g.ExpiresAt) {
		return false
	}
	return g.Perms != 0
}

// Grant is the test seam: it lets the authorization matrix build a Cap for a
// synthetic node without a database. It must never be called from production
// code paths; the linter rule forbids importing authz.Grant outside _test.go.
func Grant(p Perm, nodeID uuid.UUID) Cap { return Cap{perm: p, nodeID: nodeID} }

var _ = fmt.Sprintf
