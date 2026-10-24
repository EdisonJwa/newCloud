package swap_test

// The ownership rule, tested where it is decided.
//
// The 2026-10-23 review caught the batch MOVE overwrite destroying its own source: a full trash made
// the metadata transaction roll back with ErrTrashFull, the rollback "discarded" the staged node the
// way a COPY's duplicate is discarded, and the user's file was gone — bytes deleted, row trashed, the
// trash restore left naming nothing readable. The stage in that operation was the user's source node
// itself, not a duplicate.
//
// These tests pin the two halves of the answer at the level of the swap itself: a borrowed stage is
// never destroyed and is returned on every failure, a disposable one is always cleaned up, and a
// caller cannot name a stage without choosing. The end-to-end behaviour is covered from the HTTP
// side in internal/server.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/swap"
)

// recorder is the real tree store with the destructive calls recorded.
//
// A real path-backed store rather than a fake: the failure this guards against only exists because a
// tree layout keeps bytes at a path and a move is a rename, and a fake would let the test pass while
// the actual ordering was wrong. The tree layout also has EfficientRename=false, which is the branch
// that performs physical moves at all.
type recorder struct {
	*tree.Store
	deletes []string
	moves   int
	// failFrom makes Move fail for that source path, so a test can pick the exact physical step that
	// breaks. A real filesystem fault is the failure mode this imitates.
	failFrom string
	failErr  error
}

func (r *recorder) Delete(ctx context.Context, n layout.Node) error {
	r.deletes = append(r.deletes, n.Path)
	return r.Store.Delete(ctx, n)
}

func (r *recorder) Move(ctx context.Context, src, dst layout.Node) error {
	r.moves++
	if r.failFrom != "" && src.Path == r.failFrom {
		return r.failErr
	}
	return r.Store.Move(ctx, src, dst)
}

// fixture is one destination and one source in the same directory, with real bytes for both.
type fixture struct {
	ownerID  uuid.UUID
	owner    string
	parent   store.Node
	existing store.Node
	source   store.Node
	dstRel   string
	srcRel   string
	meta     *store.Memory
	content  *recorder
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	ownerID := uuid.New()
	meta := store.NewMemory()
	root, err := meta.EnsureRoot(ctx, ownerID)
	if err != nil {
		t.Fatal(err)
	}

	ts, err := tree.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	content := &recorder{Store: ts}

	f := &fixture{
		ownerID: ownerID, owner: ownerID.String(), meta: meta, content: content,
		dstRel: path.Join("docs", "existing.txt"),
		srcRel: path.Join("docs", "source.txt"),
	}

	if err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		parent, cerr := tx.CreateDir(ctx, store.NewNode{OwnerID: ownerID, ParentID: root.ID, Name: "docs"})
		if cerr != nil {
			return cerr
		}
		f.parent = parent
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	f.existing = createFile(t, meta, ownerID, f.parent.ID, "existing.txt", "DEST existing user data")
	put(t, content, f.owner, f.dstRel, "DEST existing user data")
	f.source = createFile(t, meta, ownerID, f.parent.ID, "source.txt", "SOURCE important user data")
	put(t, content, f.owner, f.srcRel, "SOURCE important user data")

	return f
}

func nodeAt(t *testing.T, m *store.Memory, id uuid.UUID) store.Node {
	t.Helper()
	var n store.Node
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.Node(ctx, id)
		n = got
		return err
	}); err != nil {
		t.Fatalf("read node %s: %v", id, err)
	}
	return n
}

func createFile(t *testing.T, m *store.Memory, owner, parent uuid.UUID, name, content string) store.Node {
	t.Helper()
	var n store.Node
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: parent, Name: name,
			Size: int64(len(content)), Mime: "text/plain", ETag: "etag-" + name,
		})
		n = got
		return err
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return n
}

func put(t *testing.T, c layout.ContentStore, owner, rel, body string) {
	t.Helper()
	if _, err := c.Put(context.Background(), layout.Node{Owner: owner, Path: rel},
		bytes.NewReader([]byte(body)), int64(len(body))); err != nil {
		t.Fatalf("put %s: %v", rel, err)
	}
}

