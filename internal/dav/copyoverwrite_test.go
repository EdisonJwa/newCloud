package dav

// NC-RR-005: a failed COPY OVER an existing destination must not lose the original.
//
// The independent re-review reproduced this on 2026-10-23:
//
//	COPY src/ -> dst/   (dst/ previously contained keep.txt)
//	quota-limited operation returned HTTP 507
//	GET dst/keep.txt -> HTTP 404
//
// The mechanism was that the overwrite path trashed the destination BEFORE copying, so every
// subsequent failure (quota, a permission fault on a child, a storage error) left the user's visible
// path empty. The old bytes survived in the trash, which is better than losing them outright, but
// "restore it by hand from the bin" is not the atomic replacement a client asked for: a sync client
// sees the folder vanish, reports it to the user, and may propagate the deletion.
//
// The property these tests pin is the user-visible one: after a FAILED overwrite the destination
// holds what it held before, at its own name.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// seedOverwriteTree builds src/ with four 1 KiB files and dst/ with one file that must survive.
func seedOverwriteTree(t *testing.T, e *testEnv) {
	t.Helper()
	if rec := e.do(t, "MKCOL", e.davPath("src"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol src: %d", rec.Code)
	}
	payload := strings.Repeat("s", 1024)
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		if rec := e.do(t, http.MethodPut, e.davPath("src/"+n),
			strings.NewReader(payload), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed src/%s: %d", n, rec.Code)
		}
	}
	if rec := e.do(t, "MKCOL", e.davPath("dst"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol dst: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("dst/keep.txt"),
		strings.NewReader("the original bytes"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed dst/keep.txt: %d", rec.Code)
	}
}

// TestAFailedCopyOverwriteKeepsTheOriginalDestination is the reproduced defect.
//
// The failure is induced with the quota, because it is a real cause and it is tunable: room for what
// exists is not room for a full second copy.
func TestAFailedCopyOverwriteKeepsTheOriginalDestination(t *testing.T) {
	e := newEnv(t)
	seedOverwriteTree(t, e)

	// Just enough quota for what is already stored (4 KiB of src + one small dst file), not for
	// another 4 KiB copy.
	e.meta.SetQuota(e.user.ID, 4600)

	rec := e.do(t, "COPY", e.davPath("src"), nil,
		map[string]string{"Destination": e.davPath("dst")})
	if okStatus(rec.Code) {
		t.Skip("the copy fit the quota, so the failure this test induces did not happen")
	}

	// The assertion that matters. Before the fix this was 404: the destination had been trashed
	// before the copy ran, and the copy then failed.
	got := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("after a FAILED COPY overwrite, GET dst/keep.txt = %d, want 200. The original "+
			"destination was destroyed by an operation that reported failure: a sync client sees "+
			"its folder vanish and may propagate the deletion. Trash is not atomic replacement.",
			got.Code)
	}
	if body := got.Body.String(); body != "the original bytes" {
		t.Fatalf("the destination's bytes changed during a failed overwrite: %q", body)
	}
}

// TestAFailedCopyOverwriteLeavesNoStagingArtifacts: the fix introduces intermediate names, and they
// must not be visible once the failure has been handled.
//
// A leftover sibling would be worse than the bug it fixed in one specific way: a sync client would
// upload it back to the server it came from, or a user would wonder what "copy-staged-…" is.
func TestAFailedCopyOverwriteLeavesNoStagingArtifacts(t *testing.T) {
	e := newEnv(t)
	seedOverwriteTree(t, e)
	e.meta.SetQuota(e.user.ID, 4600)

	rec := e.do(t, "COPY", e.davPath("src"), nil,
		map[string]string{"Destination": e.davPath("dst")})
	if okStatus(rec.Code) {
		t.Skip("the copy fit the quota, so no failure occurred")
	}

	var names []string
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := e.meta.Resolve(ctx, e.user.ID, "")
		if err != nil {
			return err
		}
		kids, kerr := tx.Children(ctx, nodes[len(nodes)-1].ID, 1000)
		for _, k := range kids {
			names = append(names, k.Name)
		}
		return kerr
	}); err != nil {
		t.Fatal(err)
	}

	for _, n := range names {
		if strings.Contains(n, ".newcloud-staged-") || strings.Contains(n, ".newcloud-replaced-") {
			t.Fatalf("a failed overwrite left the intermediate %q visible at the top level", n)
		}
	}
	// And the destination itself is still there, with its original child.
	var dstNames []string
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := e.meta.Resolve(ctx, e.user.ID, "dst")
		if err != nil {
			return err
		}
		kids, kerr := tx.Children(ctx, nodes[len(nodes)-1].ID, 1000)
		for _, k := range kids {
			dstNames = append(dstNames, k.Name)
		}
		return kerr
	}); err != nil {
		t.Fatalf("the destination is not resolvable after a failed overwrite: %v", err)
	}
	if len(dstNames) != 1 || dstNames[0] != "keep.txt" {
		t.Fatalf("the destination holds %v after a failed overwrite, want exactly [keep.txt]",
			dstNames)
	}
}

