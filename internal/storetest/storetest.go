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
	"time"

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
	// MemoryBlobs marks a backend that does not model blob rows at all, so the blob-identity
	// cases are skipped there. Stated explicitly rather than inferred: a backend that lost blob
	// tracking by accident would otherwise look like one that never had it.
	MemoryBlobs bool
	// Versions enables the version-history tests.
	Versions bool
	// AuditCount reports how many audit rows a store holds; used to verify that
	// a rolled-back transaction left nothing behind.
	AuditCount func(t *testing.T, s store.Store) int
	// SetQuota configures an owner's byte limit.
	SetQuota func(t *testing.T, s store.Store, owner uuid.UUID, limit int64)
	// BlobIDFor maps a content address to its blob row id. Both backends must
	// return the same id for the same address across transactions, or identical
	// content is stored twice (INV-07). Required: a backend that silently stops
	// deduplicating is the kind of defect this suite exists to catch.
	BlobIDFor func(t *testing.T, s store.Store, hash [32]byte) uuid.UUID
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

	// Conditional writes.
	//
	// These live in the shared suite because INV-02 is a property of the data
	// layer, not of one handler: if the in-memory store ignores a precondition
	// that PostgreSQL enforces, every test of the browser upload path passes while
	// the deployed system silently loses writes.
	t.Run("ConditionalWrites", func(t *testing.T) {
		t.Run("IfMatchRejectsStaleTag", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			first := mkfile(t, s, owner, r.ID, "a.txt", 1)

			// A writer that has seen the current version succeeds.
			var updated store.Node
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, _, werr := tx.WriteFile(ctx, store.WriteFile{
					NewFile: store.NewFile{
						OwnerID: owner, ParentID: r.ID, Name: "a.txt",
						Size: 2, ETag: "etag-2", BlobID: newBlobHook(t),
					},
					Expect: &store.Precondition{IfMatch: first.ETag},
				})
				updated = n
				return werr
			})
			if err != nil {
				t.Fatalf("matching If-Match must succeed: %v", err)
			}
			if updated.ID != first.ID {
				t.Fatal("WriteFile over an existing name must keep the node identity")
			}

			// A writer holding the old tag is refused rather than winning.
			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, werr := tx.WriteFile(ctx, store.WriteFile{
					NewFile: store.NewFile{
						OwnerID: owner, ParentID: r.ID, Name: "a.txt",
						Size: 3, ETag: "etag-3", BlobID: newBlobHook(t),
					},
					Expect: &store.Precondition{IfMatch: first.ETag},
				})
				return werr
			})
			if !errors.Is(err, store.ErrPrecondition) {
				t.Fatalf("stale If-Match: want ErrPrecondition, got %v", err)
			}
		})

		t.Run("IfNoneMatchStarIsCreateOnly", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, werr := tx.WriteFile(ctx, store.WriteFile{
					NewFile: store.NewFile{
						OwnerID: owner, ParentID: r.ID, Name: "new.txt",
						Size: 1, ETag: "e1", BlobID: newBlobHook(t),
					},
					Expect: &store.Precondition{IfNoneMatch: "*"},
				})
				return werr
			})
			if err != nil {
				t.Fatalf("create-only on a free name must succeed: %v", err)
			}

			err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, werr := tx.WriteFile(ctx, store.WriteFile{
					NewFile: store.NewFile{
						OwnerID: owner, ParentID: r.ID, Name: "new.txt",
						Size: 2, ETag: "e2", BlobID: newBlobHook(t),
					},
					Expect: &store.Precondition{IfNoneMatch: "*"},
				})
				return werr
			})
			if !errors.Is(err, store.ErrPrecondition) {
				t.Fatalf("create-only on a taken name: want ErrPrecondition, got %v", err)
			}
		})
	})

	// Download counting.
	//
	// T-08's invariant: Range requests for one download must not each consume the
	// allowance, and two visitors must not share one allowance. Both halves belong
	// in the shared suite because the counter is a property of the store.
	t.Run("DownloadSessions", func(t *testing.T) {
		newShare := func(t *testing.T, s store.Store, owner uuid.UUID, nodeID uuid.UUID,
			max int) store.Share {
			var sh store.Share
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				c, cerr := tx.CreateShare(ctx, store.Share{
					NodeID: nodeID, OwnerID: owner, Kind: store.ShareLink,
					TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
					MaxDownloads: &max,
				})
				sh = c
				return cerr
			})
			if err != nil {
				t.Fatalf("create share: %v", err)
			}
			return sh
		}

		t.Run("RangeRequestsShareOneCharge", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			file := mkfile(t, s, owner, r.ID, "v.mp4", 10)
			sh := newShare(t, s, owner, file.ID, 5)

			grant := []byte("visitor-a")
			for i := 0; i < 6; i++ {
				if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
						ShareID: sh.ID, NodeID: file.ID, GrantHash: grant, ETag: file.ETag,
					}, true)
					return err
				}); err != nil {
					t.Fatalf("range request %d must ride on the existing session: %v", i, err)
				}
			}

			var got store.Share
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				found, err := tx.ShareByID(ctx, sh.ID)
				got = found
				return err
			})
			if got.DownloadCount != 1 {
				t.Fatalf("six range requests are one download, count=%d", got.DownloadCount)
			}
		})

		t.Run("DistinctVisitorsPaySeparately", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			file := mkfile(t, s, owner, r.ID, "v.mp4", 10)
			sh := newShare(t, s, owner, file.ID, 5)

			for _, g := range []string{"visitor-a", "visitor-b"} {
				if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
						ShareID: sh.ID, NodeID: file.ID, GrantHash: []byte(g), ETag: file.ETag,
					}, false)
					return err
				}); err != nil {
					t.Fatalf("visitor %s: %v", g, err)
				}
			}

			var got store.Share
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				found, err := tx.ShareByID(ctx, sh.ID)
				got = found
				return err
			})
			if got.DownloadCount != 2 {
				t.Fatalf("two visitors are two downloads, count=%d", got.DownloadCount)
			}
		})

		t.Run("CapIsEnforcedAcrossSessions", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			file := mkfile(t, s, owner, r.ID, "v.mp4", 10)
			sh := newShare(t, s, owner, file.ID, 1)

			first := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
					ShareID: sh.ID, NodeID: file.ID, GrantHash: []byte("a"), ETag: file.ETag,
				}, false)
				return err
			})
			if first != nil {
				t.Fatalf("first download must be allowed: %v", first)
			}

			second := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
					ShareID: sh.ID, NodeID: file.ID, GrantHash: []byte("b"), ETag: file.ETag,
				}, false)
				return err
			})
			if !errors.Is(second, store.ErrShareExhausted) {
				t.Fatalf("second visitor past the cap: want ErrShareExhausted, got %v", second)
			}
		})

		t.Run("NewVersionRequiresNewCharge", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			file := mkfile(t, s, owner, r.ID, "v.mp4", 10)
			sh := newShare(t, s, owner, file.ID, 5)

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
					ShareID: sh.ID, NodeID: file.ID, GrantHash: []byte("a"), ETag: "etag-old",
				}, false)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// A continuation against different bytes is not the same download: the
			// visitor was shown one version and must not receive another for free.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
					ShareID: sh.ID, NodeID: file.ID, GrantHash: []byte("a"), ETag: "etag-new",
				}, true)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var got store.Share
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				found, err := tx.ShareByID(ctx, sh.ID)
				got = found
				return err
			})
			if got.DownloadCount != 2 {
				t.Fatalf("a new version is a new download, count=%d", got.DownloadCount)
			}
		})
	})

	// SHARE-05 速率: the upload rate limit. On both backends, because the PostgreSQL version is a
	// single statement with an advisory lock while the in-memory one counts a slice, and the
	// server tests exercise only the memory backend.
	t.Run("ShareRate", func(t *testing.T) {
		shareRateSuite(t, factory)
	})

	t.Run("Retention", func(t *testing.T) {
		retentionSuite(t, factory)
	})

	t.Run("OTP", func(t *testing.T) {
		otpSuite(t, factory)
	})

	t.Run("VersionLifecycle", func(t *testing.T) {
		versionLifecycleSuite(t, factory)
	})

	t.Run("DeadProps", func(t *testing.T) {
		deadPropSuite(t, factory)
	})

	t.Run("Locks", func(t *testing.T) {
		lockSuite(t, factory)
	})

	t.Run("Groups", func(t *testing.T) {
		groupSuite(t, factory)
	})

	t.Run("Credentials", func(t *testing.T) {
		credentialSuite(t, factory)
	})

	t.Run("UserLookup", func(t *testing.T) {
		userLookupSuite(t, factory)
	})

	t.Run("ShareQueries", func(t *testing.T) {
		shareQuerySuite(t, factory)
	})

	t.Run("TreeWalk", func(t *testing.T) {
		treeWalkSuite(t, factory)
	})

	t.Run("RecipientAccess", func(t *testing.T) {
		recipientAccessSuite(t, factory)
	})

	t.Run("BlobCollection", func(t *testing.T) {
		// CollectableBlobs and DeleteBlob. The PostgreSQL cases live in internal/store
		// (TestPGCollector*), where the clock can be moved and the real query runs; what belongs
		// here is the contract BOTH backends must satisfy: a backend that does not model blob
		// lifecycle reports ErrNotImplemented rather than an empty list.
		//
		// That distinction is the whole point. An empty list means "nothing to collect"; an error
		// means "cannot answer". A sweep that receives the first reports a clean run on a backend
		// that never looked, which is what this method did for several rounds while its comment
		// claimed otherwise.

		if opts.MemoryBlobs {
			t.Run("UnmodelledBackendReportsTheGap", func(t *testing.T) {
				s, owner := factory(t)
				rootOf(t, s, owner)

				err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, cerr := tx.CollectableBlobs(ctx, 0)
					return cerr
				})
				if !errors.Is(err, store.ErrNotImplemented) {
					t.Fatalf("a backend that does not model blob lifecycle returned %v, want "+
						"ErrNotImplemented. Returning an empty list would let the sweep report a "+
						"clean run on a backend that never looked.", err)
				}
			})
			return
		}

		t.Run("CollectsNothingWhileReferenced", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)

			var hash [32]byte
			hash[0] = 0x5a
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.CreateFile(ctx, store.NewFile{
					OwnerID: owner, ParentID: r.ID, Name: "held.bin", Size: 1,
					Mime: "application/octet-stream", ETag: `"h"`,
					BlobID: s.BlobIDFor(ctx, hash, 1), BlobHash: hash,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var refs []store.BlobRef
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.CollectableBlobs(ctx, 0)
				refs = got
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, ref := range refs {
				// A blob a live file references must never be a candidate: collecting it would
				// delete content the database still points at.
				if ref.Size == 1 {
					t.Fatalf("a referenced blob appeared as a collection candidate")
				}
			}
		})

		t.Run("DeleteBlobRemovesOnlyItsRow", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)

			var keep, drop [32]byte
			keep[0], drop[0] = 0x11, 0x22
			keepID := s.BlobIDFor(context.Background(), keep, 1)
			dropID := s.BlobIDFor(context.Background(), drop, 1)
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.CreateFile(ctx, store.NewFile{
					OwnerID: owner, ParentID: r.ID, Name: "keep.bin", Size: 1,
					Mime: "application/octet-stream", ETag: `"k"`, BlobID: keepID, BlobHash: keep,
				}); err != nil {
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// Delete the unreferenced one; the referenced one must stay resolvable, or a later
			// restore of it would fail with a missing row.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.DeleteBlob(ctx, dropID)
			}); err != nil {
				t.Fatalf("DeleteBlob: %v", err)
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.BlobHash(ctx, keepID)
				return err
			}); err != nil {
				t.Fatalf("deleting one blob disturbed another: %v", err)
			}
		})
	})

	t.Run("ShareCounters", func(t *testing.T) {
		counterSuite(t, factory)
	})

	t.Run("Sweeps", func(t *testing.T) {
		sweepSuite(t, factory)
	})

	t.Run("PasswordThrottle", func(t *testing.T) {
		passwordThrottleSuite(t, factory)
	})

	t.Run("Usage", func(t *testing.T) {
		usageSuite(t, factory, opts.SetQuota)
	})

	t.Run("BlobIdentity", func(t *testing.T) {
		// hasBlobs is false for a backend that does not model blob rows (the in-memory store),
		// because asserting PostgreSQL semantics there would only document the limitation.
		// The PostgreSQL conformance run is where these cases run.
		blobIdentitySuite(t, factory, opts.NewBlob != nil && !opts.MemoryBlobs)
	})

	// Blob deduplication across transactions (INV-07 / T-20).
	//
	// BlobIDFor maps a content address to one row id. If that mapping is not stable across
	// transactions, identical content gets a new row every time and the blob layout silently
	// stops deduplicating: every upload consumes its full size, and "copy" doubles disk usage.
	//
	// This was a real defect in the in-memory backend — its snapshot() declared the dedup map in
	// the clone but never copied the entries, so each transaction began empty and replaced the
	// real index on commit. Within a single transaction it looked correct, which is why only a
	// cross-transaction test finds it.
	t.Run("BlobDedupAcrossTransactions", func(t *testing.T) {
		s, ownerID := factory(t)
		rootOf(t, s, ownerID)

		// The same content address resolved in two separate transactions must give one id.
		h := [32]byte{0xab}
		first := opts.BlobIDFor(t, s, h)
		second := opts.BlobIDFor(t, s, h)
		if first == uuid.Nil {
			t.Fatal("BlobIDFor must return an id")
		}
		if first != second {
			t.Fatalf("the dedup index must be stable across transactions: got %v then %v; "+
				"identical content is being stored twice", first, second)
		}

		// And a distinct address gets a distinct id, so the test cannot pass by returning a
		// constant.
		other := opts.BlobIDFor(t, s, [32]byte{0xcd})
		if other == first {
			t.Fatal("different content must not share an id")
		}
	})
	// Cursor pagination (T-19).
	//
	// The property that matters is not "pages are the right size" but "walking the
	// pages sees every row exactly once even while the directory changes". An
	// offset-based implementation passes a static test and fails this one.
	t.Run("ChildrenAfterPaginatesStably", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		var names []string
		for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
			mkfile(t, s, owner, r.ID, n, 1)
			names = append(names, n)
		}

		seen := map[string]int{}
		cursor := ""
		for page := 0; page < 10; page++ {
			var got []store.Node
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				nodes, cerr := tx.ChildrenAfter(ctx, r.ID, cursor, 2)
				got = nodes
				return cerr
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				break
			}
			for _, n := range got {
				seen[n.Name]++
			}
			cursor = got[len(got)-1].Name

			// A sibling created mid-walk must not make an already-seen row reappear.
			if page == 0 {
				mkfile(t, s, owner, r.ID, "bb.txt", 1)
			}
		}

		for _, n := range names {
			if seen[n] != 1 {
				t.Fatalf("%q was seen %d times while paging", n, seen[n])
			}
		}
	})

	t.Run("ChildrenAfterHonoursTheCursor", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		for _, n := range []string{"a", "b", "c", "d"} {
			mkfile(t, s, owner, r.ID, n, 1)
		}

		var got []store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			nodes, cerr := tx.ChildrenAfter(ctx, r.ID, "b", 100)
			got = nodes
			return cerr
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Name != "c" || got[1].Name != "d" {
			t.Fatalf("rows after %q should be c,d; got %v", "b", namesOf(got))
		}
	})

	// The cursor contract under concurrent mutation (T-19).
	//
	// A name-sorted cursor makes a precise promise and an honest limitation, and both must be
	// stated or nobody can reason about them:
	//
	//   - INSERTED rows are never skipped. One that sorts after the cursor appears later in the
	//     walk; one that sorts before it is simply not seen until the next full listing.
	//   - DELETED rows vanish, which cannot lose anything.
	//   - RENAMED rows are the honest limitation: renaming a not-yet-seen row to a name BEFORE
	//     the cursor hides it for the rest of this walk. It is still there and the next listing
	//     shows it. An offset cursor would instead duplicate rows, which is worse — a duplicate
	//     can make a client act twice.
	//
	// These tests pin all three so the behaviour is a decision rather than an accident.
	t.Run("CursorContractUnderMutation", func(t *testing.T) {
		// walkPages walks to the end, applying mutate after the first page.
		walkPages := func(t *testing.T, s store.Store, rootID uuid.UUID, limit int,
			mutate func()) (seen []string, dupes int) {

			t.Helper()
			counts := map[string]int{}
			cursor := ""
			for page := 0; page < 20; page++ {
				var got []store.Node
				if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					nodes, cerr := tx.ChildrenAfter(ctx, rootID, cursor, limit)
					got = nodes
					return cerr
				}); err != nil {
					t.Fatal(err)
				}
				if len(got) == 0 {
					break
				}
				for _, n := range got {
					counts[n.Name]++
					seen = append(seen, n.Name)
				}
				cursor = got[len(got)-1].Name
				if page == 0 && mutate != nil {
					mutate()
				}
			}
			for _, c := range counts {
				if c > 1 {
					dupes++
				}
			}
			return seen, dupes
		}

		t.Run("InsertAfterCursorIsSeen", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			for _, n := range []string{"a", "c", "e"} {
				mkfile(t, s, owner, r.ID, n, 1)
			}

			seen, dupes := walkPages(t, s, r.ID, 2, func() {
				// Sorts after the cursor ("c"): must appear on a later page.
				mkfile(t, s, owner, r.ID, "d", 1)
			})
			if dupes != 0 {
				t.Fatalf("a name cursor must not duplicate rows, got %d duplicates in %v", dupes, seen)
			}
			if !contains(seen, "d") {
				t.Fatalf("a row inserted after the cursor must be seen later in the walk, saw %v", seen)
			}
		})

		t.Run("InsertBeforeCursorIsNotDuplicated", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			for _, n := range []string{"a", "c", "e"} {
				mkfile(t, s, owner, r.ID, n, 1)
			}

			// Sorts before the cursor: not seen this walk, and crucially not a duplicate.
			seen, dupes := walkPages(t, s, r.ID, 2, func() {
				mkfile(t, s, owner, r.ID, "bb", 1)
			})
			if dupes != 0 {
				t.Fatalf("no row may be returned twice, got duplicates in %v", seen)
			}
			// The rows that existed before the walk are all present exactly once.
			for _, n := range []string{"a", "c", "e"} {
				count := 0
				for _, got := range seen {
					if got == n {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("%q appeared %d times, want exactly 1 (saw %v)", n, count, seen)
				}
			}
		})

		t.Run("DeleteDoesNotBreakTheWalk", func(t *testing.T) {
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			for _, n := range []string{"a", "c", "e", "g"} {
				mkfile(t, s, owner, r.ID, n, 1)
			}

			seen, dupes := walkPages(t, s, r.ID, 2, func() {
				// Delete a row that has already been seen, and one that has not.
				for _, n := range []string{"a", "e"} {
					_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
						nodes, cerr := tx.Children(ctx, r.ID, 100)
						if cerr != nil {
							return cerr
						}
						for _, node := range nodes {
							if node.Name == n {
								return tx.Trash(ctx, node.ID)
							}
						}
						return nil
					})
				}
			})
			if dupes != 0 {
				t.Fatalf("a deletion must not cause duplicates, got %v", seen)
			}
			// The survivors are still all reachable.
			for _, n := range []string{"c", "g"} {
				count := 0
				for _, got := range seen {
					if got == n {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("%q appeared %d times after a concurrent delete, want 1 (saw %v)",
						n, count, seen)
				}
			}
		})

		t.Run("RenameAcrossCursorIsTheDocumentedLimitation", func(t *testing.T) {
			// This pins the limitation so it cannot change silently in either direction.
			//
			// Renaming a not-yet-seen row to a name before the cursor hides it for the rest of
			// this walk. The row is not lost — the next listing shows it — but a client walking
			// once will not see it. Accepting that is the trade for never duplicating: a
			// duplicate can make a client act twice, which is worse than a row it reads next
			// time. If this test ever fails because the behaviour improved, that is good news
			// and the contract document needs updating with it.
			s, owner := factory(t)
			r := rootOf(t, s, owner)
			for _, n := range []string{"a", "c", "e"} {
				mkfile(t, s, owner, r.ID, n, 1)
			}

			seen, dupes := walkPages(t, s, r.ID, 2, func() {
				// "e" has not been seen yet; rename it to "b", which sorts before the cursor.
				_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					nodes, cerr := tx.Children(ctx, r.ID, 100)
					if cerr != nil {
						return cerr
					}
					for _, node := range nodes {
						if node.Name == "e" {
							_, rerr := tx.Rename(ctx, node.ID, "b")
							return rerr
						}
					}
					return nil
				})
			})

			if dupes != 0 {
				t.Fatalf("no duplicate may be produced, got %v", seen)
			}
			if contains(seen, "b") {
				t.Fatal("the documented limitation changed: a row renamed behind the cursor is now seen; " +
					"update the contract in docs/adr/0012-cursor-contract.md and this test together")
			}
			// But it is not lost: a fresh listing finds it.
			var found bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				nodes, cerr := tx.Children(ctx, r.ID, 100)
				for _, node := range nodes {
					if node.Name == "b" {
						found = true
					}
				}
				return cerr
			})
			if !found {
				t.Fatal("the renamed row must still exist and be visible in a fresh listing")
			}
		})
	})
	// Role changes (T-02).
	//
	// The requirement is that removing somebody from the admin group must actually
	// demote them. Storing the new role is not enough: the session and the app
	// password they already hold are what keep the old privilege alive, and both must
	// stop working.
	t.Run("ApplyIdPRole", func(t *testing.T) {
		t.Run("DowngradeRevokesCredentials", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			// Promote to admin, then mint the credentials an admin would hold.
			var promoted store.RoleChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				rc, err := tx.ApplyIdPRole(ctx, owner, "admin")
				promoted = rc
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !promoted.Changed || promoted.Downgraded {
				t.Fatalf("promotion should be a change and not a downgrade: %+v", promoted)
			}

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if err := tx.CreateSession(ctx, store.Session{
					ID: uuid.NewString(), UserID: owner,
					ExpiresAt: time.Now().Add(time.Hour), AuthMethod: "oidc",
				}); err != nil {
					return err
				}
				return tx.CreateAppPassword(ctx, store.AppPassword{
					ID: uuid.New(), UserID: owner, Label: "dav",
					TokenHash: []byte(uuid.NewString()), Scope: "rw",
				})
			}); err != nil {
				t.Fatal(err)
			}

			// Now remove the admin group.
			var demoted store.RoleChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				rc, err := tx.ApplyIdPRole(ctx, owner, "user")
				demoted = rc
				return err
			}); err != nil {
				t.Fatal(err)
			}

			if !demoted.Changed || !demoted.Downgraded {
				t.Fatalf("removal should be a downgrade: %+v", demoted)
			}
			if demoted.SessionsRevoked != 1 {
				t.Fatalf("want 1 session revoked, got %d", demoted.SessionsRevoked)
			}
			if demoted.AppPasswordsRevoked != 1 {
				t.Fatalf("want 1 app password revoked, got %d", demoted.AppPasswordsRevoked)
			}

			var u store.User
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.UserByID(ctx, owner)
				u = got
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if u.Role != "user" {
				t.Fatalf("stored role should be user, got %q", u.Role)
			}

			var live []store.AppPassword
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.ListAppPasswords(ctx, owner)
				live = got
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, p := range live {
				if p.RevokedAt == nil {
					t.Fatal("a credential minted as admin must not survive the downgrade")
				}
			}
		})

		t.Run("UpgradeKeepsCredentials", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CreateAppPassword(ctx, store.AppPassword{
					ID: uuid.New(), UserID: owner, Label: "dav",
					TokenHash: []byte(uuid.NewString()), Scope: "rw",
				})
			}); err != nil {
				t.Fatal(err)
			}

			var rc store.RoleChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.ApplyIdPRole(ctx, owner, "admin")
				rc = got
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !rc.Changed || rc.Downgraded {
				t.Fatalf("a promotion is not a downgrade: %+v", rc)
			}
			// Signing a user out because they were granted more access would be a
			// self-inflicted outage.
			if rc.SessionsRevoked != 0 || rc.AppPasswordsRevoked != 0 {
				t.Fatalf("a promotion must not revoke anything: %+v", rc)
			}
		})

		t.Run("SameRoleIsANoOp", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			// Establish a known starting role rather than assuming the factory's
			// default: the two backends create accounts with different defaults, and
			// a test that depends on which one it got is testing the fixture.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ApplyIdPRole(ctx, owner, "user")
				return err
			}); err != nil {
				t.Fatal(err)
			}

			// Claiming the role the account already has must not count as a change,
			// or every single login would write a role-change event and sign the
			// user out.
			var rc store.RoleChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.ApplyIdPRole(ctx, owner, "user")
				rc = got
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if rc.Changed {
				t.Fatalf("the same role is not a change: %+v", rc)
			}
		})
	})

	// Resource ACL (INV-01, §4.1 layer 2).
	//
	// These run against both backends because the ancestor walk is written twice: a
	// recursive CTE in SQL and a loop in Go. If they disagree, a grant works in one
	// deployment and silently fails in another — which is exactly the class of bug
	// this shared suite exists to catch.
	t.Run("ResourceACL", func(t *testing.T) {
		// setup builds owner -> folder -> subfolder and returns the ids.
		setup := func(t *testing.T) (store.Store, store.User, store.Node, store.Node, store.Node) {
			s, ownerID := factory(t)
			r := rootOf(t, s, ownerID)
			folder := mkdir(t, s, ownerID, r.ID, "team")
			sub := mkdir(t, s, ownerID, folder.ID, "private")
			return s, store.User{ID: ownerID}, r, folder, sub
		}

		t.Run("DirectUserGrantApplies", func(t *testing.T) {
			s, _, _, folder, _ := setup(t)
			mate := addUser(t, s, "mate")

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID,
					Perms: store.PermRead | store.PermListChildren, Inherit: true,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var perms store.Perm
			var ok bool
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, found, err := tx.EffectivePermsFor(ctx, mate.ID, folder.ID)
				perms, ok = p, found
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !ok || perms&store.PermRead == 0 || perms&store.PermListChildren == 0 {
				t.Fatalf("want read+list for the granted user, got %v (found=%v)", store.EncodePerms(perms), ok)
			}
			// The grant must not smuggle in write access.
			if perms&store.PermWrite != 0 || perms&store.PermDelete != 0 {
				t.Fatalf("a read grant must not confer write or delete: %v", store.EncodePerms(perms))
			}
		})

		t.Run("InheritanceReachesDescendants", func(t *testing.T) {
			s, _, _, folder, sub := setup(t)
			mate := addUser(t, s, "mate")

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID,
					Perms: store.PermRead | store.PermWrite, Inherit: true,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var perms store.Perm
			var ok bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, found, err := tx.EffectivePermsFor(ctx, mate.ID, sub.ID)
				perms, ok = p, found
				return err
			})
			if !ok || perms&store.PermRead == 0 {
				t.Fatalf("an inheriting grant must reach the subfolder, got %v", store.EncodePerms(perms))
			}
		})

		t.Run("NonInheritingGrantStopsAtItsNode", func(t *testing.T) {
			s, _, _, folder, sub := setup(t)
			mate := addUser(t, s, "mate")

			// "private" is granted directly, and the grant on the parent does not
			// inherit — which is how a protected subfolder keeps its boundary.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID,
					Perms: store.PermRead | store.PermWrite, Inherit: false,
				}); err != nil {
					return err
				}
				_, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: sub.ID, UserID: &mate.ID,
					Perms: store.PermRead, Inherit: false,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var perms store.Perm
			var ok bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, found, err := tx.EffectivePermsFor(ctx, mate.ID, sub.ID)
				perms, ok = p, found
				return err
			})
			if !ok {
				t.Fatal("the direct grant on the subfolder must still apply")
			}
			if perms&store.PermWrite != 0 {
				t.Fatalf("the non-inheriting parent grant must not confer write here: %v", store.EncodePerms(perms))
			}
		})

		t.Run("GroupGrantAppliesToMembersOnly", func(t *testing.T) {
			s, owner, folder, _, _ := setup(t)
			mate := addUser(t, s, "mate")
			outsider := addUser(t, s, "outsider")
			_ = owner

			var group store.Group
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				g, err := tx.CreateGroup(ctx, store.Group{Name: "engineering"})
				if err != nil {
					return err
				}
				group = g
				if err := tx.AddGroupMember(ctx, g.ID, mate.ID, "editor"); err != nil {
					return err
				}
				_, err = tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, GroupID: &g.ID,
					Perms: store.PermRead | store.PermWrite, Inherit: true,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_ = group

			var matePerms, outsiderPerms store.Perm
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, _, err := tx.EffectivePermsFor(ctx, mate.ID, folder.ID)
				matePerms = p
				return err
			})
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, _, err := tx.EffectivePermsFor(ctx, outsider.ID, folder.ID)
				outsiderPerms = p
				return err
			})

			if matePerms&store.PermRead == 0 {
				t.Fatalf("a group member must inherit the group grant, got %v", store.EncodePerms(matePerms))
			}
			if outsiderPerms != 0 {
				t.Fatalf("a non-member must get nothing from a group grant, got %v", store.EncodePerms(outsiderPerms))
			}
		})

		t.Run("GrantIsReplacedNotDuplicated", func(t *testing.T) {
			s, _, _, folder, _ := setup(t)
			mate := addUser(t, s, "mate")

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID,
					Perms: store.PermRead, Inherit: true,
				}); err != nil {
					return err
				}
				_, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID,
					Perms: store.PermWrite, Inherit: true,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var entries []store.ACLEntry
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, err := tx.ACLForNode(ctx, folder.ID)
				entries = got
				return err
			})
			if len(entries) != 1 {
				t.Fatalf("a second grant for the same subject must replace the first, got %d rows", len(entries))
			}
			if entries[0].Perms != store.PermWrite {
				t.Fatalf("the replaced grant should carry the new permissions, got %v", store.EncodePerms(entries[0].Perms))
			}
		})

		t.Run("RevokeRemovesAccess", func(t *testing.T) {
			s, _, _, folder, _ := setup(t)
			mate := addUser(t, s, "mate")

			var entry store.ACLEntry
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				e, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID,
					Perms: store.PermRead, Inherit: true,
				})
				entry = e
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Revoke(ctx, entry.ID)
			}); err != nil {
				t.Fatal(err)
			}

			var perms store.Perm
			var ok bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, found, err := tx.EffectivePermsFor(ctx, mate.ID, folder.ID)
				perms, ok = p, found
				return err
			})
			if ok || perms != 0 {
				t.Fatalf("revoking the grant must remove access, got %v", store.EncodePerms(perms))
			}
		})

		t.Run("GrantRequiresExactlyOneSubject", func(t *testing.T) {
			s, _, _, folder, _ := setup(t)
			mate := addUser(t, s, "mate")

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, grantErr := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, UserID: &mate.ID, GroupID: &mate.ID,
					Perms: store.PermRead, Inherit: true,
				})
				return grantErr
			})
			if !errors.Is(err, store.ErrInvalidName) {
				t.Fatalf("a grant naming both a user and a group must be refused, got %v", err)
			}
		})
	})

	// "Shared with me": the delivery mechanism for ACL grants.
	//
	// A grant that resolves correctly but cannot be *found* is not usable: every
	// path-based entry point addresses the caller's own namespace. These cases pin the
	// listing that makes a grant reachable.
	t.Run("SharedWithMe", func(t *testing.T) {
		t.Run("ListsGrantedRootsOnly", func(t *testing.T) {
			s, ownerID := factory(t)
			owner := store.User{ID: ownerID}
			_ = owner
			r := rootOf(t, s, ownerID)

			shared := mkdir(t, s, ownerID, r.ID, "shared")
			inner := mkdir(t, s, ownerID, shared.ID, "inner")
			mate := addUser(t, s, "lister")

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: shared.ID, UserID: &mate.ID,
					Perms: store.PermRead | store.PermListChildren, Inherit: true,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var got []store.SharedNode
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				nodes, err := tx.SharedWithMe(ctx, mate.ID, 100)
				got = nodes
				return err
			}); err != nil {
				t.Fatal(err)
			}

			// One entry: the granted root. The child is covered but is not a root.
			if len(got) != 1 {
				t.Fatalf("want exactly the granted root, got %d entries", len(got))
			}
			if got[0].NodeID != shared.ID {
				t.Fatalf("want the shared folder, got %q", got[0].Name)
			}
			if got[0].Perms&store.PermRead == 0 {
				t.Fatalf("the listing must carry the resolved permissions, got %v", store.EncodePerms(got[0].Perms))
			}
			_ = inner
		})

		t.Run("ExcludesUngrantedNodes", func(t *testing.T) {
			s, ownerID := factory(t)
			r := rootOf(t, s, ownerID)
			mkdir(t, s, ownerID, r.ID, "private")
			mate := addUser(t, s, "nosy")

			var got []store.SharedNode
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				nodes, err := tx.SharedWithMe(ctx, mate.ID, 100)
				got = nodes
				return err
			})
			if len(got) != 0 {
				t.Fatalf("an ungranted user must see nothing, got %d entries", len(got))
			}
		})

		t.Run("GroupGrantAppearsForMembers", func(t *testing.T) {
			s, ownerID := factory(t)
			r := rootOf(t, s, ownerID)
			folder := mkdir(t, s, ownerID, r.ID, "for-the-group")

			member := addUser(t, s, "member")
			outsider := addUser(t, s, "outsider")

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				g, gerr := tx.CreateGroup(ctx, store.Group{Name: "team"})
				if gerr != nil {
					return gerr
				}
				if aerr := tx.AddGroupMember(ctx, g.ID, member.ID, "viewer"); aerr != nil {
					return aerr
				}
				_, aerr := tx.Grant(ctx, store.ACLEntry{
					NodeID: folder.ID, GroupID: &g.ID,
					Perms: store.PermRead, Inherit: true,
				})
				return aerr
			}); err != nil {
				t.Fatal(err)
			}

			var memberSees, outsiderSees []store.SharedNode
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.SharedWithMe(ctx, member.ID, 100)
				memberSees = n
				return err
			})
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.SharedWithMe(ctx, outsider.ID, 100)
				outsiderSees = n
				return err
			})

			if len(memberSees) != 1 {
				t.Fatalf("a group member must see the shared folder, got %d", len(memberSees))
			}
			if len(outsiderSees) != 0 {
				t.Fatalf("a non-member must see nothing, got %d", len(outsiderSees))
			}
		})

		t.Run("ExcludesTheCallersOwnNodes", func(t *testing.T) {
			s, ownerID := factory(t)
			r := rootOf(t, s, ownerID)

			// A grant to the owner on their own node must not make it appear as
			// "shared with me" — it is already theirs.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.Grant(ctx, store.ACLEntry{
					NodeID: r.ID, UserID: &ownerID,
					Perms: store.PermRead, Inherit: true,
				})
				return err
			}); err != nil {
				t.Fatal(err)
			}

			var got []store.SharedNode
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.SharedWithMe(ctx, ownerID, 100)
				got = n
				return err
			})
			if len(got) != 0 {
				t.Fatalf("your own nodes are not 'shared with you', got %d", len(got))
			}
		})
	})

	// Background job queue (INV-06).
	//
	// The requirement is that background work is idempotent, retryable and recoverable
	// after an interruption. Those are properties of the queue, not of a handler, so
	// they belong in the shared suite.
	t.Run("JobQueue", func(t *testing.T) {
		// enqueue also registers a cleanup that drains the job.
		//
		// The queue is global rather than per-user, so a subtest that leaves work behind
		// (notably the "scheduled for later" case, which is never consumed) would be
		// visible to the next one. Draining on exit keeps each case independent of the
		// order the suite happens to run them in.
		enqueue := func(t *testing.T, s store.Store, j store.Job) {
			t.Helper()
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Enqueue(ctx, j)
			}); err != nil {
				t.Fatalf("enqueue %q: %v", j.Kind, err)
			}
			t.Cleanup(func() {
				_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					// Claim with a zero lease so everything due is taken, then complete
					// each; anything not due is left to the factory's clear.
					jobs, cerr := tx.ClaimJobs(ctx, "cleanup", 100, time.Nanosecond)
					if cerr != nil {
						return cerr
					}
					for _, job := range jobs {
						_ = tx.CompleteJob(ctx, job.ID)
					}
					return nil
				})
			})
		}

		claim := func(t *testing.T, s store.Store, worker string, lease time.Duration) []store.QueuedJob {
			t.Helper()
			var out []store.QueuedJob
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				jobs, err := tx.ClaimJobs(ctx, worker, 10, lease)
				out = jobs
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}

		t.Run("ClaimReturnsQueuedWork", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.echo", Payload: map[string]any{"n": "1"}})
			jobs := claim(t, s, "w1", time.Minute)
			if len(jobs) != 1 {
				t.Fatalf("want 1 claimed job, got %d", len(jobs))
			}
			if jobs[0].Kind != "test.echo" {
				t.Fatalf("wrong kind: %q", jobs[0].Kind)
			}
			if jobs[0].Payload["n"] != "1" {
				t.Fatalf("payload not preserved: %v", jobs[0].Payload)
			}
			if jobs[0].Attempts != 1 {
				t.Fatalf("claiming counts an attempt, got %d", jobs[0].Attempts)
			}
		})

		t.Run("ClaimIsExclusiveWhileLeased", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.once"})

			first := claim(t, s, "w1", time.Minute)
			if len(first) != 1 {
				t.Fatalf("first claim should get the job, got %d", len(first))
			}
			// A second worker must not see it while the lease holds.
			second := claim(t, s, "w2", time.Minute)
			if len(second) != 0 {
				t.Fatalf("a leased job must not be claimable by another worker, got %d", len(second))
			}
		})

		t.Run("ExpiredLeaseIsRecovered", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.recover"})

			// Claim with a lease that has already expired: this models a worker that
			// died mid-job.
			if got := claim(t, s, "w-dead", time.Nanosecond); len(got) != 1 {
				t.Fatalf("setup claim: %d", len(got))
			}
			// Give the nanosecond lease time to pass, then a live worker reclaims it.
			time.Sleep(5 * time.Millisecond)
			got := claim(t, s, "w-live", time.Minute)
			if len(got) != 1 {
				t.Fatalf("an expired lease must be recoverable, got %d jobs", len(got))
			}
			if got[0].Attempts != 2 {
				t.Fatalf("the retry should be attempt 2, got %d", got[0].Attempts)
			}
		})

		t.Run("ReapExpiredLeasesReleasesTheLease", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.reap"})

			// A worker claims with an already-expired lease, as one that died mid-job would.
			if got := claim(t, s, "w-dead", time.Nanosecond); len(got) != 1 {
				t.Fatalf("setup claim: %d", len(got))
			}
			time.Sleep(5 * time.Millisecond)

			// ReapExpiredLeases is separate from claim-time recovery and was never called by
			// any test. Both exist on purpose: claim filters on the lease, so a dead worker's
			// job is reclaimable without help — but the reaper is what clears locked_by and
			// locked_until, and an operator looking at the queue needs that to be right. A job
			// stuck showing "running under w-dead" forever is the symptom of a reaper that
			// never runs.
			var reaped int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.ReapExpiredLeases(ctx)
				reaped = n
				return err
			}); err != nil {
				t.Fatalf("reap: %v", err)
			}
			if reaped != 1 {
				t.Fatalf("reaped %d expired leases, want 1", reaped)
			}

			// Running it again finds nothing: the reaper is idempotent, and a worker on a timer
			// calling it every minute must not report perpetual activity.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.ReapExpiredLeases(ctx)
				reaped = n
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if reaped != 0 {
				t.Fatalf("a second reap released %d more leases, want 0", reaped)
			}

			// The job is still claimable, and the reaper did not consume an attempt.
			got := claim(t, s, "w-live", time.Minute)
			if len(got) != 1 {
				t.Fatalf("after reaping, the job should be claimable, got %d", len(got))
			}
			if got[0].Attempts != 2 {
				t.Fatalf("attempts = %d, want 2 — the reaper releases a lease, it does not "+
					"consume an attempt; only a claim does", got[0].Attempts)
			}
		})

		t.Run("ReapIgnoresLiveAndCompletedLeases", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.live"})
			enqueue(t, s, store.Job{Kind: "test.done"})

			// One claim takes everything due, so both jobs come back together. The first becomes
			// the live lease, the second is completed — the two states the reaper must leave
			// alone.
			jobs := claim(t, s, "w-live", time.Hour)
			if len(jobs) != 2 {
				t.Fatalf("setup: claimed %d jobs, want 2", len(jobs))
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CompleteJob(ctx, jobs[1].ID)
			}); err != nil {
				t.Fatal(err)
			}

			var reaped int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, err := tx.ReapExpiredLeases(ctx)
				reaped = n
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// Reaping a live lease would let a second worker run a job that is still in progress,
			// which is exactly the double-execution the queue exists to prevent.
			if reaped != 0 {
				t.Fatalf("reaped %d leases, want 0 — a live lease must not be released", reaped)
			}
		})
		t.Run("CompleteRemovesFromPending", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.done"})
			jobs := claim(t, s, "w1", time.Minute)
			if len(jobs) != 1 {
				t.Fatal("setup claim failed")
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CompleteJob(ctx, jobs[0].ID)
			}); err != nil {
				t.Fatal(err)
			}
			if got := claim(t, s, "w2", time.Minute); len(got) != 0 {
				t.Fatalf("a completed job must not be claimable again, got %d", len(got))
			}
		})

		t.Run("FailureRetriesThenExhausts", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			// MaxAttempts 1 keeps the test off the retry backoff: the property under
			// test is "an exhausted job is reported as failed", not "backoff timing",
			// and a 30-second sleep would make the suite unusable.
			enqueue(t, s, store.Job{Kind: "test.fails", MaxAttempts: 1})

			retried := 0
			for round := 0; round < 4; round++ {
				jobs := claim(t, s, "w1", time.Minute)
				if len(jobs) == 0 {
					break
				}
				var again bool
				if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					r, err := tx.FailJob(ctx, jobs[0].ID, errors.New("boom"), false)
					again = r
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if again {
					retried++
				}
				break
			}

			// It is now exhausted: not claimable, but still visible as failed.
			if retried != 0 {
				t.Fatalf("a job with MaxAttempts 1 must not be retried, got %d retries", retried)
			}
			if got := claim(t, s, "w2", time.Minute); len(got) != 0 {
				t.Fatalf("an exhausted job must not be claimed, got %d", len(got))
			}
			var pending, running, failed int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				p, r, f, err := tx.JobCounts(ctx)
				pending, running, failed = p, r, f
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if failed != 1 {
				t.Fatalf("an exhausted job must be reported as failed, got pending=%d running=%d failed=%d",
					pending, running, failed)
			}
		})

		t.Run("DedupeKeyCollapsesOutstandingWork", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.dedupe", DedupeKey: "same"})
			enqueue(t, s, store.Job{Kind: "test.dedupe", DedupeKey: "same"})

			jobs := claim(t, s, "w1", time.Minute)
			if len(jobs) != 1 {
				t.Fatalf("a dedupe key must collapse outstanding work, got %d", len(jobs))
			}
		})

		// A job enqueued with no explicit schedule must be claimable at once.
		//
		// This is the regression for a clock-source bug: the default run_after used to come
		// from the application's clock while claiming compared against the database's
		// now(). When the two drifted the job sat unclaimed, which surfaced as an
		// intermittent "setup claim failed" in neighbouring cases - only on PostgreSQL,
		// because only that backend has two clocks to disagree.
		t.Run("DefaultScheduleIsImmediatelyClaimable", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			// No RunAfter, the way every real producer enqueues.
			enqueue(t, s, store.Job{Kind: "test.immediate"})

			got := claim(t, s, "w1", time.Minute)
			if len(got) != 1 {
				t.Fatalf("a job with no RunAfter must be claimable at once, got %d "+
					"(the default schedule must come from the database clock)", len(got))
			}
		})

		t.Run("FutureRunAfterIsNotClaimed", func(t *testing.T) {
			s, owner := factory(t)
			rootOf(t, s, owner)

			enqueue(t, s, store.Job{Kind: "test.later", RunAfter: time.Now().Add(time.Hour)})
			if got := claim(t, s, "w1", time.Minute); len(got) != 0 {
				t.Fatalf("a job scheduled for later must not be claimed now, got %d", len(got))
			}

		})
	})

	// Account administration (ID-04 approval, ID-08 disable/enable).
	//
	// The property that matters here is not "the status column changed" — it is "an account
	// that may no longer act cannot still act". A disabled user whose sessions keep working
	// is not disabled, so every removal case checks the credentials too.
	t.Run("UserAdministration", func(t *testing.T) {
		t.Run("ListFiltersByStatus", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID) // ensure the store is usable

			var all, pending []store.User

			// Two accounts in known states.
			p := addUser(t, s, "waiting")
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.SetUserStatus(ctx, p.ID, store.StatusPending)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			a := addUser(t, s, "active-one")

			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				got, _ := tx.ListUsers(ctx, "", 100)
				all = got
				pend, perr := tx.ListUsers(ctx, store.StatusPending, 100)
				pending = pend
				return perr
			})

			if len(all) < 2 {
				t.Fatalf("want at least the two accounts we made, got %d", len(all))
			}
			if len(pending) != 1 {
				t.Fatalf("the pending filter must select exactly one, got %d", len(pending))
			}
			if pending[0].ID != p.ID {
				t.Fatalf("the wrong account came back: %s", pending[0].Username)
			}
			_ = a
		})

		t.Run("ApprovePendingIsRecorded", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			u := addUser(t, s, "to-approve")
			var change store.UserStatusChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.SetUserStatus(ctx, u.ID, store.StatusPending); err != nil {
					return err
				}
				c, err := tx.SetUserStatus(ctx, u.ID, store.StatusActive)
				change = c
				return err
			}); err != nil {
				t.Fatal(err)
			}

			if !change.Changed {
				t.Fatal("approving a pending account must report a change")
			}
			if change.From != store.StatusPending || change.To != store.StatusActive {
				t.Fatalf("wrong transition: %s -> %s", change.From, change.To)
			}
		})

		t.Run("SameStatusIsANoOp", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			u := addUser(t, s, "already-active")
			var change store.UserStatusChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				c, err := tx.SetUserStatus(ctx, u.ID, store.StatusActive)
				change = c
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if change.Changed {
				t.Fatal("setting the status it already has must not report a change")
			}
		})

		t.Run("DisableRevokesCredentials", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			u := addUser(t, s, "to-disable")

			// Give the account a live session and an app password.
			var sessionID string
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				// The sessions table keys on a uuid, so the id must be one. A readable
				// "sess-<name>" would work in memory and fail only on PostgreSQL, which is
				// exactly the divergence this shared suite exists to catch.
				sessionID = uuid.New().String()
				return tx.CreateSession(ctx, store.Session{
					ID: sessionID, UserID: u.ID, IP: "127.0.0.1",
					UserAgent: "test", ExpiresAt: time.Now().Add(time.Hour),
				})
			}); err != nil {
				t.Fatal(err)
			}

			var live bool
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				var lerr error
				live, lerr = tx.SessionLive(ctx, sessionID)
				return lerr
			}); err != nil {
				t.Fatal(err)
			}
			if !live {
				t.Fatal("setup: the session should be live before disabling")
			}

			// A real app password, so the revocation count is meaningful rather than
			// trivially zero.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CreateAppPassword(ctx, store.AppPassword{
					UserID: u.ID, Label: "test", Prefix: "wc_test",
					TokenHash: []byte("hash-" + u.Username), Scope: "rw",
				})
			}); err != nil {
				t.Fatal(err)
			}

			var pwBefore int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, lerr := tx.ListAppPasswords(ctx, u.ID)
				pwBefore = len(list)
				return lerr
			}); err != nil {
				t.Fatal(err)
			}
			if pwBefore == 0 {
				t.Fatal("setup: the account should hold one app password")
			}

			var change store.UserStatusChange
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				c, cerr := tx.SetUserStatus(ctx, u.ID, store.StatusDisabled)
				change = c
				return cerr
			}); err != nil {
				t.Fatal(err)
			}

			if change.SessionsRevoked < 1 {
				t.Fatalf("disabling must revoke the live session, got %d", change.SessionsRevoked)
			}
			if change.AppPasswordsRevoked < 1 {
				t.Fatalf("disabling must revoke app passwords, got %d", change.AppPasswordsRevoked)
			}

			// The session no longer authenticates. This is the assertion the whole case
			// exists for: a disabled account whose session still works is not disabled.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				var lerr error
				live, lerr = tx.SessionLive(ctx, sessionID)
				return lerr
			}); err != nil {
				t.Fatal(err)
			}
			if live {
				t.Fatal("the revoked session must no longer be live")
			}

			var livePasswords int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, lerr := tx.ListAppPasswords(ctx, u.ID)
				for _, ap := range list {
					if ap.RevokedAt == nil {
						livePasswords++
					}
				}
				return lerr
			}); err != nil {
				t.Fatal(err)
			}
			if livePasswords != 0 {
				t.Fatalf("disabling must leave no live app password, got %d", livePasswords)
			}
		})

		t.Run("ReEnableDoesNotResurrectCredentials", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			u := addUser(t, s, "cycle")
			var revoked int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.SetUserStatus(ctx, u.ID, store.StatusDisabled); err != nil {
					return err
				}
				c, err := tx.SetUserStatus(ctx, u.ID, store.StatusActive)
				revoked = c.SessionsRevoked
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// Re-enabling revokes nothing: the point is that it does not *restore* what was
			// already cut off, which the session test above covers.
			if revoked != 0 {
				t.Fatalf("re-enabling must revoke nothing, got %d", revoked)
			}
		})

		t.Run("UnknownStatusIsRefused", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)
			u := addUser(t, s, "typo-target")

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, serr := tx.SetUserStatus(ctx, u.ID, "actve") // typo
				return serr
			})
			if err == nil {
				t.Fatal("an unknown status must be refused rather than stored")
			}
		})

		t.Run("UnknownUserIsNotFound", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, serr := tx.SetUserStatus(ctx, uuid.New(), store.StatusActive)
				return serr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
		})
	})

	// Account deletion (ID-08).
	//
	// The requirement asks for a *defined policy*, not an endpoint: files, shares, audit and
	// retained data each need a decided outcome. These cases pin all four, because a deletion
	// that silently keeps something, or silently destroys the audit trail, is a policy bug
	// rather than a cosmetic one.
	// Sibling-name semantics (T-12).
	//
	// T-12 names case/Unicode collisions as a way to overwrite an unintended target. The two
	// backends must therefore agree on what "the same name" means, or a file created through one
	// and looked up through the other resolves differently — and the server runs on either.
	t.Run("SiblingNameSemantics", func(t *testing.T) {
		t.Run("CaseCollisionIsRefused", func(t *testing.T) {
			s, ownerID := factory(t)
			root := rootOf(t, s, ownerID)

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.CreateFile(ctx, store.NewFile{
					OwnerID: ownerID, ParentID: root.ID, Name: "Readme.md", Size: 0,
					BlobID: newBlobHook(t),
				})
				return cerr
			}); err != nil {
				t.Fatalf("create the first file: %v", err)
			}

			// A second file differing only in case must not be creatable: two siblings whose
			// names differ only by case are indistinguishable on a case-insensitive client
			// (Windows, macOS default), so they would shadow each other unpredictably.
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.CreateFile(ctx, store.NewFile{
					OwnerID: ownerID, ParentID: root.ID, Name: "README.md", Size: 0,
					BlobID: newBlobHook(t),
				})
				return cerr
			})
			if err == nil {
				t.Fatal("a name that differs only in case must be refused as a collision")
			}
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("want ErrConflict, got %v", err)
			}
		})

		t.Run("LookupIsCaseInsensitive", func(t *testing.T) {
			s, ownerID := factory(t)
			root := rootOf(t, s, ownerID)

			var created store.Node
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, cerr := tx.CreateFile(ctx, store.NewFile{
					OwnerID: ownerID, ParentID: root.ID, Name: "MixedCase.txt", Size: 0,
					BlobID: newBlobHook(t),
				})
				created = n
				return cerr
			}); err != nil {
				t.Fatal(err)
			}

			// A client that sends different capitalisation must reach the same node, otherwise
			// it sees "not found" for a file it just created.
			for _, probe := range []string{"mixedcase.txt", "MIXEDCASE.TXT", "MixedCase.txt"} {
				var got store.Node
				err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					n, e := tx.ChildByName(ctx, root.ID, probe)
					got = n
					return e
				})
				if err != nil {
					t.Fatalf("lookup %q: %v", probe, err)
				}
				if got.ID != created.ID {
					t.Fatalf("lookup %q resolved to a different node", probe)
				}
			}
		})

		t.Run("UnicodeNormalisationCollides", func(t *testing.T) {
			s, ownerID := factory(t)
			root := rootOf(t, s, ownerID)

			// "café" written as a precomposed é (U+00E9) and as e + combining acute (U+0301) are
			// different byte strings but the same filename to a user. Treating them as distinct
			// lets two files appear identical in every client, which is the collision T-12 warns
			// about.
			precomposed := "caf\u00e9.txt"
			decomposed := "cafe\u0301.txt"

			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.CreateFile(ctx, store.NewFile{
					OwnerID: ownerID, ParentID: root.ID, Name: precomposed, Size: 0,
					BlobID: newBlobHook(t),
				})
				return cerr
			}); err != nil {
				t.Fatal(err)
			}

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, cerr := tx.CreateFile(ctx, store.NewFile{
					OwnerID: ownerID, ParentID: root.ID, Name: decomposed, Size: 0,
					BlobID: newBlobHook(t),
				})
				return cerr
			})
			if err == nil {
				t.Fatal("two names that normalise to the same string must collide")
			}
		})
	})
	// Lock expiry (§5.3).
	//
	// The worker sweeps expired locks every minute in production. Nothing tested that the sweep
	// does what it claims, which matters twice: an expired lock that survives blocks a user's
	// edits indefinitely, and one that is deleted early breaks a client mid-write.
	t.Run("LockExpiry", func(t *testing.T) {
		t.Run("ExpiredLockIsGone", func(t *testing.T) {
			s, ownerID := factory(t)
			root := rootOf(t, s, ownerID)
			var node store.Node
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, cerr := tx.CreateDir(ctx, store.NewNode{
					OwnerID: ownerID, ParentID: root.ID, Name: "locked-dir",
				})
				node = n
				return cerr
			}); err != nil {
				t.Fatal(err)
			}

			// One live lock and one already expired.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if err := tx.CreateLock(ctx, store.Lock{
					Token: "opaquelocktoken:live", NodeID: node.ID, UserID: &ownerID,
					OwnerXML: "<D:href>live</D:href>", Depth: "0", TimeoutSec: 3600,
					ExpiresAt: time.Now().Add(time.Hour),
				}); err != nil {
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// A second node with a lock that has already expired. CreateLock may refuse a
			// duplicate live lock on one node, so this uses its own node.
			var stale store.Node
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, cerr := tx.CreateDir(ctx, store.NewNode{
					OwnerID: ownerID, ParentID: root.ID, Name: "stale-dir",
				})
				stale = n
				return cerr
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CreateLock(ctx, store.Lock{
					Token: "opaquelocktoken:stale", NodeID: stale.ID, UserID: &ownerID,
					OwnerXML: "<D:href>stale</D:href>", Depth: "0", TimeoutSec: 1,
					ExpiresAt: time.Now().Add(-time.Minute),
				})
			}); err != nil {
				t.Fatal(err)
			}

			// The sweep removes the expired one and only that one.
			var removed int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, eerr := tx.ExpireLocks(ctx)
				removed = n
				return eerr
			}); err != nil {
				t.Fatal(err)
			}
			if removed != 1 {
				t.Fatalf("want exactly the one expired lock removed, removed %d", removed)
			}

			// The live lock is still there.
			var liveHeld bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, lerr := tx.ActiveLock(ctx, node.ID)
				liveHeld = lerr == nil
				return nil
			})
			if !liveHeld {
				t.Fatal("a live lock must survive the sweep")
			}

			// And the expired one is gone.
			var staleGone bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, lerr := tx.ActiveLock(ctx, stale.ID)
				staleGone = lerr != nil
				return nil
			})
			if !staleGone {
				t.Fatal("the expired lock must be gone after the sweep")
			}
		})

		t.Run("SweepIsIdempotent", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			// Nothing to expire: the sweep must report zero and not fail.
			var removed int
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, eerr := tx.ExpireLocks(ctx)
				removed = n
				return eerr
			}); err != nil {
				t.Fatal(err)
			}
			if removed != 0 {
				t.Fatalf("an empty sweep removes nothing, removed %d", removed)
			}
		})
	})
	t.Run("AccountDeletion", func(t *testing.T) {
		t.Run("RequiresDisableFirst", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)
			u := addUser(t, s, "still-active")

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, derr := tx.DeleteUser(ctx, u.ID)
				return derr
			})
			if err == nil {
				t.Fatal("deleting an active account must be refused: deletion is irreversible and disable is the reversible step")
			}

			// And the account is untouched.
			var got store.User
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				gu, uerr := tx.UserByID(ctx, u.ID)
				got = gu
				return uerr
			})
			if got.Status == store.StatusDeleted {
				t.Fatal("a refused deletion must not modify the account")
			}
		})

		t.Run("AppliesThePolicy", func(t *testing.T) {
			s, ownerID := factory(t)
			root := rootOf(t, s, ownerID)

			victim := addUser(t, s, "leaving")
			if _, err := s.EnsureRoot(context.Background(), victim.ID); err != nil {
				t.Fatal(err)
			}

			// The victim owns a folder with a file, holds a session and an app password.
			var folder store.Node
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				n, cerr := tx.CreateDir(ctx, store.NewNode{
					OwnerID: victim.ID, ParentID: rootOf(t, s, victim.ID).ID, Name: "theirs",
				})
				folder = n
				return cerr
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CreateSession(ctx, store.Session{
					ID: uuid.New().String(), UserID: victim.ID, IP: "127.0.0.1",
					UserAgent: "test", ExpiresAt: time.Now().Add(time.Hour),
				})
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				// The id must be set: the primary key is app_passwords.id, and leaving it zero
				// collides with any other test that also builds one by hand.
				return tx.CreateAppPassword(ctx, store.AppPassword{
					ID: uuid.New(), UserID: victim.ID, Label: "test", Prefix: "wc_x",
					TokenHash: []byte("hash-victim"), Scope: "rw",
				})
			}); err != nil {
				t.Fatal(err)
			}

			// An audit row of the victim's, which must survive.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.Audit(ctx, store.AuditEntry{
					ActorKind: "user", ActorUserID: &victim.ID, ActorEmail: victim.Email,
					Action: "file.upload", TargetKind: "node", TargetID: root.ID.String(),
					Result: "ok",
				})
			}); err != nil {
				t.Fatal(err)
			}

			// liveSessionID is minted between the disable and the delete, below.
			var liveSessionID string

			// Disable first — which revokes the credentials that exist at this moment.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, serr := tx.SetUserStatus(ctx, victim.ID, store.StatusDisabled)
				return serr
			}); err != nil {
				t.Fatal(err)
			}

			// A session minted AFTER the disable but BEFORE the delete. This is the case
			// DeleteUser's own revocation exists for: the disable cannot have caught it.
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				liveSessionID = uuid.New().String()
				return tx.CreateSession(ctx, store.Session{
					ID: liveSessionID, UserID: victim.ID,
					ExpiresAt: time.Now().Add(time.Hour), AuthMethod: "oidc",
				})
			}); err != nil {
				t.Fatalf("mint a session between disable and delete: %v", err)
			}

			var res store.DeleteUserResult
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				r, derr := tx.DeleteUser(ctx, victim.ID)
				res = r
				return derr
			}); err != nil {
				t.Fatal(err)
			}

			// Files entered the trash rather than vanishing.
			if res.NodesTrashed == 0 {
				t.Fatal("the account's files must be trashed, not left owned by a deleted account")
			}
			var liveChildren int
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				kids, kerr := tx.Children(ctx, rootOf(t, s, victim.ID).ID, 100)
				liveChildren = len(kids)
				return kerr
			})
			if liveChildren != 0 {
				t.Fatalf("the victim's namespace must be empty, %d children remain", liveChildren)
			}

			// Credentials are gone. The counts may be zero here because disabling already
			// revoked them — what matters is the end state, not which step did it. Asserting on
			// the count would make this test depend on the two-step flow having been skipped.
			var livePasswords int
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				list, lerr := tx.ListAppPasswords(ctx, victim.ID)
				for _, ap := range list {
					if ap.RevokedAt == nil {
						livePasswords++
					}
				}
				return lerr
			})
			if livePasswords != 0 {
				t.Fatalf("no live app password may remain, got %d", livePasswords)
			}
			// The session minted before the delete must be dead now.
			//
			// This assertion replaces scaffolding: a comment promising it, a closure that created
			// a random uuid and discarded it, and five `_ =` suppressions. Nothing was checked, so
			// the property the comment described was never tested — and a session outliving its
			// account is exactly the "deleted account still working" failure ID-08 prevents.
			var alive bool
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				alive, _ = tx.SessionLive(ctx, liveSessionID)
				return nil
			})
			if alive {
				t.Fatal("a session minted before the account was deleted still reports itself live. " +
					"A deleted account whose session works is not deleted, and ID-08 requires the " +
					"session to end with the account.")
			}

			// The audit history survived, and the count says so.
			if res.AuditRowsKept == 0 {
				t.Fatal("audit rows must be kept: they are the record of what happened, not the account's property")
			}

			// The account is a tombstone: deleted status, and the identifiers released.
			var after store.User
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				gu, uerr := tx.UserByID(ctx, victim.ID)
				after = gu
				return uerr
			})
			if after.Status != store.StatusDeleted {
				t.Fatalf("want status %q, got %q", store.StatusDeleted, after.Status)
			}
			if after.Username == "leaving" {
				t.Fatal("the username must be released or replaced")
			}
			if after.Email == victim.Email {
				t.Fatal("the email must be released so the address can be re-invited")
			}

			_ = folder
		})

		t.Run("DeletingTwiceIsRefused", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)
			u := addUser(t, s, "twice")

			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, serr := tx.SetUserStatus(ctx, u.ID, store.StatusDisabled); serr != nil {
					return serr
				}
				_, derr := tx.DeleteUser(ctx, u.ID)
				return derr
			})

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, derr := tx.DeleteUser(ctx, u.ID)
				return derr
			})
			if err == nil {
				t.Fatal("deleting an already-deleted account must be refused")
			}
		})

		t.Run("UnknownUserIsNotFound", func(t *testing.T) {
			s, ownerID := factory(t)
			rootOf(t, s, ownerID)

			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, derr := tx.DeleteUser(ctx, uuid.New())
				return derr
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("want ErrNotFound, got %v", err)
			}
		})
	})

	t.Run("AuditIsRecorded", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		// Baseline first: this case asserts what *it* adds, not the global total.
		before := 0
		if opts.AuditCount != nil {
			before = opts.AuditCount(t, s)
		}

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
		// Relative, not absolute: earlier cases in this suite legitimately write audit rows of
		// their own (the deletion policy deliberately keeps them), so asserting a global count
		// of one would make this case depend on the order the suite happens to run in.
		if opts.AuditCount != nil && opts.AuditCount(t, s)-before != 1 {
			t.Fatalf("committed operation must add exactly one audit entry, added %d",
				opts.AuditCount(t, s)-before)
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

// namesOf renders node names for a failure message.
func namesOf(nodes []store.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// addUser provisions a second account in the same store.
//
// Several ACL properties are about "a different user", and a test that reused the
// owner would be checking the owner path instead of the grant path.
func addUser(t *testing.T, s store.Store, username string) store.User {
	t.Helper()
	var out store.User
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer: "https://idp.test", Subject: username,
			Username: username, Email: username + "@example.test", Role: "user",
		})
		out = u
		return uerr
	})
	if err != nil {
		t.Fatalf("add user %q: %v", username, err)
	}
	return out
}

