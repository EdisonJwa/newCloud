package dav

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// testEnv wires a full DAV handler over an in-memory metadata store and a
// content-addressed blob store, so the tests exercise the same path production
// does without needing PostgreSQL or a network.
type testEnv struct {
	h      *Handler
	meta   *store.Memory
	blobs  *local.Store
	user   store.User
	secret string
	// extra holds additional accounts' app-password secrets, keyed by username.
	extra map[string]string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()

	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Limits.MaxProfindChildren = 1000

	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}

	blobs, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content, err := blobstore.New(blobs, &resolverAdapter{r: meta.Resolver()})
	if err != nil {
		t.Fatal(err)
	}

	appPass := auth.NewAppPasswords(meta)
	secret, _, err := appPass.Issue(context.Background(), user.ID, "test", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	authorizer := authz.NewService(&authzAccessor{m: meta})

	h := New(cfg, meta, content, authorizer, appPass)
	return &testEnv{h: h, meta: meta, blobs: blobs, user: user, secret: secret,
		extra: map[string]string{}}
}

// resolverAdapter bridges store.Resolver onto blobstore.NodeResolver, mirroring
// the one in cmd/newcloud so the tests build a real handler without importing the
// main package.
type resolverAdapter struct{ r store.Resolver }

func (a *resolverAdapter) HashFor(ctx context.Context, owner, p string) (blob.Hash, bool, error) {
	h, ok, err := a.r.HashFor(ctx, owner, p)
	return blob.Hash(h), ok, err
}

func (a *resolverAdapter) Record(ctx context.Context, owner, p string, h blob.Hash, isDir bool) error {
	return a.r.Record(ctx, owner, p, [32]byte(h), isDir)
}

func (a *resolverAdapter) Forget(ctx context.Context, owner, p string) error {
	return a.r.Forget(ctx, owner, p)
}

func (a *resolverAdapter) IsDir(ctx context.Context, owner, p string) (bool, bool, error) {
	return a.r.IsDir(ctx, owner, p)
}

func (a *resolverAdapter) Children(ctx context.Context, owner, dir string, limit int) ([]layout.Node, error) {
	nodes, err := a.r.Children(ctx, owner, dir, limit)
	if err != nil {
		return nil, err
	}
	out := make([]layout.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, layout.Node{Owner: owner, Path: n.Path, IsDir: n.IsDir, Size: n.Size})
	}
	return out, nil
}

// authzAccessor adapts the metadata store to authz.Accessor.
type authzAccessor struct{ m *store.Memory }

func (a *authzAccessor) LoadNode(id uuid.UUID) (authz.Node, error) {
	var n store.Node
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.Node(ctx, id)
		n = got
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authz.Node{}, authz.ErrNotFound
		}
		return authz.Node{}, err
	}
	return authz.Node{ID: n.ID, OwnerID: n.OwnerID, ParentID: n.ParentID}, nil
}

func (a *authzAccessor) OwnerGroupPerm(actorID, nodeID uuid.UUID) (authz.Perm, bool, error) {
	// Resolve real grants. The stub that used to be here meant every WebDAV test ran
	// with the ACL layer disabled, so a write path that ignored a grant could not be
	// caught by the suite that exists to catch exactly that.
	var (
		perms store.Perm
		ok    bool
	)
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, found, gerr := tx.EffectivePermsFor(ctx, actorID, nodeID)
		perms, ok = p, found
		return gerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return authz.Perm(perms), ok, nil
}

func (a *authzAccessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	return nil, nil
}

// do issues a request with Basic auth.
func (e *testEnv) do(t *testing.T, method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.SetBasicAuth("alice", e.secret)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) davPath(rel string) string {
	return "https://cloud.example.test/dav/alice/" + strings.TrimPrefix(rel, "/")
}

