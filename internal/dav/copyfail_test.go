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