// contains reports whether ss holds want.
func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// shareRateSuite exercises the upload rate limit on both backends.
//
// It lives in the shared suite rather than in the server's tests because the limiter is a property
// of the store: the PostgreSQL version is a single statement with an advisory lock, the in-memory
// version is a count over a slice, and those are different enough that "the server test passes"
// says nothing about production. The server tests run against the memory backend only.
func shareRateSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	newRateShare := func(t *testing.T, s store.Store, owner, nodeID uuid.UUID,
		rate int64, window time.Duration) store.Share {
		t.Helper()
		var sh store.Share
		windowSecs := int64(window / time.Second)
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, cerr := tx.CreateShare(ctx, store.Share{
				NodeID: nodeID, OwnerID: owner, Kind: store.ShareRequest,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"create"},
				// A request share must hide its contents — the schema enforces it with a check
				// constraint. Without this the insert is rejected with "invalid name:
				// shares_check", which is the constraint speaking, not a naming problem.
				HideContents:  true,
				MaxUploadRate: &rate, UploadRateWindowSeconds: &windowSecs,
			})
			sh = c
			return cerr
		})
		if err != nil {
			t.Fatalf("create rate-limited share: %v", err)
		}
		return sh
	}

	allow := func(s store.Store, sh store.Share, rate int64, window time.Duration) error {
		return s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.AllowShareUpload(ctx, sh.ID, rate, window, "127.0.0.1", 10)
		})
	}

	t.Run("AllowsUpToTheRateThenRefuses", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh := newRateShare(t, s, owner, r.ID, 3, time.Hour)

		for i := 0; i < 3; i++ {
			if err := allow(s, sh, 3, time.Hour); err != nil {
				t.Fatalf("upload %d of 3 must be accepted: %v", i+1, err)
			}
		}
		err := allow(s, sh, 3, time.Hour)
		if !errors.Is(err, store.ErrShareRateLimited) {
			t.Fatalf("the fourth upload through a 3/hour limit: want ErrShareRateLimited, got %v."+
				" A rate limit that does not refuse is not a limit.", err)
		}
	})

	t.Run("RateIsTheDistinctErrorFromExhaustion", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh := newRateShare(t, s, owner, r.ID, 1, time.Hour)

		if err := allow(s, sh, 1, time.Hour); err != nil {
			t.Fatal(err)
		}
		err := allow(s, sh, 1, time.Hour)
		if errors.Is(err, store.ErrShareExhausted) {
			t.Fatal("a rate-limited upload was reported as exhausted. Exhausted means the " +
				"allowance is gone for good and rate means wait: they lead a visitor to different " +
				"next steps and an operator to different causes.")
		}
		if !errors.Is(err, store.ErrShareRateLimited) {
			t.Fatalf("want ErrShareRateLimited, got %v", err)
		}
	})

	t.Run("ZeroRateIsUnlimited", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh := newRateShare(t, s, owner, r.ID, 0, time.Hour)

		// A share with no rate configured must not accumulate rows for a limit nobody set.
		for i := 0; i < 20; i++ {
			if err := allow(s, sh, 0, time.Hour); err != nil {
				t.Fatalf("an unset rate must accept everything: %v", err)
			}
		}
	})

	t.Run("RateIsScopedToTheShare", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		a := newRateShare(t, s, owner, r.ID, 1, time.Hour)
		b := newRateShare(t, s, owner, r.ID, 1, time.Hour)

		if err := allow(s, a, 1, time.Hour); err != nil {
			t.Fatal(err)
		}
		// Filling one share's window must not affect another's: the limits are per-link, and a
		// global limiter would let one busy drop box throttle every other link on the instance.
		if err := allow(s, b, 1, time.Hour); err != nil {
			t.Fatalf("a second share was rate-limited by the first share's traffic: %v", err)
		}
	})

	t.Run("PruneRemovesOnlyOldEvents", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh := newRateShare(t, s, owner, r.ID, 2, time.Hour)

		if err := allow(s, sh, 2, time.Hour); err != nil {
			t.Fatal(err)
		}

		// Pruning nothing (a keep window far longer than the events) must not free the budget.
		var removed int
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.PruneShareUploadEvents(ctx, 24*time.Hour)
			removed = n
			return err
		})
		if removed != 0 {
			t.Fatalf("pruning with a 24h keep window removed %d fresh rows", removed)
		}
		if err := allow(s, sh, 2, time.Hour); err != nil {
			t.Fatalf("the second of two uploads was refused after a non-destructive prune: %v", err)
		}

		// Pruning everything (a zero keep window) must free it.
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.PruneShareUploadEvents(ctx, 0)
			removed = n
			return err
		})
		if removed == 0 {
			t.Fatal("pruning with a zero keep window removed nothing, so the table would grow " +
				"without bound")
		}
	})
}

