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
	"net/http"
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
