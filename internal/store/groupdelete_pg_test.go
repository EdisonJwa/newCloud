package store_test

// Deleting a group turns its spaces into ordinary folders instead of being blocked by them.
//
// There is no delete-group path in the product yet, so this drives the schema directly: the point is
// that the semantic is decided and enforced before somebody writes that path, because the default
// (NO ACTION) makes the operation impossible and explains it with a constraint name.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestDeletingAGroupLeavesItsSpacesAsOrdinaryFolders(t *testing.T) {
	pool := testPool(t)
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()

	owner := newUser(t, pool)

	var groupID uuid.UUID
	var spaceID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		g, gerr := tx.CreateGroup(ctx, store.Group{Name: "doomed-" + uuid.New().String()[:8]})
		if gerr != nil {
			return gerr
		}
		groupID = g.ID
		root, rerr := tx.EnsureRoot(ctx, owner)
		if rerr != nil {
			return rerr
		}
		n, cerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: owner, ParentID: root.ID, Name: "space", OwnerGroupID: &groupID,
		})
		spaceID = n.ID
		return cerr
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The administrative act that has no route yet. Run as SQL because that is where the constraint
	// this migration changed actually lives.
	if _, err := pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, groupID); err != nil {
		t.Fatalf("deleting the group failed (%v). With the old NO ACTION constraint this is the "+
			"foreign-key violation an operator would hit, and the message would name a constraint "+
			"rather than the spaces", err)
	}

	var (
		found    bool
		hasGroup bool
	)
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.Node(ctx, spaceID)
		if nerr != nil {
			return nerr
		}
		found = true
		hasGroup = n.OwnerGroupID != nil
		return nil
	}); err != nil {
		t.Fatalf("the space did not survive its group being deleted: %v", err)
	}
	if !found {
		t.Fatal("the space's node was deleted along with the group")
	}
	if hasGroup {
		t.Fatal("the node still names a group that no longer exists")
	}
}
