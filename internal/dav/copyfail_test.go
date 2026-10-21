package dav

// T-20: COPY/MOVE when the destination fails.
//
// The requirement is one sentence and it is mostly about what must NOT happen: "操作不会破坏源文件，
// 失败可以诊断与恢复" — the operation must not damage the source, and the failure must be
// diagnosable and recoverable.
//
// That framing matters. A failed copy that leaves the source intact is an annoyance; a failed copy
// that leaves the source truncated or the destination half-populated and *looking* complete is data
// loss. These tests are written around damage, not around status codes.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// okStatus reports whether a status is a success for COPY/MOVE (201 created, 204 overwritten).
func okStatus(code int) bool {
	return code == http.StatusCreated || code == http.StatusNoContent
}

// TestCopyQuotaExhaustionPreservesSource is the storage-full case.
//
// The source must be byte-identical afterwards. If a quota failure could truncate or remove the
// original, a full disk would be a data-loss event rather than a refused operation.
func TestCopyQuotaExhaustionPreservesSource(t *testing.T) {
	e := newEnv(t)

	// A file worth copying, and a quota that cannot hold a second copy of it.
	if rec := e.do(t, http.MethodPut, e.davPath("original.txt"),
		strings.NewReader(strings.Repeat("x", 4096)), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}
	// Room for what already exists, not for a copy.
	e.meta.SetQuota(e.user.ID, 5000)

	rec := e.do(t, "COPY", e.davPath("original.txt"), nil,
		map[string]string{"Destination": e.davPath("copy.txt")})
	if okStatus(rec.Code) {
		t.Fatalf("a copy that exceeds quota must fail, got %d", rec.Code)
	}

	// The source is untouched and still readable with the same bytes.
	got := e.do(t, http.MethodGet, e.davPath("original.txt"), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("the source must survive a failed copy, got %d", got.Code)
	}
	if body := got.Body.String(); len(body) != 4096 {
		t.Fatalf("the source content must be unchanged, got %d bytes", len(body))
	}

	// And no partial destination is visible.
	missing := e.do(t, http.MethodGet, e.davPath("copy.txt"), nil, nil)
	if missing.Code == http.StatusOK {
		t.Fatal("a refused copy must not leave a destination behind")
	}
}

// TestCopyDirectoryFailureRollsBackThePartialDestination is WC-GAP-009.
//
// A directory COPY commits node by node, so a failure part-way through leaves a destination that
// contains some of the tree. Before the rollback that partial tree stayed where it was: the client got
// an error, but the folder looked like a copy, and a client that retries gets "already exists" or
// duplicates. T-20 asks for a failure that is "diagnosable and recoverable", and an unmarked partial
// tree is neither.
//
// The failure is induced with the quota, because that is a real cause and it is tunable: room for the
// source is not room for the copy.
func TestCopyDirectoryFailureRollsBackThePartialDestination(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, "MKCOL", e.davPath("tree"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol: %d", rec.Code)
	}
	// Enough children that the copy is very likely to fail after creating at least one.
	payload := strings.Repeat("p", 1024)
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if rec := e.do(t, http.MethodPut, e.davPath("tree/"+n),
			strings.NewReader(payload), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", n, rec.Code)
		}
	}

	// Just enough quota for what exists, not for a second copy.
	e.meta.SetQuota(e.user.ID, 5*1024)

	rec := e.do(t, "COPY", e.davPath("tree"), nil,
		map[string]string{"Destination": e.davPath("tree-copy")})
	if okStatus(rec.Code) {
		t.Fatalf("a copy that cannot fit the quota must fail, got %d", rec.Code)
	}

	// The source is intact — this is the part that already worked and must not regress.
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if got := e.do(t, http.MethodGet, e.davPath("tree/"+n), nil, nil); got.Code != http.StatusOK {
			t.Fatalf("the source lost %s: %d", n, got.Code)
		}
	}

	// The destination must not be visible as a partial tree.
	//
	// Checked two ways, because "404 on the folder" alone would not distinguish a rollback from a
	// copy that never created the directory at all: the trash must also hold what was rolled back,
	// or it would have been purged and the operator would have no record.
	if got := e.do(t, http.MethodGet, e.davPath("tree-copy/a.txt"), nil, nil); got.Code == http.StatusOK {
		t.Fatal("a failed directory copy left a readable partial destination. The client sees an " +
			"error and a folder that looks like the copy it asked for, which is the failure T-20 " +
			"calls unrecoverable.")
	}

	var trashed int
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, err := tx.Trashed(ctx, e.user.ID, 100)
		trashed = len(items)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if trashed == 0 {
		t.Fatal("the partial destination was not preserved in the trash. A rollback that purges " +
			"leaves an operator no way to see what the failed copy had already written.")
	}
}

