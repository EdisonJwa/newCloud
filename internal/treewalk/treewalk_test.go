package treewalk

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// newStore returns an in-memory store with one account and its root.
func newStore(t *testing.T) (*store.Memory, store.User, store.Node) {
	t.Helper()
	m := store.NewMemory()
	u := m.AddUser(store.User{Username: "walker", Email: "walker@example.test", Role: "user"})
	root, err := m.EnsureRoot(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return m, u, root
}

// mkdir creates one directory and returns it.
func mkdir(t *testing.T, m *store.Memory, owner, parent uuid.UUID, name string) store.Node {
	t.Helper()
	var out store.Node
	err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: parent, Name: name})
		out = n
		return cerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestResolveReturnsTheWholeTreeWithPaths pins the two things callers rely on: every descendant is
// present, and each node's Rel is its full path from the requested root (the content store is
// addressed by that path, so a wrong parent join would read the wrong bytes).
func TestResolveReturnsTheWholeTreeWithPaths(t *testing.T) {
	m, u, root := newStore(t)
	a := mkdir(t, m, u.ID, root.ID, "a")
	mkdir(t, m, u.ID, root.ID, "b")
	mkdir(t, m, u.ID, a.ID, "deep")
	mkdir(t, m, u.ID, a.ID, "deeper")

	tree, err := Resolve(context.Background(), m, root, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) != 2 {
		t.Fatalf("root has %d children, want 2", len(tree.Children))
	}
	got := map[string]bool{}
	var walk func(*Tree)
	walk = func(n *Tree) {
		got[n.Rel] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(tree)
	for _, want := range []string{
		"", "a", "b", "a/deep", "a/deeper",
	} {
		if !got[want] {
			t.Fatalf("the resolved tree has no node at %q (got %v)", want, got)
		}
	}
}

// TestResolveRefusesRatherThanTruncating is the core promise: a subtree over the budget is an error,
// never a prefix presented as the tree.
func TestResolveRefusesRatherThanTruncating(t *testing.T) {
	m, u, root := newStore(t)
	mkdir(t, m, u.ID, root.ID, "a")
	mkdir(t, m, u.ID, root.ID, "b")
	mkdir(t, m, u.ID, root.ID, "c")

	tree, err := Resolve(context.Background(), m, root, "", 2)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a three-node subtree with a two-node budget returned (%v, %v), want ErrTooLarge. "+
			"A partial tree here is a copy that silently lost a file.", tree, err)
	}
	if tree != nil {
		t.Fatal("a refused walk returned a tree; callers must not be able to use one")
	}
}

// TestResolveDropsTrashedChildren: a live folder can contain a trashed child, and a copy or archive
// built from this tree must not resurrect it.
func TestResolveDropsTrashedChildren(t *testing.T) {
	m, u, root := newStore(t)
	keep := mkdir(t, m, u.ID, root.ID, "keep")
	gone := mkdir(t, m, u.ID, root.ID, "gone")

	if err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Trash(ctx, gone.ID)
	}); err != nil {
		t.Fatal(err)
	}

	tree, err := Resolve(context.Background(), m, root, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) != 1 || tree.Children[0].Node.ID != keep.ID {
		t.Fatalf("the resolved tree is %v, want only %q", names(tree), keep.Name)
	}
}

// TestResolveAtTheDepthBoundIsRefused: the store's walk stops at MaxDepth, so a chain that reaches it
// cannot be proven complete and must be refused rather than described as whole.
func TestResolveAtTheDepthBoundIsRefused(t *testing.T) {
	m, u, root := newStore(t)
	parent := root
	for i := 0; i < MaxDepth; i++ {
		parent = mkdir(t, m, u.ID, parent.ID, fmt.Sprintf("d%02d", i))
	}

	if _, err := Resolve(context.Background(), m, root, "", 1000); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("a %d-deep chain resolved without ErrTooDeep (got %v); the store's walk stops there, "+
			"so completeness cannot be claimed", MaxDepth, err)
	}
}

// TestResolveOfAnEmptyDirectoryIsOneNode: nothing below the root means a one-node tree, not an
// error and not a query for children the caller does not need.
func TestResolveOfAnEmptyDirectoryIsOneNode(t *testing.T) {
	m, u, root := newStore(t)
	dir := mkdir(t, m, u.ID, root.ID, "dir")

	tree, err := Resolve(context.Background(), m, dir, "dir", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Node.ID != dir.ID || tree.Rel != "dir" || len(tree.Children) != 0 {
		t.Fatalf("resolving an empty directory returned %+v", tree)
	}
}

func names(t *Tree) []string {
	out := make([]string, 0, len(t.Children))
	for _, c := range t.Children {
		out = append(out, c.Node.Name)
	}
	return out
}
