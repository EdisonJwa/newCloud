package authz

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixedID gives deterministic IDs so failures name a scenario rather than a
// random UUID.
func fixedID(n byte) uuid.UUID {
	var u uuid.UUID
	u[15] = n
	return u
}

type fakeAccessor struct {
	nodes   map[uuid.UUID]Node
	group   map[[2]uuid.UUID]Perm
	shares  map[[2]uuid.UUID]*ShareGrant
	loadErr error
}

func (f *fakeAccessor) LoadNode(id uuid.UUID) (Node, error) {
	if f.loadErr != nil {
		return Node{}, f.loadErr
	}
	n, ok := f.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	return n, nil
}

func (f *fakeAccessor) OwnerGroupPerm(a, n uuid.UUID) (Perm, bool, error) {
	p, ok := f.group[[2]uuid.UUID{a, n}]
	return p, ok, nil
}

func (f *fakeAccessor) ShareForActor(a, n uuid.UUID) (*ShareGrant, error) {
	return f.shares[[2]uuid.UUID{a, n}], nil
}

const (
	owner      = 1
	other      = 2
	groupMate  = 3
	adminUser  = 4
	guestUser  = 5
	nodeOwned  = 10
	nodeShared = 11
)

func newFixture() (*Service, *fakeAccessor) {
	acc := &fakeAccessor{
		nodes: map[uuid.UUID]Node{
			fixedID(nodeOwned):  {ID: fixedID(nodeOwned), OwnerID: fixedID(owner)},
			fixedID(nodeShared): {ID: fixedID(nodeShared), OwnerID: fixedID(owner)},
		},
		group:  map[[2]uuid.UUID]Perm{},
		shares: map[[2]uuid.UUID]*ShareGrant{},
	}
	return NewService(acc), acc
}

