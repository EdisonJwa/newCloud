package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/consistency"
)

func consistencyFixture(t *testing.T) (*Memory, Node, Node) {
	t.Helper()
	m := NewMemory()
	u := m.AddUser(User{Username: "alice", Email: "alice@example.test", Role: "user"})
	root, err := m.EnsureRoot(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dir Node
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		n, err := tx.CreateDir(ctx, NewNode{OwnerID: u.ID, ParentID: root.ID, Name: "docs"})
		dir = n
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return m, root, dir
}

func TestMemoryRevisionSeedAndCAS(t *testing.T) {
	m, _, dir := consistencyFixture(t)
	ctx := context.Background()

	if _, err := m.Current(ctx, dir.ID); !errors.Is(err, consistency.ErrRevisionConflict) {
		t.Fatalf("unseeded Current = %v", err)
	}
	if _, err := m.SeedRevision(ctx, dir.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SeedRevision(ctx, dir.ID); !errors.Is(err, consistency.ErrRevisionConflict) {
		t.Fatalf("second seed = %v", err)
	}
	if err := m.CAS(ctx, dir.ID, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.CAS(ctx, dir.ID, 1, 2); !errors.Is(err, consistency.ErrRevisionConflict) {
		t.Fatalf("stale CAS = %v", err)
	}
	if got, err := m.Current(ctx, dir.ID); err != nil || got != 2 {
		t.Fatalf("Current = %d, %v; want 2", got, err)
	}
}

func TestMemoryEffectiveVisibilityUsesTheStagedAncestor(t *testing.T) {
	m, root, dir := consistencyFixture(t)
	ctx := context.Background()

	ok, err := consistency.EffectiveExternalVisibility(ctx, m, dir.ID)
	if err != nil || !ok {
		t.Fatalf("live node visible = %v, %v", ok, err)
	}

	// Stage the ROOT, not the target: the child row is live, so a target-only
	// staged_by check would leak it by path or direct ID.
	m.SetStagedByForTest(root.ID, uuid.New())
	ok, err = consistency.EffectiveExternalVisibility(ctx, m, dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a child of a staged root was externally visible (G2)")
	}

	m.SetStagedByForTest(root.ID, uuid.Nil)
	ok, err = consistency.EffectiveExternalVisibility(ctx, m, dir.ID)
	if err != nil || !ok {
		t.Fatalf("after clearing staging: %v, %v", ok, err)
	}
}

func TestMemoryJournalDelegatesTheStateMachine(t *testing.T) {
	m, _, _ := consistencyFixture(t)
	ctx := context.Background()
	id := uuid.New()
	if _, err := m.Create(ctx, consistency.Operation{ID: id, OwnerID: uuid.New(), Kind: "copy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, id, consistency.StateIntent, consistency.StateStaging); err != nil {
		t.Fatal(err)
	}
	open, err := m.NonTerminal(ctx, 10)
	if err != nil || len(open) != 1 || open[0].ID != id {
		t.Fatalf("NonTerminal = %v, %v", open, err)
	}
}

func TestMemoryPublishBlobCompletesTheJournalAtomically(t *testing.T) {
	m, root, _ := consistencyFixture(t)
	ctx := context.Background()
	ownerID := root.OwnerID
	opID := uuid.New()
	if _, err := m.Create(ctx, consistency.Operation{ID: opID, OwnerID: ownerID, Kind: "upload"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, opID, consistency.StateIntent, consistency.StateStaging); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, opID, consistency.StateStaging, consistency.StatePublishing); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte("atomic"))
	if err := m.PublishBlob(ctx, consistency.BlobPublish{
		OperationID: opID, OperationKind: "upload", Owner: ownerID.String(), Path: "atomic.txt", Hash: h, Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	op, err := m.Get(ctx, opID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != consistency.StateCommitted {
		t.Fatalf("journal state = %s, want COMMITTED", op.State)
	}
	if _, err := m.Resolve(ctx, ownerID, "atomic.txt"); err != nil {
		t.Fatalf("published node missing: %v", err)
	}
}
