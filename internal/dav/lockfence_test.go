package dav

// Lock fencing on the collection operations (2026-10-24 review, H2).
//
// A lock that a request does not consult is advisory in the worst way: the client believes
// it is protected, and the server has already agreed to destroy the file. Before these
// cases, COPY and MOVE never looked at the destination's collection, and DELETE never looked
// below the node it was deleting — so a client could lock a folder and another client could
// still COPY into it, MOVE out of it, or delete it with the locked file inside.
//
// The depth rules are the same ones PUT uses: a Depth: 0 lock protects a collection's
// MEMBERSHIP (adding and removing children), a Depth: infinity lock covers the whole subtree
// including content.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

const fenceLockBody = `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
	<D:lockscope><D:exclusive/></D:lockscope>
	<D:locktype><D:write/></D:locktype>
	<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`

// lockPath locks a path with the given Depth and returns the token.
func lockPath(t *testing.T, e *testEnv, rel, depth string) string {
	t.Helper()
	rec := e.do(t, "LOCK", e.davPath(rel), strings.NewReader(fenceLockBody),
		map[string]string{"Depth": depth})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK %s depth=%s: want 200, got %d (%s)", rel, depth, rec.Code, rec.Body.String())
	}
	token := strings.Trim(rec.Header().Get("Lock-Token"), "<>")
	if token == "" {
		t.Fatal("LOCK must return a Lock-Token header")
	}
	return token
}

// A DELETE of a collection must not destroy a child somebody holds a lock on. The lock is a
// promise about that child, and "your parent folder was deleted" is not an exception to it.
func TestDeleteOfACollectionIsRefusedWhileAChildIsLocked(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		e.mustMkcol(t, "shared")
		if rec := e.do(t, http.MethodPut, e.davPath("shared/held.txt"),
			strings.NewReader("precious"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d", rec.Code)
		}
		token := lockPath(t, e, "shared/held.txt", "0")

		rec := e.do(t, http.MethodDelete, e.davPath("shared"), nil, nil)
		if rec.Code != http.StatusLocked {
			t.Fatalf("DELETE of a collection with a locked child: want 423, got %d. "+
				"A lock the request does not consult is advisory.", rec.Code)
		}
		if got := e.do(t, http.MethodGet, e.davPath("shared/held.txt"), nil, nil); got.Code != http.StatusOK || got.Body.String() != "precious" {
			t.Fatalf("the refused delete still destroyed the locked file: %d %q", got.Code, got.Body.String())
		}

		// With the token submitted, the holder may proceed — the lock protects against
		// others, not against its own owner.
		rec = e.do(t, http.MethodDelete, e.davPath("shared"), nil,
			map[string]string{"If": "(<" + token + ">)"})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE with the lock token submitted: want 204, got %d", rec.Code)
		}
	})
}

// COPY adds a member to the destination collection. A Depth: 0 lock on that collection is
// exactly the right that forbids it.
func TestCopyIntoALockedCollectionIsRefused(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		e.mustMkcol(t, "inbox")
		if rec := e.do(t, http.MethodPut, e.davPath("src.txt"),
			strings.NewReader("bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d", rec.Code)
		}
		token := lockPath(t, e, "inbox", "0")

		rec := e.do(t, "COPY", e.davPath("src.txt"), nil, map[string]string{
			"Destination": e.davPath("inbox/copied.txt"),
		})
		if rec.Code != http.StatusLocked {
			t.Fatalf("COPY into a Depth:0-locked collection: want 423, got %d (%s)",
				rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("inbox/copied.txt"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("the refused COPY created the file anyway (%d)", got.Code)
		}

		rec = e.do(t, "COPY", e.davPath("src.txt"), nil, map[string]string{
			"Destination": e.davPath("inbox/copied.txt"),
			"If":          "(<" + token + ">)",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("COPY with the token submitted: want 201, got %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// MOVE removes a member from the source collection, so the same depth rule applies there.
func TestMoveOutOfALockedCollectionIsRefused(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		e.mustMkcol(t, "source")
		e.mustMkcol(t, "target")
		if rec := e.do(t, http.MethodPut, e.davPath("source/moved.txt"),
			strings.NewReader("bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d", rec.Code)
		}
		token := lockPath(t, e, "source", "0")

		rec := e.do(t, "MOVE", e.davPath("source/moved.txt"), nil, map[string]string{
			"Destination": e.davPath("target/moved.txt"),
		})
		if rec.Code != http.StatusLocked {
			t.Fatalf("MOVE out of a Depth:0-locked collection: want 423, got %d (%s)",
				rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("source/moved.txt"), nil, nil); got.Code != http.StatusOK {
			t.Fatalf("the refused MOVE removed the source anyway (%d)", got.Code)
		}

		rec = e.do(t, "MOVE", e.davPath("source/moved.txt"), nil, map[string]string{
			"Destination": e.davPath("target/moved.txt"),
			"If":          "(<" + token + ">)",
		})
		if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
			t.Fatalf("MOVE with the token submitted: want 201/204, got %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// A COPY of a collection reads its descendants' bytes. A Depth: infinity lock on a child
// means the child's content is protected, so the copy must refuse rather than duplicate it.
func TestCopyOfACollectionIsRefusedWhileAChildIsLocked(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		e.mustMkcol(t, "docs")
		if rec := e.do(t, http.MethodPut, e.davPath("docs/held.txt"),
			strings.NewReader("bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d", rec.Code)
		}
		lockPath(t, e, "docs/held.txt", "0")

		rec := e.do(t, "COPY", e.davPath("docs"), nil, map[string]string{
			"Destination": e.davPath("docs-copy"),
			"Depth":       "infinity",
		})
		if rec.Code != http.StatusLocked {
			t.Fatalf("COPY of a collection containing a locked file: want 423, got %d (%s)",
				rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("docs-copy/held.txt"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("the refused COPY produced a duplicate of the locked file (%d)", got.Code)
		}
	})
}