func readFile(t *testing.T, c layout.ContentStore, owner, rel string) string {
	t.Helper()
	rc, err := c.Open(context.Background(), layout.Node{Owner: owner, Path: rel})
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func deps(f *fixture, trashCap int64) swap.Deps {
	return swap.Deps{
		Meta: f.meta, Content: f.content, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TrashCap: trashCap,
	}
}

// borrowStage relocates the source the way a MOVE overwrite does, and returns the function that puts
// it back. It is the caller's half of the protocol under test.
func (f *fixture) borrow(t *testing.T, restoreCalls *int) swap.Replacement {
	t.Helper()

	return swap.Borrowed(
		func(ctx context.Context, stagedName string) (store.Node, error) {
			moved, err := relocateForTest(ctx, f, f.source, f.srcRel, stagedName)
			if err != nil {
				return store.Node{}, err
			}
			return moved, nil
		},
		func(ctx context.Context, staged store.Node) error {
			*restoreCalls++
			// Restore is the inverse of the relocation: metadata back to the original name and
			// parent, bytes back to the original path.
			_, err := relocateForTest(ctx, f, staged, path.Join("docs", staged.Name), f.source.Name)
			return err
		},
	)
}

// relocateForTest renames a node and moves its bytes, the same two steps the real handler performs.
func relocateForTest(ctx context.Context, f *fixture, n store.Node, fromRel, newName string) (store.Node, error) {
	var moved store.Node
	if err := f.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		renamed, err := tx.Rename(ctx, n.ID, newName)
		if err != nil {
			return err
		}
		moved = renamed
		return nil
	}); err != nil {
		return store.Node{}, err
	}
	toRel := path.Join("docs", newName)
	if fromRel != toRel {
		if err := f.content.Move(ctx,
			layout.Node{Owner: f.owner, Path: fromRel, IsDir: n.IsDir()},
			layout.Node{Owner: f.owner, Path: toRel, IsDir: n.IsDir()}); err != nil {
			return store.Node{}, err
		}
	}
	return moved, nil
}

// TestBorrowedStageIsReturnedNotDeleted is the P0 regression: a full trash must not cost the source.
func TestBorrowedStageIsReturnedNotDeleted(t *testing.T) {
	f := newFixture(t)
	restoreCalls := 0

	_, err := swap.Install(context.Background(), deps(f, 1), swap.Request{
		Owner: f.owner, Parent: f.parent, ParentRel: "docs", DstName: "existing.txt",
		Existing: f.existing, StagedIsDir: false,
		Replacement: f.borrow(t, &restoreCalls),
	})
	if !errors.Is(err, store.ErrTrashFull) {
		t.Fatalf("want the trash cap to refuse the overwrite with ErrTrashFull, got %v", err)
	}

	// The policy: no destructive content call at all, and the caller's node put back exactly once.
	if len(f.content.deletes) != 0 {
		t.Fatalf("a borrowed stage was DELETED (paths %v). That is the data-loss defect: the "+
			"staged node is the caller's own source file, so it must be returned, never removed.",
			f.content.deletes)
	}
	if restoreCalls != 1 {
		t.Fatalf("restore calls = %d, want exactly 1: a failed install must return a borrowed node "+
			"(and only once)", restoreCalls)
	}

	// Both files are exactly as they were, under their original names.
	if got := readFile(t, f.content, f.owner, f.srcRel); got != "SOURCE important user data" {
		t.Fatalf("the source's bytes after a failed overwrite = %q; the source file is gone", got)
	}
	if got := readFile(t, f.content, f.owner, f.dstRel); got != "DEST existing user data" {
		t.Fatalf("the destination's bytes after a failed overwrite = %q", got)
	}

	var srcBack store.Node
	if err := f.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.ChildByName(ctx, f.parent.ID, "source.txt")
		srcBack = n
		return cerr
	}); err != nil {
		t.Fatalf("the source's metadata is not back at its original name: %v", err)
	}
	if srcBack.ID != f.source.ID {
		t.Fatalf("the restored source is a different node (%s, was %s); identity must be preserved",
			srcBack.ID, f.source.ID)
	}
	if srcBack.TrashedAt != nil {
		t.Fatal("the source node was trashed by a failed overwrite")
	}
}

// TestDisposableStageIsDeleted is the other half: a COPY's stage really is garbage after a failed
// overwrite, and leaving it would hold quota under a name the user never created.
func TestDisposableStageIsDeleted(t *testing.T) {
	f := newFixture(t)
	var stagedRel string

	_, err := swap.Install(context.Background(), deps(f, 1), swap.Request{
		Owner: f.owner, Parent: f.parent, ParentRel: "docs", DstName: "existing.txt",
		Existing: f.existing, StagedIsDir: false,
		Replacement: swap.Disposable(func(ctx context.Context, stagedName string) (store.Node, error) {
			stagedRel = path.Join("docs", stagedName)
			put(t, f.content, f.owner, stagedRel, "a duplicate")
			return createFile(t, f.meta, f.ownerID, f.parent.ID, stagedName, "a duplicate"), nil
		}),
	})
	if !errors.Is(err, store.ErrTrashFull) {
		t.Fatalf("want ErrTrashFull, got %v", err)
	}

	if len(f.content.deletes) != 1 || f.content.deletes[0] != stagedRel {
		t.Fatalf("disposable staged bytes were not removed: deletes = %v, want [%s]",
			f.content.deletes, stagedRel)
	}
	// The destination is untouched, which is the property the copy path already had.
	if got := readFile(t, f.content, f.owner, f.dstRel); got != "DEST existing user data" {
		t.Fatalf("the destination's bytes after a failed overwrite = %q", got)
	}
}

