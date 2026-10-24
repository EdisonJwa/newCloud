package dav

// R8-DAV-001: a failed directory COPY must not roll back another request's success.
//
// The rollback used to re-read the destination by NAME and trash whatever it found. If a
// concurrent MKCOL/COPY had won the name, the losing COPY's rollback removed the winner's node
// (and, once it had children, unpublished the winner's files). The fix drives the rollback from
// the exact node IDs this request created, so a conflict at the destination root has nothing to
// roll back.
//
// The concurrency is deterministic rather than timing-dependent: the content-store wrapper runs
// request B from inside request A's physical Mkdir, i.e. after A has resolved its source but
// before A commits its metadata. Both layouts are exercised with the real handler.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

var errInjectedCopySourceRead = errors.New("injected: the COPY source could not be read")

// interleavingMkdir runs a second, independent request while the COPY is inside its Mkdir.
// fired makes the injection reentrancy-safe: the nested request's own Mkdir must be delegated,
// not injected again.
type interleavingMkdir struct {
	layout.ContentStore
	armed  atomic.Bool
	fired  atomic.Bool
	target string
	run    func()
}

func (c *interleavingMkdir) Mkdir(ctx context.Context, n layout.Node) error {
	if c.armed.Load() && n.Path == c.target && c.fired.CompareAndSwap(false, true) {
		c.run()
	}
	return c.ContentStore.Mkdir(ctx, n)
}

// runFailedCopyOverConcurrentRequest seeds an empty source, runs request B while request A's
// COPY is inside Mkdir, and then requires A to fail without disturbing B.
func runFailedCopyOverConcurrentRequest(t *testing.T, e *testEnv, mode layout.Mode, withFile bool) {
	t.Helper()

	e.mustMkcol(t, "source")

	var (
		winnerID    uuid.UUID
		beforeTrash int
		beforeAudit int
	)

	inter := &interleavingMkdir{ContentStore: e.h.content, target: "copied"}
	inter.run = func() {
		// Request B: a complete, successful MKCOL for the same destination name.
		if rec := e.do(t, "MKCOL", e.davPath("copied"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: concurrent B MKCOL = %d (%s)", mode, rec.Code, rec.Body.String())
		}
		if withFile {
			if rec := e.do(t, http.MethodPut, e.davPath("copied/important.txt"),
				strings.NewReader("IMPORTANT NEW DATA"), nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: concurrent B PUT = %d (%s)", mode, rec.Code, rec.Body.String())
			}
		}
		winnerID = nodeIDAt(t, e, "copied")
		beforeTrash = trashedCount(t, e)
		beforeAudit = len(e.meta.AuditEntries())
	}
	e.h.content = inter
	inter.armed.Store(true)

	rec := e.do(t, "COPY", e.davPath("source"), nil,
		map[string]string{"Destination": e.davPath("copied")})
	if !inter.fired.Load() {
		t.Fatal("the concurrent request was never injected; the test would prove nothing")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%s: a COPY that lost the destination name to a concurrent request = %d, want "+
			"405 (%s)", mode, rec.Code, rec.Body.String())
	}

	// B's resource is still the same node, readable, and not in the trash.
	if gotID := nodeIDAt(t, e, "copied"); gotID != winnerID {
		t.Fatalf("%s: the failed COPY replaced the concurrent winner's node (%s -> %s)", mode,
			winnerID, gotID)
	}
	if after := trashedCount(t, e); after != beforeTrash {
		t.Fatalf("%s: the failed COPY changed the trash (%d -> %d items); the concurrent "+
			"winner's node was rolled back", mode, beforeTrash, after)
	}
	// No audit row may describe a rollback or target the winner's node. Comparing total length
	// would be wrong: request A's own authentication writes an app-password-use row after B's
	// baseline, which is legitimate and unrelated.
	for _, row := range e.meta.AuditEntries()[beforeAudit:] {
		if row.TargetID == winnerID.String() || row.Metadata["rollback"] != nil {
			t.Fatalf("%s: the failed COPY wrote an audit row about the concurrent winner "+
				"(action=%s result=%s target=%s metadata=%v)", mode, row.Action, row.Result,
				row.TargetID, row.Metadata)
		}
	}
	if withFile {
		got := e.do(t, http.MethodGet, e.davPath("copied/important.txt"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "IMPORTANT NEW DATA" {
			t.Fatalf("%s: after a failed COPY, the concurrent winner's file is %d %q, want 200 "+
				"with its original bytes. A rollback keyed by destination name revoked a success "+
				"the client already received (R8-DAV-001).", mode, got.Code, got.Body.String())
		}
	}
	if _, err := e.meta.Resolve(context.Background(), e.user.ID, "copied"); err != nil {
		t.Fatalf("%s: the concurrent winner's directory is not resolvable after the failed COPY: %v",
			mode, err)
	}
}

// TestAFailedCopyDoesNotTrashAConcurrentMkcol is the empty-directory half of the review's
// reproduction: B's MKCOL answers 201, A's COPY fails, and B's collection must still resolve.
func TestAFailedCopyDoesNotTrashAConcurrentMkcol(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		runFailedCopyOverConcurrentRequest(t, e, mode, false)
	})
}

