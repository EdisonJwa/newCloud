package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// aclAccessor adapts a real store to the authorizer.
//
// This mirrors the adapter in cmd/newcloud on purpose: these tests are about the
// *wire* between stored grants and an authorization decision, and a fake accessor
// would let that wire be wrong while every test passed.
type aclAccessor struct{ m store.Store }

func (a *aclAccessor) LoadNode(id uuid.UUID) (authz.Node, error) {
	var n store.Node
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, gerr := tx.Node(ctx, id)
		n = got
		return gerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authz.Node{}, authz.ErrNotFound
		}
		return authz.Node{}, err
	}
	return authz.Node{ID: n.ID, OwnerID: n.OwnerID, ParentID: n.ParentID}, nil
}

func (a *aclAccessor) OwnerGroupPerm(actorID, nodeID uuid.UUID) (authz.Perm, bool, error) {
	var (
		perms store.Perm
		ok    bool
	)
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, found, gerr := tx.EffectivePermsFor(ctx, actorID, nodeID)
		perms, ok = p, found
		return gerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return authz.Perm(perms), ok, nil
}

func (a *aclAccessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	return nil, nil
}

// TestStoreGrantReachesTheAuthorizer is the integration test INV-01 needs.
//
// The conformance suite proves a grant is stored and resolved correctly. The authz
// matrix proves the authorizer applies a grant it is handed. Neither proves the two
// are connected — and they were not, for the whole life of the project, because
// OwnerGroupPerm was a stub returning "no grant". This closes that gap.
func TestStoreGrantReachesTheAuthorizer(t *testing.T) {
	ctx := context.Background()
	meta := store.NewMemory()

	owner := meta.AddUser(store.User{Username: "owner", Email: "owner@example.test", Role: "user"})
	mate := meta.AddUser(store.User{Username: "mate", Email: "mate@example.test", Role: "user"})
	outsider := meta.AddUser(store.User{Username: "outsider", Email: "out@example.test", Role: "user"})

	root, err := meta.EnsureRoot(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	var folder store.Node
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: owner.ID, ParentID: root.ID, Name: "shared",
		})
		folder = n
		return cerr
	}); err != nil {
		t.Fatal(err)
	}

	svc := authz.NewService(&aclAccessor{m: meta})
	mateActor := authz.NewUserActor(mate.ID, mate.Email, authz.RoleUser,
		authz.ScopeRead|authz.ScopeWrite)
	outsiderActor := authz.NewUserActor(outsider.ID, outsider.Email, authz.RoleUser,
		authz.ScopeRead|authz.ScopeWrite)

	// Before the grant: denied. This is the fail-closed baseline.
	if _, err := svc.Authorize(mateActor, folder.ID); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("without a grant the answer must be denied, got %v", err)
	}

	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, gerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: folder.ID, UserID: &mate.ID,
			Perms: store.PermRead | store.PermListChildren, Inherit: true,
		})
		return gerr
	}); err != nil {
		t.Fatal(err)
	}

	cap, err := svc.Authorize(mateActor, folder.ID)
	if err != nil {
		t.Fatalf("the granted user must be allowed: %v", err)
	}
	if !cap.Can(authz.PermRead) || !cap.Can(authz.PermListChildren) {
		t.Fatalf("want read+list, got %v", cap.Perms())
	}
	if cap.Can(authz.PermWrite) || cap.Can(authz.PermDelete) {
		t.Fatalf("a read grant must not confer write: %v", cap.Perms())
	}

	if _, err := svc.Authorize(outsiderActor, folder.ID); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("an ungranted user must stay denied, got %v", err)
	}

	// A group grant reaches its members through the same path.
	groupMate := meta.AddUser(store.User{Username: "gm", Email: "gm@example.test", Role: "user"})
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		g, gerr := tx.CreateGroup(ctx, store.Group{Name: "readers"})
		if gerr != nil {
			return gerr
		}
		if aerr := tx.AddGroupMember(ctx, g.ID, groupMate.ID, "viewer"); aerr != nil {
			return aerr
		}
		_, aerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: folder.ID, GroupID: &g.ID,
			Perms: store.PermRead, Inherit: true,
		})
		return aerr
	}); err != nil {
		t.Fatal(err)
	}

	groupActor := authz.NewUserActor(groupMate.ID, groupMate.Email, authz.RoleUser,
		authz.ScopeRead|authz.ScopeWrite)
	gcap, err := svc.Authorize(groupActor, folder.ID)
	if err != nil {
		t.Fatalf("a group member must be allowed by the group grant: %v", err)
	}
	if !gcap.Can(authz.PermRead) {
		t.Fatalf("want read from the group grant, got %v", gcap.Perms())
	}

	// A read-only credential stays read-only even with a read-write grant: the
	// credential scope is applied last.
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, gerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: folder.ID, UserID: &outsider.ID,
			Perms: store.PermRead | store.PermWrite, Inherit: true,
		})
		return gerr
	}); err != nil {
		t.Fatal(err)
	}
	roActor := authz.NewUserActor(outsider.ID, outsider.Email, authz.RoleUser, authz.ScopeRead)
	roCap, err := svc.Authorize(roActor, folder.ID)
	if err != nil {
		t.Fatalf("a read-only credential on a readable node must be allowed: %v", err)
	}
	if roCap.Can(authz.PermWrite) {
		t.Fatal("a read-only credential must never come out of Authorize with write")
	}
}

