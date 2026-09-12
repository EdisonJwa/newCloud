package server_test

// Cross-interface write arbitration (§5.3).
//
// The requirement says the web/API and WebDAV must enter the same write arbitration layer, and
// that a lock only the server internally understands is not compatible. Before this round the
// rule held in one direction only: WebDAV honoured locks and the JSON API ignored them. A
// browser could overwrite a file a WebDAV client had locked, which made the lock advisory for
// whoever did not bother to take one.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// lockNode takes a WebDAV-style lock as the owner and returns its token.
func (h *harness) lockNode(t *testing.T, nodeID uuid.UUID) string {
	t.Helper()
	return h.lockNodeDepth(t, nodeID, "0")
}

// lockNodeDepth takes a lock with an explicit Depth.
//
// Depth is not cosmetic: RFC 4918 §7.4 says a Depth: 0 collection lock protects membership while
// Depth: infinity covers the subtree. Tests that need to tell the two apart have to set it.
func (h *harness) lockNodeDepth(t *testing.T, nodeID uuid.UUID, depth string) string {
	t.Helper()
	token := "opaquelocktoken:" + uuid.New().String()
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.CreateLock(ctx, store.Lock{
			Token:      token,
			NodeID:     nodeID,
			UserID:     &h.user.ID,
			OwnerXML:   "<D:href>test</D:href>",
			Depth:      depth,
			TimeoutSec: 3600,
			ExpiresAt:  time.Now().Add(time.Hour),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

// TestHTTPOverwriteHonoursWebDAVLock is the rule itself.
func TestHTTPOverwriteHonoursWebDAVLock(t *testing.T) {
	h := newHarness(t)
	node := h.uploadFile(t, "locked.txt", "the original")
	token := h.lockNode(t, node.ID)

	// An HTTP overwrite without the token must be refused with 423, not silently applied.
	mb := multipartBody(t, "", "locked.txt", "the clobber")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("an HTTP write against a locked file must be 423, got %d", resp.StatusCode)
	}

	// The content is untouched — the point of the lock.
	dl := h.do(http.MethodGet, "/api/files/download?path=locked.txt", nil, nil)
	defer dl.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(dl.Body)
	if buf.String() != "the original" {
		t.Fatalf("the locked file must be unchanged, got %q", buf.String())
	}

	// The holder may write through its own lock by presenting the token.
	mb2 := multipartBody(t, "", "locked.txt", "the authorized edit")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/files/upload", mb2)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+mb2.Boundary())
	req.Header.Set("If", "<"+token+">")
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
	ok, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("the lock holder with its token must be allowed to write, got %d", ok.StatusCode)
	}
}

// TestHTTPDeleteHonoursWebDAVLock: deletion is a write, and a lock must block it.
func TestHTTPDeleteHonoursWebDAVLock(t *testing.T) {
	h := newHarness(t)
	node := h.uploadFile(t, "protected.txt", "keep me")
	h.lockNode(t, node.ID)

	resp := h.do(http.MethodDelete, "/api/files?path=protected.txt", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("deleting a locked file must be 423, got %d", resp.StatusCode)
	}

	// It is still there.
	dl := h.do(http.MethodGet, "/api/files/download?path=protected.txt", nil, nil)
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("the file must survive, got %d", dl.StatusCode)
	}
}

// TestHTTPRenameHonoursWebDAVLock.
func TestHTTPRenameHonoursWebDAVLock(t *testing.T) {
	h := newHarness(t)
	node := h.uploadFile(t, "fixed.txt", "content")
	h.lockNode(t, node.ID)

	body, _ := json.Marshal(map[string]string{"path": "fixed.txt", "name": "moved.txt"})
	resp := h.do(http.MethodPost, "/api/files/rename", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("renaming a locked file must be 423, got %d", resp.StatusCode)
	}
}