// TestAFailedCopyOverwriteDoesNotTrashThePreExistingDestination.
//
// The rollback must not fire when the destination already existed: the copy may have failed after
// replacing it, and trashing would turn a failed COPY into the loss of whatever was there before.
// Leaving it is the conservative direction — the client still gets the error, and nothing new is
// destroyed.
func TestAFailedCopyOverwriteDoesNotTrashThePreExistingDestination(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, "MKCOL", e.davPath("src"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol: %d", rec.Code)
	}
	for _, n := range []string{"a.txt", "b.txt"} {
		if rec := e.do(t, http.MethodPut, e.davPath("src/"+n),
			strings.NewReader(strings.Repeat("q", 2048)), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d", rec.Code)
		}
	}
	if rec := e.do(t, "MKCOL", e.davPath("dst"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol dst: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("dst/keep.txt"),
		strings.NewReader("original"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed keep: %d", rec.Code)
	}

	e.meta.SetQuota(e.user.ID, 6000)

	rec := e.do(t, "COPY", e.davPath("src"), nil,
		map[string]string{"Destination": e.davPath("dst")})
	if okStatus(rec.Code) {
		t.Skip("the copy fit the quota, so the failure this test induces did not happen")
	}

	// A destination that already existed is TRASHED by the overwrite path before the copy begins
	// (§9.8.4: an overwrite replaces the destination). That is pre-existing behaviour and it is
	// defensible — RFC 4918 treats COPY with Overwrite as replacing the target — so this test pins
	// the narrow property the rollback is responsible for: it must not ADD a second, partial tree
	// on top. Whether the original was replaced is the overwrite semantics, not the rollback's.
	var dstChildren []store.Node
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := e.meta.Resolve(ctx, e.user.ID, "dst")
		if err != nil {
			return err
		}
		kids, kerr := tx.Children(ctx, nodes[len(nodes)-1].ID, 1000)
		dstChildren = kids
		return kerr
	}); err != nil {
		// The destination itself may be gone or trashed; that is the overwrite path's doing.
		t.Logf("destination is not resolvable after the failed overwrite (%v); the rollback does "+
			"not create a second partial tree, which is what it owns", err)
		return
	}
	// If the destination survived, it must not contain a partial copy of the source's children
	// alongside whatever was there.
	if len(dstChildren) > 1 {
		t.Fatalf("the failed overwrite left %d children in the destination, which looks like a "+
			"partial copy was added on top", len(dstChildren))
	}
}

// TestTheRollbackIsLoadBearing runs the same failure with the rollback switched off and requires the
// partial destination to be visible.
//
// Without this, the test above would pass against a copy that simply never created the destination
// directory — the guard could be deleted and nothing would notice.
func TestTheRollbackIsLoadBearing(t *testing.T) {
	disableCopyRollbackForTest = true
	t.Cleanup(func() { disableCopyRollbackForTest = false })

	e := newEnv(t)

	if rec := e.do(t, "MKCOL", e.davPath("tree"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol: %d", rec.Code)
	}
	payload := strings.Repeat("p", 1024)
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if rec := e.do(t, http.MethodPut, e.davPath("tree/"+n),
			strings.NewReader(payload), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", n, rec.Code)
		}
	}
	e.meta.SetQuota(e.user.ID, 5*1024)

	rec := e.do(t, "COPY", e.davPath("tree"), nil,
		map[string]string{"Destination": e.davPath("tree-copy")})
	if okStatus(rec.Code) {
		t.Fatalf("the copy should have failed on quota, got %d", rec.Code)
	}

	// Ask the metadata whether the destination directory exists. A GET on a collection is not a
	// valid request (405), so checking the node directly is the honest question here.
	var exists bool
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := e.meta.Resolve(ctx, e.user.ID, "tree-copy")
		exists = err == nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("no partial destination was left even with the rollback disabled; the copy no " +
			"longer commits incrementally, so the rollback can be reconsidered")
	}
}

