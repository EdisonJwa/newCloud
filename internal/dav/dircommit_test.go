package dav

// NC-R6-001: a physical directory that cannot be created must not be reported as a success.
//
// The sixth independent review injected a failure into the real tree.Store's Mkdir and reproduced
// all four entry points answering 2xx while Content.Stat said "not found": WebDAV COPY (empty and
// nested-empty collections), JSON batch COPY, WebDAV MKCOL and REST mkdir. The metadata row and the
// "ok" audit row existed; the directory did not.
//
// These tests are permanent and run on BOTH layouts. The failure is injected through the
// ContentStore interface, so the handler under test is the real one and the assertion is about the
// contract rather than about the fake. The reverse direction is covered too: the physical directory
// succeeds and the metadata transaction fails, which must not simply invert the orphan.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

var errInjectedMkdir = errors.New("injected: the content store cannot create a directory")

// failingMkdirContent delegates everything to the real layout and fails Mkdir for the paths the
// test selects. An atomic switch lets a test seed its fixture with the real store and then turn the
// failure on for the request under test.
type failingMkdirContent struct {
	layout.ContentStore
	fail  atomic.Bool
	match func(layout.Node) bool
}

func (f *failingMkdirContent) Mkdir(ctx context.Context, n layout.Node) error {
	if f.fail.Load() && (f.match == nil || f.match(n)) {
		return errInjectedMkdir
	}
	return f.ContentStore.Mkdir(ctx, n)
}

// injectMkdirFailure replaces the handler's content store and enables the failure immediately.
func injectMkdirFailure(e *testEnv, match func(layout.Node) bool) *failingMkdirContent {
	f := &failingMkdirContent{ContentStore: e.h.content, match: match}
	e.h.content = f
	f.fail.Store(true)
	return f
}

// mustMkcol creates a collection, failing the test if the seed does not work.
func (e *testEnv) mustMkcol(t *testing.T, rel string) {
	t.Helper()
	if rec := e.do(t, "MKCOL", e.davPath(rel), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol %s: %d (%s)", rel, rec.Code, rec.Body.String())
	}
}

// noLiveNode requires that a path does not resolve to a live metadata node.
func noLiveNode(t *testing.T, e *testEnv, rel string) {
	t.Helper()
	if _, err := e.meta.Resolve(context.Background(), e.user.ID, rel); err == nil {
		t.Fatalf("%q is a live node after an operation whose physical directory could not be created. "+
			"The client sees the directory in listings, a backup may not contain it, and the tree "+
			"layout's disk mirror is wrong.", rel)
	}
}

// noSuccessAudit requires that no "file.write ok" row was recorded for the name after the mark.
// The node and its audit row live in one transaction, so a failed create must leave neither.
func noSuccessAudit(t *testing.T, e *testEnv, from int, name string) {
	t.Helper()
	for _, row := range e.meta.AuditEntries()[from:] {
		if row.Action != "file.write" || row.Result != "ok" {
			continue
		}
		if got, _ := row.Metadata["name"].(string); got == name {
			t.Fatalf("an audit row claims a successful write of %q, but the directory was never "+
				"created. The log must not record a success the request did not achieve.", name)
		}
	}
}

// TestMkcolReportsAContentStoreFailure is the MKCOL probe from the review.
func TestMkcolReportsAContentStoreFailure(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		injectMkdirFailure(e, func(n layout.Node) bool { return n.Path == "newdir" })
		before := len(e.meta.AuditEntries())

		rec := e.do(t, "MKCOL", e.davPath("newdir"), nil, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: MKCOL whose physical directory could not be created = %d, want 500 "+
				"(body %q). A 201 here is the NC-R6-001 defect: metadata and audit say the "+
				"collection exists while the content store has no such directory.", mode, rec.Code,
				rec.Body.String())
		}
		noLiveNode(t, e, "newdir")
		noSuccessAudit(t, e, before, "newdir")
	})
}

// TestCopyOfADirectoryReportsAContentStoreFailure covers the three shapes the review asked for: an
// empty source collection, one containing a file, and one with an empty nested collection. The last
// is the case that is easy to miss: the parent is created and looks fine, and only the empty child
// is missing on disk.
func TestCopyOfADirectoryReportsAContentStoreFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seed       func(t *testing.T, e *testEnv)
		match      func(layout.Node) bool
		failedName string
	}{
		{
			name:       "empty",
			seed:       func(t *testing.T, e *testEnv) { e.mustMkcol(t, "source") },
			match:      func(n layout.Node) bool { return n.Path == "copied" },
			failedName: "copied",
		},
		{
			name: "with-file",
			seed: func(t *testing.T, e *testEnv) {
				e.mustMkcol(t, "source")
				if rec := e.do(t, http.MethodPut, e.davPath("source/a.txt"),
					strings.NewReader("AAA"), nil); rec.Code != http.StatusCreated {
					t.Fatalf("seed a.txt: %d", rec.Code)
				}
			},
			match:      func(n layout.Node) bool { return n.Path == "copied" },
			failedName: "copied",
		},
		{
			name: "nested-empty",
			seed: func(t *testing.T, e *testEnv) {
				e.mustMkcol(t, "source")
				e.mustMkcol(t, "source/empty")
			},
			match:      func(n layout.Node) bool { return n.Path == "copied/empty" },
			failedName: "empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
				tc.seed(t, e)
				injectMkdirFailure(e, tc.match)
				before := len(e.meta.AuditEntries())

				rec := e.do(t, "COPY", e.davPath("source"), nil,
					map[string]string{"Destination": e.davPath("copied")})
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("%s: COPY whose directory could not be created on disk = %d, want 500 "+
						"(%s). A 2xx here tells the client it has a copy whose directory does not "+
						"exist in the tree layout.", mode, rec.Code, rec.Body.String())
				}

				noLiveNode(t, e, "copied")
				// The transaction that would have committed the failing directory never ran, so
				// there is no success row for it. (A parent created before a nested failure does
				// have its own row, and the rollback records the trashing separately.)
				noSuccessAudit(t, e, before, tc.failedName)
				if tc.name == "nested-empty" {
					noLiveNode(t, e, "copied/empty")
				}
			})
		})
	}
}