// TestAFailedCopyDoesNotUnpublishAConcurrentWritersFile is the stronger MKCOL half: B's MKCOL
// and PUT both succeed, and A's failure must not make the file disappear from the namespace.
func TestAFailedCopyDoesNotUnpublishAConcurrentWritersFile(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		runFailedCopyOverConcurrentRequest(t, e, mode, true)
	})
}

// interleavingOpen runs a competing request while the COPY is reading one source file, then
// fails that read. It gives deterministic control over the moment after an earlier file of the
// copy has been committed and before a later one is written.
type interleavingOpen struct {
	layout.ContentStore
	armed  atomic.Bool
	fired  atomic.Bool
	target string
	run    func()
}

func (c *interleavingOpen) Open(ctx context.Context, n layout.Node) (io.ReadSeekCloser, error) {
	if c.armed.Load() && n.Path == c.target && c.fired.CompareAndSwap(false, true) {
		c.run()
		return nil, errInjectedCopySourceRead
	}
	return c.ContentStore.Open(ctx, n)
}

// TestAFailedCopyDoesNotUndoAConcurrentPut is the first follow-up P0: A creates
// copied/a.txt, B overwrites it with a successful PUT, and A later fails on b.txt. The file now
// belongs to B's committed update; A's rollback must not trash it by ID.
func TestAFailedCopyDoesNotUndoAConcurrentPut(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.mustMkcol(t, "source")
		for _, f := range []struct{ name, body string }{
			{"a.txt", "A CONTENT"},
			{"b.txt", "B CONTENT"},
		} {
			if rec := e.do(t, http.MethodPut, e.davPath("source/"+f.name),
				strings.NewReader(f.body), nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: seed source/%s: %d", mode, f.name, rec.Code)
			}
		}

		var (
			winnerID    uuid.UUID
			beforeTrash int
		)
		inter := &interleavingOpen{ContentStore: e.h.content, target: "source/b.txt"}
		inter.run = func() {
			if rec := e.do(t, http.MethodPut, e.davPath("copied/a.txt"),
				strings.NewReader("B IRREPLACEABLE CONTENT"), nil); rec.Code != http.StatusNoContent {
				t.Fatalf("%s: concurrent B PUT of the partially copied file = %d (%s)",
					mode, rec.Code, rec.Body.String())
			}
			winnerID = nodeIDAt(t, e, "copied/a.txt")
			beforeTrash = trashedCount(t, e)
		}
		e.h.content = inter
		inter.armed.Store(true)

		rec := e.do(t, "COPY", e.davPath("source"), nil,
			map[string]string{"Destination": e.davPath("copied")})
		if !inter.fired.Load() {
			t.Fatal("the concurrent PUT was never injected; the test would prove nothing")
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: COPY that failed on a later file = %d, want 500 (%s)", mode, rec.Code,
				rec.Body.String())
		}

		got := e.do(t, http.MethodGet, e.davPath("copied/a.txt"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "B IRREPLACEABLE CONTENT" {
			t.Fatalf("%s: after the failed COPY, the concurrently updated file is %d %q, want "+
				"200 with B's bytes. The rollback treated 'created by A' as 'still A's to "+
				"delete', undoing a PUT the client was told had succeeded.", mode, got.Code,
				got.Body.String())
		}
		if afterID := nodeIDAt(t, e, "copied/a.txt"); afterID != winnerID {
			t.Fatalf("%s: the failed COPY replaced the file after B's successful PUT (%s -> %s)",
				mode, winnerID, afterID)
		}
		if after := trashedCount(t, e); after != beforeTrash {
			t.Fatalf("%s: the failed COPY trashed the file B had just updated (%d -> %d trash items)",
				mode, beforeTrash, after)
		}
	})
}