// TestASuccessfulCopyOverwriteReplacesTheDestinationAndTrashesTheOriginal: the fix must not turn
// "preserve on failure" into "never replace".
//
// RFC 4918 §9.8.4: COPY with Overwrite: T replaces the destination. The replaced content belongs in
// the trash — that is the recoverable half of the guarantee, and a test that only checked the new
// content would pass against an implementation that silently purged it.
func TestASuccessfulCopyOverwriteReplacesTheDestinationAndTrashesTheOriginal(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, "MKCOL", e.davPath("src"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol src: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("src/new.txt"),
		strings.NewReader("new content"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed src: %d", rec.Code)
	}
	if rec := e.do(t, "MKCOL", e.davPath("dst"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol dst: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("dst/keep.txt"),
		strings.NewReader("original"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed dst: %d", rec.Code)
	}

	rec := e.do(t, "COPY", e.davPath("src"), nil,
		map[string]string{"Destination": e.davPath("dst")})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a COPY over an existing destination must answer 204 (§9.8.5), got %d", rec.Code)
	}

	// The new content is live, at the real destination name.
	if got := e.do(t, http.MethodGet, e.davPath("dst/new.txt"), nil, nil); got.Code != http.StatusOK {
		t.Fatalf("the copy did not land: GET dst/new.txt = %d", got.Code)
	} else if got.Body.String() != "new content" {
		t.Fatalf("dst/new.txt = %q", got.Body.String())
	}

	// The old content is gone from the live path...
	if got := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil); got.Code == http.StatusOK {
		t.Fatal("the previous destination's file survived the overwrite in the live tree")
	}

	// ...and present in the trash, where it is recoverable.
	var trashed []store.Node
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, err := tx.Trashed(ctx, e.user.ID, 100)
		trashed = items
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range trashed {
		// The displaced original is trashed under its reserved backup name, which embeds the
		// destination name. Checking the embedded name rather than an exact match is what keeps
		// this assertion working as the reserved-name shape changes.
		if strings.Contains(n.Name, ".newcloud-replaced-") && strings.HasSuffix(n.Name, "dst") {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(trashed))
		for _, n := range trashed {
			names = append(names, n.Name)
		}
		t.Fatalf("the replaced destination is not in the trash (items: %v), so the overwrite "+
			"purged the user's previous data instead of making it recoverable", names)
	}
}

// TestAFailedCopyOverwriteOfAFileKeepsTheOriginalBytes covers the single-file case.
//
// The directory case is the one the review reproduced, but the same trash-then-copy ordering applied
// to a file destination, and a file is what most COPY requests actually target.
func TestAFailedCopyOverwriteOfAFileKeepsTheOriginalBytes(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, http.MethodPut, e.davPath("src.bin"),
		strings.NewReader(strings.Repeat("a", 2048)), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed src: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("dst.bin"),
		strings.NewReader("precious"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed dst: %d", rec.Code)
	}

	// No room for a second 2 KiB copy.
	e.meta.SetQuota(e.user.ID, 2100)

	rec := e.do(t, "COPY", e.davPath("src.bin"), nil,
		map[string]string{"Destination": e.davPath("dst.bin")})
	if okStatus(rec.Code) {
		t.Skip("the copy fit the quota, so the failure this test induces did not happen")
	}

	got := e.do(t, http.MethodGet, e.davPath("dst.bin"), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("a failed file overwrite removed the destination (GET = %d)", got.Code)
	}
	if body := got.Body.String(); body != "precious" {
		t.Fatalf("after a failed overwrite dst.bin = %q, want the original %q", body, "precious")
	}
}

// TestCopyNeedsOnlyReadOnTheSource is the P1 permission mismatch the re-review reported.
//
// COPY required `write` on the source as well as `download`. V0.2 §4.2 asks for read on the source
// and create on the target, and the practical consequence of demanding write was that a grantee
// holding a read-only or read+download grant could not copy the bytes they are explicitly allowed to
// fetch: `download` on the source had no route that could exercise it.
//
// The rule is tested through sourcePermits rather than through an HTTP request because the WebDAV
// handler resolves paths inside the caller's own namespace only (resolvePath uses a.ID()), so a grant
// on another account's tree is not addressable as a DAV path. Pinning the predicate tests the rule
// that was wrong without pretending to exercise a route that does not exist; the API's
// /shared/{id}/content route is where cross-account bytes are served.
func TestCopyNeedsOnlyReadOnTheSource(t *testing.T) {
	// §4.1's read-only grant: read + download, no write and no delete. It must permit a COPY.
	if !sourcePermits(authz.PermRead|authz.PermDownload, false) {
		t.Fatal("read+download on the source must permit COPY. Requiring write made a read-only " +
			"grant unable to copy bytes it may already download (§4.2, P1).")
	}
	// A grant that carries `write` must still work: the relaxation widens the rule, it does not
	// replace it.
	if !sourcePermits(authz.PermWrite|authz.PermDownload|authz.PermRead, false) {
		t.Fatal("a full write grant must still permit COPY")
	}
	// download alone is not enough: the node must also be visible.
	if sourcePermits(authz.PermDownload, false) {
		t.Fatal("COPY must require read on the source as well as download: a grant that can " +
			"fetch bytes without being able to see the node is not a coherent source right")
	}
	// read alone is not enough either: seeing a file is not fetching it (authz deliberately does
	// not expand read into download).
	if sourcePermits(authz.PermRead, false) {
		t.Fatal("COPY must require download on the source: read alone does not imply fetch")
	}
}

// TestMoveStillNeedsDeleteOnTheSource is the counter-test to the COPY relaxation.
//
// MOVE relocates the node, consuming the source name, which is categorically different from reading
// the bytes: a read+download grantee must not be able to make a file disappear from where its owner
// expects it.
func TestMoveStillNeedsDeleteOnTheSource(t *testing.T) {
	if sourcePermits(authz.PermRead|authz.PermDownload, true) {
		t.Fatal("MOVE must require delete-class authority on the source; read+download is enough " +
			"to COPY but never to relocate")
	}
	if !sourcePermits(authz.PermDownload|authz.PermDelete, true) {
		t.Fatal("download+delete must permit MOVE, or the operation would be unreachable")
	}
	// And the recursive bar matches the single-node bar, or a directory MOVE would pass the root
	// check and fail at the first child for a reason the client cannot see.
	if subtreeSourcePerm(true) != authz.PermDelete {
		t.Fatalf("subtree MOVE must require delete per node, got %v", subtreeSourcePerm(true))
	}
	if subtreeSourcePerm(false) != authz.PermRead {
		t.Fatalf("subtree COPY must require only read per node, got %v", subtreeSourcePerm(false))
	}
}

// TestTheOverwritePreservationIsLoadBearing runs the same failure with the staged swap switched off
// and requires the ORIGINAL DEFECT to reappear.
//
// Without this, the tests above would pass against a path that simply never overwrote anything, or
// against a quota that happened to be generous. Reproducing the review's 404 on demand is what makes
// the other assertions evidence rather than coincidence.
func TestTheOverwritePreservationIsLoadBearing(t *testing.T) {
	disableCopyOverwritePreserveForTest = true
	t.Cleanup(func() { disableCopyOverwritePreserveForTest = false })

	e := newEnv(t)
	seedOverwriteTree(t, e)
	e.meta.SetQuota(e.user.ID, 4600)

	rec := e.do(t, "COPY", e.davPath("src"), nil,
		map[string]string{"Destination": e.davPath("dst")})
	if okStatus(rec.Code) {
		t.Skip("the copy fit the quota, so the failure this test induces did not happen")
	}

	// With the guard off this must fail: the destination was trashed before the copy ran. A test
	// that passed here would mean the preservation is coming from somewhere other than the staged
	// swap, and the guard could be deleted with nothing noticing.
	if got := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil); got.Code == http.StatusOK {
		t.Fatal("with the staged overwrite disabled, the failed COPY still preserved the " +
			"destination — so these tests do not prove the staged swap is what protects it")
	}
}