// retentionSuite exercises ExpireRetained on both backends.
//
// It belongs in the shared suite for the same reason the rate limiter does: the PostgreSQL version
// is a single UPDATE with a predicate over timestamptz, the in-memory version compares time.Time
// values, and those disagree at boundaries easily. The server test for retention runs against the
// memory backend, so the SQL had never executed under test.
func retentionSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	// mkDelivered creates a file with a retention deadline, returning it.
	mkDelivered := func(t *testing.T, s store.Store, owner, parentID uuid.UUID,
		name string, retain *time.Time) store.Node {
		t.Helper()
		var n store.Node
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, cerr := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: parentID, Name: name, Size: 1,
				Mime: "application/octet-stream", ETag: `"x"`,
				BlobID: newBlobHook(t), BlobHash: [32]byte{byte(len(name))},
				RetainUntil: retain,
			})
			n = c
			return cerr
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return n
	}

	expire := func(s store.Store) int {
		var n int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.ExpireRetained(ctx)
			n = got
			return err
		}); err != nil {
			t.Fatalf("expire: %v", err)
		}
		return n
	}

	t.Run("OnlyPastDeadlinesAreTrashed", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		past := time.Now().Add(-time.Hour)
		future := time.Now().Add(24 * time.Hour)
		expired := mkDelivered(t, s, owner, r.ID, "expired.bin", &past)
		kept := mkDelivered(t, s, owner, r.ID, "kept.bin", &future)
		none := mkDelivered(t, s, owner, r.ID, "none.bin", nil)

		if n := expire(s); n != 1 {
			t.Fatalf("expired %d nodes, want 1", n)
		}

		var gotExpired, gotKept, gotNone store.Node
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var e1, e2, e3 error
			gotExpired, e1 = tx.NodeIncludingTrashed(ctx, expired.ID)
			gotKept, e2 = tx.NodeIncludingTrashed(ctx, kept.ID)
			gotNone, e3 = tx.NodeIncludingTrashed(ctx, none.ID)
			return errors.Join(e1, e2, e3)
		})
		if gotExpired.TrashedAt == nil {
			t.Fatal("a delivery past its deadline was not trashed")
		}
		if gotKept.TrashedAt != nil {
			t.Fatal("a delivery still inside its window was trashed")
		}
		if gotNone.TrashedAt != nil {
			t.Fatal("a file with no retention at all was trashed")
		}
	})

	t.Run("TrashingDoesNotDeleteContent", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		past := time.Now().Add(-time.Hour)
		n := mkDelivered(t, s, owner, r.ID, "recoverable.bin", &past)

		expire(s)

		// The point of trashing rather than deleting: the row still exists and the owner can
		// restore it. Deleting outright would make a misconfigured retention_seconds
		// unrecoverable, which is a much worse failure than keeping data too long.
		var got store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			found, err := tx.NodeIncludingTrashed(ctx, n.ID)
			got = found
			return err
		}); err != nil {
			t.Fatalf("the expired delivery is gone entirely, not trashed: %v", err)
		}
		if got.TrashedAt == nil {
			t.Fatal("want the node trashed so it is recoverable")
		}
	})

	t.Run("SecondRunIsIdempotent", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		past := time.Now().Add(-time.Hour)
		mkDelivered(t, s, owner, r.ID, "once.bin", &past)

		if n := expire(s); n != 1 {
			t.Fatalf("first run expired %d, want 1", n)
		}
		// A second sweep must not re-count the same node: the predicate excludes already-trashed
		// rows, and a worker running every six hours would otherwise report perpetual activity.
		if n := expire(s); n != 0 {
			t.Fatalf("second run expired %d, want 0 — an already-trashed node must not be counted "+
				"again", n)
		}
	})
}

