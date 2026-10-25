package storetest

// Directory ETag propagation (M1, competitiveness-analysis §5).
//
// The suite exists because the propagation is implemented twice — one recursive CTE and one Go
// walk — and because it touches every mutation path: a backend that stamps on create but not
// on trash would pass a single-feature test and still force sync clients to walk. Each case
// asserts the same chain of directories changed token and that an unrelated subtree did not.

import (
	"context"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// etagOf re-reads a node's current etag.
func etagOf(t *testing.T, s store.Store, id uuid.UUID) string {
	t.Helper()
	var etag string
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.NodeIncludingTrashed(ctx, id)
		if err != nil {
			return err
		}
		etag = n.ETag
		return nil
	}); err != nil {
		t.Fatalf("read etag: %v", err)
	}
	return etag
}

// mkdirAt creates one directory under an arbitrary parent.
func mkdirAt(t *testing.T, s store.Store, owner, parent uuid.UUID, name string) store.Node {
	t.Helper()
	var out store.Node
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: parent, Name: name})
		out = n
		return err
	})
	if err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return out
}

// rewriteFile overwrites the file's content through the shared upsert path.
func rewriteFile(t *testing.T, s store.Store, f store.Node) {
	t.Helper()
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, _, err := tx.WriteFile(ctx, store.WriteFile{
			NewFile: store.NewFile{
				OwnerID: f.OwnerID, ParentID: *f.ParentID, Name: f.Name,
				Size: 9, Mime: f.Mime, ETag: `"rewritten"`,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
}

// DirETagPropagationCases is the shared suite.
func DirETagPropagationCases(factory Factory) func(*testing.T) {
	return func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		// The tree: root/a/b (the watched chain) and root/c (the untouched control).
		a := mkdirAt(t, s, owner, r.ID, "a")
		b := mkdirAt(t, s, owner, a.ID, "b")
		c := mkdirAt(t, s, owner, r.ID, "c")
		f := mkfile(t, s, owner, b.ID, "watched.txt", 8)

		// Every directory in the changed chain carries a token; the control does not,
		// because nothing happened under it.
		rootBefore, aBefore, bBefore := etagOf(t, s, r.ID), etagOf(t, s, a.ID), etagOf(t, s, b.ID)
		cBefore := etagOf(t, s, c.ID)
		if rootBefore == "" || aBefore == "" || bBefore == "" {
			t.Fatalf("directories carry no change token after a write under them: root=%q a=%q b=%q",
				rootBefore, aBefore, bBefore)
		}
		if cBefore != "" {
			t.Fatalf("the untouched control subtree carries a token (%q): an over-stamping "+
				"propagation invalidates every cache in the tree on every write", cBefore)
		}

		t.Run("ASecondWriteUnderTheSameChainChangesEveryToken", func(t *testing.T) {
			rewriteFile(t, s, f)
			rootNow, aNow, bNow := etagOf(t, s, r.ID), etagOf(t, s, a.ID), etagOf(t, s, b.ID)
			if rootNow == rootBefore || aNow == aBefore || bNow == bBefore {
				t.Fatalf("a second write did not change the chain: root %q→%q a %q→%q b %q→%q",
					rootBefore, rootNow, aBefore, aNow, bBefore, bNow)
			}
			if etagOf(t, s, c.ID) != cBefore {
				t.Fatal("the control subtree's token changed although nothing happened under it")
			}
		})

		t.Run("TheFileKeepsItsContentETag", func(t *testing.T) {
			// A sibling directory under a must not rewrite the FILE's etag: the etag is the
			// content hash and the conditional-write vocabulary is written in terms of it.
			before := etagOf(t, s, f.ID)
			mkdirAt(t, s, owner, a.ID, "sibling")
			if after := etagOf(t, s, f.ID); after != before {
				t.Fatalf("a directory token landed on a file: %q → %q", before, after)
			}
		})

		t.Run("TrashAndRestoreChangeTheChain", func(t *testing.T) {
			before := etagOf(t, s, b.ID)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Trash(ctx, f.ID)
			}); err != nil {
				t.Fatal(err)
			}
			if after := etagOf(t, s, b.ID); after == before {
				t.Fatal("trashing a file did not change its containing directory's token")
			}

			restored := etagOf(t, s, b.ID)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Restore(ctx, f.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if after := etagOf(t, s, b.ID); after == restored {
				t.Fatal("restoring a file did not change its containing directory's token")
			}
		})

		t.Run("RenameAndMoveChangeBothEnds", func(t *testing.T) {
			before := etagOf(t, s, b.ID)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Rename(ctx, f.ID, "renamed.txt")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if after := etagOf(t, s, b.ID); after == before {
				t.Fatal("a rename did not change the directory's token")
			}

			// A move changes both ends: b loses the entry, c gains it.
			bLosing := etagOf(t, s, b.ID)
			cGaining := etagOf(t, s, c.ID)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Move(ctx, f.ID, c.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if after := etagOf(t, s, b.ID); after == bLosing {
				t.Fatal("the directory a file moved OUT of kept its token")
			}
			if after := etagOf(t, s, c.ID); after == cGaining {
				t.Fatal("the directory a file moved INTO kept its token")
			}
		})
	}
}