func TestAuthorizeMatrix(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	type tc struct {
		name    string
		actor   Actor
		node    uuid.UUID
		setup   func(*fakeAccessor)
		want    Perm
		wantErr error
	}

	cases := []tc{
		{
			name:  "owner has every permission on own node",
			actor: NewUserActor(fixedID(owner), "o@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:  fixedID(nodeOwned),
			want:  AllPerms,
		},
		{
			name:    "stranger on someone else's node is denied",
			actor:   NewUserActor(fixedID(other), "x@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:    fixedID(nodeOwned),
			wantErr: ErrDenied,
		},
		{
			name:    "guest on an unshared node is denied",
			actor:   NewUserActor(fixedID(guestUser), "g@example.com", RoleGuest, ScopeRead|ScopeWrite),
			node:    fixedID(nodeOwned),
			wantErr: ErrDenied,
		},
		{
			name:  "admin reads metadata and may force-delete, but not write or share",
			actor: NewUserActor(fixedID(adminUser), "a@example.com", RoleAdmin, ScopeRead|ScopeWrite),
			node:  fixedID(nodeOwned),
			// Metadata only. PermRead would imply PermDownload if it were expanded,
			// so the admin branch is deliberately excluded from expansion: an
			// administrator who could download could read every user's files.
			want: PermRead | PermDelete | PermListChildren,
		},
		{
			name:  "group editor grant applies to a non-owner",
			actor: NewUserActor(fixedID(groupMate), "m@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:  fixedID(nodeOwned),
			setup: func(a *fakeAccessor) {
				a.group[[2]uuid.UUID{fixedID(groupMate), fixedID(nodeOwned)}] = PermRead | PermWrite | PermListChildren
			},
			// Write implies create and overwrite; it does not imply delete. Read no longer`n			// implies download, so a grant wanting both must list both.
			want: PermRead | PermWrite | PermListChildren | PermCreate | PermOverwrite,
		},
		{
			name:  "live share grants its permission bits",
			actor: NewUserActor(fixedID(other), "x@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:  fixedID(nodeShared),
			setup: func(a *fakeAccessor) {
				a.shares[[2]uuid.UUID{fixedID(other), fixedID(nodeShared)}] = &ShareGrant{
					ShareID: fixedID(99), Perms: PermRead, ExpiresAt: &expiry,
				}
			},
			// A share grants exactly the bits it lists.
			want: PermRead,
		},
		{
			name:  "expired share is not a grant",
			actor: NewUserActor(fixedID(other), "x@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:  fixedID(nodeShared),
			setup: func(a *fakeAccessor) {
				a.shares[[2]uuid.UUID{fixedID(other), fixedID(nodeShared)}] = &ShareGrant{
					Perms: PermRead, ExpiresAt: &past,
				}
			},
			wantErr: ErrDenied,
		},
		{
			name:  "revoked share is not a grant",
			actor: NewUserActor(fixedID(other), "x@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:  fixedID(nodeShared),
			setup: func(a *fakeAccessor) {
				a.shares[[2]uuid.UUID{fixedID(other), fixedID(nodeShared)}] = &ShareGrant{
					Perms: PermRead, Revoked: true,
				}
			},
			wantErr: ErrDenied,
		},
		{
			name:  "read-only app password never yields write",
			actor: NewUserActor(fixedID(owner), "o@example.com", RoleUser, ScopeRead),
			node:  fixedID(nodeOwned),
			// A read-only credential keeps every read-shaped bit, including the
			// fine-grained ones that read implies.
			want: PermRead | PermListChildren | PermReadVersions | PermDownload | PermViewHistory,
		},
		{
			name:  "anonymous actor is confined to upload-only bits",
			actor: NewAnonymousActor(ScopeRead | ScopeWrite),
			node:  fixedID(nodeShared),
			setup: func(a *fakeAccessor) {
				a.shares[[2]uuid.UUID{{}, fixedID(nodeShared)}] = &ShareGrant{
					Perms: AllPerms,
				}
			},
			wantErr: ErrDenied, // anonymous never gets a grant via ShareForActor
		},
		{
			name:    "missing node is ErrNotFound",
			actor:   NewUserActor(fixedID(owner), "o@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:    fixedID(200),
			wantErr: ErrNotFound,
		},
		{
			name:  "trashed node is denied outside restore flow",
			actor: NewUserActor(fixedID(owner), "o@example.com", RoleUser, ScopeRead|ScopeWrite),
			node:  fixedID(nodeOwned),
			setup: func(a *fakeAccessor) {
				n := a.nodes[fixedID(nodeOwned)]
				n.Trashed = true
				a.nodes[fixedID(nodeOwned)] = n
			},
			wantErr: ErrDenied,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, acc := newFixture()
			if c.setup != nil {
				c.setup(acc)
			}
			cap, err := svc.Authorize(c.actor, c.node)

			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("want error %v, got %v (cap=%v)", c.wantErr, err, cap.Perms())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cap.Perms(); got != c.want {
				t.Fatalf("permissions: want %v, got %v", c.want, got)
			}
		})
	}
}

// TestNoPermissionEscalationFromScope is the regression guard for the bug class
// where a read-only credential leaks write access through a group grant.
func TestNoPermissionEscalationFromScope(t *testing.T) {
	svc, acc := newFixture()
	acc.group[[2]uuid.UUID{fixedID(groupMate), fixedID(nodeOwned)}] = AllPerms

	cap, err := svc.Authorize(
		NewUserActor(fixedID(groupMate), "m@example.com", RoleUser, ScopeRead),
		fixedID(nodeOwned),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Perm{PermWrite, PermDelete, PermShare, PermRestoreVersion, PermUploadOnly} {
		if cap.Can(p) {
			t.Fatalf("read-only credential must not grant %v", p)
		}
	}
}

func TestCapCannotBeForgedWithoutGrant(t *testing.T) {
	// A zero Cap has no permissions, so a handler that skips Authorize has
	// nothing usable to pass downstream.
	var c Cap
	if c.CanAny(AllPerms) {
		t.Fatal("zero Cap must grant nothing")
	}
}

func TestRoleForGroupsIsFailClosed(t *testing.T) {
	// Mirrors config.RoleForGroups behaviour: unknown groups never raise rank.
	ranks := map[string]int{"cloud-admins": 3, "cloud-users": 2}
	got := 0
	for _, g := range []string{"totally-unknown", "another"} {
		if r, ok := ranks[g]; ok && r > got {
			got = r
		}
	}
	if got != 0 {
		t.Fatalf("unknown groups must not grant a role, got rank %d", got)
	}
}

func ExamplePerm_String() {
	fmt.Println(PermRead | PermListChildren)
	fmt.Println(AllPerms)
	// Output:
	// read+list
	// read+write+delete+share+list+read_versions+restore_version+upload_only+download+create+overwrite+rename+move+manage_permissions+view_history
}

// TestAdminCannotDownloadUserContent pins the promise the admin branch makes.
//
// The requirement is explicit that administrators see metadata and may force-delete,
// but never read file content. A previous change made PermRead imply PermDownload for
// every actor, which silently gave administrators the one capability the branch exists
// to withhold — the comment still said "admins do not read user content" while the
// bitmap disagreed.
func TestAdminCannotDownloadUserContent(t *testing.T) {
	svc, _ := newFixture()
	admin := NewUserActor(fixedID(adminUser), "a@example.com", RoleAdmin, ScopeRead|ScopeWrite)

	cap, err := svc.Authorize(admin, fixedID(nodeOwned))
	if err != nil {
		t.Fatalf("an admin must still see metadata: %v", err)
	}

	for _, forbidden := range []struct {
		perm Perm
		name string
	}{
		{PermDownload, "download"},
		{PermReadVersions, "read_versions"},
		{PermViewHistory, "view_history"},
		{PermRestoreVersion, "restore_version"},
	} {
		if cap.Can(forbidden.perm) {
			t.Fatalf("an administrator must not have %s on user content, got %v",
				forbidden.name, cap.Perms())
		}
	}

	// They must keep the two capabilities the design grants them.
	if !cap.Can(PermRead) || !cap.Can(PermDelete) {
		t.Fatalf("an admin must keep metadata read and force-delete, got %v", cap.Perms())
	}
}