// TestAFailedPhysicalInstallReturnsTheBorrowedSource is the third named failure point.
//
// The trash cap fails inside the metadata transaction; an audit store fails while the source is being
// staged; this one fails while the staged bytes are being installed, before any metadata has changed.
// The rollback still has two jobs — put the displaced original's bytes back and return the borrowed
// source — and both must happen without deleting anything.
func TestAFailedPhysicalInstallReturnsTheBorrowedSource(t *testing.T) {
	f := newFixture(t)
	restoreCalls := 0

	// The install is the move FROM the reserved stage path; failing it leaves the destination path
	// empty with the original at the backup path, which is the state the compensation must repair.

	_, err := swap.Install(context.Background(), deps(f, 0), swap.Request{
		Owner: f.owner, Parent: f.parent, ParentRel: "docs", DstName: "existing.txt",
		Existing: f.existing, StagedIsDir: false,
		Replacement: swap.Borrowed(
			func(ctx context.Context, stagedName string) (store.Node, error) {
				// From here on, a move whose source is the staged path is the install.
				f.content.failFrom = path.Join("docs", stagedName)
				f.content.failErr = errors.New("injected storage fault on the install move")
				return relocateForTest(ctx, f, f.source, f.srcRel, stagedName)
			},
			func(ctx context.Context, staged store.Node) error {
				restoreCalls++
				f.content.failFrom = "" // compensation must be allowed to run
				_, rerr := relocateForTest(ctx, f, staged, path.Join("docs", staged.Name), f.source.Name)
				return rerr
			},
		),
	})
	if err == nil {
		t.Fatal("the injected storage fault did not fail the install")
	}

	if len(f.content.deletes) != 0 {
		t.Fatalf("a borrowed stage was DELETED on a physical failure: %v", f.content.deletes)
	}
	if restoreCalls != 1 {
		t.Fatalf("restore calls = %d, want 1", restoreCalls)
	}
	if got := readFile(t, f.content, f.owner, f.dstRel); got != "DEST existing user data" {
		t.Fatalf("the destination's bytes after a failed install = %q; the original must be put back",
			got)
	}
	if got := readFile(t, f.content, f.owner, f.srcRel); got != "SOURCE important user data" {
		t.Fatalf("the source's bytes after a failed install = %q", got)
	}
	if src := nodeAt(t, f.meta, f.source.ID); src.Name != "source.txt" || src.TrashedAt != nil {
		t.Fatalf("the source node is %q (trashed=%v), want its original name and a live row",
			src.Name, src.TrashedAt != nil)
	}
}

// TestAnUndeclaredStageIsRefused is the type-forcing half.
//
// The mistake this prevents is an omission: a caller that passes only a builder is silently treated
// as owning a disposable copy, and that is exactly how the MOVE lost its source. Refusing the zero
// value turns a future omission into a loud error instead of destroyed data.
func TestAnUndeclaredStageIsRefused(t *testing.T) {
	f := newFixture(t)

	_, err := swap.Install(context.Background(), deps(f, 0), swap.Request{
		Owner: f.owner, Parent: f.parent, ParentRel: "docs", DstName: "existing.txt",
		Existing: f.existing,
	})
	if err == nil {
		t.Fatal("a swap with no declared replacement was accepted; a caller must state whether the " +
			"stage is disposable or borrowed")
	}
	if f.content.moves != 0 || len(f.content.deletes) != 0 {
		t.Fatalf("a refused swap touched the content store (moves=%d deletes=%v)",
			f.content.moves, f.content.deletes)
	}
	if got := readFile(t, f.content, f.owner, f.dstRel); got != "DEST existing user data" {
		t.Fatalf("the destination changed: %q", got)
	}
}

// TestAnAbsoluteParentPathIsRefusedBeforeAnythingMoves pins the other pre-condition.
//
// The REST layer hands out "/docs/a.txt" spellings, and a caller that passed one straight through
// would fail only after the source had been relocated. Refusing up front keeps that mistake cheap.
func TestAnAbsoluteParentPathIsRefusedBeforeAnythingMoves(t *testing.T) {
	f := newFixture(t)
	restoreCalls := 0

	_, err := swap.Install(context.Background(), deps(f, 0), swap.Request{
		Owner: f.owner, Parent: f.parent, ParentRel: "/docs", DstName: "existing.txt",
		Existing: f.existing, StagedIsDir: false,
		Replacement: f.borrow(t, &restoreCalls),
	})
	if err == nil {
		t.Fatal("an absolute parent path was accepted")
	}
	if restoreCalls != 0 || f.content.moves != 0 || len(f.content.deletes) != 0 {
		t.Fatalf("the refusal happened after work began (moves=%d deletes=%v restores=%d)",
			f.content.moves, f.content.deletes, restoreCalls)
	}
	if got := readFile(t, f.content, f.owner, f.srcRel); got != "SOURCE important user data" {
		t.Fatalf("the source's bytes changed: %q", got)
	}
}