// TestOwnerStillOutranksACL: a grant on a node must never take anything away from its
// owner, or adding a collaborator could break the account that added them.
func TestOwnerStillOutranksACL(t *testing.T) {
	ctx := context.Background()
	meta := store.NewMemory()

	owner := meta.AddUser(store.User{Username: "owner2", Email: "o2@example.test", Role: "user"})
	root, err := meta.EnsureRoot(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A narrow grant to the owner on their own node.
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, gerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: root.ID, UserID: &owner.ID,
			Perms: store.PermRead, Inherit: false,
		})
		return gerr
	}); err != nil {
		t.Fatal(err)
	}

	svc := authz.NewService(&aclAccessor{m: meta})
	actor := authz.NewUserActor(owner.ID, owner.Email, authz.RoleUser,
		authz.ScopeRead|authz.ScopeWrite)

	cap, err := svc.Authorize(actor, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Can(authz.PermWrite) || !cap.Can(authz.PermDelete) || !cap.Can(authz.PermShare) {
		t.Fatalf("the owner must keep full control, got %v", cap.Perms())
	}
}

// TestRecycledEmailIsRefusedNotInherited is the §3 acceptance case: an IdP
// reassigns an address to a new subject.
//
// The requirement is precise — the new person must not inherit the old person's
// files. It says nothing about what should happen instead, and the previous behaviour
// was a 500, which satisfies "no inheritance" only by accident and tells the operator
// nothing. This asserts the defined outcome: a conflict error the caller can map to a
// clear refusal.
func TestRecycledEmailIsRefusedNotInherited(t *testing.T) {
	ctx := context.Background()
	meta := store.NewMemory()

	// The original holder of the address.
	first, err := upsertIdP(meta, store.UpsertUser{
		Issuer: "https://idp.test", Subject: "subject-original",
		Username: "alice", Email: "alice@example.test", Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meta.EnsureRoot(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	root, err := meta.Root(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	var file store.Node
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: first.ID, ParentID: root.ID, Name: "private.txt",
			Size: 10, ETag: "e", BlobID: uuid.New(),
		})
		file = n
		return cerr
	}); err != nil {
		t.Fatal(err)
	}

	// The IdP now hands the same address to a different subject.
	second, err := upsertIdP(meta, store.UpsertUser{
		Issuer: "https://idp.test", Subject: "subject-replacement",
		Username: "alice2", Email: "alice@example.test", Role: "user",
	})

	// The defined outcome is a refusal: the address is already attached to another
	// identity, and admitting a second account with it would make
	// "share with alice@example.com" ambiguous. Both backends must agree on this —
	// they did not before, because only PostgreSQL had the constraint.
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a recycled address must be a conflict, got user=%v err=%v", second.ID, err)
	}

	// And the old account's files must remain reachable only by the old account,
	// whatever happened to the new one.
	var ownerID uuid.UUID
	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, file.ID)
		ownerID = n.OwnerID
		return nerr
	}); err != nil {
		t.Fatal(err)
	}
	if ownerID != first.ID {
		t.Fatalf("file ownership must not migrate, want %s got %s", first.ID, ownerID)
	}
}

// upsertIdP runs UpsertIdPUser in its own transaction.
func upsertIdP(s store.Store, in store.UpsertUser) (store.User, error) {
	var out store.User
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, in)
		out = u
		return uerr
	})
	return out, err
}