// otpSuite exercises the verification throttle and single-use codes on both backends.
//
// These are security invariants, not convenience features: ConsumeRecipientCode is the only thing
// stopping a captured code from being replayed, and OTPFailureCount is the only thing limiting
// online guessing. Both were exercised solely through the share service's tests, which run against
// the in-memory backend — so the PostgreSQL statements had never been executed under test. A
// divergence here would not be a rendering bug; it would be an authentication bypass on one
// backend and not the other.
func otpSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	const email = "recipient@example.test"

	// newEmailShare creates a share with one invited recipient.
	newEmailShare := func(t *testing.T, s store.Store, owner uuid.UUID) store.Share {
		t.Helper()
		var sh store.Share
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			r, rerr := tx.EnsureRoot(ctx, owner)
			if rerr != nil {
				return rerr
			}
			c, cerr := tx.CreateShare(ctx, store.Share{
				NodeID: r.ID, OwnerID: owner, Kind: store.ShareEmail,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
			})
			if cerr != nil {
				return cerr
			}
			sh = c
			return tx.AddRecipient(ctx, store.ShareRecipient{ShareID: c.ID, Email: email})
		})
		if err != nil {
			t.Fatalf("create email share: %v", err)
		}
		return sh
	}

	t.Run("ConsumeRecipientCodeIsSingleUse", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)

		// The first consumption succeeds.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.ConsumeRecipientCode(ctx, sh.ID, email)
		}); err != nil {
			t.Fatalf("first use of a fresh code: %v", err)
		}

		// The second must fail. This is replay protection: without it a code that leaked from a
		// mailbox, a mail scanner's link prefetch, or a shared terminal keeps working.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.ConsumeRecipientCode(ctx, sh.ID, email)
		})
		if !errors.Is(err, store.ErrCodeConsumed) {
			t.Fatalf("reusing a consumed code: want ErrCodeConsumed, got %v. A code that can be "+
				"replayed is not a one-time code.", err)
		}
	})

	t.Run("ConsumeRecipientCodeRequiresAnInvitation", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)

		// An address that was never invited must not be consumable: the UPDATE matches no row,
		// which has to be a refusal rather than a silent success.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.ConsumeRecipientCode(ctx, sh.ID, "stranger@example.test")
		})
		if err == nil {
			t.Fatal("consuming a code for an address that was never invited succeeded")
		}
	})

	t.Run("FailureCountCountsOnlyFailuresInsideTheWindow", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)
		ctx := context.Background()

		count := func() int {
			var n int
			if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				got, err := tx.OTPFailureCount(ctx, sh.ID, email, time.Hour)
				n = got
				return err
			}); err != nil {
				t.Fatalf("count: %v", err)
			}
			return n
		}

		if n := count(); n != 0 {
			t.Fatalf("a fresh share reports %d failures", n)
		}

		// Two failures and one success.
		for _, success := range []bool{false, false, true} {
			if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				return tx.RecordOTPAttempt(ctx, store.OTPAttempt{
					ShareID: sh.ID, Email: email, IP: "127.0.0.1",
					Success: success, Reason: "test",
				})
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
		}

		// Only the failures count toward the throttle. Counting successes would let a legitimate
		// visitor lock themselves out by using the share correctly.
		if n := count(); n != 2 {
			t.Fatalf("failure count = %d, want 2 — successes must not count toward the throttle", n)
		}
	})

	t.Run("FailureCountIsScopedToTheAddress", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)
		ctx := context.Background()

		if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.RecordOTPAttempt(ctx, store.OTPAttempt{
				ShareID: sh.ID, Email: email, IP: "127.0.0.1", Success: false,
			})
		}); err != nil {
			t.Fatal(err)
		}

		// A different invited address must not inherit the first one's failure count: one
		// recipient being attacked must not lock out the others.
		var other int
		_ = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, err := tx.OTPFailureCount(ctx, sh.ID, "someone-else@example.test", time.Hour)
			other = n
			return err
		})
		if other != 0 {
			t.Fatalf("an unrelated address reports %d failures, so one recipient's failures "+
				"would lock out another", other)
		}
	})

	t.Run("MarkRecipientVerifiedIsIdempotent", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)
		ctx := context.Background()

		for i := 0; i < 2; i++ {
			if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				return tx.MarkRecipientVerified(ctx, sh.ID, email)
			}); err != nil {
				t.Fatalf("verify pass %d: %v", i+1, err)
			}
		}

		var r store.ShareRecipient
		if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			got, err := tx.RecipientFor(ctx, sh.ID, email)
			r = got
			return err
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if r.VerifiedAt == nil {
			t.Fatal("the recipient was not marked verified")
		}
	})
}