// TestAnOverwriteOfALongNonASCIIFileKeepsUtf8Valid is a guard on the intermediate name itself.
//
// reservedName truncates the destination name to keep the intermediate inside the 255-byte limit the
// store enforces. A byte-offset truncation can split a multi-byte character, and the resulting name
// is not just ugly: layout.ValidateSegment rejects invalid UTF-8, so the NAME fails validation and
// the overwrite fails with "invalid name" for a file whose only crime was a long non-ASCII name.
func TestAnOverwriteOfALongNonASCIIFileKeepsUtf8Valid(t *testing.T) {
	e := newEnv(t)

	for _, n := range []string{"staged", "replaced"} {
		name := reservedName(n, strings.Repeat("文", 80))
		if !utf8.ValidString(name) {
			t.Fatalf("reservedName(%q, <long non-ASCII>) produced invalid UTF-8: %q", n, name)
		}
	}

	// And end to end: an overwrite onto a long non-ASCII destination must succeed.
	long := strings.Repeat("文", 79) + ".txt" // 4 bytes short of the truncation point
	if rec := e.do(t, http.MethodPut, e.davPath("src-long.txt"),
		strings.NewReader("new"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed src: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath(long),
		strings.NewReader("old"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed dst: %d", rec.Code)
	}

	rec := e.do(t, "COPY", e.davPath("src-long.txt"), nil,
		map[string]string{"Destination": e.davPath(long)})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("COPY onto a long non-ASCII name = %d, want 204. %s", rec.Code, rec.Body.String())
	}

	got := e.do(t, http.MethodGet, e.davPath(long), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("the overwritten destination is not readable: %d", got.Code)
	}
	if got.Body.String() != "new" {
		t.Fatalf("destination bytes = %q, want %q", got.Body.String(), "new")
	}
}

// TestAnOverwriteWithOverwriteFalseIsRefusedUntouched keeps the RFC 4918 §9.8.4 refusal exact: the
// destination must not be touched at all when the client said Overwrite: F.
func TestAnOverwriteWithOverwriteFalseIsRefusedUntouched(t *testing.T) {
	e := newEnv(t)
	seedOverwriteTree(t, e)

	rec := e.do(t, "COPY", e.davPath("src"), nil, map[string]string{
		"Destination": e.davPath("dst"),
		"Overwrite":   "F",
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("Overwrite: F over an existing destination must be 412, got %d", rec.Code)
	}

	got := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil)
	if got.Code != http.StatusOK || got.Body.String() != "the original bytes" {
		t.Fatalf("a refused overwrite changed the destination: %d %q", got.Code, got.Body.String())
	}
}