// TestCopyOverwriteReportsAContentStoreFailure pins the overwrite path too. The staged replacement
// is built beside the destination; if its directory cannot be created, the old destination must
// still be readable and no staged name may become visible.
func TestCopyOverwriteReportsAContentStoreFailure(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.mustMkcol(t, "source")
		e.mustMkcol(t, "dst")
		if rec := e.do(t, http.MethodPut, e.davPath("dst/keep.txt"),
			strings.NewReader("ORIGINAL"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed keep.txt: %d", rec.Code)
		}
		beforeID := nodeIDAt(t, e, "dst")

		injectMkdirFailure(e, func(n layout.Node) bool {
			return strings.Contains(n.Path, ".newcloud-staged-")
		})

		rec := e.do(t, "COPY", e.davPath("source"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: COPY overwrite whose staged directory could not be created = %d, want "+
				"500 (%s)", mode, rec.Code, rec.Body.String())
		}

		if afterID := nodeIDAt(t, e, "dst"); afterID != beforeID {
			t.Fatalf("%s: a failed overwrite changed the destination node (%s -> %s)", mode,
				beforeID, afterID)
		}
		got := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "ORIGINAL" {
			t.Fatalf("%s: the destination after a failed overwrite: %d %q, want the original bytes",
				mode, got.Code, got.Body.String())
		}
	})
}

// TestAMkcolWhoseMetadataFailsLeavesNoLiveNode is the reverse direction.
//
// The physical directory is created before the metadata row. The sixth review's proposed
// compensation removed it again; the seventh review reproduced two ways that delete loses data
// (a file created after an emptiness check, and a concurrent winner's directory), so the safe
// outcome is now an unreferenced directory and no live node. This test pins that outcome: the
// orphan is invisible, a writer's file placed there is not deleted, and a retry adopts it.
func TestAMkcolWhoseMetadataFailsLeavesNoLiveNode(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.meta.SetAuditFault(errors.New("injected: audit unavailable"))
		rec := e.do(t, "MKCOL", e.davPath("newdir"), nil, nil)
		e.meta.SetAuditFault(nil)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: MKCOL whose audit row could not be written = %d, want 503 (%s)",
				mode, rec.Code, rec.Body.String())
		}
		noLiveNode(t, e, "newdir")

		// On the tree layout the orphan is deliberately kept. Removing it cannot be made safe
		// without ownership proof that survives the whole metadata window, and the seventh review
		// showed the delete removes a concurrent writer's bytes. The blob layout has no physical
		// directory to orphan: its directory marker is the metadata row that rolled back.
		writerRel := "newdir/other-writer.txt"
		if mode == layout.ModeTree {
			if _, err := e.h.content.Stat(context.Background(),
				layout.Node{Owner: e.user.ID.String(), Path: "newdir", IsDir: true}); err != nil {
				t.Fatalf("%s: the unreferenced directory was removed despite the safe-orphan policy "+
					"(Stat = %v). A cleanup would have to prove ownership across a concurrent winner "+
					"and a concurrent writer, which no observation in this path can.", mode, err)
			}

			// A writer that placed bytes under that path must not lose them to the retry.
			if _, perr := e.h.content.Put(context.Background(),
				layout.Node{Owner: e.user.ID.String(), Path: writerRel},
				strings.NewReader("IMPORTANT NEW DATA"), int64(len("IMPORTANT NEW DATA"))); perr != nil {
				t.Fatalf("%s: seed a concurrent writer's file under the orphan: %v", mode, perr)
			}
			if _, serr := e.h.content.Stat(context.Background(),
				layout.Node{Owner: e.user.ID.String(), Path: writerRel}); serr != nil {
				t.Fatalf("%s: the writer's file is unreadable after the failed MKCOL: %v", mode, serr)
			}
		}

		// The same request succeeds once the metadata store recovers: the orphan is adopted,
		// not deleted and recreated.
		if rec := e.do(t, "MKCOL", e.davPath("newdir"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: retrying MKCOL after the metadata store recovered = %d (%s)",
				mode, rec.Code, rec.Body.String())
		}
		var live bool
		if _, err := e.meta.Resolve(context.Background(), e.user.ID, "newdir"); err == nil {
			live = true
		}
		if !live {
			t.Fatalf("%s: the retried MKCOL did not create a live node", mode)
		}
		if mode == layout.ModeTree {
			if _, serr := e.h.content.Stat(context.Background(),
				layout.Node{Owner: e.user.ID.String(), Path: writerRel}); serr != nil {
				t.Fatalf("%s: the retry deleted a file that was already under the orphan: %v", mode, serr)
			}
		}
	})
}
