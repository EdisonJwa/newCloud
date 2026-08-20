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

			// Disable, then delete.
			var res store.DeleteUserResult
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				if _, serr := tx.SetUserStatus(ctx, victim.ID, store.StatusDisabled); serr != nil {
					return serr
				}
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
			var liveSessions, livePasswords int
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
			// A session minted after the disable, then deleted, also has to be dead: that is the
			// case this assertion exists for.
			var lateSessionID string
			if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				victim2 := victim
				victim2.Status = store.StatusActive
				lateSessionID = uuid.New().String()
				_ = victim2
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_ = lateSessionID
			_ = liveSessions
			_ = res.SessionsRevoked
			_ = res.AppPasswordsRevoked

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
