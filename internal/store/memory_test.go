package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func newStore(t *testing.T) (*Memory, uuid.UUID, Node) {
	t.Helper()
	m := NewMemory()
	owner := uuid.New()
	root, err := m.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return m, owner, root
}

// mkdir and mkfile are small helpers so the tests read as scenarios.
func mkdir(t *testing.T, m *Memory, owner, parent uuid.UUID, name string) Node {
	t.Helper()
	var out Node
	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		n, err := tx.CreateDir(ctx, NewNode{OwnerID: owner, ParentID: parent, Name: name})
		out = n
		return err
	})
	if err != nil {
		t.Fatalf("mkdir %q: %v", name, err)
	}
	return out
}

func mkfile(t *testing.T, m *Memory, owner, parent uuid.UUID, name string, size int64) Node {
	t.Helper()
	var out Node
	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		n, err := tx.CreateFile(ctx, NewFile{
			OwnerID: owner, ParentID: parent, Name: name,
			Size: size, Mime: "text/plain", ETag: "etag-1",
			BlobID: uuid.New(), BlobHash: [32]byte{1},
		})
		out = n
		return err
	})
	if err != nil {
		t.Fatalf("mkfile %q: %v", name, err)
	}
	return out
}

func TestEnsureRootIsIdempotent(t *testing.T) {
	m := NewMemory()
	owner := uuid.New()

	a, err := m.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatal("EnsureRoot must be idempotent")
	}
	if !a.IsDir() {
		t.Fatal("root must be a directory")
	}
	if a.ParentID != nil {
		t.Fatal("root must have no parent")
	}
}

func TestResolvePaths(t *testing.T) {
	m, owner, root := newStore(t)
	docs := mkdir(t, m, owner, root.ID, "docs")
	sub := mkdir(t, m, owner, docs.ID, "sub")
	file := mkfile(t, m, owner, sub.ID, "a.txt", 10)

	nodes, err := m.Resolve(context.Background(), owner, "docs/sub/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 {
		t.Fatalf("want root+3, got %d", len(nodes))
	}
	if nodes[len(nodes)-1].ID != file.ID {
		t.Fatal("resolved the wrong leaf")
	}

	// Leading slash and absent are equivalent.
	if _, err := m.Resolve(context.Background(), owner, "/docs/sub/a.txt"); err != nil {
		t.Fatalf("leading slash should work: %v", err)
	}
	if _, err := m.Resolve(context.Background(), owner, ""); err != nil {
		t.Fatalf("empty path is the root: %v", err)
	}

	for _, p := range []string{"docs/missing", "nope", "docs/sub/a.txt/deeper"} {
		if _, err := m.Resolve(context.Background(), owner, p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q: want ErrNotFound, got %v", p, err)
		}
	}
}

// TestResolveIsCaseExact: a case-insensitive lookup that silently resolves
// "Readme.md" to "README.md" makes two distinct files collide on a
// case-sensitive client.
func TestResolveIsCaseExact(t *testing.T) {
	m, owner, root := newStore(t)
	mkfile(t, m, owner, root.ID, "Readme.md", 1)

	if _, err := m.Resolve(context.Background(), owner, "Readme.md"); err != nil {
		t.Fatalf("exact case must resolve: %v", err)
	}
	if _, err := m.Resolve(context.Background(), owner, "readme.md"); !errors.Is(err, ErrNotFound) {
		t.Fatal("different case must not resolve")
	}
}

func TestSiblingNameConflict(t *testing.T) {
	m, owner, root := newStore(t)
	mkfile(t, m, owner, root.ID, "a.txt", 1)

	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.CreateFile(ctx, NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "a.txt",
			Size: 1, BlobID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// TestTrashedSiblingDoesNotBlockReuse is the behaviour the partial unique index
// provides: after deleting a file you can create one with the same name.
func TestTrashedSiblingDoesNotBlockReuse(t *testing.T) {
	m, owner, root := newStore(t)
	a := mkfile(t, m, owner, root.ID, "a.txt", 1)

	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.Trash(ctx, a.ID)
	}); err != nil {
		t.Fatal(err)
	}

	b := mkfile(t, m, owner, root.ID, "a.txt", 2)
	if b.ID == a.ID {
		t.Fatal("should be a new node")
	}
}

func TestInvalidNamesRejected(t *testing.T) {
	m, owner, root := newStore(t)
	bad := []string{"", ".", "..", "a/b", "a\\b", "a\x00b", "CON", "report.", strings.Repeat("x", 256)}
	for _, name := range bad {
		err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
			_, err := tx.CreateDir(ctx, NewNode{OwnerID: owner, ParentID: root.ID, Name: name})
			return err
		})
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name %q: want ErrInvalidName, got %v", name, err)
		}
	}
}

