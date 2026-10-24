package dircommit

// The sixth review (NC-R6-001) was about the ORDER of the two writes: a directory whose
// physical creation failed must not be committed in metadata with a success audit row. The
// seventh review then reproduced two ways the compensating delete used for the reverse
// failure could lose data (P0-R7-001/002). These tests pin both halves:
//
//   - the metadata callback never runs when the content store refused the directory;
//   - a failed metadata commit never deletes anything afterwards: not a concurrent winner's
//     directory, not a file added while the transaction ran, and not an existing path.
//
// The cleanup is deliberately absent. A test that only checks "the directory is gone" would
// reward exactly the unsafe behaviour the seventh review reproduced, so the tests below
// assert the safe outcome instead: no live metadata and no destructive call.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func testStore(t *testing.T) (*tree.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := tree.New(root)
	if err != nil {
		t.Fatalf("tree store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func docsNode() layout.Node {
	return layout.Node{Owner: "alice", Path: "docs", IsDir: true}
}

// The metadata row must not be created before the directory exists on disk. Otherwise the
// callback could commit a node for a path the content store will then refuse.
func TestCreateWritesTheContentStoreBeforeTheMetadataCommit(t *testing.T) {
	content, _ := testStore(t)

	var sawDir bool
	err := Create(context.Background(), content, docsNode(), func(ctx context.Context) error {
		_, serr := content.Stat(ctx, docsNode())
		sawDir = serr == nil
		return nil
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !sawDir {
		t.Fatal("the metadata commit ran before the content store had the directory")
	}
}

// The injected failure from the review: the physical create fails. The metadata transaction
// (node plus success audit row) must never run, so the client cannot be told about a directory
// that does not exist.
func TestAFailedContentCreateNeverRunsTheMetadataCommit(t *testing.T) {
	content, root := testStore(t)

	// "alice" is a file, so creating "alice/docs" fails with ENOTDIR.
	if err := os.WriteFile(filepath.Join(root, "alice"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	committed := false
	err := Create(context.Background(), content, docsNode(), func(context.Context) error {
		committed = true
		return nil
	})
	if err == nil {
		t.Fatal("a content-store Mkdir failure must be returned, not swallowed")
	}
	if committed {
		t.Fatal("the metadata transaction ran after the content store refused the directory")
	}
	if _, serr := content.Stat(context.Background(), docsNode()); !errors.Is(serr, layout.ErrNotFound) {
		t.Fatalf("the directory exists after a failed create: %v", serr)
	}
}

// P0-R7-002: a losing request must not delete the directory a concurrent winner's live
// metadata node owns. The winner here commits through the real memory store, so the assertion
// is against a real node, not a fabricated one.
func TestAFailedMetadataCommitNeverDeletesAConcurrentWinnersDirectory(t *testing.T) {
	content, _ := testStore(t)
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test"})
	rootNode, err := meta.EnsureRoot(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	var winner store.Node
	loserErr := errors.New("injected: audit metadata unavailable")
	err = Create(context.Background(), content, docsNode(), func(ctx context.Context) error {
		// Request B creates and commits the SAME path while request A's transaction is open.
		if berr := Create(ctx, content, docsNode(), func(ctx context.Context) error {
			return meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				n, cerr := tx.CreateDir(ctx, store.NewNode{
					OwnerID: owner.ID, ParentID: rootNode.ID, Name: "docs",
				})
				if cerr != nil {
					return cerr
				}
				winner = n
				return nil
			})
		}); berr != nil {
			t.Fatalf("the concurrent winner failed: %v", berr)
		}
		return loserErr
	})
	if !errors.Is(err, loserErr) {
		t.Fatalf("Create = %v, want the injected metadata failure", err)
	}

	// The winner's metadata is live...
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, nerr := tx.Node(ctx, winner.ID)
		if nerr != nil {
			return nerr
		}
		if got.Name != "docs" {
			t.Fatalf("winner node %s is named %q, want docs", winner.ID, got.Name)
		}
		return nil
	}); err != nil {
		t.Fatalf("the winner's live metadata node disappeared: %v", err)
	}

	// ...so its physical directory must still exist. This is the assertion the pre-R7 code
	// failed: the loser's cleanup removed it because its pre-create Stat had said "absent".
	if _, serr := content.Stat(context.Background(), docsNode()); serr != nil {
		t.Fatalf("the loser removed the directory its winning request's live metadata node "+
			"names; Stat = %v", serr)
	}
}

// recordingContent counts the destructive calls a failed metadata commit makes after the
// physical directory is created. P0-R7-001 was a List-then-recursive-Delete gap; the safe
// policy is that a failure never asks the content store to delete anything.
type recordingContent struct {
	layout.ContentStore
	armed       atomic.Bool
	destructive atomic.Int32
}

func (r *recordingContent) List(ctx context.Context, dir layout.Node, limit int) ([]layout.Node, error) {
	if r.armed.Load() {
		r.destructive.Add(1)
	}
	return r.ContentStore.List(ctx, dir, limit)
}

func (r *recordingContent) Delete(ctx context.Context, n layout.Node) error {
	if r.armed.Load() {
		r.destructive.Add(1)
	}
	return r.ContentStore.Delete(ctx, n)
}

// P0-R7-001: no cleanup may run after a failed metadata commit, because any check it makes
// and any delete it then performs can race a concurrent writer. The strongest form of that
// rule is to make no destructive call at all.
func TestAFailedMetadataCommitMakesNoDestructiveCall(t *testing.T) {
	content, _ := testStore(t)
	rec := &recordingContent{ContentStore: content}
	sentinel := errors.New("injected: metadata unavailable")

	if err := Create(context.Background(), rec, docsNode(), func(context.Context) error {
		// From here on, the pre-R7 cleanup would List and then Delete.
		rec.armed.Store(true)
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Create = %v, want the metadata failure", err)
	}
	if n := rec.destructive.Load(); n != 0 {
		t.Fatalf("a failed metadata commit made %d destructive content-store call(s); the "+
			"List-then-Delete gap can remove a concurrent writer's bytes", n)
	}

	// The deliberately kept orphan is unreferenced and empty, and a retry adopts it.
	if _, serr := content.Stat(context.Background(), docsNode()); serr != nil {
		t.Fatalf("the unreferenced directory is unexpectedly absent: %v", serr)
	}
	kids, err := content.List(context.Background(), docsNode(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 0 {
		t.Fatalf("the orphan directory is not empty: %v", kids)
	}
}

// A file written into the directory while the metadata transaction runs must survive the
// failure. This is the case the sixth-review cleanup already handled; it stays as a guard
// against reintroducing a recursive delete.
func TestAFailedMetadataCommitNeverDeletesContentAddedWhileItRan(t *testing.T) {
	content, root := testStore(t)
	keep := filepath.Join(root, "alice", "docs", "keep.txt")

	sentinel := errors.New("metadata unavailable")
	if err := Create(context.Background(), content, docsNode(), func(context.Context) error {
		if err := os.WriteFile(keep, []byte("a concurrent writer's bytes"), 0o600); err != nil {
			t.Fatalf("simulate a concurrent write: %v", err)
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Create = %v, want the metadata failure", err)
	}
	got, err := os.ReadFile(keep)
	if err != nil {
		t.Fatalf("the failure path removed a file another writer created: %v", err)
	}
	if string(got) != "a concurrent writer's bytes" {
		t.Fatalf("the file's bytes changed: %q", got)
	}
}

// A directory that already existed before this call is not this call's to remove, whatever
// the metadata outcome.
func TestAFailedMetadataCommitNeverDeletesAnExistingDirectory(t *testing.T) {
	content, root := testStore(t)
	full := filepath.Join(root, "alice", "docs")
	if err := os.MkdirAll(full, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "existing.txt"), []byte("existing bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("metadata unavailable")
	if err := Create(context.Background(), content, docsNode(), func(context.Context) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Create = %v, want the metadata failure", err)
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("a pre-existing directory was removed by a failed metadata commit: %v", err)
	}
}