// versionLifecycleSuite covers RestoreVersion and ExpireVersions.
//
// ExpireVersions had a PostgreSQL-only test (lifecycle_pg_test.go) and RestoreVersion had none
// anywhere. Both are reachable by a user: version history is a feature, and a restore that
// silently loses the current content is data loss. A behaviour both stores must share belongs in
// the shared suite.
//
// Note on scope: the in-memory store deliberately does not model blob lifecycle (its
// CollectableBlobs returns an error rather than an empty list, precisely so the gap is not
// mistaken for "nothing to do"). These cases therefore assert the version ROWS, which both
// backends maintain, and not the refcount, which only PostgreSQL has.
func versionLifecycleSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	// versionsOf reads the version list for a node.
	versionsOf := func(t *testing.T, s store.Store, nodeID uuid.UUID) []store.Version {
		t.Helper()
		var out []store.Version
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			v, err := tx.Versions(ctx, nodeID, 0)
			out = v
			return err
		}); err != nil {
			t.Fatalf("list versions: %v", err)
		}
		return out
	}

	// writeVersion replaces a file's content, keeping the previous content as a version.
	writeVersion := func(t *testing.T, s store.Store, owner, nodeID uuid.UUID, tag int) {
		t.Helper()
		var hash [32]byte
		hash[0] = byte(tag)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.UpdateFile(ctx, store.UpdateFile{
				NodeID: nodeID, Size: int64(10 + tag), ETag: "e" + string(rune('a'+tag)),
				BlobID: newBlobHook(t), BlobHash: hash,
				KeepVersion: true, MaxVersions: 10,
			})
			return err
		}); err != nil {
			t.Fatalf("write version %d: %v", tag, err)
		}
	}

	t.Run("RestoreVersionSwapsCurrentAndKeepsIt", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		file := mkfile(t, s, owner, r.ID, "v.txt", 10)

		writeVersion(t, s, owner, file.ID, 1)
		writeVersion(t, s, owner, file.ID, 2)

		vs := versionsOf(t, s, file.ID)
		if len(vs) != 2 {
			t.Fatalf("want 2 versions before restore, got %d", len(vs))
		}

		// The oldest kept version.
		oldest := vs[len(vs)-1].VersionNo
		var restored store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.RestoreVersion(ctx, file.ID, oldest)
			restored = n
			return err
		}); err != nil {
			t.Fatalf("restore version %d: %v", oldest, err)
		}

		// The restored content is now current.
		var cur store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.Node(ctx, file.ID)
			cur = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if cur.Size != restored.Size {
			t.Fatalf("current size %d does not match the restored version %d", cur.Size, restored.Size)
		}

		// And the content it replaced is now a version. Restoring must never destroy the
		// current content: that would make "restore an old version" a way to lose the new one.
		// Two versions before, plus the content the restore replaced: 3. Restoring must never
		// destroy the content it displaced, or "restore an old version" becomes a way to lose the
		// new one — which is the opposite of what undo means.
		after := versionsOf(t, s, file.ID)
		if len(after) != 3 {
			t.Fatalf("after restoring, want 3 versions (2 kept + the replaced current one), got %d. "+
				"A restore that discards the content it replaced turns version history into a way "+
				"to destroy data.", len(after))
		}
	})

	t.Run("RestoreVersionRejectsAnUnknownVersion", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		file := mkfile(t, s, owner, r.ID, "missing.txt", 10)
		writeVersion(t, s, owner, file.ID, 1)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.RestoreVersion(ctx, file.ID, 9999)
			return err
		}); err == nil {
			t.Fatal("restoring a version number that does not exist succeeded")
		}
	})

	t.Run("ExpireVersionsKeepsRecentAndRemovesOld", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		file := mkfile(t, s, owner, r.ID, "age.txt", 10)
		writeVersion(t, s, owner, file.ID, 1)
		writeVersion(t, s, owner, file.ID, 2)

		before := versionsOf(t, s, file.ID)
		if len(before) != 2 {
			t.Fatalf("want 2 versions, got %d", len(before))
		}

		// A retention longer than the versions' age removes nothing. This is the safe direction:
		// a misconfigured retention must not delete history that is inside its window.
		var removed int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.ExpireVersions(ctx, 365*24*time.Hour)
			removed = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Fatalf("a one-year retention removed %d fresh versions", removed)
		}
		if got := versionsOf(t, s, file.ID); len(got) != 2 {
			t.Fatalf("versions disappeared under a retention they are well inside of: %d", len(got))
		}

		// A retention of zero means "never expire", NOT "expire everything". That is a
		// deliberate data-loss guard, the same one PurgeTrash uses: an omitted or zero retention
		// must not delete a user's history, so a deployment that has not decided a policy keeps
		// everything. Asserting the opposite here would have encoded a data-loss bug as expected
		// behaviour.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.ExpireVersions(ctx, 0)
			removed = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Fatalf("a zero retention removed %d versions. Zero means \"keep forever\": it is "+
				"the safe default for a setting that deletes history.", removed)
		}
		if got := versionsOf(t, s, file.ID); len(got) != 2 {
			t.Fatalf("a zero retention removed versions: %d remain, want 2", len(got))
		}
	})
}

// deadPropSuite covers WebDAV dead-property storage on both backends.
//
// Only DeadProps had any test, and it ran against PostgreSQL alone. SetDeadProp upserts and
// DeleteDeadProp removes; neither had a test on either backend. Dead properties are what a WebDAV
// client thinks it is persisting — a colour label, a sync token, a custom application field — and
// a divergence here is silent: the property is written, the client is told it succeeded, and a
// later PROPFIND on the other backend returns nothing.
func deadPropSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	const (
		ns   = "http://example.test/ns"
		name = "label"
	)

	// fileNode creates a file to hang properties on.
	fileNode := func(t *testing.T, s store.Store, owner uuid.UUID) store.Node {
		t.Helper()
		r := rootOf(t, s, owner)
		return mkfile(t, s, owner, r.ID, "prop.txt", 10)
	}

	read := func(t *testing.T, s store.Store, id uuid.UUID) []store.DeadProp {
		t.Helper()
		var out []store.DeadProp
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			p, err := tx.DeadProps(ctx, id)
			out = p
			return err
		}); err != nil {
			t.Fatalf("read props: %v", err)
		}
		return out
	}

	set := func(t *testing.T, s store.Store, id uuid.UUID, value string) {
		t.Helper()
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.SetDeadProp(ctx, id, ns, name, value)
		}); err != nil {
			t.Fatalf("set prop: %v", err)
		}
	}

	t.Run("SetThenReadRoundTrips", func(t *testing.T) {
		s, owner := factory(t)
		f := fileNode(t, s, owner)

		if got := read(t, s, f.ID); len(got) != 0 {
			t.Fatalf("a fresh file has %d dead properties", len(got))
		}
		set(t, s, f.ID, "one")

		got := read(t, s, f.ID)
		if len(got) != 1 {
			t.Fatalf("after setting one property, read back %d", len(got))
		}
		if got[0].Value != "one" || got[0].Space != ns || got[0].Name != name {
			t.Fatalf("property round-trip mismatch: %+v", got[0])
		}
	})

	t.Run("SetIsAnUpsert", func(t *testing.T) {
		s, owner := factory(t)
		f := fileNode(t, s, owner)

		set(t, s, f.ID, "first")
		set(t, s, f.ID, "second")

		// Overwriting the same (space, name) must replace, not append. Two rows would make a
		// PROPFIND return the property twice with different values, which clients handle
		// inconsistently — and the ON CONFLICT in the SQL makes the intent explicit.
		got := read(t, s, f.ID)
		if len(got) != 1 {
			t.Fatalf("setting the same property twice produced %d rows, want 1", len(got))
		}
		if got[0].Value != "second" {
			t.Fatalf("value = %q, want the latest write %q", got[0].Value, "second")
		}
	})

	t.Run("DistinctNamespacesAreDistinctProperties", func(t *testing.T) {
		s, owner := factory(t)
		f := fileNode(t, s, owner)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if err := tx.SetDeadProp(ctx, f.ID, ns, name, "a"); err != nil {
				return err
			}
			// Same local name, different namespace: a different property, not an overwrite.
			return tx.SetDeadProp(ctx, f.ID, "http://other.test/ns", name, "b")
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, s, f.ID); len(got) != 2 {
			t.Fatalf("two namespaces produced %d properties, want 2", len(got))
		}
	})

	t.Run("DeleteRemovesOnlyThatProperty", func(t *testing.T) {
		s, owner := factory(t)
		f := fileNode(t, s, owner)

		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if err := tx.SetDeadProp(ctx, f.ID, ns, "keep", "v"); err != nil {
				return err
			}
			return tx.SetDeadProp(ctx, f.ID, ns, "drop", "v")
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.DeleteDeadProp(ctx, f.ID, ns, "drop")
		}); err != nil {
			t.Fatalf("delete: %v", err)
		}

		got := read(t, s, f.ID)
		if len(got) != 1 || got[0].Name != "keep" {
			t.Fatalf("deleting one property left %+v", got)
		}
	})

	t.Run("DeleteIsIdempotent", func(t *testing.T) {
		s, owner := factory(t)
		f := fileNode(t, s, owner)

		// Deleting a property that was never set is not an error: a PROPPATCH that removes a
		// property twice is a client retrying, not a mistake.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.DeleteDeadProp(ctx, f.ID, ns, "never-set")
		}); err != nil {
			t.Fatalf("deleting an absent property: %v", err)
		}
	})
}

// lockSuite covers WebDAV LOCK ownership on both backends.
//
// None of CreateLock, RefreshLock, DeleteLock, ExpireLocks or LocksForUser had any test on
// either backend — not even a PostgreSQL-only one. LOCK is a P0 requirement and its integrity
// property is ownership: only the holder may extend or release a lock. Without that, any
// authenticated user can keep somebody else's lock alive indefinitely or drop it and write
// underneath them, which defeats the point of locking at all.
//
// The rules both backends must share, and which are asserted here:
//
//	owned lock    only its holder may refresh or delete it
//	ownerless     a system lock (user_id NULL) may be released by anyone, because there is no
//	              holder to ask — this is the expiry/recovery path
//	expired       invisible to ActiveLock and not refreshable, so a stale row cannot block a file
func lockSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	future := func(d time.Duration) time.Time { return time.Now().Add(d) }

	// drainExpired removes any expired lock the case created.
	//
	// ExpireLocks counts GLOBALLY, and the pre-existing LockExpiry case asserts an exact count of
	// 1. A case here that leaves an expired lock behind therefore breaks that one — which is how
	// this leak was found: a full-suite run failed while each case passed in isolation.
	drainExpired := func(t *testing.T, s store.Store) {
		t.Helper()
		t.Cleanup(func() {
			_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ExpireLocks(ctx)
				return err
			})
		})
	}

	// otherUser creates a distinct account, used as the lock holder so the "only the holder"
	// cases have a real second principal rather than reusing the owner.
	otherUser := func(t *testing.T, s store.Store, name string) uuid.UUID {
		t.Helper()
		u, err := upsertUser(s, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "lock-" + name,
			Username: name, Email: name + "@example.test", Role: "user",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return u.ID
	}

	// mkLocked creates a file and puts a lock on it owned by `holder`.
	mkLocked := func(t *testing.T, s store.Store, owner, holder uuid.UUID,
		token string, expires time.Time) store.Node {
		t.Helper()
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "locked-"+token+".txt", 10)
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.CreateLock(ctx, store.Lock{
				Token: token, NodeID: f.ID, UserID: &holder,
				OwnerXML: "<owner>" + token + "</owner>", Depth: "0",
				TimeoutSec: 3600, CreatedAt: time.Now(), ExpiresAt: expires,
			})
		})
		if err != nil {
			t.Fatalf("create lock %q: %v", token, err)
		}
		return f
	}

	t.Run("OnlyTheHolderCanDelete", func(t *testing.T) {
		s, owner := factory(t)
		holder := otherUser(t, s, "lock-holder")
		f := mkLocked(t, s, owner, holder, "tok-own", future(time.Hour))

		// A different user must not be able to release it.
		var removed bool
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			ok, err := tx.DeleteLock(ctx, f.ID, "tok-own", owner)
			removed = ok
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatal("a user who does not hold the lock deleted it. That lets one client unlock a " +
				"file another is editing and write underneath it, which is the failure LOCK exists " +
				"to prevent.")
		}

		// The holder can.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			ok, err := tx.DeleteLock(ctx, f.ID, "tok-own", holder)
			removed = ok
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatal("the lock holder could not delete its own lock")
		}
	})

	t.Run("DeleteReportsWhetherItRemovedSomething", func(t *testing.T) {
		s, owner := factory(t)
		holder := otherUser(t, s, "lock-holder2")
		f := mkLocked(t, s, owner, holder, "tok-once", future(time.Hour))

		var first, second bool
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			ok, err := tx.DeleteLock(ctx, f.ID, "tok-once", holder)
			first = ok
			return err
		})
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			ok, err := tx.DeleteLock(ctx, f.ID, "tok-once", holder)
			second = ok
			return err
		})
		if !first || second {
			t.Fatalf("delete reported %v then %v, want true then false. The bool is how an "+
				"ownership mismatch is told apart from success, so it has to be accurate.", first, second)
		}
	})

	t.Run("OnlyTheHolderCanRefresh", func(t *testing.T) {
		s, owner := factory(t)
		holder := otherUser(t, s, "lock-holder3")
		f := mkLocked(t, s, owner, holder, "tok-ext", future(time.Hour))

		// A stranger's refresh must fail: otherwise anyone can keep a lock alive forever and
		// block the real owner from ever editing the file again.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.RefreshLock(ctx, f.ID, "tok-ext", owner, future(2*time.Hour))
			return rerr
		})
		if err == nil {
			t.Fatal("a non-holder refreshed the lock, which lets one client extend another's lock " +
				"indefinitely")
		}

		// The holder can, and the expiry actually moves.
		var got store.Lock
		target := future(3 * time.Hour)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			l, rerr := tx.RefreshLock(ctx, f.ID, "tok-ext", holder, target)
			got = l
			return rerr
		}); err != nil {
			t.Fatalf("the holder could not refresh: %v", err)
		}
		// Compared with a tolerance, not exactly: PostgreSQL stores timestamptz to microsecond
		// precision and rounds the nanoseconds Go carries, so an exact comparison fails on the
		// PostgreSQL backend while passing in memory. The suite has to state what the contract
		// actually is — the expiry moves to the requested time — rather than a storage detail.
		if d := got.ExpiresAt.Sub(target); d > time.Millisecond || d < -time.Millisecond {
			t.Fatalf("refresh returned expiry %v, want %v (off by %v)", got.ExpiresAt, target, d)
		}
	})

	t.Run("OwnerlessLockIsReleasableByAnyone", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "system-locked.txt", 10)

		// A system lock has no holder to ask, so anyone with write access may release it. This
		// is the recovery path: a lock whose owner row was removed must not strand the file.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.CreateLock(ctx, store.Lock{
				Token: "sys", NodeID: f.ID, UserID: nil,
				Depth: "0", TimeoutSec: 3600, CreatedAt: time.Now(), ExpiresAt: future(time.Hour),
			})
		}); err != nil {
			t.Fatal(err)
		}

		var removed bool
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			ok, err := tx.DeleteLock(ctx, f.ID, "sys", owner)
			removed = ok
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatal("an ownerless lock could not be released, so a lock without a holder would " +
				"strand the file permanently")
		}
	})

	t.Run("ExpiredLockIsNotActiveAndNotRefreshable", func(t *testing.T) {
		s, owner := factory(t)
		holder := otherUser(t, s, "lock-holder4")
		f := mkLocked(t, s, owner, holder, "tok-old", future(-time.Minute))
		drainExpired(t, s)

		// ActiveLock must not report it: an expired lock that still blocks a file is worse than
		// no lock, because the client that held it is gone and cannot release it.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, lerr := tx.ActiveLock(ctx, f.ID)
			return lerr
		})
		if err == nil {
			t.Fatal("an expired lock is still reported as active")
		}

		// And it cannot be refreshed back to life.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.RefreshLock(ctx, f.ID, "tok-old", holder, future(time.Hour))
			return rerr
		}); err == nil {
			t.Fatal("an expired lock was refreshed back into existence")
		}
	})

	t.Run("LocksForUserListsOnlyLiveOwnedLocks", func(t *testing.T) {
		s, owner := factory(t)
		holder := otherUser(t, s, "lock-holder5")
		live := mkLocked(t, s, owner, holder, "tok-live", future(time.Hour))
		mkLocked(t, s, owner, holder, "tok-expired", future(-time.Minute))
		drainExpired(t, s)
		other := otherUser(t, s, "lock-other")
		mkLocked(t, s, owner, other, "tok-theirs", future(time.Hour))

		var locks []store.Lock
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			l, err := tx.LocksForUser(ctx, holder)
			locks = l
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// One live lock; the expired one and the other user's must not appear. A cleanup worker
		// acting on this list would otherwise try to release locks that are not the user's.
		if len(locks) != 1 {
			t.Fatalf("LocksForUser returned %d locks, want 1", len(locks))
		}
		if locks[0].NodeID != live.ID {
			t.Fatalf("LocksForUser returned the wrong node")
		}
	})

	t.Run("SecondLockOnALockedNodeConflicts", func(t *testing.T) {
		s, owner := factory(t)
		holder := otherUser(t, s, "lock-holder6")
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "contested.txt", 10)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.CreateLock(ctx, store.Lock{
				Token: "first", NodeID: f.ID, UserID: &holder,
				Depth: "0", TimeoutSec: 3600, CreatedAt: time.Now(), ExpiresAt: future(time.Hour),
			})
		}); err != nil {
			t.Fatal(err)
		}

		// A second lock on the same node must fail, and the check has to be in the same statement
		// as the insert or two racing clients both win.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.CreateLock(ctx, store.Lock{
				Token: "second", NodeID: f.ID, UserID: &holder,
				Depth: "0", TimeoutSec: 3600, CreatedAt: time.Now(), ExpiresAt: future(time.Hour),
			})
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a second lock on a locked node: want ErrConflict, got %v", err)
		}
	})
}

