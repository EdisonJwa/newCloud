package dav

// Regression tests for the 2026-10-25 review findings.
//
// Each test names the finding it pins. The behaviours covered here were all live defects: a
// folded-name PUT that replaced a differently-named sibling, a PROPPATCH that walked past locks,
// a lock refresh that failed on the tagged If form clients actually send, depth headers compared
// case-sensitively, a COPY that ignored Depth: 0, an inner-node permission refusal answered as
// "locked", a new lock granted under someone else's Depth: infinity lock, a lock response that
// dropped the owner and the lockroot, a quota property that vanished instead of being answered,
// and an MKCOL that reported "method not allowed" for a bad name.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

const reviewLockOwnerBody = `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
	<D:lockscope><D:exclusive/></D:lockscope>
	<D:locktype><D:write/></D:locktype>
	<D:owner>alice-laptop</D:owner></D:lockinfo>`

// reviewProppatchBody sets one dead property.
const reviewProppatchBody = `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:example:">
	<D:set><D:prop><Z:colour>blue</Z:colour></D:prop></D:set></D:propertyupdate>`

// TestPutDoesNotReplaceAFoldNamedSibling pins the review's P1.
//
// ChildByName matches the folded name_key, so a PUT for "readme.md" found the node created as
// "README.md" and updated it: the client got 204, the original's content was replaced, and a GET
// of the requested spelling answered 404 because the read path is case-exact. A name the store
// folds to one key is a CONFLICT (ADR-0004 §4), the same answer the store's WriteFile guard
// gives.
func TestPutDoesNotReplaceAFoldNamedSibling(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("README.md"),
			strings.NewReader("upper content"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed PUT README.md: %d (%s)", mode, rec.Code, rec.Body.String())
		}

		rec := e.do(t, http.MethodPut, e.davPath("readme.md"),
			strings.NewReader("lower content"), nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: PUT over a case-folded sibling = %d, want 409. The folded name is a "+
				"different spelling of an existing file, not an update target.", mode, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "name-conflict") {
			t.Fatalf("%s: the 409 must carry a DAV:error body naming the condition:\n%s", mode, body)
		}

		// The original file is untouched, and the requested spelling never came into existence.
		if got := e.do(t, http.MethodGet, e.davPath("README.md"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "upper content" {
			t.Fatalf("%s: the folded PUT replaced the sibling's content: %d %q",
				mode, got.Code, got.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("readme.md"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s: GET of the requested spelling = %d, want 404", mode, got.Code)
		}

		// The exact spelling is still an ordinary update.
		if rec := e.do(t, http.MethodPut, e.davPath("README.md"),
			strings.NewReader("second version"), nil); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: PUT to the exact name = %d, want 204", mode, rec.Code)
		}

		// The same rule for the Unicode fold: NFC and NFD spellings of "café" are one name.
		nfc := e.davPath("caf%C3%A9.md")  // U+00E9, precomposed
		nfd := e.davPath("cafe%CC%81.md") // e + U+0301, decomposed
		if rec := e.do(t, http.MethodPut, nfc, strings.NewReader("composed"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed PUT NFC name: %d (%s)", mode, rec.Code, rec.Body.String())
		}
		if rec := e.do(t, http.MethodPut, nfd, strings.NewReader("decomposed"), nil); rec.Code != http.StatusConflict {
			t.Fatalf("%s: PUT over a Unicode-folded sibling = %d, want 409", mode, rec.Code)
		}
		if got := e.do(t, http.MethodGet, nfc, nil, nil); got.Code != http.StatusOK || got.Body.String() != "composed" {
			t.Fatalf("%s: the Unicode-folded PUT replaced the sibling's content: %d %q",
				mode, got.Code, got.Body.String())
		}
		if got := e.do(t, http.MethodGet, nfd, nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s: GET of the NFD spelling = %d, want 404", mode, got.Code)
		}
	})
}

// TestProppatchHonorsLocks pins the review's P2: PROPPATCH checked PermWrite alone, so a client
// could rewrite dead properties on a resource another client held a lock on. A dead-property
// write is a content-level change, so it is fenced the way PUT fences an update: a lock on the
// target, or a Depth: infinity lock on an ancestor, blocks it unless the token is submitted.
func TestProppatchHonorsLocks(t *testing.T) {
	e := newEnv(t)

	// A lock on the file itself.
	e.do(t, http.MethodPut, e.davPath("locked.txt"), strings.NewReader("x"), nil)
	token := lockPath(t, e, "locked.txt", "0")

	rec := e.do(t, "PROPPATCH", e.davPath("locked.txt"),
		strings.NewReader(reviewProppatchBody), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("PROPPATCH on a locked file without the token = %d, want 423", rec.Code)
	}
	if got := e.do(t, "PROPFIND", e.davPath("locked.txt"),
		strings.NewReader(propfindAllProp), map[string]string{"Depth": "0"}); strings.Contains(got.Body.String(), "colour") {
		t.Fatalf("the refused PROPPATCH stored the property anyway:\n%s", got.Body.String())
	}

	rec = e.do(t, "PROPPATCH", e.davPath("locked.txt"), strings.NewReader(reviewProppatchBody),
		map[string]string{"If": "(<" + token + ">)"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH with the token submitted = %d, want 207 (%s)", rec.Code, rec.Body.String())
	}

	// A Depth: infinity lock on the parent reaches the child's properties; a Depth: 0 lock
	// does not, because it protects the parent's membership, not this node's content.
	e.mustMkcol(t, "docs")
	e.do(t, http.MethodPut, e.davPath("docs/f.txt"), strings.NewReader("x"), nil)
	folderToken := lockPath(t, e, "docs", "infinity")

	rec = e.do(t, "PROPPATCH", e.davPath("docs/f.txt"), strings.NewReader(reviewProppatchBody), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("PROPPATCH under a Depth:infinity folder lock without the token = %d, want 423", rec.Code)
	}
	rec = e.do(t, "PROPPATCH", e.davPath("docs/f.txt"), strings.NewReader(reviewProppatchBody),
		map[string]string{"If": "(<" + folderToken + ">)"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH under a Depth:infinity folder lock with the token = %d, want 207 (%s)",
			rec.Code, rec.Body.String())
	}

	// An unlocked resource is unaffected.
	e.do(t, http.MethodPut, e.davPath("free.txt"), strings.NewReader("x"), nil)
	if rec := e.do(t, "PROPPATCH", e.davPath("free.txt"),
		strings.NewReader(reviewProppatchBody), nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH on an unlocked file = %d, want 207", rec.Code)
	}
}

// TestLockRefreshWithTaggedIfHeader pins the review's P2: lockTokenFromIf returned the FIRST
// <...> substring, and in the tagged form "If: <url> (<token>)" that is the resource URL — so
// every refresh sent with a tagged If failed with 412. Windows and Office tag their lists.
func TestLockRefreshWithTaggedIfHeader(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("busy.txt"), strings.NewReader("x"), nil)
	token := lockPath(t, e, "busy.txt", "0")

	// The untagged form keeps working.
	rec := e.do(t, "LOCK", e.davPath("busy.txt"), nil,
		map[string]string{"If": "(<" + token + ">)"})
	if rec.Code != http.StatusOK {
		t.Fatalf("untagged refresh = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// The tagged form names the resource, then the token. It must refresh too.
	rec = e.do(t, "LOCK", e.davPath("busy.txt"), nil, map[string]string{
		"If": "<" + e.davPath("busy.txt") + "> (<" + token + ">)",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("tagged refresh = %d, want 200. The first <...> in a tagged If is the "+
			"resource URL, not the token.", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, token) {
		t.Fatalf("the refresh response must report the refreshed token:\n%s", body)
	}

	// A tagged header with the WRONG token is still a failed refresh.
	rec = e.do(t, "LOCK", e.davPath("busy.txt"), nil, map[string]string{
		"If": "<" + e.davPath("busy.txt") + "> (<opaquelocktoken:not-the-token>)",
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("tagged refresh with a foreign token = %d, want 412", rec.Code)
	}
}

// TestDepthHeaderValuesAreCaseInsensitive pins the review's P3: only the lowercase spelling was
// accepted, so a client sending "Infinity" — the exact spelling in RFC 4918's examples — got 400.
func TestDepthHeaderValuesAreCaseInsensitive(t *testing.T) {
	e := newEnv(t)
	e.mustMkcol(t, "folder")

	// PROPFIND: this server refuses infinite depth with 403; the capitalized spelling must get
	// that same answer, not 400 invalid-depth.
	e.do(t, http.MethodPut, e.davPath("folder/a.txt"), strings.NewReader("x"), nil)
	rec := e.do(t, "PROPFIND", e.davPath(""), nil, map[string]string{"Depth": "Infinity"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PROPFIND Depth: Infinity = %d, want 403 propfind-finite-depth", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "propfind-finite-depth") {
		t.Fatalf("PROPFIND Depth: Infinity must be refused by the finite-depth policy:\n%s", body)
	}

	// LOCK: "Infinity" is a valid depth, and the lock records the canonical spelling.
	rec = e.do(t, "LOCK", e.davPath("folder"), strings.NewReader(fenceLockBody),
		map[string]string{"Depth": "Infinity"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK Depth: Infinity = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "<D:depth>infinity</D:depth>") {
		t.Fatalf("LOCK Depth: Infinity must report the canonical depth:\n%s", body)
	}
	// And a truly invalid depth is still refused.
	if rec := e.do(t, "LOCK", e.davPath("folder"), strings.NewReader(fenceLockBody),
		map[string]string{"Depth": "1"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("LOCK Depth: 1 = %d, want 400", rec.Code)
	}
}

// TestCopyWithDepthZeroCopiesOnlyTheCollection pins the review's P3: COPY of a collection always
// recursed, ignoring Depth: 0 (RFC 4918 §9.8.3: the destination collection is created with no
// children). An invalid depth is refused, and the default is still a full copy.
func TestCopyWithDepthZeroCopiesOnlyTheCollection(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.mustMkcol(t, "src")
		e.do(t, http.MethodPut, e.davPath("src/a.txt"), strings.NewReader("alpha"), nil)
		e.do(t, http.MethodPut, e.davPath("src/b.txt"), strings.NewReader("beta"), nil)

		rec := e.do(t, "COPY", e.davPath("src"), nil, map[string]string{
			"Destination": e.davPath("dst"),
			"Depth":       "0",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: shallow COPY = %d, want 201 (%s)", mode, rec.Code, rec.Body.String())
		}
		// The collection exists; its children were not copied.
		if got := e.do(t, http.MethodGet, e.davPath("dst/a.txt"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s: Depth: 0 copied a child anyway: GET dst/a.txt = %d", mode, got.Code)
		}
		if got := e.do(t, http.MethodGet, e.davPath("dst/b.txt"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s: Depth: 0 copied a child anyway: GET dst/b.txt = %d", mode, got.Code)
		}
		if got := e.do(t, "PROPFIND", e.davPath("dst"), nil,
			map[string]string{"Depth": "0"}); got.Code != http.StatusMultiStatus {
			t.Fatalf("%s: the shallow destination collection is missing: %d", mode, got.Code)
		}
		// The source is untouched.
		if got := e.do(t, http.MethodGet, e.davPath("src/a.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "alpha" {
			t.Fatalf("%s: the shallow COPY disturbed the source: %d %q", mode, got.Code, got.Body.String())
		}

		// An invalid depth on a COPY is refused rather than silently treated as infinity.
		rec = e.do(t, "COPY", e.davPath("src"), nil, map[string]string{
			"Destination": e.davPath("dst"),
			"Depth":       "1",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: COPY Depth: 1 = %d, want 400", mode, rec.Code)
		}

		// A file COPY is inherently shallow and stays a plain copy under Depth: 0.
		e.do(t, http.MethodPut, e.davPath("solo.txt"), strings.NewReader("solo"), nil)
		rec = e.do(t, "COPY", e.davPath("solo.txt"), nil, map[string]string{
			"Destination": e.davPath("solo-copy.txt"),
			"Depth":       "0",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: file COPY with Depth: 0 = %d, want 201 (%s)", mode, rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("solo-copy.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "solo" {
			t.Fatalf("%s: file COPY with Depth: 0 lost the bytes: %d %q", mode, got.Code, got.Body.String())
		}
	})

	// The default (no Depth header) is still a full recursive copy, and the capitalized
	// spelling of infinity behaves like the lowercase one.
	e := newEnv(t)
	e.mustMkcol(t, "src")
	e.do(t, http.MethodPut, e.davPath("src/a.txt"), strings.NewReader("alpha"), nil)
	rec := e.do(t, "COPY", e.davPath("src"), nil, map[string]string{
		"Destination": e.davPath("dst"),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("default COPY = %d, want 201", rec.Code)
	}
	if got := e.do(t, http.MethodGet, e.davPath("dst/a.txt"), nil, nil); got.Code != http.StatusOK {
		t.Fatalf("the default COPY must stay recursive: GET dst/a.txt = %d", got.Code)
	}
	rec = e.do(t, "COPY", e.davPath("src"), nil, map[string]string{
		"Destination": e.davPath("dst2"),
		"Depth":       "Infinity",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("COPY Depth: Infinity = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if got := e.do(t, http.MethodGet, e.davPath("dst2/a.txt"), nil, nil); got.Code != http.StatusOK {
		t.Fatalf("COPY Depth: Infinity must stay recursive: GET dst2/a.txt = %d", got.Code)
	}
}

// TestMoveWithAnInnerNodeTheActorCannotDeleteIsForbidden pins the review's P3: a permission
// refusal on an inner node was answered 423 Locked with an empty body, which told the client a
// lock existed when none did and gave it nothing to act on. It is a permission answer: 403 with
// a named condition. The root-denied case stays 404 (the enumeration-oracle policy in
// resolvePath), so the two answers stay distinguishable exactly as before.
//
// The guard is defense in depth: the handler resolves every path inside the caller's own
// namespace, and a namespace owner holds all permissions on their own nodes, so the HTTP surface
// cannot normally produce an inner denial. The test therefore constructs the state directly — a
// foreign-owned node with a narrower grant inside the actor's tree — which is the state the walk
// exists to refuse.
func TestMoveWithAnInnerNodeTheActorCannotDeleteIsForbidden(t *testing.T) {
	e := newEnv(t)

	// Bob is a second account: the handler resolves every path inside the caller's OWN
	// namespace, so the scenario lives entirely in bob's tree, with one foreign-owned node
	// planted inside it.
	bobUser := e.meta.AddUser(store.User{Username: "bob", Email: "bob@example.test", Role: "user"})
	if _, err := e.meta.EnsureRoot(context.Background(), bobUser.ID); err != nil {
		t.Fatal(err)
	}
	bobSecret, _, err := e.h.appPass.Issue(context.Background(), bobUser.ID, "test", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Bob's own folder with one ordinary file.
	if rec := e.doAs(t, "bob", bobSecret, "MKCOL", "/dav/bob/folder", nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("bob MKCOL: %d", rec.Code)
	}
	if rec := e.doAs(t, "bob", bobSecret, http.MethodPut, "/dav/bob/folder/keep.txt",
		strings.NewReader("KEEP"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("bob PUT keep.txt: %d", rec.Code)
	}
	if rec := e.doAs(t, "bob", bobSecret, "MKCOL", "/dav/bob/target", nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("bob MKCOL target: %d", rec.Code)
	}

	// Inside bob's folder: a node owned by ALICE, with a grant giving bob read, write and
	// download but not delete. The MOVE must stop there.
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := e.meta.Resolve(ctx, bobUser.ID, "folder")
		if err != nil {
			return err
		}
		made, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: e.user.ID, ParentID: nodes[len(nodes)-1].ID, Name: "guard.txt",
			Size: 5, Mime: "text/plain",
		})
		if cerr != nil {
			return cerr
		}
		bobID := bobUser.ID
		_, gerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: made.ID, UserID: &bobID,
			Perms:   store.PermRead | store.PermWrite | store.PermDownload,
			Inherit: true,
		})
		return gerr
	}); err != nil {
		t.Fatal(err)
	}

	rec := e.doAs(t, "bob", bobSecret, "MOVE", "/dav/bob/folder", nil,
		map[string]string{"Destination": "/dav/bob/target/moved"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MOVE over an inner node without delete = %d, want 403 (a lock exists nowhere "+
			"in this scenario, so 423 lied about the cause)", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "forbidden-by-permission") {
		t.Fatalf("the 403 must carry a DAV:error body naming the failure:\n%s", body)
	}

	// The refusal had no side effects: the source is intact.
	if got := e.doAs(t, "bob", bobSecret, http.MethodGet, "/dav/bob/folder/keep.txt", nil, nil); got.Code != http.StatusOK ||
		got.Body.String() != "KEEP" {
		t.Fatalf("the refused MOVE disturbed the source: %d %q", got.Code, got.Body.String())
	}
	if got := e.doAs(t, "bob", bobSecret, "PROPFIND", "/dav/bob/folder", nil,
		map[string]string{"Depth": "1"}); got.Code != http.StatusMultiStatus ||
		!strings.Contains(got.Body.String(), "guard.txt") {
		t.Fatalf("the guarded node did not survive the refused MOVE: %d", got.Code)
	}
}

// TestLockUnderAnInfinityAncestorLockIsRefused pins the review's P3: a NEW lock was granted by
// checking the node's own lock alone, so a second exclusive write lock could be created inside
// someone else's Depth: infinity lock — two holders, one set of bytes.
func TestLockUnderAnInfinityAncestorLockIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mustMkcol(t, "folder")
	e.do(t, http.MethodPut, e.davPath("folder/child.txt"), strings.NewReader("x"), nil)
	folderToken := lockPath(t, e, "folder", "infinity")

	// Without the ancestor's token, the lock is refused.
	rec := e.do(t, "LOCK", e.davPath("folder/child.txt"),
		strings.NewReader(fenceLockBody), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("LOCK under a Depth:infinity ancestor lock = %d, want 423", rec.Code)
	}
	// With the ancestor's token, the holder may proceed — nested locking.
	rec = e.do(t, "LOCK", e.davPath("folder/child.txt"), strings.NewReader(fenceLockBody),
		map[string]string{"If": "(<" + folderToken + ">)"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK under the holder's own infinity lock with the token = %d, want 200 (%s)",
			rec.Code, rec.Body.String())
	}
	// The child now has its own exclusive lock: a second one on it is refused as before.
	if rec := e.do(t, "LOCK", e.davPath("folder/child.txt"),
		strings.NewReader(fenceLockBody), nil); rec.Code != http.StatusLocked {
		t.Fatalf("second LOCK on the child = %d, want 423", rec.Code)
	}
}

// TestLockResponseEchoesOwnerAndLockroot pins the review's P3: the submitted <D:owner> was stored
// and never echoed, and the activelock carried no <D:lockroot> — so a co-authoring client could
// not show "locked by" nor match the lock to the URL it edited (RFC 4918 §14.1/§14.2).
func TestLockResponseEchoesOwnerAndLockroot(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("owned.txt"), strings.NewReader("x"), nil)

	rec := e.do(t, "LOCK", e.davPath("owned.txt"), strings.NewReader(reviewLockOwnerBody), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	ownedToken := strings.Trim(rec.Header().Get("Lock-Token"), "<>")
	body := rec.Body.String()
	if !strings.Contains(body, "<D:owner>alice-laptop</D:owner>") {
		t.Fatalf("the LOCK response must echo the submitted owner:\n%s", body)
	}
	wantRoot := "<D:lockroot><D:href>/dav/alice/owned.txt</D:href></D:lockroot>"
	if !strings.Contains(body, wantRoot) {
		t.Fatalf("the LOCK response must name the locked resource as lockroot:\n%s", body)
	}

	// lockdiscovery reports the same two, on both the owner and a fresh reader.
	rec = e.do(t, "PROPFIND", e.davPath("owned.txt"),
		strings.NewReader(propfindAllProp), map[string]string{"Depth": "0"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "<D:owner>alice-laptop</D:owner>") ||
		!strings.Contains(body, wantRoot) {
		t.Fatalf("lockdiscovery must carry the owner and the lockroot:\n%s", body)
	}

	// The Office-style owner — a nested <D:href> — must survive the round trip too. encoding/xml
	// empties a plain string field on nested elements, so the parse keeps the inner XML and the
	// echo escapes it: client-supplied markup never re-enters a response as markup.
	nested := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>/dav/alice/office.docx</D:href></D:owner></D:lockinfo>`
	e.do(t, http.MethodPut, e.davPath("office.docx"), strings.NewReader("x"), nil)
	rec = e.do(t, "LOCK", e.davPath("office.docx"), strings.NewReader(nested), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK office.docx: %d (%s)", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "&lt;D:href&gt;/dav/alice/office.docx&lt;/D:href&gt;") {
		t.Fatalf("a nested-href owner must be preserved and escaped in the response:\n%s", body)
	}

	// A refresh echoes the owner the lock was created with.
	rec = e.do(t, "LOCK", e.davPath("owned.txt"), nil,
		map[string]string{"If": "(<" + ownedToken + ">)"})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "alice-laptop") {
		t.Fatalf("a refresh must echo the stored owner:\n%s", body)
	}
}

// TestPropfindAnswersQuotaPropertiesAsNotFound pins the review's P3: propIsEmitted claimed the
// quota pair while quota() is a stub, so a requested quota property was neither emitted nor
// answered — it vanished, leaving the client unable to tell "no quota" from "ignored". Until
// quota reporting is wired, the 404 propstat is the honest answer.
func TestPropfindAnswersQuotaPropertiesAsNotFound(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("quota.txt"), strings.NewReader("x"), nil)

	ask := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:">
		<D:prop><D:quota-used-bytes/><D:quota-available-bytes/><D:getetag/></D:prop></D:propfind>`
	rec := e.do(t, "PROPFIND", e.davPath("quota.txt"), strings.NewReader(ask),
		map[string]string{"Depth": "0"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: want 207, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "HTTP/1.1 404 Not Found") {
		t.Fatalf("the quota properties must be answered in a 404 propstat, not omitted:\n%s", body)
	}
	for _, want := range []string{"<D:quota-used-bytes/>", "<D:quota-available-bytes/>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the 404 propstat must name %s:\n%s", want, body)
		}
	}
	// The property that does exist stays in the 200 propstat.
	if strings.Contains(body, "<D:getetag/>") {
		t.Fatalf("getetag was reported as missing:\n%s", body)
	}
}

// TestMkcolRefusesUnsafeNamesWithTheRightStatus pins the review's P3: MKCOL discarded splitPath's
// error and fell into the "no name" 405 branch, telling a client the METHOD was wrong when the
// NAME was. PUT honours the same error; MKCOL now does too.
func TestMkcolRefusesUnsafeNamesWithTheRightStatus(t *testing.T) {
	e := newEnv(t)

	// A traversal segment is an unsafe path: 400.
	if rec := e.do(t, "MKCOL", e.davPath("a/../evil"), nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("MKCOL with a traversal segment = %d, want 400 (405 means the method, not the name)",
			rec.Code)
	}
	// A swap's reserved staging segment is unaddressable: 404, the same answer the GET/PUT
	// paths give so the refusal cannot probe for an in-progress swap.
	if rec := e.do(t, "MKCOL", e.davPath(".newcloud-staged-probe"), nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("MKCOL on a reserved staging name = %d, want 404", rec.Code)
	}
	// A real name still creates.
	if rec := e.do(t, "MKCOL", e.davPath("docs"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("ordinary MKCOL = %d, want 201", rec.Code)
	}
}