// TestAFailedCopyDoesNotUndoAConcurrentProppatch is P0-R10-001: a successful PROPPATCH leaves
// the node's ETag/size/mtime untouched, so the creation snapshot alone would still consider the
// file removable. The interim rollback guard reads the dead-property set before trashing; the
// complete fix remains operation visibility / a store-wide revision.
func TestAFailedCopyDoesNotUndoAConcurrentProppatch(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.mustMkcol(t, "source")
		for _, f := range []struct{ name, body string }{
			{"a.txt", "A CONTENT"},
			{"b.txt", "B CONTENT"},
		} {
			if rec := e.do(t, http.MethodPut, e.davPath("source/"+f.name),
				strings.NewReader(f.body), nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: seed source/%s: %d", mode, f.name, rec.Code)
			}
		}

		var winnerID uuid.UUID
		inter := &interleavingOpen{ContentStore: e.h.content, target: "source/b.txt"}
		inter.run = func() {
			body := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:"><D:set><D:prop>` +
				`<x:color xmlns:x="urn:newcloud:test">blue</x:color></D:prop></D:set></D:propertyupdate>`
			rec := e.do(t, "PROPPATCH", e.davPath("copied/a.txt"), strings.NewReader(body),
				map[string]string{"Content-Type": "application/xml"})
			if rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "200 OK") {
				t.Fatalf("%s: concurrent B PROPPATCH = %d (%s), want 207 with a 200 propstat",
					mode, rec.Code, rec.Body.String())
			}
			winnerID = nodeIDAt(t, e, "copied/a.txt")
		}
		e.h.content = inter
		inter.armed.Store(true)

		rec := e.do(t, "COPY", e.davPath("source"), nil,
			map[string]string{"Destination": e.davPath("copied")})
		if !inter.fired.Load() {
			t.Fatal("the concurrent PROPPATCH was never injected; the test would prove nothing")
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: COPY that failed on a later file = %d, want 500 (%s)", mode, rec.Code,
				rec.Body.String())
		}

		got := e.do(t, http.MethodGet, e.davPath("copied/a.txt"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "A CONTENT" {
			t.Fatalf("%s: after the failed COPY, the file with a successful PROPPATCH is %d %q, "+
				"want 200. The rollback revoked a property update the client was told succeeded "+
				"(P0-R10-001).", mode, got.Code, got.Body.String())
		}
		if afterID := nodeIDAt(t, e, "copied/a.txt"); afterID != winnerID {
			t.Fatalf("%s: the failed COPY replaced the node after the successful PROPPATCH (%s -> %s)",
				mode, winnerID, afterID)
		}
		var props []store.DeadProp
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var derr error
			props, derr = tx.DeadProps(ctx, winnerID)
			return derr
		}); err != nil {
			t.Fatalf("%s: read dead properties: %v", mode, err)
		}
		var found bool
		for _, p := range props {
			if p.Space == "urn:newcloud:test" && p.Name == "color" && p.Value == "blue" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: the PROPPATCH property disappeared after the failed COPY: %v", mode, props)
		}
	})
}

// TestAFailedCopyDoesNotUndoAConcurrentMove is the second follow-up P0: B successfully moves the
// partially copied directory out of the way; A's later failure must not trash the moved subtree
// by node ID.
func TestAFailedCopyDoesNotUndoAConcurrentMove(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.mustMkcol(t, "source")
		for _, f := range []struct{ name, body string }{
			{"a.txt", "A CONTENT"},
			{"b.txt", "B CONTENT"},
		} {
			if rec := e.do(t, http.MethodPut, e.davPath("source/"+f.name),
				strings.NewReader(f.body), nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: seed source/%s: %d", mode, f.name, rec.Code)
			}
		}

		var (
			winnerID    uuid.UUID
			beforeTrash int
		)
		inter := &interleavingOpen{ContentStore: e.h.content, target: "source/b.txt"}
		inter.run = func() {
			if rec := e.do(t, "MOVE", e.davPath("copied"), nil,
				map[string]string{"Destination": e.davPath("renamed")}); rec.Code != http.StatusCreated {
				t.Fatalf("%s: concurrent B MOVE of the partially copied directory = %d (%s)",
					mode, rec.Code, rec.Body.String())
			}
			winnerID = nodeIDAt(t, e, "renamed")
			beforeTrash = trashedCount(t, e)
		}
		e.h.content = inter
		inter.armed.Store(true)

		rec := e.do(t, "COPY", e.davPath("source"), nil,
			map[string]string{"Destination": e.davPath("copied")})
		if !inter.fired.Load() {
			t.Fatal("the concurrent MOVE was never injected; the test would prove nothing")
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: COPY that failed on a later file = %d, want 500 (%s)", mode, rec.Code,
				rec.Body.String())
		}

		got := e.do(t, http.MethodGet, e.davPath("renamed/a.txt"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "A CONTENT" {
			t.Fatalf("%s: after the failed COPY, the moved subtree is %d %q, want 200 with its "+
				"original bytes. The rollback trashed nodes a successful MOVE had relocated.",
				mode, got.Code, got.Body.String())
		}
		if afterID := nodeIDAt(t, e, "renamed"); afterID != winnerID {
			t.Fatalf("%s: the failed COPY replaced the moved directory (%s -> %s)", mode,
				winnerID, afterID)
		}
		if after := trashedCount(t, e); after != beforeTrash {
			t.Fatalf("%s: the failed COPY trashed the directory B had just moved (%d -> %d trash "+
				"items)", mode, beforeTrash, after)
		}
	})
}
