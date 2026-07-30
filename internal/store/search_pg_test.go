package store_test

// Search against PostgreSQL.
//
// The in-memory implementation and the SQL one agree on the semantics, but only one of them
// can be wrong about SQL. These cover the two things the query could plausibly get wrong and
// the fake cannot: the path assembly for nested hits, and owner scoping.

import (
	"context"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestPGSearchFindsNestedHits: the path has to locate the file, not just name it.
func TestPGSearchFindsNestedHits(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	ctx := context.Background()
	owner := newUser(t, pool)
	s := store.NewPG(pool)

	root, err := s.EnsureRoot(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	docs := newDirAt(t, s, owner, root.ID, "docs")
	year := newDirAt(t, s, owner, docs.ID, "2026")
	newFileAt(t, s, owner, year.ID, "budget-final.txt", newBlob(t, pool))
	newFileAt(t, s, owner, root.ID, "notes.txt", newBlob(t, pool))

	hits, err := s.Search(ctx, owner, "budget", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Path != "docs/2026/budget-final.txt" {
		t.Fatalf("path = %q, want docs/2026/budget-final.txt", hits[0].Path)
	}
}

// TestPGSearchIsScopedToTheOwner: search must not become a way to read another account.
func TestPGSearchIsScopedToTheOwner(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	ctx := context.Background()
	mine := newUser(t, pool)
	theirs := newUser(t, pool)
	s := store.NewPG(pool)

	myRoot, err := s.EnsureRoot(ctx, mine)
	if err != nil {
		t.Fatal(err)
	}
	theirRoot, err := s.EnsureRoot(ctx, theirs)
	if err != nil {
		t.Fatal(err)
	}
	newFileAt(t, s, mine, myRoot.ID, "mine-report.txt", newBlob(t, pool))
	newFileAt(t, s, theirs, theirRoot.ID, "theirs-report.txt", newBlob(t, pool))

	hits, err := s.Search(ctx, mine, "report", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Node.Name != "mine-report.txt" {
		t.Fatalf("leaked another owner's file: %q", hits[0].Node.Name)
	}
}
