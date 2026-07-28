package store_test

// RecentFiles against PostgreSQL.
//
// This exists because the in-memory conformance suite cannot catch a SQL error. The first
// version of this query joined two relations that both exposed `id` and failed at runtime with
// "column reference id is ambiguous" — the Go code was fine and every memory-backed test
// passed. A query that only ever runs against a fake is not tested.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// Mutations live on Tx, not on Store, so each helper runs one transaction. That mirrors how
// the server calls the store and keeps the test honest about the interface it actually uses.
func inTx(t *testing.T, s store.Store, fn func(ctx context.Context, tx store.Tx) error) {
	t.Helper()
	if err := s.WithTx(context.Background(), fn); err != nil {
		t.Fatalf("transaction: %v", err)
	}
}

// newFileAt creates one file inside parent, with the blob row its foreign key needs.
func newFileAt(t *testing.T, s store.Store, owner, parent uuid.UUID, name string, blob uuid.UUID) store.Node {
	t.Helper()
	var out store.Node
	inTx(t, s, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID:  owner,
			ParentID: parent,
			Name:     name,
			Size:     3,
			Mime:     "text/plain",
			ETag:     "etag-" + name,
			BlobID:   blob,
		})
		out = n
		return err
	})
	return out
}

func newDirAt(t *testing.T, s store.Store, owner, parent uuid.UUID, name string) store.Node {
	t.Helper()
	var out store.Node
	inTx(t, s, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: parent, Name: name})
		out = n
		return err
	})
	return out
}

// TestPGRecentFiles covers ordering, nesting and the trashed exclusion against the real query.
func TestPGRecentFiles(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	ctx := context.Background()
	owner := newUser(t, pool)
	s := store.NewPG(pool)

	root, err := s.EnsureRoot(ctx, owner)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	older := newFileAt(t, s, owner, root.ID, "older.txt", newBlob(t, pool))

	docs := newDirAt(t, s, owner, root.ID, "docs")
	year := newDirAt(t, s, owner, docs.ID, "2026")
	buried := newFileAt(t, s, owner, year.ID, "buried.txt", newBlob(t, pool))

	// Touch the older file so the ordering cannot be explained by insertion order.
	inTx(t, s, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.UpdateFile(ctx, store.UpdateFile{
			NodeID: older.ID,
			Size:   4,
			Mime:   "text/plain",
			ETag:   "etag-touched",
			BlobID: newBlob(t, pool),
		})
		return err
	})

	recent, err := s.RecentFiles(ctx, owner, 10)
	if err != nil {
		t.Fatalf("RecentFiles: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("want 2 recents, got %d", len(recent))
	}
	if recent[0].Node.ID != older.ID {
		t.Errorf("the touched file must come first, got %q", recent[0].Node.Name)
	}
	if recent[0].Path != "older.txt" {
		t.Errorf("root path = %q, want older.txt", recent[0].Path)
	}
	if recent[1].Path != "docs/2026/buried.txt" {
		t.Errorf("nested path = %q, want docs/2026/buried.txt", recent[1].Path)
	}

	inTx(t, s, func(ctx context.Context, tx store.Tx) error { return tx.Trash(ctx, buried.ID) })
	after, err := s.RecentFiles(ctx, owner, 10)
	if err != nil {
		t.Fatalf("RecentFiles after trash: %v", err)
	}
	for _, item := range after {
		if item.Node.ID == buried.ID {
			t.Fatal("a trashed file must not appear in recents")
		}
	}
}

// TestPGRecentFilesIsScopedToTheOwner: recents are a personal view, not a global one.
func TestPGRecentFilesIsScopedToTheOwner(t *testing.T) {
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

	newFileAt(t, s, mine, myRoot.ID, "mine.txt", newBlob(t, pool))
	newFileAt(t, s, theirs, theirRoot.ID, "theirs.txt", newBlob(t, pool))

	recent, err := s.RecentFiles(ctx, mine, 10)
	if err != nil {
		t.Fatalf("RecentFiles: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("want 1 recent, got %d", len(recent))
	}
	if recent[0].Node.Name != "mine.txt" {
		t.Fatalf("leaked another owner's file: %q", recent[0].Node.Name)
	}
}

// TestPGRecentFilesRespectsTheLimit: the home screen asks for a handful, and the query must
// not return the whole library because a caller forgot a LIMIT.
func TestPGRecentFilesRespectsTheLimit(t *testing.T) {
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
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		newFileAt(t, s, owner, root.ID, name, newBlob(t, pool))
	}

	recent, err := s.RecentFiles(ctx, owner, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("want 2, got %d", len(recent))
	}
}