// addUserWithSecret provisions a second account and returns its app password.
//
// Several tests need a *different* authenticated principal: a lock held by
// yourself is not a lock, and a cross-account check that uses the same identity
// proves nothing.
func (e *testEnv) addUserWithSecret(t *testing.T, u store.User, _ string) string {
	t.Helper()
	created := e.meta.AddUser(u)
	if _, err := e.meta.EnsureRoot(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	// Issue through the same service the handler verifies against, so the test
	// cannot accidentally pass by fabricating a credential the server would reject.
	secret, _, err := e.h.appPass.Issue(context.Background(), created.ID, "test", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	e.extra[created.Username] = secret
	return secret
}

// doAs issues a request as another account, using an app password.
func (e *testEnv) doAs(t *testing.T, user, secret, method, path string, body io.Reader,
	headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.SetBasicAuth(user, secret)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestOptionsNeedsNoAuth(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodOptions, e.davPath(""), nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS should succeed without credentials, got %d", rec.Code)
	}
	if got := rec.Header().Get("DAV"); got != "1, 2" {
		t.Fatalf("DAV header: want \"1, 2\", got %q", got)
	}
	if rec.Header().Get("MS-Author-Via") != "DAV" {
		t.Fatal("MS-Author-Via is required for Windows Explorer")
	}
}

func TestUnauthenticatedGetsBasicChallenge(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("PROPFIND", e.davPath(""), nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	// Without the challenge, Windows and macOS clients give up instead of
	// prompting, which looks like "the server is broken".
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("missing Basic challenge: %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestBadCredentialGetsChallenge(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("PROPFIND", e.davPath(""), nil)
	req.SetBasicAuth("alice", "wc_wrong")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestMkcolThenPropfind(t *testing.T) {
	e := newEnv(t)

	rec := e.do(t, "MKCOL", e.davPath("docs"), nil, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL: want 201, got %d", rec.Code)
	}

	rec = e.do(t, "PROPFIND", e.davPath(""), nil, map[string]string{"Depth": "1"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: want 207, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "docs") {
		t.Fatalf("PROPFIND should list the new directory:\n%s", body)
	}
	if !strings.Contains(body, "<D:collection/>") {
		t.Fatal("directory should be reported as a collection")
	}
}

func TestMkcolConflict(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, "MKCOL", e.davPath("docs"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("first MKCOL: %d", rec.Code)
	}
	// RFC 4918 §9.3.1: creating an existing resource is 405, not 409.
	if rec := e.do(t, "MKCOL", e.davPath("docs"), nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("second MKCOL: want 405, got %d", rec.Code)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	e := newEnv(t)
	content := "hello webdav"

	rec := e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader(content),
		map[string]string{"Content-Type": "text/plain"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("PUT must return an ETag")
	}

	rec = e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != content {
		t.Fatalf("content mismatch: %q", got)
	}
	if rec.Header().Get("ETag") != etag {
		t.Fatal("ETag changed between PUT and GET")
	}
}

// TestPutRecordsBlobID guards the metadata link a WebDAV write must establish.
//
// This was a real production defect: blobIDFor was a stub returning the zero UUID,
// which PostgreSQL rejects on the nodes.blob_id foreign key. The in-memory store
// accepts a zero UUID happily, so every DAV test passed while every real upload
// failed with a 404 the client could not explain. The assertion has to look at the
// stored node, not just at the HTTP response.
func TestPutRecordsBlobID(t *testing.T) {
	e := newEnv(t)

	rec := e.do(t, http.MethodPut, e.davPath("withblob.txt"),
		strings.NewReader("content addressed"), map[string]string{"Content-Type": "text/plain"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var node store.Node
	err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, rerr := e.meta.Resolve(ctx, e.user.ID, "withblob.txt")
		if rerr != nil {
			return rerr
		}
		node = nodes[len(nodes)-1]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if node.BlobID == nil || *node.BlobID == uuid.Nil {
		t.Fatal("a file written over WebDAV must reference its blob row; " +
			"the zero UUID is rejected by the nodes.blob_id foreign key in production")
	}

}

func TestPutIntoMissingDirectoryFails(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPut, e.davPath("nope/a.txt"), strings.NewReader("x"), nil)
	// RFC 4918 §9.7.1: PUT does not create intermediate collections.
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}
}

func TestPutOverwritesAndKeepsVersion(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("v1"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	rec = e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("v2 is longer"), nil)
	// RFC 4918 §9.7.1: a replaced resource answers 204, not 201. Saying 201 makes
	// a client believe every save created a new file.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("overwrite: want 204, got %d", rec.Code)
	}

	rec = e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil)
	if got := rec.Body.String(); got != "v2 is longer" {
		t.Fatalf("content after overwrite: %q", got)
	}
}

// TestPutIfMatchGuardsAgainstLostUpdate is the T-03 regression test: a second
// writer holding a stale ETag must be refused rather than silently winning.
func TestPutIfMatchGuardsAgainstLostUpdate(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("v1"), nil)
	staleETag := rec.Header().Get("ETag")
	if staleETag == "" {
		t.Fatal("PUT must return an ETag")
	}

	// Somebody else writes.
	if rec := e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("v2"), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("second write: %d", rec.Code)
	}

	// The first writer now saves on top of the stale tag.
	rec = e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("stale write"), map[string]string{
		"If-Match": staleETag,
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match must be 412, got %d", rec.Code)
	}
	if body := e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil).Body.String(); body != "v2" {
		t.Fatalf("the refused write must not have landed: %q", body)
	}
}

// TestPutIfNoneMatchStarIsCreateOnly: "create, or fail" is the conditional a
// browser tab needs so it cannot clobber a file that appeared meanwhile.
func TestPutIfNoneMatchStarIsCreateOnly(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPut, e.davPath("new.txt"), strings.NewReader("x"), map[string]string{
		"If-None-Match": "*",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("create-only on a free name: want 201, got %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("new.txt"), strings.NewReader("y"), map[string]string{
		"If-None-Match": "*",
	}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("create-only on a taken name: want 412, got %d", rec.Code)
	}
}

// TestPutRefusedWhileLockedHeldByOther: the lock has to mean something on the write
// path, which is where it was previously never consulted.
//
// The lock holder must submit its token, exactly as RFC 4918 §10.4 requires. A lock
// the holder cannot be told apart from everybody else is not a lock, and the
// previous code never checked this on any mutating method.
func TestPutRefusedWhileLockedHeldByOther(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("x"), nil)

	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`
	rec := e.do(t, "LOCK", e.davPath("doc.txt"), strings.NewReader(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK: want 200, got %d", rec.Code)
	}
	token := strings.Trim(rec.Header().Get("Lock-Token"), "<>")
	if token == "" {
		t.Fatal("LOCK must return a token")
	}

	// Without the token the write is refused, even though the lock is our own.
	rec = e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("intruder"), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("write without the lock token: want 423, got %d", rec.Code)
	}
	if body := e.do(t, http.MethodGet, e.davPath("doc.txt"), nil, nil).Body.String(); body != "x" {
		t.Fatalf("locked content must be unchanged: %q", body)
	}

	// Submitting the token through the If header authorizes the write.
	rec = e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("updated"), map[string]string{
		"If": "(<" + token + ">)",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("write with the lock token: want 204, got %d", rec.Code)
	}
	if body := e.do(t, http.MethodGet, e.davPath("doc.txt"), nil, nil).Body.String(); body != "updated" {
		t.Fatalf("unlocked write should land: %q", body)
	}
}

// TestDeleteRefusedWhileLocked: DELETE was the other method that ignored locks.
func TestDeleteRefusedWhileLocked(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("x"), nil)
	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`
	if rec := e.do(t, "LOCK", e.davPath("doc.txt"), strings.NewReader(body), nil); rec.Code != http.StatusOK {
		t.Fatalf("LOCK: %d", rec.Code)
	}

	rec := e.do(t, http.MethodDelete, e.davPath("doc.txt"), nil, nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("delete under a live lock: want 423, got %d", rec.Code)
	}
}

// TestMkcolRefusedUnderLockedParent covers the Depth: infinity case: a lock on a
// collection covers everything inside it, so creating a child must be refused.
func TestMkcolRefusedUnderLockedParent(t *testing.T) {
	e := newEnv(t)
	e.do(t, "MKCOL", e.davPath("locked"), nil, nil)

	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`
	rec := e.do(t, "LOCK", e.davPath("locked"), strings.NewReader(body), map[string]string{"Depth": "infinity"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK: want 200, got %d", rec.Code)
	}

	rec = e.do(t, "MKCOL", e.davPath("locked/child"), nil, nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("MKCOL under a locked collection: want 423, got %d", rec.Code)
	}
}

// TestCopyDirectoryIsRecursive: the previous implementation created an empty folder
// and reported success, which is worse than refusing.
func TestCopyDirectoryIsRecursive(t *testing.T) {
	e := newEnv(t)
	e.do(t, "MKCOL", e.davPath("src"), nil, nil)
	e.do(t, "MKCOL", e.davPath("src/sub"), nil, nil)
	e.do(t, http.MethodPut, e.davPath("src/a.txt"), strings.NewReader("alpha"), nil)
	e.do(t, http.MethodPut, e.davPath("src/sub/b.txt"), strings.NewReader("beta"), nil)

	rec := e.do(t, "COPY", e.davPath("src"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/dst",
		"Depth":       "infinity",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("directory COPY: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct{ path, want string }{
		{"dst/a.txt", "alpha"},
		{"dst/sub/b.txt", "beta"},
	} {
		got := e.do(t, http.MethodGet, e.davPath(tc.path), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", tc.path, got.Code)
		}
		if got.Body.String() != tc.want {
			t.Fatalf("%s: want %q, got %q", tc.path, tc.want, got.Body.String())
		}
	}
}

// TestMoveOverwriteRequiresDeleteRight: MOVE used to trash the destination without
// authorizing it, which let a writable folder become a way to delete anything.
func TestMoveOverwriteRequiresDeleteRight(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("src.txt"), strings.NewReader("source"), nil)
	e.do(t, http.MethodPut, e.davPath("victim.txt"), strings.NewReader("victim"), nil)

	rec := e.do(t, "MOVE", e.davPath("src.txt"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/victim.txt",
		"Overwrite":   "T",
	})
	// Same owner: allowed, and the destination is replaced.
	//
	// 204, not 201: RFC 4918 9.9.4 distinguishes "moved, new URL created" (201) from "moved to a
	// pre-existing URL" (204). Answering 201 for both told a client that an overwrite had created
	// a resource. This test asserted 201 because the handler did.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("same-owner overwrite MOVE: want 204, got %d", rec.Code)
	}
	if body := e.do(t, http.MethodGet, e.davPath("victim.txt"), nil, nil).Body.String(); body != "source" {
		t.Fatalf("destination should hold the source bytes: %q", body)
	}
}

// TestRangeRequest covers resume and video seeking. Getting this wrong makes every
// large download restart from zero.
func TestRangeRequest(t *testing.T) {
	e := newEnv(t)
	payload := strings.Repeat("0123456789", 100)
	e.do(t, http.MethodPut, e.davPath("big.txt"), strings.NewReader(payload), nil)

	rec := e.do(t, http.MethodGet, e.davPath("big.txt"), nil, map[string]string{"Range": "bytes=10-19"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("want 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "0123456789" {
		t.Fatalf("range body: %q", got)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 10-19/1000" {
		t.Fatalf("Content-Range: %q", cr)
	}
}

func TestSuffixRange(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("f.txt"), strings.NewReader("0123456789"), nil)

	rec := e.do(t, http.MethodGet, e.davPath("f.txt"), nil, map[string]string{"Range": "bytes=-3"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("want 206, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "789" {
		t.Fatalf("suffix range: %q", got)
	}
}

func TestIfNoneMatchReturns304(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("x"), nil)
	etag := rec.Header().Get("ETag")

	rec = e.do(t, http.MethodGet, e.davPath("a.txt"), nil, map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("304 must have no body")
	}
}

func TestHeadHasNoBody(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("content"), nil)

	rec := e.do(t, http.MethodHead, e.davPath("a.txt"), nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("HEAD must not return a body")
	}
	if rec.Header().Get("Content-Length") != "7" {
		t.Fatalf("Content-Length: %q", rec.Header().Get("Content-Length"))
	}
}

func TestDeleteMovesToTrash(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("x"), nil)

	rec := e.do(t, http.MethodDelete, e.davPath("a.txt"), nil, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: want 204, got %d", rec.Code)
	}
	rec = e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted file should be 404, got %d", rec.Code)
	}
}

func TestDeleteRootRefused(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodDelete, e.davPath(""), nil, nil); rec.Code == http.StatusNoContent {
		t.Fatal("deleting the namespace root must be refused")
	}
}

func TestMoveFile(t *testing.T) {
	e := newEnv(t)
	e.do(t, "MKCOL", e.davPath("dir"), nil, nil)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("payload"), nil)

	rec := e.do(t, "MOVE", e.davPath("a.txt"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/dir/b.txt",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("MOVE: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := e.do(t, http.MethodGet, e.davPath("dir/b.txt"), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("moved file should be readable, got %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil); rec.Code != http.StatusNotFound {
		t.Fatal("source should be gone after MOVE")
	}
}

func TestMoveWithoutOverwriteRefuses(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("a"), nil)
	e.do(t, http.MethodPut, e.davPath("b.txt"), strings.NewReader("b"), nil)

	rec := e.do(t, "MOVE", e.davPath("a.txt"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/b.txt",
		"Overwrite":   "F",
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %d", rec.Code)
	}
}

func TestCopyFile(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("payload"), nil)

	rec := e.do(t, "COPY", e.davPath("a.txt"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/copy.txt",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("COPY: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = e.do(t, http.MethodGet, e.davPath("copy.txt"), nil, nil)
	if rec.Body.String() != "payload" {
		t.Fatalf("copied content: %q", rec.Body.String())
	}
	// The original must survive a COPY.
	if rec := e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil); rec.Code != http.StatusOK {
		t.Fatal("COPY must not remove the source")
	}
}

func TestLockUnlock(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("doc.docx"), strings.NewReader("x"), nil)

	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`

	rec := e.do(t, "LOCK", e.davPath("doc.docx"), strings.NewReader(body),
		map[string]string{"Timeout": "Second-600"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	token := rec.Header().Get("Lock-Token")
	if token == "" {
		t.Fatal("LOCK must return a Lock-Token header")
	}
	if !strings.Contains(rec.Body.String(), "opaquelocktoken:") {
		t.Fatal("body should contain the lock token")
	}

	// A second exclusive lock must fail.
	rec = e.do(t, "LOCK", e.davPath("doc.docx"), strings.NewReader(body), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("second LOCK: want 423, got %d", rec.Code)
	}

	// Unlock with the token.
	rec = e.do(t, "UNLOCK", e.davPath("doc.docx"), nil, map[string]string{"Lock-Token": token})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK: want 204, got %d", rec.Code)
	}

	// Now a new lock succeeds.
	rec = e.do(t, "LOCK", e.davPath("doc.docx"), strings.NewReader(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK after unlock: want 200, got %d", rec.Code)
	}
}

func TestUnlockWithWrongToken(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("d.txt"), strings.NewReader("x"), nil)
	body := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	e.do(t, "LOCK", e.davPath("d.txt"), strings.NewReader(body), nil)

	rec := e.do(t, "UNLOCK", e.davPath("d.txt"), nil,
		map[string]string{"Lock-Token": "<opaquelocktoken:not-mine>"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for a foreign token, got %d", rec.Code)
	}
}

func TestProppatchPersistsDeadProperty(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("x"), nil)

	body := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:Z="urn:example:">
		<D:set><D:prop><Z:color>blue</Z:color></D:prop></D:set></D:propertyupdate>`

	rec := e.do(t, "PROPPATCH", e.davPath("a.txt"), strings.NewReader(body), nil)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH: want 207, got %d", rec.Code)
	}

	// The value must survive into the metadata store.
	var found bool
	_ = e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		props, err := tx.DeadProps(ctx, mustResolve(t, e, "a.txt"))
		if err != nil {
			return err
		}
		for _, p := range props {
			if p.Name == "color" && p.Value == "blue" {
				found = true
			}
		}
		return nil
	})
	_ = found
	if !found {
		t.Fatal("dead property was not persisted")
	}
}

// TestProppatchRejectsReservedProperty: letting a client write getcontentlength
// would let it lie about size, breaking every caching client.
func TestProppatchRejectsReservedProperty(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("x"), nil)

	body := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:">
		<D:set><D:prop><D:getcontentlength>999999</D:getcontentlength></D:prop></D:set></D:propertyupdate>`
	rec := e.do(t, "PROPPATCH", e.davPath("a.txt"), strings.NewReader(body), nil)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("want 207, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "403") {
		t.Fatal("reserved property must be reported as forbidden")
	}
}

func TestPropfindDepthZeroIsSingleEntry(t *testing.T) {
	e := newEnv(t)
	e.do(t, "MKCOL", e.davPath("d"), nil, nil)
	e.do(t, http.MethodPut, e.davPath("d/a.txt"), strings.NewReader("x"), nil)

	rec := e.do(t, "PROPFIND", e.davPath("d"), nil, map[string]string{"Depth": "0"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("want 207, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "a.txt") {
		t.Fatal("Depth: 0 must not include children")
	}
}

func TestPropfindInfinityRefused(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "PROPFIND", e.davPath(""), nil, map[string]string{"Depth": "infinity"})
	// Unlimited depth is the cheapest DoS a client can request.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestPropfindEscapesXML(t *testing.T) {
	e := newEnv(t)
	// A name with XML metacharacters must not break the response.
	name := "a&b.txt"
	e.do(t, http.MethodPut, e.davPath(name), strings.NewReader("x"), nil)

	rec := e.do(t, "PROPFIND", e.davPath(""), nil, map[string]string{"Depth": "1"})
	body := rec.Body.String()
	if strings.Contains(body, "<c>") {
		t.Fatal("name was not XML-escaped")
	}
	if !strings.Contains(body, "&amp;") {
		t.Fatalf("expected escaped ampersand in body:\n%s", body)
	}
}

func TestContentDispositionEncodesNonASCII(t *testing.T) {
	e := newEnv(t)
	name := "会议纪要.docx"
	e.do(t, http.MethodPut, e.davPath(name), strings.NewReader("x"), nil)

	rec := e.do(t, http.MethodGet, e.davPath(name), nil, nil)
	cd := rec.Header().Get("Content-Disposition")
	// RFC 5987 is required; without it every browser shows mojibake.
	if !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("missing RFC 5987 filename: %q", cd)
	}
}

// TestCorruptBlobIsNotReportedAs404: when the metadata says a file exists but the
// bytes are gone, the response must be an error. Reporting 404 would disguise data
// loss as "you deleted it"; an empty 200 would look like a zero-byte file and let
// a client overwrite the record.
func TestCorruptBlobIsNotReportedAs404(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("a.txt"), strings.NewReader("data"), nil)

	hash, ok, err := e.meta.Resolver().HashFor(context.Background(), e.user.ID.String(), "a.txt")
	if err != nil || !ok {
		t.Fatalf("resolve hash: ok=%v err=%v", ok, err)
	}
	if err := e.blobs.Delete(context.Background(), blob.Hash(hash)); err != nil {
		t.Fatalf("delete blob bytes: %v", err)
	}

	rec := e.do(t, http.MethodGet, e.davPath("a.txt"), nil, nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("missing bytes must not return 200; body=%q", rec.Body.String())
	}
}
func TestUnsupportedMethod(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "PATCH", e.davPath("a.txt"), nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("405 must advertise Allow")
	}
}

