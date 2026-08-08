package server_test

// SHARE-06, the half that was left open: what a renamed or moved file does to its link.
//
// The requirement asks for the effect of "文件删除/移动/权限变化" on a link to be defined. Deletion
// was closed in round 27. Move was left as "the node id does not change, so the link keeps working
// — design, not omission, but untested". An untested design decision is the one that gets broken
// by a later refactor and nobody notices until a colleague's link stops working.
//
// Two properties, and they are different claims:
//
//  1. A rename or move keeps the link working. The grant is attached to the node, and the node is
//     the same object with a different name — that is why sharing survives reorganising a tree.
//  2. A move into the trash does NOT keep it working. That is the deletion path, and it must win
//     over the move path.

import (
	"context"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestShareSurvivesRename pins the design decision.
func TestShareSurvivesRename(t *testing.T) {
	h := newHarness(t)
	file := h.uploadFile(t, "before.txt", "the contents")
	token := "share06-rename"
	h.createShareWithToken(t, file.ID, token)

	// Sanity: it serves before the rename.
	if code, _ := h.sharePage(t, token); code != http.StatusOK {
		t.Fatalf("setup: the link should serve before the rename, got %d", code)
	}

	// Rename it.
	resp := h.do(http.MethodPost, "/api/files/rename", jsonBody(t, map[string]string{
		"path": "/before.txt", "name": "after.txt",
	}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The link must still work, and must now name the new name — a link that still worked but
	// reported the old name would mean the share cached a path.
	code, body := h.sharePage(t, token)
	if code != http.StatusOK {
		t.Fatalf("a rename must not break the link (the grant is on the node, not its name), got %d", code)
	}
	if body["root_name"] != "after.txt" {
		t.Fatalf("the link reports %v, want the new name", body["root_name"])
	}
}

// TestShareSurvivesMoveToAnotherFolder.
//
// A move changes the parent, not the node. This is the case a naive implementation gets wrong by
// storing the share's path instead of its node id.
func TestShareSurvivesMoveToAnotherFolder(t *testing.T) {
	h := newHarness(t)
	file := h.uploadFile(t, "portable.txt", "the contents")
	token := "share06-move"
	h.createShareWithToken(t, file.ID, token)

	// A destination folder.
	resp := h.do(http.MethodPost, "/api/files/mkdir", jsonBody(t, map[string]string{
		"path": "/archive",
	}), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("mkdir: want 201/200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodPost, "/api/files/move", jsonBody(t, map[string]string{
		"from": "/portable.txt", "to": "/archive/portable.txt",
	}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	code, body := h.sharePage(t, token)
	if code != http.StatusOK {
		t.Fatalf("a move must not break the link, got %d", code)
	}
	if body["root_name"] != "portable.txt" {
		t.Fatalf("the link no longer names the file: %v", body["root_name"])
	}

	// And it still serves the bytes, not just a listing: a share whose page works but whose
	// download 404s is the partial failure this test exists to exclude.
	dl := h.shareDownload(t, token)
	if dl != http.StatusOK {
		t.Fatalf("the link's download must still work after the move, got %d", dl)
	}
}

// TestMovingToTrashStillBreaksTheLink.
//
// The move path and the delete path meet here: trashing is implemented as a move in some
// codebases. Both orders must end with the link refusing, which is the property round 27 pinned
// for plain deletion — this checks it survives a rename first.
func TestMovingToTrashStillBreaksTheLink(t *testing.T) {
	h := newHarness(t)
	file := h.uploadFile(t, "doomed.txt", "the contents")
	token := "share06-trash-after-rename"
	h.createShareWithToken(t, file.ID, token)

	// Rename, then trash.
	resp := h.do(http.MethodPost, "/api/files/rename", jsonBody(t, map[string]string{
		"path": "/doomed.txt", "name": "still-doomed.txt",
	}), nil)
	resp.Body.Close()
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Trash(ctx, file.ID)
	}); err != nil {
		t.Fatal(err)
	}

	if code, _ := h.sharePage(t, token); code == http.StatusOK {
		t.Fatal("a trashed file must not be served, even after a rename")
	}
}

// shareDownload fetches the content through the link and returns only the status.
func (h *harness) shareDownload(t *testing.T, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