// groupSuite covers group identity and membership on both backends.
//
// The whole group surface had no test on either backend, and it is not decorative: ACL evaluation
// resolves a grant whose subject is a group by asking whether the caller is a member, so a
// divergence here grants or withholds access. Nothing renders wrong; the wrong people simply get
// in.
func groupSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	newUser := func(t *testing.T, s store.Store, name string) uuid.UUID {
		t.Helper()
		u, err := upsertUser(s, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "grp-" + name,
			Username: name, Email: name + "@example.test", Role: "user",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return u.ID
	}

	createGroup := func(t *testing.T, s store.Store, name string) store.Group {
		t.Helper()
		var g store.Group
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.CreateGroup(ctx, store.Group{Name: name, DisplayName: name + " team"})
			g = out
			return err
		}); err != nil {
			t.Fatalf("create group %q: %v", name, err)
		}
		return g
	}

	addMember := func(t *testing.T, s store.Store, gid, uid uuid.UUID, role string) {
		t.Helper()
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.AddGroupMember(ctx, gid, uid, role)
		}); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}

	t.Run("CreateGroupIsIdempotentByName", func(t *testing.T) {
		s, _ := factory(t)
		first := createGroup(t, s, "engineering")
		second := createGroup(t, s, "engineering")

		// Creating a group that already exists returns it rather than making a second one: the
		// IdP syncs groups by name, and a duplicate would split membership between two ids.
		if first.ID != second.ID {
			t.Fatalf("creating %q twice produced two groups (%s and %s), so membership would "+
				"split between them", "engineering", first.ID, second.ID)
		}
	})

	t.Run("ResolveByNameAndID", func(t *testing.T) {
		s, _ := factory(t)
		g := createGroup(t, s, "research")

		var byName, byID store.Group
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var e1 error
			byName, e1 = tx.GroupByName(ctx, "research")
			return e1
		}); err != nil {
			t.Fatalf("by name: %v", err)
		}
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var e1 error
			byID, e1 = tx.GroupByID(ctx, g.ID)
			return e1
		}); err != nil {
			t.Fatalf("by id: %v", err)
		}
		if byName.ID != g.ID || byID.Name != "research" {
			t.Fatalf("resolvers disagree: byName=%+v byID=%+v", byName, byID)
		}

		// A name that does not exist is a refusal, not an empty group: a caller that treats the
		// zero value as "found" would grant access to nobody's group.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.GroupByName(ctx, "does-not-exist")
			return err
		}); err == nil {
			t.Fatal("resolving a group name that does not exist succeeded")
		}
	})

	t.Run("MembershipIsVisibleFromBothSides", func(t *testing.T) {
		s, _ := factory(t)
		g := createGroup(t, s, "support")
		alice := newUser(t, s, "alice")
		addMember(t, s, g.ID, alice, "editor")

		// From the user: which groups is this account in?
		var groups []store.Group
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.GroupsForUser(ctx, alice)
			groups = out
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].ID != g.ID {
			t.Fatalf("GroupsForUser returned %+v, want just %s", groups, g.ID)
		}

		// From the group: who is in it? Both directions must agree, because ACL evaluation asks
		// the first question while an admin screen asks the second.
		var members []store.GroupMember
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.GroupMembers(ctx, g.ID)
			members = out
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(members) != 1 || members[0].UserID != alice {
			t.Fatalf("GroupMembers returned %+v, want just %s", members, alice)
		}
	})

	t.Run("AddMemberIsIdempotent", func(t *testing.T) {
		s, _ := factory(t)
		g := createGroup(t, s, "ops")
		bob := newUser(t, s, "bob")

		addMember(t, s, g.ID, bob, "viewer")
		addMember(t, s, g.ID, bob, "editor") // role change, not a duplicate row

		var members []store.GroupMember
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.GroupMembers(ctx, g.ID)
			members = out
			return err
		})
		// Two rows would make a member count wrong and a "remove" incomplete.
		if len(members) != 1 {
			t.Fatalf("adding the same member twice produced %d rows, want 1", len(members))
		}
		if members[0].Role != "editor" {
			t.Fatalf("role = %q, want the latest %q — the second add is a role change", members[0].Role, "editor")
		}
	})

	t.Run("RemoveMemberRevokesAccessFromBothSides", func(t *testing.T) {
		s, _ := factory(t)
		g := createGroup(t, s, "temp")
		carol := newUser(t, s, "carol")
		dave := newUser(t, s, "dave")
		addMember(t, s, g.ID, carol, "viewer")
		addMember(t, s, g.ID, dave, "viewer")

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RemoveGroupMember(ctx, g.ID, carol)
		}); err != nil {
			t.Fatalf("remove: %v", err)
		}

		var groups []store.Group
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.GroupsForUser(ctx, carol)
			groups = out
			return err
		})
		// A removed member must stop resolving, or the ACL grant the group carries keeps applying
		// to somebody who was explicitly taken out of it.
		if len(groups) != 0 {
			t.Fatalf("a removed member still resolves %d group(s); access granted through the "+
				"group would still apply", len(groups))
		}

		// The other member is untouched.
		var remaining []store.GroupMember
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.GroupMembers(ctx, g.ID)
			remaining = out
			return err
		})
		if len(remaining) != 1 || remaining[0].UserID != dave {
			t.Fatalf("removing one member disturbed another: %+v", remaining)
		}
	})

	t.Run("RemovingANonMemberIsHarmless", func(t *testing.T) {
		s, _ := factory(t)
		g := createGroup(t, s, "nobody")
		erin := newUser(t, s, "erin")

		// Removing somebody who was never added is not an error: an IdP sync that reconciles
		// membership will issue removals for accounts that already left.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RemoveGroupMember(ctx, g.ID, erin)
		}); err != nil {
			t.Fatalf("removing a non-member failed: %v", err)
		}
	})
}

// credentialSuite covers session and app-password revocation on both backends.
//
// None of RevokeSession, RevokeUserSessions, RevokeAppPassword, RevokeUserAppPasswords,
// AppPasswordByHash or TouchAppPassword had a direct test on either backend. They were exercised
// only through ApplyIdPRole and DisableUser, which is not the same thing: those test "a downgrade
// revokes", not "revocation actually stops authentication".
//
// The property that matters is end to end: after revocation, the credential must no longer
// authenticate. A revocation that sets a column but leaves the lookup returning the row is a
// credential that keeps working — for a WebDAV app password, that means months.
func credentialSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	newUser := func(t *testing.T, s store.Store, name string) uuid.UUID {
		t.Helper()
		u, err := upsertUser(s, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "cred-" + name,
			Username: name, Email: name + "@example.test", Role: "user",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return u.ID
	}

	// mkSession returns the jti it created.
	//
	// The sessions table keys on a uuid, so a readable label like "jti-one" is rejected. The
	// caller gets the generated id back rather than choosing it — which is also how production
	// works, since the jti comes from the ticket.
	mkSession := func(t *testing.T, s store.Store, uid uuid.UUID) string {
		t.Helper()
		jti := uuid.New().String()
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.CreateSession(ctx, store.Session{
				ID: jti, UserID: uid, ExpiresAt: time.Now().Add(time.Hour),
				IP: "127.0.0.1", UserAgent: "test", AuthMethod: "oidc",
			})
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
		return jti
	}

	live := func(t *testing.T, s store.Store, jti string) bool {
		t.Helper()
		var ok bool
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			v, err := tx.SessionLive(ctx, jti)
			ok = v
			return err
		}); err != nil {
			t.Fatalf("session live: %v", err)
		}
		return ok
	}

	mkPassword := func(t *testing.T, s store.Store, uid uuid.UUID, label string, hash []byte) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.CreateAppPassword(ctx, store.AppPassword{
				ID: id, UserID: uid, Label: label, Prefix: "wc_" + label,
				TokenHash: hash, Scope: "rw", CreatedAt: time.Now(),
			})
		}); err != nil {
			t.Fatalf("create app password: %v", err)
		}
		return id
	}

	t.Run("RevokeSessionStopsItAuthenticating", func(t *testing.T) {
		s, owner := factory(t)
		jti := mkSession(t, s, owner)

		if !live(t, s, jti) {
			t.Fatal("a freshly created session is not live")
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeSession(ctx, jti)
		}); err != nil {
			t.Fatal(err)
		}

		// The whole point: SessionLive must say no. A revocation that only stamps a column is a
		// logout that does not log anybody out.
		if live(t, s, jti) {
			t.Fatal("a revoked session still reports itself live, so logging out would not end " +
				"the session")
		}
	})

	t.Run("RevokeSessionLeavesOtherSessionsAlive", func(t *testing.T) {
		s, owner := factory(t)
		a := mkSession(t, s, owner)
		b := mkSession(t, s, owner)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeSession(ctx, a)
		}); err != nil {
			t.Fatal(err)
		}

		// Logging out of one device must not sign the user out everywhere; that is what
		// RevokeUserSessions is for, and conflating them would make a single logout destructive.
		if !live(t, s, b) {
			t.Fatal("revoking one session ended another")
		}
	})

	t.Run("RevokeUserSessionsStopsEverySessionOfThatUser", func(t *testing.T) {
		s, owner := factory(t)
		other := newUser(t, s, "bystander")
		u1 := mkSession(t, s, owner)
		u2 := mkSession(t, s, owner)
		o1 := mkSession(t, s, other)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeUserSessions(ctx, owner.String())
		}); err != nil {
			t.Fatal(err)
		}

		if live(t, s, u1) || live(t, s, u2) {
			t.Fatal("a user-wide revocation left one of that user's sessions live")
		}
		// And it must be scoped: revoking one account must not sign out the instance.
		if !live(t, s, o1) {
			t.Fatal("revoking one user's sessions ended another user's session")
		}
	})

	t.Run("RevokeUserSessionsRejectsAMalformedID", func(t *testing.T) {
		s, _ := factory(t)
		// A malformed id must be a refusal rather than a silent no-op that leaves every session
		// alive while the caller believes it revoked them.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeUserSessions(ctx, "not-a-uuid")
		})
		if err == nil {
			t.Fatal("revoking a malformed user id succeeded, so a caller with a bad id would " +
				"believe every session was killed when none were")
		}
	})

	t.Run("RevokeAppPasswordStopsItAuthenticating", func(t *testing.T) {
		s, owner := factory(t)
		hash := []byte{1, 2, 3, 4}
		id := mkPassword(t, s, owner, "laptop", hash)

		var found store.AppPassword
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			p, err := tx.AppPasswordByHash(ctx, hash)
			found = p
			return err
		}); err != nil {
			t.Fatalf("lookup before revoke: %v", err)
		}
		if found.RevokedAt != nil {
			t.Fatal("a fresh app password is already marked revoked")
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeAppPassword(ctx, id)
		}); err != nil {
			t.Fatal(err)
		}

		// The row still resolves — WebDAV auth reads it to find out WHY it is refused — but it
		// must carry a revocation timestamp, which is what the auth layer checks.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			p, err := tx.AppPasswordByHash(ctx, hash)
			found = p
			return err
		}); err != nil {
			t.Fatalf("after revoking, the row should still resolve so auth can report it as "+
				"revoked rather than unknown: %v", err)
		}
		if found.RevokedAt == nil {
			t.Fatal("a revoked app password is not marked revoked, so a WebDAV client would keep " +
				"authenticating with it — potentially for months")
		}
	})

	t.Run("RevokeUserAppPasswordsCountsAndScopes", func(t *testing.T) {
		s, owner := factory(t)
		other := newUser(t, s, "unrelated")
		mkPassword(t, s, owner, "one", []byte{1})
		mkPassword(t, s, owner, "two", []byte{2})
		otherID := mkPassword(t, s, other, "theirs", []byte{3})

		var revoked int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.RevokeUserAppPasswords(ctx, owner)
			revoked = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// The count is recorded in the audit entry for a disable, so it has to be accurate rather
		// than "however many rows we felt like touching".
		if revoked != 2 {
			t.Fatalf("revoked %d passwords for an account with 2, want 2", revoked)
		}

		// The other account's credential is untouched: disabling one user must not break another's
		// WebDAV clients.
		var theirs store.AppPassword
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			p, err := tx.AppPasswordByHash(ctx, []byte{3})
			theirs = p
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if theirs.RevokedAt != nil || theirs.ID != otherID {
			t.Fatal("revoking one user's app passwords affected another user's")
		}
	})

	t.Run("TouchAppPasswordRecordsUse", func(t *testing.T) {
		s, owner := factory(t)
		hash := []byte{9, 9}
		id := mkPassword(t, s, owner, "phone", hash)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.TouchAppPassword(ctx, id, "10.0.0.7")
		}); err != nil {
			t.Fatal(err)
		}

		var got store.AppPassword
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			p, err := tx.AppPasswordByHash(ctx, hash)
			got = p
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// Last-used is how an owner spots a credential they no longer recognise. An untracked
		// one is indistinguishable from an unused one.
		if got.LastUsed == nil {
			t.Fatal("touching an app password did not record last_used, so an owner cannot tell " +
				"a live credential from an abandoned one")
		}
		// Compared by prefix, not exactly: PostgreSQL stores this as inet, whose text form for a
		// host address is "10.0.0.7/32". The contract is "the address of the last use is
		// recorded", not which notation the backend renders it in — asserting the literal would
		// encode one backend's representation as the interface.
		if !strings.HasPrefix(got.LastIP, "10.0.0.7") {
			t.Fatalf("last_used_ip = %q, want the address of the last use", got.LastIP)
		}
	})
}

// userLookupSuite covers the three ways an account is resolved from an external string.
//
// All three had no test on either backend, and each is on an authorization path: UserByIdPSubject
// resolves a login to an account, while UserByUsername and UserByEmail resolve the SUBJECT of an
// ACL grant when an owner grants access to "alice" or "alice@example.test". A divergence here
// either refuses a legitimate login or grants to the wrong account.
//
// The interesting property is case handling. The in-memory backend compares with
// strings.EqualFold; PostgreSQL compares on a citext column. Those agree only because the column
// type was chosen to match the Go comparison — a detail nothing enforced until now, and one that
// would silently invert if the column were ever changed to text.
func userLookupSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	newUser := func(t *testing.T, s store.Store, username, email string) store.User {
		t.Helper()
		u, err := upsertUser(s, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "lookup-" + username,
			Username: username, Email: email, Role: "user",
		})
		if err != nil {
			t.Fatalf("create %s: %v", username, err)
		}
		return u
	}

	lookup := func(t *testing.T, s store.Store, fn func(ctx context.Context, tx store.Tx) (store.User, error)) (store.User, error) {
		t.Helper()
		var out store.User
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			u, err := fn(ctx, tx)
			out = u
			return err
		})
		return out, err
	}

	t.Run("ResolvesByExactUsernameAndEmail", func(t *testing.T) {
		s, _ := factory(t)
		// Names and addresses here are prefixed to keep them unique across the whole shared suite.
		// The group suite already creates an "erin", and upsertUser keys on the IdP identity, so
		// a second "erin@example.test" would silently be the same account under a different
		// subject — which is how this case failed in the full run while passing alone.
		want := newUser(t, s, "lu-dana", "lu-dana@example.test")

		byName, err := lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByUsername(ctx, "lu-dana")
		})
		if err != nil {
			t.Fatalf("by username: %v", err)
		}
		if byName.ID != want.ID {
			t.Fatalf("UserByUsername returned %s, want %s", byName.ID, want.ID)
		}

		byEmail, err := lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByEmail(ctx, "lu-dana@example.test")
		})
		if err != nil {
			t.Fatalf("by email: %v", err)
		}
		if byEmail.ID != want.ID {
			t.Fatalf("UserByEmail returned %s, want %s", byEmail.ID, want.ID)
		}
	})

	t.Run("UsernameAndEmailMatchCaseInsensitively", func(t *testing.T) {
		s, _ := factory(t)
		want := newUser(t, s, "lu-erin", "lu-erin@example.test")

		// Both directions matter. A grant written as "Erin" must still resolve to erin, or an
		// owner typing a name with a capital letter silently grants to nobody.
		byName, err := lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByUsername(ctx, "LU-ERIN")
		})
		if err != nil {
			t.Fatalf("username lookup is case-sensitive: %v. The in-memory backend compares with "+
				"EqualFold and PostgreSQL relies on citext, so the two agree only while the column "+
				"type matches the Go comparison.", err)
		}
		if byName.ID != want.ID {
			t.Fatalf("case-insensitive username resolved the wrong account")
		}

		byEmail, err := lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByEmail(ctx, "Lu-Erin@Example.Test")
		})
		if err != nil {
			t.Fatalf("email lookup is case-sensitive: %v", err)
		}
		if byEmail.ID != want.ID {
			t.Fatalf("case-insensitive email resolved the wrong account")
		}
	})

	t.Run("UnknownUsernameIsNotFound", func(t *testing.T) {
		s, _ := factory(t)

		_, err := lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByUsername(ctx, "nobody-at-all")
		})
		// A lookup that returned the zero User with a nil error would be catastrophic at the ACL
		// call site: aclmanage.go falls back to UserByEmail when the username fails, and both
		// returning a zero user would grant to the empty uuid.
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown username: want ErrNotFound, got %v", err)
		}
	})

	t.Run("ResolvesByIdPSubjectAndScopesByIssuer", func(t *testing.T) {
		s, owner := factory(t)
		rootOf(t, s, owner)

		u, err := upsertUser(s, store.UpsertUser{
			Issuer: "https://idp-a.test", Subject: "shared-subject",
			Username: "frank", Email: "frank@example.test", Role: "user",
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err := lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByIdPSubject(ctx, "https://idp-a.test", "shared-subject")
		})
		if err != nil {
			t.Fatalf("by subject: %v", err)
		}
		if got.ID != u.ID {
			t.Fatalf("UserByIdPSubject returned the wrong account")
		}

		// The issuer is part of the key. Two IdPs can issue the same subject string, and matching
		// on subject alone would let a user from one IdP log in as a user of another — the classic
		// subject-confusion bug.
		_, err = lookup(t, s, func(ctx context.Context, tx store.Tx) (store.User, error) {
			return tx.UserByIdPSubject(ctx, "https://idp-b.test", "shared-subject")
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the same subject from a different issuer resolved: want ErrNotFound, got %v. "+
				"Subject alone is not an identity — it is only unique within an issuer.", err)
		}
	})
}