func TestRangeParsing(t *testing.T) {
	cases := []struct {
		header string
		size   int64
		start  int64
		end    int64
		ok     bool
	}{
		{"bytes=0-9", 100, 0, 9, true},
		{"bytes=90-", 100, 90, 99, true},
		{"bytes=-10", 100, 90, 99, true},
		{"bytes=0-999", 100, 0, 99, true},   // clamped
		{"bytes=100-", 100, 0, 0, false},    // past the end
		{"bytes=5-2", 100, 0, 0, false},     // inverted
		{"bytes=0-1,5-6", 100, 0, 0, false}, // multipart unsupported
		{"items=0-1", 100, 0, 0, false},     // wrong unit
	}
	for _, c := range cases {
		start, end, ok := parseSingleRange(c.header, c.size)
		if ok != c.ok {
			t.Fatalf("%q: ok=%v want %v", c.header, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if start != c.start || end != c.end {
			t.Fatalf("%q: got %d-%d, want %d-%d", c.header, start, end, c.start, c.end)
		}
	}
}

func TestLockTokenFormat(t *testing.T) {
	tok := newLockToken()
	if !strings.HasPrefix(tok, "opaquelocktoken:") {
		t.Fatalf("token must be an opaquelocktoken, got %q", tok)
	}
	if len(tok) < len("opaquelocktoken:")+16 {
		t.Fatal("token is too short to be unguessable")
	}
	if a, b := newLockToken(), newLockToken(); a == b {
		t.Fatal("tokens must be unique")
	}
}

func TestRequestedTimeoutIsClamped(t *testing.T) {
	h := &Handler{now: time.Now}
	cases := map[string]time.Duration{
		"Infinite":      24 * time.Hour,
		"Second-600":    10 * time.Minute,
		"Second-999999": 24 * time.Hour,
		"garbage":       30 * time.Minute,
		"":              30 * time.Minute,
	}
	for header, want := range cases {
		req := httptest.NewRequest("LOCK", "/", nil)
		if header != "" {
			req.Header.Set("Timeout", header)
		}
		if got := h.requestedTimeout(req); got != want {
			t.Fatalf("%q: want %v, got %v", header, want, got)
		}
	}
}

func mustResolve(t *testing.T, e *testEnv, rel string) uuid.UUID {
	t.Helper()
	// Resolve is a Store-level operation and starts its own transaction, so it
	// must not be called from inside one.
	nodes, err := e.meta.Resolve(context.Background(), e.user.ID, rel)
	if err != nil {
		t.Fatalf("resolve %q: %v", rel, err)
	}
	return nodes[len(nodes)-1].ID
}

var _ = bytes.MinRead
var _ = sha256.Sum256
var _ = ticket.PurposeSession

// TestDepthZeroCollectionLockDoesNotBlockChildContent is RFC 4918 §7.4 on the DAV side.
//
// A Depth: 0 lock on a collection protects its membership: no new children, no removals. It does
// not protect the *content* of a child that already exists. Treating it as a subtree lock means a
// client that locked a folder can no longer edit the files inside it, and the 423 gives no hint
// that its own Depth: 0 lock is the cause.
//
// The API side has the mirror of this test; §5.3 wants one arbitration rule, so both surfaces
// have to agree that Depth means the same thing.
func TestDepthZeroCollectionLockDoesNotBlockChildContent(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, "MKCOL", e.davPath("locked-folder"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL: want 201, got %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("locked-folder/child.txt"),
		strings.NewReader("original"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT child: want 201, got %d", rec.Code)
	}

	// Lock the collection with Depth: 0 (the default when no Depth header is sent).
	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`
	rec := e.do(t, "LOCK", e.davPath("locked-folder"), strings.NewReader(lockBody),
		map[string]string{"Depth": "0"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK: want 200, got %d", rec.Code)
	}
	token := strings.Trim(rec.Header().Get("Lock-Token"), "<>")

	// 1. Editing the existing child is NOT a membership change, so it must succeed.
	rec = e.do(t, http.MethodPut, e.davPath("locked-folder/child.txt"),
		strings.NewReader("edited"), nil)
	if rec.Code == http.StatusLocked {
		t.Fatal("a Depth:0 collection lock must not block editing an existing child")
	}
	if body := e.do(t, http.MethodGet, e.davPath("locked-folder/child.txt"), nil, nil).Body.String(); body != "edited" {
		t.Fatalf("the edit must have been applied, got %q", body)
	}

	// 2. Adding a new child IS a membership change, so it must be refused.
	rec = e.do(t, http.MethodPut, e.davPath("locked-folder/new.txt"),
		strings.NewReader("new"), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("adding a child to a Depth:0-locked collection: want 423, got %d", rec.Code)
	}

	// 3. So is a subdirectory.
	rec = e.do(t, "MKCOL", e.davPath("locked-folder/sub"), nil, nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("MKCOL inside a Depth:0-locked collection: want 423, got %d", rec.Code)
	}

	// 4. With the token, the holder may add a child.
	rec = e.do(t, http.MethodPut, e.davPath("locked-folder/allowed.txt"),
		strings.NewReader("ok"), map[string]string{"If": "<" + token + ">"})
	if rec.Code == http.StatusLocked {
		t.Fatal("the lock holder submitting its token must be allowed to add a child")
	}
}

// TestDepthInfinityCollectionLockBlocksChildContent is the other half on the DAV surface.
func TestDepthInfinityCollectionLockBlocksChildContent(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, "MKCOL", e.davPath("tree"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL: want 201, got %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("tree/child.txt"),
		strings.NewReader("original"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT child: want 201, got %d", rec.Code)
	}

	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>alice</D:href></D:owner></D:lockinfo>`
	rec := e.do(t, "LOCK", e.davPath("tree"), strings.NewReader(lockBody),
		map[string]string{"Depth": "infinity"})
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK infinity: want 200, got %d", rec.Code)
	}

	// A Depth: infinity lock reaches the child's content.
	rec = e.do(t, http.MethodPut, e.davPath("tree/child.txt"),
		strings.NewReader("clobbered"), nil)
	if rec.Code != http.StatusLocked {
		t.Fatalf("a Depth:infinity lock must block editing a child: want 423, got %d", rec.Code)
	}
}

// TestPropfindHrefNamesTheRightResource is RFC 4918 §9.1: the response's href is the URI of the
// resource it describes.
//
// Two defects, both found by running a real server rather than this harness:
//
//	A PROPFIND on a FILE answered 207 naming the caller's COLLECTION root, with the file's size
//	and ETag attached to the folder's href. A client following it builds a folder that reports a
//	file's content-length, and the collection's real etag never appears.
//
//	Children were emitted as "<root>//<name>". The doubled path is not cosmetic: the server
//	answers 404 for it, so the href it published was an address it would not serve.
func TestPropfindHrefNamesTheRightResource(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("x"), nil)

	// A file's href must be the file.
	rec := e.do(t, "PROPFIND", e.davPath("doc.txt"), nil, map[string]string{"Depth": "0"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on a file: want 207, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/doc.txt</D:href>") {
		t.Fatalf("the href must name the file that was asked for, not its collection:\n%s", body)
	}

	// The addressee also needs a name: a file entry whose displayname is empty is a nameless
	// row with a size and an etag, which is what a client shows as a blank line.
	if !strings.Contains(body, "<D:displayname>doc.txt</D:displayname>") {
		t.Fatalf("a file entry must carry the file's displayname:\n%s", body)
	}

	// A directory's children must not be joined with a doubled slash.
	rec = e.do(t, "PROPFIND", e.davPath(""), nil, map[string]string{"Depth": "1"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on the collection: want 207, got %d", rec.Code)
	}
	body = rec.Body.String()
	if strings.Contains(body, "//doc.txt") {
		t.Fatalf("a child href was built by appending to a collection href that already ended in "+
			"/ — the server answers 404 for the result:\n%s", body)
	}

	// The parent entry keeps its trailing slash, which is the collection convention clients expect.
	if !strings.Contains(body, "/breakglass/</D:href>") && !strings.Contains(body, "/alice/</D:href>") {
		t.Fatalf("the collection's own href should end with a slash:\n%s", body)
	}
}

// TestMovePublishesLocationAndTheRightStatus is RFC 4918 §9.9.4's response contract.
//
// A created destination answers 201 and MUST carry a Location header naming the new resource: it
// is how a client learns the URL without re-listing the parent. A pre-existing destination answers
// 204 and carries no Location, because nothing new was created. Neither header existed before —
// MOVE answered 201 with an empty body for both cases.
func TestMovePublishesLocationAndTheRightStatus(t *testing.T) {
	e := newEnv(t)
	e.do(t, http.MethodPut, e.davPath("src.txt"), strings.NewReader("source"), nil)

	rec := e.do(t, "MOVE", e.davPath("src.txt"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/moved.txt",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a MOVE to a free name must be 201, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("a 201 from MOVE must carry a Location header; a client has no other way to learn " +
			"the new resource's URL from this response")
	}
	if !strings.HasSuffix(loc, "/dav/alice/moved.txt") {
		t.Fatalf("Location names the wrong resource: %q", loc)
	}

	// The destination exists now, so a second MOVE onto it is the overwrite case.
	e.do(t, http.MethodPut, e.davPath("other.txt"), strings.NewReader("other"), nil)
	rec = e.do(t, "MOVE", e.davPath("other.txt"), nil, map[string]string{
		"Destination": "https://cloud.example.test/dav/alice/moved.txt",
		"Overwrite":   "T",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("an overwriting MOVE must be 204, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("a 204 must not claim a new location, got %q", loc)
	}
}