func TestTrashIsRecursive(t *testing.T) {
	m, owner, root := newStore(t)
	dir := mkdir(t, m, owner, root.ID, "d")
	sub := mkdir(t, m, owner, dir.ID, "sub")
	file := mkfile(t, m, owner, sub.ID, "deep.txt", 5)

	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.Trash(ctx, dir.ID)
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []uuid.UUID{dir.ID, sub.ID, file.ID} {
		if _, err := m.Resolve(context.Background(), owner, "d"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("trashed subtree should be unreachable from %v", id)
		}
	}
	// A trashed node is invisible to Node() as well.
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.Node(ctx, file.ID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatal("trashed node must be ErrNotFound")
	}
}

func TestRestore(t *testing.T) {
	m, owner, root := newStore(t)
	file := mkfile(t, m, owner, root.ID, "f.txt", 1)

	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.Trash(ctx, file.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.Restore(ctx, file.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(context.Background(), owner, "f.txt"); err != nil {
		t.Fatalf("restored file should resolve: %v", err)
	}
}

// TestMoveIntoOwnSubtreeRejected guards against orphaning a subtree.
func TestMoveIntoOwnSubtreeRejected(t *testing.T) {
	m, owner, root := newStore(t)
	parent := mkdir(t, m, owner, root.ID, "parent")
	child := mkdir(t, m, owner, parent.ID, "child")

	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.Move(ctx, parent.ID, child.ID)
		return err
	})
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("want ErrCycle, got %v", err)
	}
}

func TestMoveSiblingConflict(t *testing.T) {
	m, owner, root := newStore(t)
	a := mkdir(t, m, owner, root.ID, "a")
	b := mkdir(t, m, owner, root.ID, "b")
	mkfile(t, m, owner, b.ID, "a", 1)

	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.Move(ctx, a.ID, b.ID)
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestRename(t *testing.T) {
	m, owner, root := newStore(t)
	file := mkfile(t, m, owner, root.ID, "old.txt", 1)

	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.Rename(ctx, file.ID, "new.txt")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(context.Background(), owner, "new.txt"); err != nil {
		t.Fatal("renamed file should resolve")
	}
	if _, err := m.Resolve(context.Background(), owner, "old.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old name must not resolve")
	}
}

func TestQuotaEnforcedOnCreate(t *testing.T) {
	m, owner, root := newStore(t)
	m.SetQuota(owner, 100)
	mkfile(t, m, owner, root.ID, "a.bin", 60)

	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.CreateFile(ctx, NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "b.bin",
			Size: 60, BlobID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("want ErrQuota, got %v", err)
	}

	used, err := m.Usage(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if used != 60 {
		t.Fatalf("rejected write must not consume quota, used=%d", used)
	}
}

// TestQuotaUsesDeltaOnOverwrite: re-uploading the same file must not consume
// quota twice. This is the bug class where quota accounting subtracts the new
// size instead of the difference.
func TestQuotaUsesDeltaOnOverwrite(t *testing.T) {
	m, owner, root := newStore(t)
	m.SetQuota(owner, 100)
	file := mkfile(t, m, owner, root.ID, "a.bin", 80)

	// Same size: delta is zero, so it must succeed even at 80/100.
	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		_, err := tx.UpdateFile(ctx, UpdateFile{
			NodeID: file.ID, Size: 80, BlobID: uuid.New(), ETag: "e2",
		})
		return err
	})
	if err != nil {
		t.Fatalf("same-size overwrite must not exceed quota: %v", err)
	}

	used, _ := m.Usage(context.Background(), owner)
	if used != 80 {
		t.Fatalf("usage should stay 80, got %d", used)
	}
}

func TestQuotaReleasesOnDelete(t *testing.T) {
	m, owner, root := newStore(t)
	m.SetQuota(owner, 100)
	file := mkfile(t, m, owner, root.ID, "a.bin", 80)

	// §4.3 defines the logical quota as the current version of user-VISIBLE files and lists trash
	// separately as retained usage. This test used to assert the opposite ("trash must not silently
	// free quota"), which was a decision made before the requirement's definition was applied: it
	// meant a user who emptied their bin was still told they were full.
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		return tx.Trash(ctx, file.ID)
	}); err != nil {
		t.Fatal(err)
	}
	used, _ := m.Usage(context.Background(), owner)
	if used != 0 {
		t.Fatalf("trashing must release the logical quota (the bytes stay held, and counted, as "+
			"retained usage), got %d", used)
	}
}

// TestTransactionRollbackLeavesNoTrace: a failed operation must not leave a
// half-created node, a consumed quota unit, or an audit entry claiming success.
func TestTransactionRollbackLeavesNoTrace(t *testing.T) {
	m, owner, root := newStore(t)
	m.SetQuota(owner, 50)

	err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		if _, err := tx.CreateFile(ctx, NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "ok.bin", Size: 10, BlobID: uuid.New(),
		}); err != nil {
			return err
		}
		if err := tx.Audit(ctx, AuditEntry{Action: "file.write", Result: "ok"}); err != nil {
			return err
		}
		// Second file exceeds the quota and aborts the whole transaction.
		_, err := tx.CreateFile(ctx, NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "too-big.bin", Size: 100, BlobID: uuid.New(),
		})
		return err
	})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("want ErrQuota, got %v", err)
	}

	if _, err := m.Resolve(context.Background(), owner, "ok.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rolled-back create must not be visible")
	}
	if used, _ := m.Usage(context.Background(), owner); used != 0 {
		t.Fatalf("rolled-back create must not consume quota, got %d", used)
	}
	if len(m.AuditLog()) != 0 {
		t.Fatal("rolled-back operation must not leave an audit entry")
	}
}