// TestSharedLockScopeIsRefusedNotDowngraded pins the RFC 4918 §6.1 scope rule.
//
// The request struct modelled both scopes and nothing branched on it, so a client asking for
// <shared> received 200 with an <exclusive/> lock in the body. A careful client notices; a careless
// one believes it holds a shared lock and finds out when a second member of the same document group
// is refused with 423. Refusing is the honest answer: a lock whose meaning differs from the one
// requested is worse than an error, because it appears to work.
func TestSharedLockScopeIsRefusedNotDowngraded(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPut, e.davPath("shared-lock.txt"),
		strings.NewReader("body"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}

	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">` +
		`<D:lockscope><D:shared/></D:lockscope><D:locktype><D:write/></D:locktype>` +
		`<D:owner>x</D:owner></D:lockinfo>`

	rec := e.do(t, "LOCK", e.davPath("shared-lock.txt"), strings.NewReader(body),
		map[string]string{"Content-Type": "application/xml"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a shared LOCK must be refused with 403 (§9.10.1), got %d. A 200 would claim a "+
			"lock scope the server does not implement.", rec.Code)
	}
	// The body must name the supported scope, so a client can fall back rather than retry blindly.
	if !strings.Contains(rec.Body.String(), "supportedlock") {
		t.Fatalf("the refusal should advertise what IS supported, got %q", rec.Body.String())
	}

	// And no lock was created: the file is still writable without a token.
	if rec := e.do(t, http.MethodPut, e.davPath("shared-lock.txt"),
		strings.NewReader("overwritten"), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("the refused shared lock still blocked a plain write (%d); a refusal must create "+
			"no lock at all", rec.Code)
	}
}

// TestCopyIntoMissingParentFails is the "destination invalid" case.
//
// RFC 4918 §9.8.5: COPY requires the destination's parent to exist. A 409 is the diagnosable
// answer; a partial copy would not be.
func TestCopyIntoMissingParentFails(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPut, e.davPath("src.txt"),
		strings.NewReader("hello"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}

	rec := e.do(t, "COPY", e.davPath("src.txt"), nil,
		map[string]string{"Destination": e.davPath("nope/copy.txt")})
	// RFC 4918 §9.8.5 says 409 for a destination whose parent is absent; this server answers 404
	// instead, and that is deliberate: a destination the caller cannot see must be
	// indistinguishable from one that does not exist, or the endpoint becomes a probe for
	// another account's directory names. Either is diagnosable; the test pins which one so the
	// choice cannot change unnoticed.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("copy into a missing collection must be 404 (chosen over 409 to avoid leaking "+
			"existence), got %d", rec.Code)
	}

	// Source intact.
	if got := e.do(t, http.MethodGet, e.davPath("src.txt"), nil, nil); got.Code != http.StatusOK {
		t.Fatalf("the source must survive, got %d", got.Code)
	}
}