// shareQuerySuite covers resolving and editing shares.
//
// UpdateShare, ShareByTokenHash, SharesByOwner and SharesForNode had no test on either backend.
// RevokeShare did, but only through the service layer.
//
// The property that matters is that revocation actually stops a link: a revoked share must still
// RESOLVE (so the handler can report "this link is no longer available" rather than "unknown"), but
// the resolver that decides whether it is usable must see the revocation. A share that keeps
// working after revocation is a leaked link nobody can turn off.
func shareQuerySuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	mkShare := func(t *testing.T, s store.Store, owner, nodeID uuid.UUID, label string) (store.Share, []byte) {
		t.Helper()
		// Unique token per share so parallel cases cannot collide on the unique index.
		tokenHash := []byte("token-" + label + "-" + uuid.New().String())
		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, err := tx.CreateShare(ctx, store.Share{
				NodeID: nodeID, OwnerID: owner, Kind: store.ShareLink,
				Label: label, TokenHash: tokenHash, Permissions: []string{"read"},
			})
			sh = c
			return err
		}); err != nil {
			t.Fatalf("create share %q: %v", label, err)
		}
		return sh, tokenHash
	}

	t.Run("ResolvesByTokenHash", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		want, tokenHash := mkShare(t, s, owner, r.ID, "resolve")

		var got store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			v, err := tx.ShareByTokenHash(ctx, tokenHash)
			got = v
			return err
		}); err != nil {
			t.Fatalf("resolve by token hash: %v", err)
		}
		if got.ID != want.ID {
			t.Fatalf("resolved the wrong share")
		}

		// An unknown token is ErrNotFound, not a zero share with a nil error. A caller that
		// treated the zero value as a hit would serve a link that does not exist.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.ShareByTokenHash(ctx, []byte("no-such-token"))
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown token: want ErrNotFound, got %v", err)
		}
	})

	t.Run("RevokedShareStillResolvesButIsMarkedRevoked", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh, tokenHash := mkShare(t, s, owner, r.ID, "revoked")

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeShare(ctx, sh.ID)
		}); err != nil {
			t.Fatalf("revoke: %v", err)
		}

		// It must still resolve: the handler needs the row to say "this link is no longer
		// available" rather than the identical 404 it gives an unknown token. Both are refusals,
		// but only one lets an owner be told their own link was revoked.
		var got store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			v, err := tx.ShareByTokenHash(ctx, tokenHash)
			got = v
			return err
		}); err != nil {
			t.Fatalf("a revoked share stopped resolving entirely: %v", err)
		}
		if got.RevokedAt == nil {
			t.Fatal("a revoked share is not marked revoked, so the resolver would treat it as live " +
				"and the link would keep working")
		}
	})

	t.Run("RevokingTwiceIsRefused", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh, _ := mkShare(t, s, owner, r.ID, "double")

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeShare(ctx, sh.ID)
		}); err != nil {
			t.Fatal(err)
		}
		// The second call reports ErrNotFound rather than succeeding silently: the row is already
		// revoked, and a caller that treated that as success would log a revocation that did not
		// happen at that time.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeShare(ctx, sh.ID)
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("revoking an already-revoked share: want ErrNotFound, got %v", err)
		}
	})

	t.Run("UpdateShareAppliesAnEdit", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh, _ := mkShare(t, s, owner, r.ID, "editable")

		// An owner changing the label and the expiry. The revision must be visible on read-back,
		// or an edit would appear to succeed and change nothing.
		sh.Label = "renamed"
		limit := int64(7)
		sh.MaxDownloads = nil
		sh.MaxBytesPerFile = &limit

		var updated store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.UpdateShare(ctx, sh)
			updated = out
			return err
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Label != "renamed" {
			t.Fatalf("label did not change: %q", updated.Label)
		}
		if updated.MaxBytesPerFile == nil || *updated.MaxBytesPerFile != 7 {
			t.Fatalf("per-file cap did not change: %v", updated.MaxBytesPerFile)
		}

		// And the change is persisted, not merely returned.
		var reread store.Share
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			v, err := tx.ShareByID(ctx, sh.ID)
			reread = v
			return err
		})
		if reread.Label != "renamed" {
			t.Fatal("the update was returned but not stored")
		}
	})

	t.Run("UpdateShareCannotResurrectARevokedShare", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh, _ := mkShare(t, s, owner, r.ID, "no-resurrect")

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeShare(ctx, sh.ID)
		}); err != nil {
			t.Fatal(err)
		}

		// Revocation must be final. If UpdateShare matched a revoked row, an owner could edit a
		// dead link back into service — or, worse, a client replaying a stale edit could.
		sh.Label = "still-dead"
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.UpdateShare(ctx, sh)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("editing a revoked share: want ErrNotFound, got %v", err)
		}
	})

	t.Run("NodeAndOwnerListingsDifferOnRevokedShares", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		live, _ := mkShare(t, s, owner, r.ID, "live")
		dead, _ := mkShare(t, s, owner, r.ID, "dead")

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeShare(ctx, dead.ID)
		}); err != nil {
			t.Fatal(err)
		}

		// SharesForNode feeds "who can reach this file", so a revoked share must not appear.
		var forNode []store.Share
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.SharesForNode(ctx, r.ID)
			forNode = out
			return err
		})
		if len(forNode) != 1 || forNode[0].ID != live.ID {
			t.Fatalf("SharesForNode returned %d shares, want just the live one — a revoked share "+
				"must not appear in the list of ways to reach a file", len(forNode))
		}

		// SharesByOwner feeds the owner's management screen, which has to SHOW revoked shares so
		// they can be distinguished from deleted ones. The asymmetry with SharesForNode is
		// deliberate: one answers "is this reachable", the other "what did I create".
		var byOwner []store.Share
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			out, err := tx.SharesByOwner(ctx, owner)
			byOwner = out
			return err
		})
		found := map[uuid.UUID]bool{}
		for _, sh := range byOwner {
			found[sh.ID] = true
		}
		if !found[live.ID] || !found[dead.ID] {
			t.Fatalf("SharesByOwner returned %d shares and is missing one; it must include revoked "+
				"shares so an owner can see them", len(byOwner))
		}
	})
}

// treeWalkSuite covers Descendants and AllRoots.
//
// Descendants is what a recursive operation uses to check inner protections rather than only the
// root (§4.2 says a recursive COPY or DELETE must not ignore a nested grant). AllRoots feeds
// whole-deployment operations like the restore drill. Neither had a test on either backend.
//
// The properties that matter are the ones a hand-written walk and a recursive CTE can disagree
// about: breadth-first ordering, the depth cap, and what the truncation flag means.
func treeWalkSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	walk := func(t *testing.T, s store.Store, rootID uuid.UUID, limit int) ([]store.Node, bool) {
		t.Helper()
		var (
			out   []store.Node
			trunc bool
		)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, tr, err := tx.Descendants(ctx, rootID, limit)
			out, trunc = n, tr
			return err
		}); err != nil {
			t.Fatalf("descendants: %v", err)
		}
		return out, trunc
	}

	// buildTree creates root/ with a.txt, b.txt, and sub/ containing c.txt and deep/ containing
	// d.txt — three levels, enough to exercise ordering and depth.
	buildTree := func(t *testing.T, s store.Store, owner uuid.UUID) (store.Node, map[string]store.Node) {
		t.Helper()
		r := rootOf(t, s, owner)
		nodes := map[string]store.Node{}
		var err error
		if err = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var e error
			if nodes["a.txt"], e = tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "a.txt", Size: 1,
				Mime: "text/plain", ETag: `"a"`, BlobID: newBlobHook(t), BlobHash: [32]byte{1},
			}); e != nil {
				return e
			}
			if nodes["b.txt"], e = tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "b.txt", Size: 1,
				Mime: "text/plain", ETag: `"b"`, BlobID: newBlobHook(t), BlobHash: [32]byte{2},
			}); e != nil {
				return e
			}
			if nodes["sub"], e = tx.CreateDir(ctx, store.NewNode{
				OwnerID: owner, ParentID: r.ID, Name: "sub",
			}); e != nil {
				return e
			}
			if nodes["c.txt"], e = tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: nodes["sub"].ID, Name: "c.txt", Size: 1,
				Mime: "text/plain", ETag: `"c"`, BlobID: newBlobHook(t), BlobHash: [32]byte{3},
			}); e != nil {
				return e
			}
			if nodes["deep"], e = tx.CreateDir(ctx, store.NewNode{
				OwnerID: owner, ParentID: nodes["sub"].ID, Name: "deep",
			}); e != nil {
				return e
			}
			nodes["d.txt"], e = tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: nodes["deep"].ID, Name: "d.txt", Size: 1,
				Mime: "text/plain", ETag: `"d"`, BlobID: newBlobHook(t), BlobHash: [32]byte{4},
			})
			return e
		}); err != nil {
			t.Fatalf("build tree: %v", err)
		}
		return r, nodes
	}

	t.Run("WalksTheWholeSubtreeBreadthFirst", func(t *testing.T) {
		s, owner := factory(t)
		r, _ := buildTree(t, s, owner)

		got, complete := walk(t, s, r.ID, 0)
		if !complete {
			t.Fatalf("a %d-node subtree came back incomplete at the default limit", len(got))
		}
		// Named expectations rather than a count: the factory may seed its own children under the
		// root, and asserting a literal made this fail for a reason that had nothing to do with
		// the walk.
		seen := map[string]bool{}
		for _, n := range got {
			seen[n.Name] = true
		}
		for _, want := range []string{"a.txt", "b.txt", "sub", "c.txt", "deep", "d.txt"} {
			if !seen[want] {
				t.Fatalf("the walk missed %q; got %d nodes: %v", want, len(got), namesOf(got))
			}
		}

		// Breadth-first, checked structurally rather than by index.
		//
		// A depth-first walk would interleave levels, and a caller checking inner protections in
		// order would see a nested grant after a sibling's. The SQL states the order with
		// ORDER BY depth, name, so the memory walk must match it. Asserting by index was fragile:
		// the factory seeds its own children, so any absolute position was an assumption about a
		// fixture I do not own.
		depthOf := func(n store.Node) int {
			d := 0
			cur := n
			for cur.ParentID != nil {
				d++
				var err error
				parent := store.Node{}
				_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					p, perr := tx.Node(ctx, *cur.ParentID)
					parent, err = p, perr
					return perr
				})
				if err != nil {
					cur = parent
					break
				}
				cur = parent
			}
			return d
		}
		lastDepth := 0
		for _, n := range got {
			d := depthOf(n)
			if d < lastDepth {
				t.Fatalf("node %q at depth %d came after a node at depth %d — the walk is not "+
					"breadth-first, so a caller checking inner protections sees them out of order",
					n.Name, d, lastDepth)
			}
			lastDepth = d
		}
	})

	t.Run("TruncatesAndSaysSo", func(t *testing.T) {
		s, owner := factory(t)
		r, _ := buildTree(t, s, owner)

		// The bool is `complete`, not `truncated`. A limit below the tree size must both cut the
		// result and report INCOMPLETE — the documented consequence being that a caller checking
		// inner permissions has to treat an incomplete walk as a refusal, because the node it did
		// not see may be the protected one.
		full, _ := walk(t, s, r.ID, 0)
		got, complete := walk(t, s, r.ID, 2)
		if len(got) != 2 {
			t.Fatalf("limit 2 returned %d nodes", len(got))
		}
		if complete {
			t.Fatal("a walk that stopped at the limit reported itself complete, so a caller " +
				"checking inner protections would treat a partial view as approval")
		}

		// A limit at or above the size is complete. The size is measured, not assumed, for the
		// same reason as above.
		if _, complete := walk(t, s, r.ID, len(full)); !complete {
			t.Fatalf("a limit equal to the subtree size (%d) reported itself incomplete", len(full))
		}
	})

	t.Run("ExcludesTheRootAndTrashedChildren", func(t *testing.T) {
		s, owner := factory(t)
		r, nodes := buildTree(t, s, owner)

		// Trash a file: a recursive operation must not act on something already in the trash, and
		// a walk that included it would double-count against a quota that already released it.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, nodes["a.txt"].ID)
		}); err != nil {
			t.Fatal(err)
		}

		got, _ := walk(t, s, r.ID, 0)
		for _, n := range got {
			if n.ID == r.ID {
				t.Fatal("the walk returned the root itself; descendants are the nodes BELOW it")
			}
		}
	})

	t.Run("ALeafHasNoDescendants", func(t *testing.T) {
		s, owner := factory(t)
		_, nodes := buildTree(t, s, owner)

		got, complete := walk(t, s, nodes["d.txt"].ID, 0)
		if len(got) != 0 || !complete {
			t.Fatalf("walking from a leaf file returned %d nodes, complete=%v", len(got), complete)
		}
	})

	t.Run("AllRootsReturnsEveryNamespaceRoot", func(t *testing.T) {
		s, owner := factory(t)
		rootOf(t, s, owner)

		second, err := upsertUser(s, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "roots-second",
			Username: "roots-second", Email: "roots-second@example.test", Role: "user",
		})
		if err != nil {
			t.Fatal(err)
		}
		secondRoot := rootOf(t, s, second.ID)

		var roots []store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			r, err := tx.AllRoots(ctx)
			roots = r
			return err
		}); err != nil {
			t.Fatal(err)
		}

		found := map[uuid.UUID]bool{}
		for _, r := range roots {
			found[r.ID] = true
			if !r.IsDir() || r.ParentID != nil {
				t.Fatalf("AllRoots returned a node that is not a parentless directory: %+v", r)
			}
		}
		// The drill walks every namespace, so a missing root is a silently incomplete restore.
		if !found[secondRoot.ID] {
			t.Fatalf("AllRoots returned %d roots and is missing a user's root", len(roots))
		}
	})
}

// recipientAccessSuite covers the access record for an email share.
//
// MarkRecipientAccess and RecipientsFor had no test on either backend. They record WHO opened an
// email-gated link and WHEN, which is the only durable answer to "did the recipient ever read it"
// — and the first-access timestamp is what an owner or an auditor asks about, since last-access
// alone cannot distinguish "read once" from "read on every day since".
func recipientAccessSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	const (
		first  = "first@example.test"
		second = "second@example.test"
	)

	// newEmailShare creates an email share with two invited recipients.
	newEmailShare := func(t *testing.T, s store.Store, owner uuid.UUID) store.Share {
		t.Helper()
		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			r, rerr := tx.EnsureRoot(ctx, owner)
			if rerr != nil {
				return rerr
			}
			c, cerr := tx.CreateShare(ctx, store.Share{
				NodeID: r.ID, OwnerID: owner, Kind: store.ShareEmail,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
			})
			if cerr != nil {
				return cerr
			}
			sh = c
			for _, email := range []string{first, second} {
				if err := tx.AddRecipient(ctx, store.ShareRecipient{
					ShareID: c.ID, Email: email,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("create email share: %v", err)
		}
		return sh
	}

	recipients := func(t *testing.T, s store.Store, shareID uuid.UUID) []store.ShareRecipient {
		t.Helper()
		var out []store.ShareRecipient
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			r, err := tx.RecipientsFor(ctx, shareID)
			out = r
			return err
		}); err != nil {
			t.Fatalf("recipients: %v", err)
		}
		return out
	}

	t.Run("ListsEveryRecipient", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)

		got := recipients(t, s, sh.ID)
		if len(got) != 2 {
			t.Fatalf("RecipientsFor returned %d recipients, want 2", len(got))
		}
		seen := map[string]bool{}
		for _, r := range got {
			seen[r.Email] = true
			if r.FirstAccessAt != nil {
				t.Fatalf("a freshly invited recipient already has a first-access time")
			}
		}
		if !seen[first] || !seen[second] {
			t.Fatalf("RecipientsFor returned %v, missing an invited address", seen)
		}
	})

	t.Run("FirstAccessIsRecordedOnceAndKept", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.MarkRecipientAccess(ctx, sh.ID, first)
		}); err != nil {
			t.Fatalf("mark access: %v", err)
		}

		afterFirst := recipients(t, s, sh.ID)
		var got store.ShareRecipient
		for _, r := range afterFirst {
			if r.Email == first {
				got = r
			}
		}
		if got.FirstAccessAt == nil || got.LastAccessAt == nil {
			t.Fatal("access was not recorded: both first_access_at and last_access_at should be set")
		}
		firstAt := *got.FirstAccessAt

		// A second access must move last_access_at but NOT first_access_at. The SQL does this with
		// COALESCE(first_access_at, now()); losing the original would make "when did they first
		// open it" unanswerable, which is the question an owner actually asks.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.MarkRecipientAccess(ctx, sh.ID, first)
		}); err != nil {
			t.Fatal(err)
		}

		var again store.ShareRecipient
		for _, r := range recipients(t, s, sh.ID) {
			if r.Email == first {
				again = r
			}
		}
		if again.FirstAccessAt == nil || !again.FirstAccessAt.Equal(firstAt) {
			t.Fatalf("the first-access time moved on a second visit (%v then %v). It records the "+
				"FIRST access and must not be overwritten.", firstAt, again.FirstAccessAt)
		}
	})

	t.Run("AccessIsScopedToOneRecipient", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.MarkRecipientAccess(ctx, sh.ID, first)
		}); err != nil {
			t.Fatal(err)
		}

		// The other invitee must still be untouched: an access on behalf of one recipient must not
		// mark the others as having read the share.
		for _, r := range recipients(t, s, sh.ID) {
			if r.Email == second && r.FirstAccessAt != nil {
				t.Fatal("accessing as one recipient recorded access for another")
			}
		}
	})

	t.Run("AccessForAnUninvitedAddressDoesNothing", func(t *testing.T) {
		s, owner := factory(t)
		sh := newEmailShare(t, s, owner)

		// The UPDATE matches no row, which must be a quiet no-op rather than an error or — worse —
		// a new row. Creating one would let anybody add themselves to a share's recipient list.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.MarkRecipientAccess(ctx, sh.ID, "stranger@example.test")
		}); err != nil {
			t.Fatalf("marking access for an uninvited address failed: %v", err)
		}

		if got := recipients(t, s, sh.ID); len(got) != 2 {
			t.Fatalf("after marking access for an uninvited address there are %d recipients, want "+
				"2 — it must not create a row", len(got))
		}
	})
}

// blobIdentitySuite covers BlobHash and SampleBlobHashes.
//
// BlobHash is what the restore drill uses to get the digest the metadata RECORDED, so it can
// compare restored bytes against what was written rather than against the live copy — comparing
// against the live copy would pass if the same corruption existed on both sides. SampleBlobHashes
// is what the integrity scrub uses to pick what to verify.
//
// Neither had a test on either backend. The in-memory store deliberately does not model blob
// lifecycle, so these cases are gated on whether the backend records content addresses at all.
func blobIdentitySuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID), hasBlobs bool) {
	t.Helper()

	if !hasBlobs {
		// The in-memory backend does not track blob rows: its CollectableBlobs reports that
		// rather than returning an empty list, precisely so the gap cannot be mistaken for
		// "nothing to do". Asserting the PostgreSQL semantics here would only produce a test that
		// documents the limitation, so this states it once and stops.
		t.Skip("this backend does not model blob rows; the PostgreSQL conformance run covers these")
		return
	}

	t.Run("BlobHashReturnsTheRecordedAddress", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		var hash [32]byte
		for i := range hash {
			hash[i] = byte(i + 1)
		}
		var blobID uuid.UUID
		var node store.Node
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			blobID = s.BlobIDFor(ctx, hash, 10)
			var e error
			node, e = tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "digest.txt", Size: 10,
				Mime: "text/plain", ETag: `"d"`, BlobID: blobID, BlobHash: hash,
			})
			return e
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		_ = node

		var got []byte
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			raw, err := tx.BlobHash(ctx, blobID)
			got = raw
			return err
		}); err != nil {
			t.Fatalf("BlobHash: %v", err)
		}

		// The digest must be the full SHA-256 the file was written with. The drill compares this
		// against restored bytes, so a truncated or re-encoded value would make every restore look
		// corrupted — or, worse, make a corrupted one look fine.
		if len(got) != 32 {
			t.Fatalf("BlobHash returned %d bytes, want a 32-byte SHA-256", len(got))
		}
		for i := range hash {
			if got[i] != hash[i] {
				t.Fatalf("BlobHash returned a different digest than the file was written with")
			}
		}

		// An unknown blob id is an error, not a zero digest: the drill treats a nil digest as
		// "cannot verify" and reports a problem, so returning zeros with a nil error would let it
		// silently skip a file.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.BlobHash(ctx, uuid.New())
			return err
		}); err == nil {
			t.Fatal("BlobHash on an unknown blob id succeeded; the drill would then compare " +
				"restored bytes against a zero digest")
		}
	})

	t.Run("SampleBlobHashesReturnsDistinctAddresses", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)

		// SampleBlobHashes reads the WHOLE blobs table, not this case's fixture — the table is
		// global and other cases leave rows in it. So the assertions below are about the shape of
		// the result (distinct, bounded) and about whether this case's content appears, not about
		// an absolute count. Assuming a clean table is the fixture-shape mistake this suite has
		// now produced three times.
		//
		// BlobIDFor is resolved BEFORE the transaction opens. It takes a lock of its own on
		// PostgreSQL, so calling it inside WithTx deadlocks — and the in-memory store refuses
		// nested transactions outright. The suite hung for a minute rather than failing, which is
		// why this comment is worth its length.
		contents := [][32]byte{{1}, {2}, {1}}
		ids := make([]uuid.UUID, len(contents))
		for i, h := range contents {
			ids[i] = s.BlobIDFor(context.Background(), h, 1)
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			for i, h := range contents {
				name := "s" + string(rune('a'+i)) + ".bin"
				if _, err := tx.CreateFile(ctx, store.NewFile{
					OwnerID: owner, ParentID: r.ID, Name: name, Size: 1,
					Mime: "application/octet-stream", ETag: `"x"`,
					BlobID: ids[i], BlobHash: h,
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var got [][32]byte
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			h, err := tx.SampleBlobHashes(ctx, 100)
			got = h
			return err
		}); err != nil {
			t.Fatalf("sample: %v", err)
		}
		// Distinct: the same address must not be returned twice, or the scrub would read one blob
		// repeatedly while others went unverified.
		seen := map[[32]byte]bool{}
		for _, h := range got {
			if seen[h] {
				t.Fatalf("the sample returned the same address twice (%x)", h[:4])
			}
			seen[h] = true
		}
		// And this case's two distinct contents both appear, so the sample is not filtering them
		// out for a reason of its own.
		want := [][32]byte{{1}, {2}}
		for _, w := range want {
			if !seen[w] {
				t.Fatalf("the sample is missing content %x that this case created", w[:4])
			}
		}

		// The limit must bound the pass: the scrub's whole cost model is "read at most N blobs per
		// cycle", so a sampler that ignored the limit would turn it into a full read of the store.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			h, err := tx.SampleBlobHashes(ctx, 1)
			got = h
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) > 1 {
			t.Fatalf("sample with limit 1 returned %d addresses", len(got))
		}
	})
}