// TestUnlockedWritesStillWork is the negative control.
//
// Without it the three cases above would all pass if the guard refused everything, which is the
// failure mode this project has hit before: a check that "works" because it never lets anything
// through.
func TestUnlockedWritesStillWork(t *testing.T) {
	h := newHarness(t)

	// Upload (create).
	node := h.uploadFile(t, "free.txt", "v1")

	// Overwrite with no lock.
	mb := multipartBody(t, "", "free.txt", "v2")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("an unlocked overwrite must succeed, got %d", resp.StatusCode)
	}

	// Rename.
	body, _ := json.Marshal(map[string]string{"path": "free.txt", "name": "renamed.txt"})
	ren := h.do(http.MethodPost, "/api/files/rename", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	ren.Body.Close()
	if ren.StatusCode != http.StatusOK {
		t.Fatalf("an unlocked rename must succeed, got %d", ren.StatusCode)
	}

	// Delete.
	del := h.do(http.MethodDelete, "/api/files?path=renamed.txt", nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusOK && del.StatusCode != http.StatusNoContent {
		t.Fatalf("an unlocked delete must succeed, got %d", del.StatusCode)
	}

	_ = node
}

// TestExpiredLockDoesNotBlock: a lock's timeout is meaningful, otherwise a crashed client would
// make a file permanently unwritable.
func TestExpiredLockDoesNotBlock(t *testing.T) {
	h := newHarness(t)
	node := h.uploadFile(t, "stale.txt", "v1")

	// A lock that has already expired.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.CreateLock(ctx, store.Lock{
			Token:      "opaquelocktoken:" + uuid.New().String(),
			NodeID:     node.ID,
			UserID:     &h.user.ID,
			OwnerXML:   "<D:href>stale</D:href>",
			Depth:      "0",
			TimeoutSec: 1,
			ExpiresAt:  time.Now().Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}

	mb := multipartBody(t, "", "stale.txt", "v2")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("an expired lock must not block a write, got %d", resp.StatusCode)
	}
}

// TestDepthZeroParentLockBlocksMembershipNotContent is the RFC 4918 §7.4 distinction.
//
// A Depth: 0 lock on a collection protects its *membership*: you may not add or remove children.
// It says nothing about the content of an existing child. Getting this wrong in either direction
// is a real bug:
//
//   - too strict: a client locks a folder and can no longer edit any file inside it, and the 423
//     gives no hint that its own Depth: 0 lock is the cause;
//   - too lax: a client locks a folder to prevent deletions and a browser deletes a file anyway.
func TestDepthZeroParentLockBlocksMembershipNotContent(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "shared-folder")
	folder := h.resolveNode(t, "shared-folder")

	// Seed the child INSIDE the folder. Uploading it at the root would make the later edit a
	// creation rather than a replacement, and the test would then prove nothing about content.
	seed := multipartBody(t, "shared-folder", "inside.txt", "original")
	seedResp := h.do(http.MethodPost, "/api/files/upload", seed,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + seed.Boundary()})
	seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusCreated {
		t.Fatalf("seed upload into the folder: %d", seedResp.StatusCode)
	}

	// A Depth: 0 lock on the folder.
	h.lockNodeDepth(t, folder.ID, "0")

	// 1. Creating a new child is a membership change: blocked.
	mb := multipartBody(t, "shared-folder", "added.txt", "new file")
	addResp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	addResp.Body.Close()
	if addResp.StatusCode != http.StatusLocked {
		t.Fatalf("adding a child to a Depth:0-locked collection must be 423, got %d",
			addResp.StatusCode)
	}

	// 2. Creating a subdirectory is also a membership change: blocked.
	dirBody, _ := json.Marshal(map[string]string{"path": "shared-folder/sub"})
	dirResp := h.do(http.MethodPost, "/api/files/mkdir", bytes.NewReader(dirBody),
		map[string]string{"Content-Type": "application/json"})
	dirResp.Body.Close()
	if dirResp.StatusCode != http.StatusLocked {
		t.Fatalf("mkdir inside a Depth:0-locked collection must be 423, got %d", dirResp.StatusCode)
	}

	// 3. Replacing the CONTENT of the existing child is not a membership change: allowed.
	edit := multipartBody(t, "shared-folder", "inside.txt", "edited content")
	editResp := h.do(http.MethodPost, "/api/files/upload", edit,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + edit.Boundary()})
	editResp.Body.Close()
	if editResp.StatusCode == http.StatusLocked {
		body, _ := io.ReadAll(editResp.Body)
		t.Fatalf("a Depth:0 lock on the parent must not block editing an existing child file: status=%d body=%s", editResp.StatusCode, body)
	}
}

// TestDepthInfinityParentLockBlocksContent is the other half: a Depth: infinity lock does reach
// down, so the same edit must be refused.
func TestDepthInfinityParentLockBlocksContent(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "deep-folder")
	folder := h.resolveNode(t, "deep-folder")

	// Upload into the folder, then lock it.
	seed := multipartBody(t, "deep-folder", "target.txt", "original")
	seedResp := h.do(http.MethodPost, "/api/files/upload", seed,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + seed.Boundary()})
	seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusCreated {
		t.Fatalf("seed upload: %d", seedResp.StatusCode)
	}

	h.lockNodeDepth(t, folder.ID, "infinity")

	edit := multipartBody(t, "deep-folder", "target.txt", "clobbered")
	resp := h.do(http.MethodPost, "/api/files/upload", edit,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + edit.Boundary()})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("a Depth:infinity lock on the parent must block editing a child, got %d",
			resp.StatusCode)
	}
}