func TestAuditAndJobsAreTransactional(t *testing.T) {
	m, owner, root := newStore(t)

	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		if _, err := tx.CreateFile(ctx, NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "a.bin", Size: 1, BlobID: uuid.New(),
		}); err != nil {
			return err
		}
		if err := tx.Audit(ctx, AuditEntry{Action: "file.write", Result: "ok"}); err != nil {
			return err
		}
		return tx.Enqueue(ctx, Job{Kind: "scan", DedupeKey: "blob:1"})
	}); err != nil {
		t.Fatal(err)
	}

	if len(m.AuditLog()) != 1 {
		t.Fatalf("want 1 audit entry, got %d", len(m.AuditLog()))
	}
	if len(m.Jobs()) != 1 {
		t.Fatalf("want 1 job, got %d", len(m.Jobs()))
	}
}

func TestVersions(t *testing.T) {
	m, owner, root := newStore(t)
	file := mkfile(t, m, owner, root.ID, "d.txt", 10)

	for i := 0; i < 3; i++ {
		if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
			_, err := tx.UpdateFile(ctx, UpdateFile{
				NodeID: file.ID, Size: 20, BlobID: uuid.New(),
				ETag: "e", KeepVersion: true, MaxVersions: 10,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	var versions []Version
	if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		v, err := tx.Versions(ctx, file.ID, 0)
		versions = v
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("want 3 versions, got %d", len(versions))
	}
	// Newest first.
	if versions[0].VersionNo <= versions[len(versions)-1].VersionNo {
		t.Fatal("versions must be newest-first")
	}
}

func TestVersionsAreCapped(t *testing.T) {
	m, owner, root := newStore(t)
	file := mkfile(t, m, owner, root.ID, "d.txt", 10)

	for i := 0; i < 5; i++ {
		if err := m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
			_, err := tx.UpdateFile(ctx, UpdateFile{
				NodeID: file.ID, Size: 20, BlobID: uuid.New(),
				ETag: "e", KeepVersion: true, MaxVersions: 2,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	var versions []Version
	_ = m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
		v, err := tx.Versions(ctx, file.ID, 0)
		versions = v
		return err
	})
	if len(versions) != 2 {
		t.Fatalf("keep-last-2 must leave 2 versions, got %d", len(versions))
	}
}

// TestResolverRoundTrip proves the storage layer can read metadata written by
// the metadata layer, which is what makes the blob layout work at all.
func TestResolverRoundTrip(t *testing.T) {
	m, owner, root := newStore(t)
	docs := mkdir(t, m, owner, root.ID, "docs")
	mkfile(t, m, owner, docs.ID, "a.txt", 42)

	ownerStr := owner.String()
	res := m.Resolver()

	hash, ok, err := res.HashFor(context.Background(), ownerStr, "docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("resolver should find the file")
	}
	if hash == [32]byte{} {
		t.Fatal("resolver should return the content address")
	}

	isDir, found, err := res.IsDir(context.Background(), ownerStr, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !isDir {
		t.Fatal("docs should be a directory")
	}

	children, err := res.Children(context.Background(), ownerStr, "docs", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Path != "docs/a.txt" {
		t.Fatalf("children mismatch: %+v", children)
	}
}

func TestResolverRejectsBadOwner(t *testing.T) {
	m, _, _ := newStore(t)
	_, _, err := m.Resolver().HashFor(context.Background(), "not-a-uuid", "a.txt")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("want ErrInvalidName, got %v", err)
	}
}

func TestConcurrentCreatesAreSerialized(t *testing.T) {
	m, owner, root := newStore(t)
	const n = 20

	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			errs <- m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
				_, err := tx.CreateFile(ctx, NewFile{
					OwnerID: owner, ParentID: root.ID,
					Name: "f" + string(rune('a'+i)) + ".txt",
					Size: 1, BlobID: uuid.New(),
				})
				return err
			})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	used, _ := m.Usage(context.Background(), owner)
	if used != n {
		t.Fatalf("want %d bytes used, got %d", n, used)
	}
}

// TestConcurrentSameNameOnlyOneWins is the race the partial unique index
// exists for: two simultaneous uploads of the same filename must not both
// succeed.
func TestConcurrentSameNameOnlyOneWins(t *testing.T) {
	m, owner, root := newStore(t)
	const n = 8

	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			errs <- m.WithTx(context.Background(), func(ctx context.Context, tx Tx) error {
				_, err := tx.CreateFile(ctx, NewFile{
					OwnerID: owner, ParentID: root.ID, Name: "same.txt",
					Size: 1, BlobID: uuid.New(),
				})
				return err
			})
		}()
	}

	ok, conflict := 0, 0
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one create must win, got %d", ok)
	}
	if conflict != n-1 {
		t.Fatalf("others must conflict, got %d", conflict)
	}
}