// counterSuite covers IncrementDownload and IncrementUpload directly.
//
// The service layer has a concurrency test for the download cap, but it runs against the in-memory
// backend. The guard these methods implement is a WHERE clause — `download_count < max_downloads` —
// and a WHERE clause is exactly what a hand-written memory implementation can get right for the
// wrong reason, or get wrong in a way no single-threaded test notices.
//
// These cases are single-threaded on purpose: what they pin is the CONTRACT (the cap refuses the
// over-limit call, the counter and the byte total move together, a rejected call changes nothing),
// which both backends must satisfy. Actual racing lives in the service-layer test and in the
// PostgreSQL row lock.
func counterSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	mkShare := func(t *testing.T, s store.Store, owner, nodeID uuid.UUID,
		maxDownloads *int, maxUploads *int, maxTotalBytes *int64) store.Share {
		t.Helper()
		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, err := tx.CreateShare(ctx, store.Share{
				NodeID: nodeID, OwnerID: owner, Kind: store.ShareLink,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
				MaxDownloads: maxDownloads, MaxUploads: maxUploads,
				MaxTotalBytes: maxTotalBytes,
			})
			sh = c
			return err
		}); err != nil {
			t.Fatalf("create share: %v", err)
		}
		return sh
	}

	readShare := func(t *testing.T, s store.Store, id uuid.UUID) store.Share {
		t.Helper()
		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.ShareByID(ctx, id)
			sh = got
			return err
		}); err != nil {
			t.Fatalf("read share: %v", err)
		}
		return sh
	}

	t.Run("DownloadCapRefusesTheOverLimitCall", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		two := 2
		sh := mkShare(t, s, owner, r.ID, &two, nil, nil)

		for i := 0; i < 2; i++ {
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.IncrementDownload(ctx, sh.ID)
			}); err != nil {
				t.Fatalf("download %d of 2: %v", i+1, err)
			}
		}

		// The third must be refused. This is the statement that makes "usable three times" mean
		// three and not four — the whole reason the cap is a WHERE clause rather than a check in
		// Go.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.IncrementDownload(ctx, sh.ID)
		})
		if !errors.Is(err, store.ErrShareExhausted) {
			t.Fatalf("the third download through a 2-download cap: want ErrShareExhausted, got %v", err)
		}

		// And the rejected call must not have moved the counter.
		if got := readShare(t, s, sh.ID).DownloadCount; got != 2 {
			t.Fatalf("a refused download left the counter at %d, want 2 — a cap that increments "+
				"before refusing lets a share drift past its limit", got)
		}
	})

	t.Run("UploadCounterAndBytesMoveTogether", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		fifty := int64(50)
		sh := mkShare(t, s, owner, r.ID, nil, nil, &fifty)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.IncrementUpload(ctx, sh.ID, 30)
		}); err != nil {
			t.Fatalf("first upload: %v", err)
		}

		got := readShare(t, s, sh.ID)
		if got.UploadCount != 1 || got.ReceivedBytes != 30 {
			t.Fatalf("after one 30-byte upload: count=%d bytes=%d, want 1 and 30",
				got.UploadCount, got.ReceivedBytes)
		}

		// 30 + 30 crosses the 50-byte cap. Both must be untouched afterwards: a counter that
		// moved while the bytes were refused would let the count and the total disagree.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.IncrementUpload(ctx, sh.ID, 30)
		})
		if !errors.Is(err, store.ErrShareExhausted) {
			t.Fatalf("an upload that crosses the byte cap: want ErrShareExhausted, got %v", err)
		}
		got = readShare(t, s, sh.ID)
		if got.UploadCount != 1 || got.ReceivedBytes != 30 {
			t.Fatalf("a refused upload changed the totals: count=%d bytes=%d, want 1 and 30",
				got.UploadCount, got.ReceivedBytes)
		}
	})

	t.Run("RejectedUploadDoesNotInflateTheByteTotal", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		one := 1
		sh := mkShare(t, s, owner, r.ID, nil, &one, nil)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.IncrementUpload(ctx, sh.ID, 10)
		}); err != nil {
			t.Fatal(err)
		}

		// Refused by the COUNT cap, with bytes that would otherwise fit. The byte total must not
		// absorb them: the SELECT that enforces the count runs in the same statement, so a store
		// that updated bytes first and checked the count second would drift.
		err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.IncrementUpload(ctx, sh.ID, 10)
		})
		if !errors.Is(err, store.ErrShareExhausted) {
			t.Fatalf("want ErrShareExhausted, got %v", err)
		}
		if got := readShare(t, s, sh.ID).ReceivedBytes; got != 10 {
			t.Fatalf("received_bytes = %d after a refused upload, want 10", got)
		}
	})

	t.Run("NoCapMeansNoRefusal", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		sh := mkShare(t, s, owner, r.ID, nil, nil, nil)

		// A share with no caps must accept any number. An implementation that treated a nil cap as
		// zero would refuse everything.
		for i := 0; i < 20; i++ {
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.IncrementDownload(ctx, sh.ID)
			}); err != nil {
				t.Fatalf("download %d with no cap: %v", i+1, err)
			}
		}
		if got := readShare(t, s, sh.ID).DownloadCount; got != 20 {
			t.Fatalf("count = %d after 20 uncapped downloads", got)
		}
	})
}

// sweepSuite covers the three housekeeping sweeps: ExpireDownloadSessions, PruneOTPAttempts and
// the opt-in behaviour of PurgeTrash.
//
// Each has a production caller on a timer (worker.go), and each is a DELETE bounded by a time
// predicate — the shape where a hand-written memory loop and a SQL interval most easily disagree,
// as the share-rate prune already demonstrated once.
//
// The property every one of them must share is the same: delete what is older than the window,
// keep everything else, be idempotent, and treat a zero/negative window as "keep everything"
// rather than "delete everything" — a data-loss guard these sweeps all rely on.
func sweepSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	t.Run("ExpireDownloadSessionsKeepsLiveOnes", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "dl.bin", 10)

		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, err := tx.CreateShare(ctx, store.Share{
				NodeID: f.ID, OwnerID: owner, Kind: store.ShareLink,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
			})
			sh = c
			return err
		}); err != nil {
			t.Fatal(err)
		}

		// One session that expires long in the future, one already expired.
		grant := func(g string, ttl time.Duration) error {
			return s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, _, err := tx.AuthorizeDownload(ctx, store.DownloadSession{
					ShareID: sh.ID, NodeID: f.ID, GrantHash: []byte(g),
					ETag: f.ETag, ExpiresAt: time.Now().Add(ttl),
				}, false)
				return err
			})
		}
		if err := grant("live-visitor", time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := grant("stale-visitor", -time.Minute); err != nil {
			t.Fatal(err)
		}

		var removed int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.ExpireDownloadSessions(ctx)
			removed = n
			return err
		}); err != nil {
			t.Fatalf("expire: %v", err)
		}
		if removed != 1 {
			t.Fatalf("expired %d download sessions, want 1 — removing a live session would let a "+
				"Range request be charged again", removed)
		}

		// Idempotent: a worker on a timer must not report perpetual activity.
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.ExpireDownloadSessions(ctx)
			removed = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Fatalf("a second sweep expired %d more sessions, want 0", removed)
		}
	})

	t.Run("PruneOTPAttemptsKeepsRecentOnes", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, err := tx.CreateShare(ctx, store.Share{
				NodeID: r.ID, OwnerID: owner, Kind: store.ShareEmail,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
			})
			sh = c
			return err
		}); err != nil {
			t.Fatal(err)
		}

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RecordOTPAttempt(ctx, store.OTPAttempt{
				ShareID: sh.ID, Email: "prune@example.test", Success: false,
			})
		}); err != nil {
			t.Fatal(err)
		}

		// A keep window far longer than the row's age removes nothing. Pruning a row still inside
		// the throttle window would reset an attacker's failure count.
		var removed int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.PruneOTPAttempts(ctx, 24*time.Hour)
			removed = n
			return err
		}); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if removed != 0 {
			t.Fatalf("a 24-hour keep window removed %d fresh OTP attempts", removed)
		}

		// And the failure count still sees it, which is what the throttle depends on.
		var failures int
		_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.OTPFailureCount(ctx, sh.ID, "prune@example.test", time.Hour)
			failures = n
			return err
		})
		if failures != 1 {
			t.Fatalf("after a non-destructive prune the failure count is %d, want 1", failures)
		}
	})

	t.Run("PurgeTrashIsOptIn", func(t *testing.T) {
		s, owner := factory(t)
		r := rootOf(t, s, owner)
		f := mkfile(t, s, owner, r.ID, "keep-me.bin", 10)

		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.Trash(ctx, f.ID)
		}); err != nil {
			t.Fatal(err)
		}

		// Zero means "never purge", not "purge everything". This is the data-loss guard the
		// worker relies on when retention is unset, and a sweep that read zero as "delete now"
		// would empty every trash on a default configuration.
		var res store.PurgeResult
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PurgeTrash(ctx, 0)
			res = got
			return err
		}); err != nil {
			t.Fatalf("purge with zero retention: %v", err)
		}
		if res.Nodes != 0 {
			t.Fatalf("a zero retention purged %d nodes. Zero means keep forever: a deployment that "+
				"has not set a policy must not have its trash emptied.", res.Nodes)
		}
	})
}

// passwordThrottleSuite covers the SHARE-02 password-guess limit.
//
// OTP verification had a rate limit, a validity period, an attempt cap and replay protection.
// Password verification had none: a wrong password was audited, so an owner could see a burst
// afterwards, but nothing slowed it down. A share password is usually short and human-chosen,
// which makes online guessing the realistic attack.
func passwordThrottleSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID)) {
	t.Helper()

	mkShare := func(t *testing.T, s store.Store, owner uuid.UUID) store.Share {
		t.Helper()
		r := rootOf(t, s, owner)
		var sh store.Share
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			c, err := tx.CreateShare(ctx, store.Share{
				NodeID: r.ID, OwnerID: owner, Kind: store.ShareLink,
				TokenHash: []byte(uuid.New().String()), Permissions: []string{"read"},
				PasswordHash: []byte("not-a-real-hash"),
			})
			sh = c
			return err
		}); err != nil {
			t.Fatalf("create share: %v", err)
		}
		return sh
	}

	count := func(t *testing.T, s store.Store, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			got, err := tx.PasswordFailureCount(ctx, id, store.PasswordWindow)
			n = got
			return err
		}); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	record := func(t *testing.T, s store.Store, id uuid.UUID, ok bool) {
		t.Helper()
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RecordPasswordAttempt(ctx, id, "127.0.0.1", ok)
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	t.Run("CountsFailuresNotSuccesses", func(t *testing.T) {
		s, owner := factory(t)
		sh := mkShare(t, s, owner)

		record(t, s, sh.ID, false)
		record(t, s, sh.ID, false)
		record(t, s, sh.ID, true)

		// Successes must not count toward the cap, or a visitor who keeps entering the right
		// password would eventually lock themselves out of a link they can open.
		if n := count(t, s, sh.ID); n != 2 {
			t.Fatalf("failure count = %d, want 2", n)
		}
	})

	t.Run("LimitIsReachedAtTheConfiguredAttempts", func(t *testing.T) {
		s, owner := factory(t)
		sh := mkShare(t, s, owner)

		for i := 0; i < store.PasswordMaxAttempts; i++ {
			if n := count(t, s, sh.ID); n != i {
				t.Fatalf("before guess %d the count is %d", i+1, n)
			}
			record(t, s, sh.ID, false)
		}
		// At exactly the cap the caller refuses; the constant and the counter have to agree, or the
		// limit is off by one in whichever direction the code happens to compare.
		if n := count(t, s, sh.ID); n != store.PasswordMaxAttempts {
			t.Fatalf("after %d failures the count is %d", store.PasswordMaxAttempts, n)
		}
	})

	t.Run("CountIsScopedToOneShare", func(t *testing.T) {
		s, owner := factory(t)
		a := mkShare(t, s, owner)
		b := mkShare(t, s, owner)

		for i := 0; i < store.PasswordMaxAttempts; i++ {
			record(t, s, a.ID, false)
		}
		// A different link must not inherit another's failures: one link being hammered must not
		// lock every other link the owner created.
		if n := count(t, s, b.ID); n != 0 {
			t.Fatalf("an unrelated share reports %d failures", n)
		}
	})

	t.Run("PruneRemovesOnlyOldAttempts", func(t *testing.T) {
		s, owner := factory(t)
		sh := mkShare(t, s, owner)
		record(t, s, sh.ID, false)

		var removed int
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := tx.PrunePasswordAttempts(ctx, 24*time.Hour)
			removed = n
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// A keep window longer than the row's age must remove nothing, or pruning would reset an
		// attacker's failure count.
		if removed != 0 {
			t.Fatalf("a 24-hour keep window removed %d fresh attempts", removed)
		}
		if n := count(t, s, sh.ID); n != 1 {
			t.Fatalf("after a non-destructive prune the count is %d, want 1", n)
		}
	})
}

// usageSuite covers the reported usage figure.
//
// UserByID on the in-memory backend returned the stored struct without its usage, because the
// memory store tracks bytes in a separate map. PostgreSQL reads the real column. The two backends
// therefore disagreed on a number an admin screen displays and a quota calculation uses — and
// nothing noticed until an end-to-end test could not see a 24-byte delivery it had just made.
//
// The property is simple and worth stating: after a write, the usage the store REPORTS must have
// moved by the bytes written.
func usageSuite(t *testing.T, factory func(t *testing.T) (store.Store, uuid.UUID), setQuota func(t *testing.T, s store.Store, owner uuid.UUID, limit int64)) {
	t.Helper()

	usageOf := func(t *testing.T, s store.Store, id uuid.UUID) int64 {
		t.Helper()
		var used int64
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			u, err := tx.UserByID(ctx, id)
			used = u.UsedBytes
			return err
		}); err != nil {
			t.Fatalf("read user: %v", err)
		}
		return used
	}

	t.Run("ReportsUsageAfterAWrite", func(t *testing.T) {
		// A quota must be set, or the memory store skips its accounting entirely — with no limit
		// there is nothing to compare against, so it never records usage.
		s, owner := factory(t)
		if setQuota != nil {
			// A limit must be set: the memory store skips accounting entirely when there is no
			// quota to compare against, so an unlimited owner would show zero usage on that
			// backend and the real figure on PostgreSQL.
			setQuota(t, s, owner, 1<<20)
		}
		r := rootOf(t, s, owner)

		before := usageOf(t, s, owner)
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "usage.bin", Size: 4096,
				Mime: "application/octet-stream", ETag: `"u"`,
				BlobID: newBlobHook(t), BlobHash: [32]byte{9},
			})
			return err
		}); err != nil {
			t.Fatalf("create: %v", err)
		}

		after := usageOf(t, s, owner)
		if after-before != 4096 {
			t.Fatalf("reported usage moved by %d after a 4096-byte write. A store that creates the "+
				"file but does not report the bytes is invisible to every admin view and to the "+
				"next quota check.", after-before)
		}
	})

	// Every way of LOOKING UP a user has to report the same figures.
	//
	// Usage and limit are what an admin screen and /api/me display, and they live outside the
	// stored identity struct. One backend filled them on one lookup path and not on the others, so
	// the number depended on WHICH METHOD had answered — /api/me showed zero while the ACL path
	// showed the truth. A single-field fix would have left the next path wrong.
	t.Run("EveryLookupReportsTheSameFigures", func(t *testing.T) {
		s, owner := factory(t)
		if setQuota != nil {
			setQuota(t, s, owner, 1<<20)
		}
		r := rootOf(t, s, owner)

		// Give the account a known quota and usage, then read it back four ways.
		var wantUsed, wantQuota int64
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.CreateFile(ctx, store.NewFile{
				OwnerID: owner, ParentID: r.ID, Name: "figure.bin", Size: 1000,
				Mime: "application/octet-stream", ETag: `"f"`,
				BlobID: newBlobHook(t), BlobHash: [32]byte{7},
			}); err != nil {
				return err
			}
			u, err := tx.UserByID(ctx, owner)
			wantUsed, wantQuota = u.UsedBytes, u.QuotaBytes
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if wantUsed != 1000 {
			t.Fatalf("UserByID reports used_bytes=%d after a 1000-byte write", wantUsed)
		}
		if wantQuota != 1<<20 {
			t.Fatalf("UserByID reports quota_bytes=%d, want the configured %d", wantQuota, int64(1<<20))
		}

		// Read the owner's OWN identity so the lookups below address a real account rather than
		// guessing fixture values. The fixture is not ours, and asserting its shape has already
		// produced three failures in this suite.
		var self store.User
		if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			u, err := tx.UserByID(ctx, owner)
			self = u
			return err
		}); err != nil {
			t.Fatal(err)
		}

		lookups := map[string]func(context.Context, store.Tx) (store.User, error){
			"UserByID": func(ctx context.Context, tx store.Tx) (store.User, error) {
				return tx.UserByID(ctx, owner)
			},
			"UserByUsername": func(ctx context.Context, tx store.Tx) (store.User, error) {
				return tx.UserByUsername(ctx, self.Username)
			},
			"UserByEmail": func(ctx context.Context, tx store.Tx) (store.User, error) {
				return tx.UserByEmail(ctx, self.Email)
			},
			"UserByIdPSubject": func(ctx context.Context, tx store.Tx) (store.User, error) {
				return tx.UserByIdPSubject(ctx, self.IdPIssuer, self.IdPSubject)
			},
		}
		// An account created outside an IdP — a breakglass or local account — has no subject, so
		// that lookup cannot resolve it. That is a real state, not a failure, and skipping it here
		// keeps the case about the figures rather than about provisioning.
		if self.IdPSubject == "" {
			delete(lookups, "UserByIdPSubject")
		}
		{
		}
		for name, fn := range lookups {
			var got store.User
			err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				u, err := fn(ctx, tx)
				got = u
				return err
			})
			if err != nil {
				t.Fatalf("%s could not resolve the account it was given: %v. Every lookup must "+
					"reach the same account, or one screen shows a user another cannot find.", name, err)
			}
			if got.UsedBytes != wantUsed || got.QuotaBytes != wantQuota {
				t.Fatalf("%s reports used=%d quota=%d while UserByID reports %d and %d. The figure "+
					"must not depend on which method answered the question.",
					name, got.UsedBytes, got.QuotaBytes, wantUsed, wantQuota)
			}
		}
	})
}