// TestWebDAVLockBlocksAPIWrites is §5.3's Lock 的语义需适用于跨接口操作.
//
// What this adds over the cases above: they are thorough, but they take the lock by inserting a
// lock ROW (h.lockNode calls tx.CreateLock). That proves the API consults the lock table; it does
// not prove that a lock taken through the WebDAV ENDPOINT is the same lock the API reads. A bug in
// the DAV handler — a different scope, a different node id, a token stored somewhere else — would
// leave every one of those tests green.
//
// So this takes the lock the way a real client does: LOCK over HTTP with Basic auth against an app
// password, on the /dav mount. Only then does the end-to-end claim hold, that a WebDAV client's
// lock actually protects the file from the web UI.
func TestWebDAVLockBlocksAPIWrites(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A file to lock, captured so the test can ask the store what lock exists.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "locked.txt")
	_, _ = io.WriteString(fw, "original")
	_ = mw.Close()
	upResp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	upResp.Body.Close()

	// An app password, because WebDAV authenticates with Basic auth rather than the session cookie.
	apResp := h.do(http.MethodPost, "/api/apppasswords/",
		strings.NewReader(`{"label":"lock-client","scope":"rw"}`),
		map[string]string{"Content-Type": "application/json"})
	if apResp.StatusCode != http.StatusCreated {
		t.Fatalf("app password: %d %s", apResp.StatusCode, readBody(apResp))
	}
	secret, _ := decode(t, apResp)["secret"].(string)
	if secret == "" {
		t.Fatal("no app password secret returned")
	}

	// LOCK over WebDAV.
	lockBody := `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">
		<D:lockscope><D:exclusive/></D:lockscope>
		<D:locktype><D:write/></D:locktype>
		<D:owner><D:href>api-test</D:href></D:owner></D:lockinfo>`
	req, err := http.NewRequest("LOCK", h.srv.URL+"/dav/"+h.user.Username+"/locked.txt", strings.NewReader(lockBody))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(h.user.Username, secret)
	lockResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if lockResp.StatusCode != http.StatusOK {
		t.Fatalf("LOCK: %d %s", lockResp.StatusCode, readBody(lockResp))
	}
	token := strings.Trim(lockResp.Header.Get("Lock-Token"), "<>")
	lockResp.Body.Close()
	if token == "" {
		t.Fatal("LOCK returned no token")
	}

	// Every API mutation must refuse while the lock is live. Without the token, exactly as a
	// second client would arrive.
	rename := h.do(http.MethodPost, "/api/files/rename",
		strings.NewReader(`{"path":"locked.txt","name":"renamed.txt"}`),
		map[string]string{"Content-Type": "application/json"})
	renameStatus := rename.StatusCode
	rename.Body.Close()
	if renameStatus != http.StatusLocked {
		t.Fatalf("renaming a WebDAV-locked file through the JSON API: want 423, got %d. The API "+
			"mirrors the DAV lock check, and if that mirror is wrong the lock is advisory to one "+
			"interface only.", renameStatus)
	}

	del := h.do(http.MethodDelete, "/api/files/?path=locked.txt", nil, nil)
	delStatus := del.StatusCode
	del.Body.Close()
	if delStatus != http.StatusLocked {
		t.Fatalf("deleting a WebDAV-locked file through the JSON API: want 423, got %d", delStatus)
	}

	// And the file is untouched, which is the property the status code stands for.
	get := h.do(http.MethodGet, "/api/files/download?path=locked.txt", nil, nil)
	content := readBody(get)
	get.Body.Close()
	if content != "original" {
		t.Fatalf("the locked file's content changed to %q", content)
	}

	// The holder can still write, through the API, by presenting the token. A lock its own holder
	// cannot use is a bug in the other direction.
	rename2 := h.do(http.MethodPost, "/api/files/rename",
		strings.NewReader(`{"path":"locked.txt","name":"renamed.txt"}`),
		map[string]string{"Content-Type": "application/json", "X-Lock-Token": token})
	rename2Status := rename2.StatusCode
	rename2.Body.Close()
	if rename2Status != http.StatusOK {
		t.Fatalf("the lock holder presenting its token through the API: want 200, got %d. The "+
			"token must be honoured across interfaces, or a WebDAV client's lock would make its "+
			"own file unwritable from the web UI.", rename2Status)
	}
	_ = ctx
}
