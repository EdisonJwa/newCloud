// Package storetest is the conformance suite every metadata Store implementation
// must pass.
//
// The reason this exists: the in-memory implementation is used by the API layer in
// development and by every unit test, while PostgreSQL is used in production. If
// the two disagree, a test that passes locally proves nothing about production.
// Running one suite against both keeps them honest.
//
// It lives outside internal/store on purpose: a test in package store importing a
// suite that imports store is an import cycle, so the suite is a sibling package
// and the store's tests use the external _test package form.
package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// Factory returns a Store plus a fresh owner. It must also create the user row,
// because quota accounting and foreign keys need it.
type Factory func(t *testing.T) (s store.Store, owner uuid.UUID)

// Options selects the parts of the suite an implementation supports.

func mkdir(t *testing.T, s store.Store, owner, parent uuid.UUID, name string) store.Node {
	t.Helper()
	var out store.Node
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: parent, Name: name})
		out = n
		return err
	})
	if err != nil {
		t.Fatalf("mkdir %q: %v", name, err)
	}
	return out
}

var newBlobHook func(t *testing.T) uuid.UUID

func mkfile(t *testing.T, s store.Store, owner, parent uuid.UUID, name string, size int64) store.Node {
	t.Helper()
	var out store.Node
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: parent, Name: name,
			Size: size, Mime: "text/plain", ETag: "etag-1",
			BlobID: newBlobHook(t), BlobHash: [32]byte{1},
		})
		out = n
		return err
	})
	if err != nil {
		t.Fatalf("mkfile %q: %v", name, err)
	}
	return out
}

func rootOf(t *testing.T, s store.Store, owner uuid.UUID) store.Node {
	t.Helper()
	n, err := s.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	return n
}

// Options selects the parts of the suite an implementation supports.
type Options struct {
	// NewBlob creates a blob row and returns its id. The PostgreSQL schema has a
	// foreign key from nodes.blob_id to blobs.id, so a file cannot be created
	// with a fabricated id; the in-memory store does not care. Making this a hook
	// keeps the suite identical for both instead of weakening the constraint.
	NewBlob func(t *testing.T) uuid.UUID
	// Versions enables the version-history tests.
	Versions bool
	// AuditCount reports how many audit rows a store holds; used to verify that
	// a rolled-back transaction left nothing behind.
	AuditCount func(t *testing.T, s store.Store) int
	// SetQuota configures an owner's byte limit.
	SetQuota func(t *testing.T, s store.Store, owner uuid.UUID, limit int64)
}

