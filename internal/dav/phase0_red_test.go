//go:build phase0red

// Phase 0 regression probes for the defects ADR-0013 must eliminate.
//
// These tests are intentionally excluded from the normal suite: they fail against
// the current implementation and stay red until their owning phase lands. Run
// them explicitly with:
//
//	go test -tags phase0red ./internal/dav -run TestPhase0 -count=1
//
// A test here is a protocol reproduction, not a claim that the defect is already
// fixed. The failure message records the observable violation so a green run
// later means the protocol changed, not that the assertion was weakened.

package dav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func phase0Node(e *testEnv, rel string, isDir bool) layout.Node {
	return layout.Node{Owner: e.user.ID.String(), Path: rel, IsDir: isDir}
}

// TestPhase0TreePutStaleIfMatchKeepsOldBytes reproduces the P0 where the tree
// layout writes the new bytes before it evaluates If-Match and then answers 412.
func TestPhase0TreePutStaleIfMatchKeepsOldBytes(t *testing.T) {
	e := newTreeEnv(t)

	if rec := e.do(t, http.MethodPut, e.davPath("race.txt"), strings.NewReader("OLD"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed PUT: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := e.do(t, http.MethodPut, e.davPath("race.txt"), strings.NewReader("NEW"),
		map[string]string{"If-Match": `"definitely-not-the-current-etag"`})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match PUT = %d, want 412 (%s)", rec.Code, rec.Body.String())
	}

	got := e.do(t, http.MethodGet, e.davPath("race.txt"), nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "OLD" {
		t.Fatalf("after a 412 stale If-Match, GET = %d %q, want 200 with the old bytes. "+
			"The tree PUT published before evaluating the precondition.", got.Code, got.Body.String())
	}
}

// phase0InterleavingPut runs a competing request immediately before the COPY
// delegates a file Put, so the winner's bytes are already committed at the
// final path when the COPY overwrites them.
type phase0InterleavingPut struct {
	layout.ContentStore
	armed  atomic.Bool
	fired  atomic.Bool
	target string
	run    func()
}

func (c *phase0InterleavingPut) Put(ctx context.Context, n layout.Node, r io.Reader, size int64) (layout.PutResult, error) {
	if c.armed.Load() && n.Path == c.target && !n.IsDir && c.fired.CompareAndSwap(false, true) {
		c.run()
	}
	return c.ContentStore.Put(ctx, n, r, size)
}

// TestPhase0TreeCopyDoesNotOverwriteConcurrentPutBytes reproduces P0-TREE-COPY-001:
// COPY writes the source bytes to the final path before its metadata insert, so a
// concurrent PUT that already returned 2xx is overwritten by the losing COPY.
func TestPhase0TreeCopyDoesNotOverwriteConcurrentPutBytes(t *testing.T) {
	e := newTreeEnv(t)

	if rec := e.do(t, http.MethodPut, e.davPath("src.txt"), strings.NewReader("SOURCE BYTES"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed source: %d (%s)", rec.Code, rec.Body.String())
	}
	// The destination is intentionally absent: this is the COPY-to-a-new-name
	// path, where copyNode writes the final path before CreateFile. A pre-existing
	// destination would take the staged overwrite path instead.

	inter := &phase0InterleavingPut{ContentStore: e.h.content, target: "dst.txt"}
	var bCode int
	inter.run = func() {
		rec := e.do(t, http.MethodPut, e.davPath("dst.txt"), strings.NewReader("B WINS"), nil)
		bCode = rec.Code
	}
	e.h.content = inter
	inter.armed.Store(true)

	rec := e.do(t, "COPY", e.davPath("src.txt"), nil,
		map[string]string{"Destination": e.davPath("dst.txt")})
	if !inter.fired.Load() {
		t.Fatal("the concurrent PUT was never injected; the test would prove nothing")
	}
	if bCode != http.StatusCreated && bCode != http.StatusNoContent {
		t.Fatalf("concurrent B PUT = %d, want a successful create/update", bCode)
	}

	got := e.do(t, http.MethodGet, e.davPath("dst.txt"), nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "B WINS" {
		t.Fatalf("after COPY %d, GET dst.txt = %d %q, want 200 with B's committed bytes. "+
			"The COPY overwrote a winner whose success was already returned to the client.",
			rec.Code, got.Code, got.Body.String())
	}
}

// TestPhase0DeleteReleasesTheTreeLivePath reproduces the Phase 2A blocker:
// DELETE only changes metadata, leaving the bytes at the live path. A same-name
// no-replace publish would fail with a physical EEXIST while metadata says free.
func TestPhase0DeleteReleasesTheTreeLivePath(t *testing.T) {
	e := newTreeEnv(t)
	ctx := context.Background()

	if rec := e.do(t, http.MethodPut, e.davPath("reuse.txt"), strings.NewReader("OLD"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed PUT: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodDelete, e.davPath("reuse.txt"), nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if _, err := e.meta.Resolve(ctx, e.user.ID, "reuse.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("metadata still resolves reuse.txt after DELETE: %v", err)
	}

	_, statErr := e.h.content.Stat(ctx, phase0Node(e, "reuse.txt", false))
	if !errors.Is(statErr, layout.ErrNotFound) {
		t.Fatalf("after DELETE, the live tree path still holds bytes (Stat err = %v). "+
			"Phase 2A requires the content to be relocated before no-replace publish is enabled.", statErr)
	}
}

// TestPhase0ReaderNeverSeesTheReplacementWindow simulates the protocol's two
// physical transition points against the real tree store: old->backup and
// staged->final. Readers must see the old complete file until metadata commits.
func TestPhase0ReaderNeverSeesTheReplacementWindow(t *testing.T) {
	e := newTreeEnv(t)
	ctx := context.Background()

	if rec := e.do(t, http.MethodPut, e.davPath("gap.txt"), strings.NewReader("OLD COMPLETE"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed PUT: %d (%s)", rec.Code, rec.Body.String())
	}
	oldETag := e.do(t, http.MethodHead, e.davPath("gap.txt"), nil, nil).Header().Get("ETag")
	if oldETag == "" {
		t.Fatal("HEAD did not return an ETag")
	}

	if err := e.h.content.Move(ctx, phase0Node(e, "gap.txt", false), phase0Node(e, ".phase0-backup", false)); err != nil {
		t.Fatalf("simulate old->backup: %v", err)
	}

	first := e.do(t, http.MethodGet, e.davPath("gap.txt"), nil, nil)
	if first.Code != http.StatusOK || first.Body.String() != "OLD COMPLETE" {
		t.Errorf("after old->backup but before metadata commit, GET = %d %q, want 200 with the old bytes. "+
			"The reader was released into the physical transition window.", first.Code, first.Body.String())
	}

	if _, err := e.h.content.Put(ctx, phase0Node(e, "gap.txt", false), strings.NewReader("NEW STAGED"), int64(len("NEW STAGED"))); err != nil {
		t.Fatalf("simulate staged->final: %v", err)
	}
	second := e.do(t, http.MethodGet, e.davPath("gap.txt"), nil, nil)
	if second.Code != http.StatusOK || second.Body.String() != "OLD COMPLETE" {
		t.Errorf("after staged->final but before metadata commit, GET = %d %q (ETag %q), want 200 with the old bytes and ETag %q",
			second.Code, second.Body.String(), second.Header().Get("ETag"), oldETag)
	}
}

// TestPhase0FailedCopyDoesNotUndoAConcurrentLock reproduces R11: a successful
// LOCK changes no node field the snapshot checks, so rollbackCopy trashes a node
// whose lock the other client was told had succeeded.
func TestPhase0FailedCopyDoesNotUndoAConcurrentLock(t *testing.T) {
	e := newTreeEnv(t)

	e.mustMkcol(t, "source")
	for _, f := range []struct{ name, body string }{
		{"a.txt", "A CONTENT"},
		{"b.txt", "B CONTENT"},
	} {
		if rec := e.do(t, http.MethodPut, e.davPath("source/"+f.name), strings.NewReader(f.body), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed source/%s: %d", f.name, rec.Code)
		}
	}

	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">` +
		`<D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype>` +
		`<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`

	inter := &interleavingOpen{ContentStore: e.h.content, target: "source/b.txt"}
	inter.run = func() {
		rec := e.do(t, "LOCK", e.davPath("copied/a.txt"), strings.NewReader(lockBody), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("concurrent B LOCK = %d (%s)", rec.Code, rec.Body.String())
		}
	}
	e.h.content = inter
	inter.armed.Store(true)

	rec := e.do(t, "COPY", e.davPath("source"), nil,
		map[string]string{"Destination": e.davPath("copied")})
	if !inter.fired.Load() {
		t.Fatal("the concurrent LOCK was never injected; the test would prove nothing")
	}
	if rec.Code == http.StatusNoContent || rec.Code == http.StatusCreated {
		t.Fatalf("COPY unexpectedly succeeded (%d); the injected source read should fail", rec.Code)
	}

	got := e.do(t, http.MethodGet, e.davPath("copied/a.txt"), nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "A CONTENT" {
		t.Fatalf("after a failed COPY, the locked file is %d %q, want 200 with its bytes. "+
			"rollbackCopy trashed a node whose LOCK had already succeeded (R11).", got.Code, got.Body.String())
	}

	second := e.do(t, "LOCK", e.davPath("copied/a.txt"), strings.NewReader(lockBody), nil)
	if second.Code != http.StatusLocked {
		t.Fatalf("the already-successful lock was revoked: second LOCK = %d, want 423", second.Code)
	}
}

// TestPhase0LegacyWriterBypassesDeleteFence is the review's G1 simulation: a
// legacy PUT that does not join the operation fence can return success for a path
// that the new DELETE then removes. The physical move is simulated because Phase
// 2A is not enabled.
func TestPhase0LegacyWriterBypassesDeleteFence(t *testing.T) {
	e := newTreeEnv(t)
	ctx := context.Background()

	if rec := e.do(t, http.MethodPut, e.davPath("fenced.txt"), strings.NewReader("OLD"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}
	// A's Phase 2A physical move, before its metadata trash commit.
	if err := e.h.content.Move(ctx, phase0Node(e, "fenced.txt", false), phase0Node(e, ".phase0-trash-fenced", false)); err != nil {
		t.Fatalf("simulate Phase 2A move: %v", err)
	}
	// B is a legacy writer that does not take the new fence.
	b := e.do(t, http.MethodPut, e.davPath("fenced.txt"), strings.NewReader("B SUCCESS"), nil)
	if b.Code != http.StatusCreated && b.Code != http.StatusNoContent {
		t.Fatalf("legacy B PUT = %d, want success", b.Code)
	}
	// A's metadata trash commit must not make B's successful write disappear.
	if rec := e.do(t, http.MethodDelete, e.davPath("fenced.txt"), nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("A DELETE = %d", rec.Code)
	}
	got := e.do(t, http.MethodGet, e.davPath("fenced.txt"), nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "B SUCCESS" {
		t.Fatalf("after a successful legacy PUT, GET = %d %q; the DELETE fence did not "+
			"cover the legacy writer (G1).", got.Code, got.Body.String())
	}
}

// TestPhase0BackfillMovesNewLiveBytes is the review's backfill TOCTOU: the check
// sees no live owner, a new PUT creates one, and the unfenced physical move then
// takes the new file's bytes.
func TestPhase0BackfillMovesNewLiveBytes(t *testing.T) {
	e := newTreeEnv(t)
	ctx := context.Background()

	if rec := e.do(t, http.MethodPut, e.davPath("legacy.txt"), strings.NewReader("OLD"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodDelete, e.davPath("legacy.txt"), nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	// The backfill checks the path and sees only a trashed row, so it plans a move.
	// Before the move, a new live PUT wins the name.
	if rec := e.do(t, http.MethodPut, e.davPath("legacy.txt"), strings.NewReader("NEW LIVE"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("new live PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	if err := e.h.content.Move(ctx, phase0Node(e, "legacy.txt", false), phase0Node(e, ".phase0-backfill-legacy", false)); err != nil {
		t.Fatalf("simulate backfill move: %v", err)
	}
	got := e.do(t, http.MethodGet, e.davPath("legacy.txt"), nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "NEW LIVE" {
		t.Fatalf("after the backfill race, GET = %d %q, want 200 with the new live bytes. "+
			"The backfill moved a path a newer live node owns (G1/D3).", got.Code, got.Body.String())
	}
}