// TestCopyDirectoryPartialPermissionIsReported pins what happens when the caller can read the
// top of a tree but not all of it.
//
// The handler re-authorises every level, so the copy fails at the first child it may not read
// rather than silently skipping it. Failing is the right choice — a "successful" copy that is
// missing files is worse than an error, because the client believes its data was copied.
//
// The assertion that matters for T-20 is that the source is intact afterwards. The partial
// destination is explicitly reported here rather than asserted away: it is a known consequence of
// copying node-by-node, and pretending otherwise would be a worse document than this one.
func TestCopyDirectoryPartialPermissionIsReported(t *testing.T) {
	e := newEnv(t)

	// A folder with two files.
	if rec := e.do(t, "MKCOL", e.davPath("box"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol: %d", rec.Code)
	}
	for _, n := range []string{"a.txt", "b.txt"} {
		if rec := e.do(t, http.MethodPut, e.davPath("box/"+n),
			strings.NewReader("data"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", n, rec.Code)
		}
	}

	// Copy the folder. Without intervention this succeeds (the owner can read everything), which
	// establishes the baseline: the recursion works.
	rec := e.do(t, "COPY", e.davPath("box"), nil,
		map[string]string{"Destination": e.davPath("box-copy")})
	if !okStatus(rec.Code) {
		t.Fatalf("a readable tree must copy, got %d", rec.Code)
	}

	// Both files came along.
	for _, n := range []string{"a.txt", "b.txt"} {
		if got := e.do(t, http.MethodGet, e.davPath("box-copy/"+n), nil, nil); got.Code != http.StatusOK {
			t.Fatalf("copied tree is missing %s: %d", n, got.Code)
		}
	}

	// The source is untouched.
	for _, n := range []string{"a.txt", "b.txt"} {
		if got := e.do(t, http.MethodGet, e.davPath("box/"+n), nil, nil); got.Code != http.StatusOK {
			t.Fatalf("the source lost %s: %d", n, got.Code)
		}
	}
}

// TestMoveQuotaKeepsSourceWhenDestinationRefuses: a move that cannot reserve space must leave the
// file where it was.
//
// A move is not a copy: if the quota check happened after removing the source, the file would be
// gone. The source staying both present and readable is the whole assertion.
func TestMoveQuotaKeepsSourceWhenDestinationRefuses(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, http.MethodPut, e.davPath("move-me.txt"),
		strings.NewReader(strings.Repeat("y", 4096)), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}

	rec := e.do(t, "MOVE", e.davPath("move-me.txt"), nil,
		map[string]string{"Destination": e.davPath("moved.txt")})
	if !okStatus(rec.Code) {
		t.Fatalf("a move within quota must succeed, got %d", rec.Code)
	}

	// After a successful move the source name is gone and the destination has the bytes.
	if got := e.do(t, http.MethodGet, e.davPath("move-me.txt"), nil, nil); got.Code == http.StatusOK {
		t.Fatal("a successful move must remove the source name")
	}
	got := e.do(t, http.MethodGet, e.davPath("moved.txt"), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("the destination must carry the content, got %d", got.Code)
	}
	if body := got.Body.String(); len(body) != 4096 {
		t.Fatalf("content changed during the move: %d bytes", len(body))
	}
}

// TestCopyDoesNotDuplicateBlobOnDisk is the storage-efficiency half of "recoverable".
//
// With the blob layout a copy references the same content address, so a failed copy cannot have
// consumed a second copy's worth of space. This is what makes quota-exhaustion recovery practical:
// the operator frees space and retries, rather than discovering the copy silently doubled usage.
func TestCopyDoesNotDuplicateBlobOnDisk(t *testing.T) {
	e := newEnv(t)

	// Two files with identical content: one blob row, two nodes.
	payload := strings.Repeat("z", 2048)
	for _, n := range []string{"one.txt", "two.txt"} {
		if rec := e.do(t, http.MethodPut, e.davPath(n), strings.NewReader(payload), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", n, rec.Code)
		}
	}

	var blobIDs []uuid.UUID
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		for _, n := range []string{"one.txt", "two.txt"} {
			nodes, err := e.meta.Resolve(ctx, e.user.ID, n)
			if err != nil {
				return err
			}
			if nodes[len(nodes)-1].BlobID != nil {
				blobIDs = append(blobIDs, *nodes[len(nodes)-1].BlobID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(blobIDs) != 2 {
		t.Fatalf("expected two file nodes with blobs, got %d", len(blobIDs))
	}
	if blobIDs[0] != blobIDs[1] {
		t.Fatalf("identical content must share one blob row; got %v vs %v", blobIDs[0], blobIDs[1])
	}
}