// Run executes the suite.
func Run(t *testing.T, factory Factory, opts Options) {
	t.Helper()

	if opts.SetQuota == nil {
		t.Fatal("storetest: Options.SetQuota is required; quota tests would otherwise be skipped silently")
	}
	if opts.NewBlob == nil {
		t.Fatal("storetest: Options.NewBlob is required")
	}
	newBlobHook = opts.NewBlob

	t.Run("EnsureRootIsIdempotent", func(t *testing.T) {
		s, owner := factory(t)
		a := rootOf(t, s, owner)
		b := rootOf(t, s, owner)
		if a.ID != b.ID {
			t.Fatal("EnsureRoot must be idempotent")
		}
		if !a.IsDir() || a.ParentID != nil {
			t.Fatalf("root must be a parentless directory: %+v", a)
		}
	})

	t.Run("ResolvePaths", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		docs := mkdir(t, s, owner, r.ID, "docs")
		sub := mkdir(t, s, owner, docs.ID, "sub")
		file := mkfile(t, s, owner, sub.ID, "a.txt", 10)

		nodes, err := s.Resolve(context.Background(), owner, "docs/sub/a.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 4 {
			t.Fatalf("want root+3, got %d", len(nodes))
		}
		if nodes[len(nodes)-1].ID != file.ID {
			t.Fatal("resolved the wrong leaf")
		}
		if _, err := s.Resolve(context.Background(), owner, "/docs/sub/a.txt"); err != nil {
			t.Fatalf("leading slash should work: %v", err)
		}
		if _, err := s.Resolve(context.Background(), owner, ""); err != nil {
			t.Fatalf("empty path is the root: %v", err)
		}
		for _, p := range []string{"docs/missing", "nope", "docs/sub/a.txt/deeper"} {
			if _, err := s.Resolve(context.Background(), owner, p); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("%q: want ErrNotFound, got %v", p, err)
			}
		}
	})

	t.Run("ResolveIsCaseExact", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		mkfile(t, s, owner, r.ID, "Readme.md", 1)

		if _, err := s.Resolve(context.Background(), owner, "Readme.md"); err != nil {
			t.Fatalf("exact case must resolve: %v", err)
		}
		if _, err := s.Resolve(context.Background(), owner, "readme.md"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("different case must not resolve")
		}
	})

	t.Run("SiblingConflict", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		mkfile(t, s, owner, r.ID, "a.txt", 1)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "a.txt",
				Size: 1, BlobID: newBlobHook(t),
			})
			return err
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})

	t.Run("TrashedSiblingDoesNotBlockReuse", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		a := mkfile(t, s, owner, r.ID, "a.txt", 1)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, a.ID)
		}); err != nil {
			t.Fatal(err)
		}
		b := mkfile(t, s, owner, r.ID, "a.txt", 2)
		if b.ID == a.ID {
			t.Fatal("recreating a deleted name must make a new node")
		}
	})

	t.Run("InvalidNamesRejected", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		bad := []string{"", ".", "..", "a/b", "a\\b", "a\x00b", "CON", "report.", strings.Repeat("x", 256)}
		for _, name := range bad {
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.CreateDir(ctx, store.NewNode{OwnerID: owner, ParentID: r.ID, Name: name})
				return err
			})
			if !errors.Is(err, store.ErrInvalidName) {
				t.Fatalf("name %q: want ErrInvalidName, got %v", name, err)
			}
		}
	})

	t.Run("TrashIsRecursive", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		dir := mkdir(t, s, owner, r.ID, "d")
		sub := mkdir(t, s, owner, dir.ID, "sub")
		file := mkfile(t, s, owner, sub.ID, "deep.txt", 5)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, dir.ID)
		}); err != nil {
			t.Fatal(err)
		}

		// The whole subtree must be unreachable, not just the top directory.
		if _, err := s.Resolve(context.Background(), owner, "d"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("trashed directory should not resolve")
		}
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Node(ctx, file.ID)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("trashed descendant must be ErrNotFound, got %v", err)
		}
	})

	// TestRestoreIsRecursive is the regression guard for the limitation noted in
	// ADR-0004: restoring a directory without its children leaves it visibly
	// empty and the children unreachable forever.
	t.Run("RestoreIsRecursive", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		dir := mkdir(t, s, owner, r.ID, "d")
		sub := mkdir(t, s, owner, dir.ID, "sub")
		mkfile(t, s, owner, sub.ID, "deep.txt", 5)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, dir.ID)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Restore(ctx, dir.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		nodes, err := s.Resolve(context.Background(), owner, "d/sub/deep.txt")
		if err != nil {
			t.Fatalf("restored children must be reachable: %v", err)
		}
		if nodes[len(nodes)-1].Name != "deep.txt" {
			t.Fatal("resolved the wrong node after restore")
		}
	})

	t.Run("MoveIntoOwnSubtreeRejected", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		parent := mkdir(t, s, owner, r.ID, "parent")
		child := mkdir(t, s, owner, parent.ID, "child")

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Move(ctx, parent.ID, child.ID)
			return err
		})
		if !errors.Is(err, store.ErrCycle) {
			t.Fatalf("want ErrCycle, got %v", err)
		}
	})

	t.Run("MoveSiblingConflict", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		a := mkdir(t, s, owner, r.ID, "a")
		b := mkdir(t, s, owner, r.ID, "b")
		mkfile(t, s, owner, b.ID, "a", 1)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.Move(ctx, a.ID, b.ID)
			return err
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})

	t.Run("MoveAndRename", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		dir := mkdir(t, s, owner, r.ID, "dir")
		file := mkfile(t, s, owner, r.ID, "old.txt", 1)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.Rename(ctx, file.ID, "new.txt"); err != nil {
				return err
			}
			_, err := tx.Move(ctx, file.ID, dir.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Resolve(context.Background(), owner, "dir/new.txt"); err != nil {
			t.Fatalf("moved file should resolve: %v", err)
		}
		if _, err := s.Resolve(context.Background(), owner, "old.txt"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("old path must not resolve")
		}
	})

	t.Run("QuotaEnforcedOnCreate", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		opts.SetQuota(t, s, owner, 100)
		mkfile(t, s, owner, r.ID, "a.bin", 60)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "b.bin",
				Size: 60, BlobID: newBlobHook(t),
			})
			return err
		})
		if !errors.Is(err, store.ErrQuota) {
			t.Fatalf("want ErrQuota, got %v", err)
		}
		if used, _ := s.Usage(context.Background(), owner); used != 60 {
			t.Fatalf("rejected write must not consume quota, used=%d", used)
		}
	})

	// TestQuotaUsesDeltaOnOverwrite guards the classic accounting bug: charging
	// the new size instead of the difference makes repeated saves of a large file
	// hit the quota early.
	t.Run("QuotaUsesDeltaOnOverwrite", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		opts.SetQuota(t, s, owner, 100)
		file := mkfile(t, s, owner, r.ID, "a.bin", 80)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.UpdateFile(ctx, store.UpdateFile{
				NodeID: file.ID, Size: 80, BlobID: newBlobHook(t), ETag: "e2",
			})
			return err
		})
		if err != nil {
			t.Fatalf("same-size overwrite must not exceed quota: %v", err)
		}
		if used, _ := s.Usage(context.Background(), owner); used != 80 {
			t.Fatalf("usage should stay 80, got %d", used)
		}
	})

	t.Run("TransactionRollbackLeavesNoTrace", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		opts.SetQuota(t, s, owner, 50)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "ok.bin", Size: 10, BlobID: newBlobHook(t),
			}); err != nil {
				return err
			}
			if err := tx.Audit(ctx, store.AuditEntry{Action: "file.write", Result: "ok"}); err != nil {
				return err
			}
			_, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "too-big.bin", Size: 100, BlobID: newBlobHook(t),
			})
			return err
		})
		if !errors.Is(err, store.ErrQuota) {
			t.Fatalf("want ErrQuota, got %v", err)
		}

		if _, err := s.Resolve(context.Background(), owner, "ok.bin"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("rolled-back create must not be visible")
		}
		if used, _ := s.Usage(context.Background(), owner); used != 0 {
			t.Fatalf("rolled-back create must not consume quota, got %d", used)
		}
		if opts.AuditCount != nil {
			if n := opts.AuditCount(t, s); n != 0 {
				t.Fatalf("rolled-back operation left %d audit entries", n)
			}
		}
	})

	// TestConcurrentSameNameOnlyOneWins is the race the partial unique index
	// exists for: two simultaneous uploads of the same filename must not both
	// succeed.
	t.Run("ConcurrentSameNameOnlyOneWins", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		const n = 8

		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, err := tx.CreateFile(ctx, store.NewFile{
						OwnerID: owner, ParentID: r.ID, Name: "same.txt",
						Size: 1, BlobID: newBlobHook(t),
					})
					return err
				})
			}(i)
		}
		close(start)
		wg.Wait()

		ok, conflict := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, store.ErrConflict):
				conflict++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("exactly one create must win, got %d (conflicts=%d)", ok, conflict)
		}
	})

	t.Run("AuditIsRecorded", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "a.bin", Size: 1, BlobID: newBlobHook(t),
			}); err != nil {
				return err
			}
			return tx.Audit(ctx, store.AuditEntry{
				Action: "file.write", Result: "ok", TargetKind: "node", TargetID: "x",
			})
		}); err != nil {
			t.Fatal(err)
		}
		if opts.AuditCount != nil && opts.AuditCount(t, s) != 1 {
			t.Fatal("committed operation must leave exactly one audit entry")
		}
	})

	if opts.Versions {
		t.Run("Versions", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			file := mkfile(t, s, owner, r.ID, "d.txt", 10)

			for i := 0; i < 3; i++ {
				if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, err := tx.UpdateFile(ctx, store.UpdateFile{
						NodeID: file.ID, Size: 20, BlobID: newBlobHook(t),
						ETag: "e", KeepVersion: true, MaxVersions: 10,
					})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			var versions []store.Version
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				v, err := tx.Versions(ctx, file.ID, 0)
				versions = v
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(versions) != 3 {
				t.Fatalf("want 3 versions, got %d", len(versions))
			}
			for i := 1; i < len(versions); i++ {
				if versions[i-1].VersionNo <= versions[i].VersionNo {
					t.Fatal("versions must be newest-first")
				}
			}
		})

		t.Run("VersionsAreCapped", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			file := mkfile(t, s, owner, r.ID, "d.txt", 10)

			for i := 0; i < 5; i++ {
				if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, err := tx.UpdateFile(ctx, store.UpdateFile{
						NodeID: file.ID, Size: 20, BlobID: newBlobHook(t),
						ETag: "e", KeepVersion: true, MaxVersions: 2,
					})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			var versions []store.Version
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				v, err := tx.Versions(ctx, file.ID, 0)
				versions = v
				return err
			})
			if len(versions) != 2 {
				t.Fatalf("keep-last-2 must leave 2 versions, got %d", len(versions))
			}
		})
	}

	// Identity provisioning.
	//
	// These cases live in the shared suite because they were a real divergence: the
	// PostgreSQL path failed a pre-provisioned user's first login with a unique
	// violation while the in-memory path succeeded, so every test passed and the
	// deployed system returned 500. A behaviour both stores must share belongs here,
	// not in one store's own test file.
	t.Run("UpsertIdPUser", func(t *testing.T) {
		t.Run("ClaimsPreprovisionedAccount", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner) // ensure the owner exists in this store

			up := store.UpsertUser{
				Issuer: "https://idp.test", Subject: "subject-1",
				Username: "ivy", Email: "ivy@example.test", Role: "user",
			}
			first, err := upsertUser(s, up)
			if err != nil {
				t.Fatalf("first login: %v", err)
			}

			// A second login with the same identity returns the same account rather
			// than creating a duplicate.
			again, err := upsertUser(s, up)
			if err != nil {
				t.Fatalf("returning login: %v", err)
			}
			if again.ID != first.ID {
				t.Fatalf("returning login created a second account: %s vs %s", first.ID, again.ID)
			}
		})

		t.Run("BindsIdentityOnFirstLogin", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			// The account exists with the IdP subject already recorded, then a login
			// arrives: the email and display name are refreshed but the id is stable.
			up := store.UpsertUser{
				Issuer: "https://idp.test", Subject: "subject-2",
				Username: "jack", Email: "old@example.test", Role: "user",
			}
			created, err := upsertUser(s, up)
			if err != nil {
				t.Fatal(err)
			}

			up.Email = "new@example.test"
			up.DisplayName = "Jack Example"
			updated, err := upsertUser(s, up)
			if err != nil {
				t.Fatal(err)
			}
			if updated.ID != created.ID {
				t.Fatal("the account id must not change on login")
			}
			if updated.Email != "new@example.test" {
				t.Fatalf("email should follow the IdP claim, got %q", updated.Email)
			}
		})
	})
}

// upsertUser runs UpsertIdPUser in a transaction.
func upsertUser(s store.Store, in store.UpsertUser) (store.User, error) {
	var out store.User
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, in)
		out = u
		return uerr
	})
	return out, err
}
