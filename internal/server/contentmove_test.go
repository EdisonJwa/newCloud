package server_test

// Cross-layout write correctness (§9.2: 两种布局 ... 不得在文件授权或数据完整性上提供不同保证).
//
// A layout that keeps bytes at a path must move them when the metadata says the file moved. If it
// cannot, the two disagree: the row points at a new path and the bytes are at the old one, so the
// file reads as empty or missing while every listing says it is fine.
//
// The metadata commit and the byte move cannot be one transaction. The server's answer is to put
// the metadata back when the bytes cannot follow, and to tell the client the move failed — rather
// than the previous behaviour, which discarded the error and answered 200.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// failingMoveContent is a content store whose bytes cannot be moved.
//
// EfficientRename is forced to false so the handler treats it like the tree layout — the layout
// for which the call is real. The default blob layout skips the call entirely, which is why the
// discarded error went unnoticed for so long.
type failingMoveContent struct {
	layout.ContentStore
	err error
}

func (f failingMoveContent) Capabilities() layout.Caps {
	caps := f.ContentStore.Capabilities()
	caps.EfficientRename = false
	return caps
}

func (f failingMoveContent) Move(context.Context, layout.Node, layout.Node) error { return f.err }

var errContentMove = errors.New("simulated: the bytes cannot be moved")

// TestRenameFailsWhenTheBytesCannotFollow.
func TestRenameFailsWhenTheBytesCannotFollow(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) {
		d.Content = failingMoveContent{ContentStore: d.Content, err: errContentMove}
	})
	h.uploadFile(t, "movable.txt", "content")

	resp := h.do(http.MethodPost, "/api/files/rename",
		strings.NewReader(`{"path":"movable.txt","name":"renamed.txt"}`),
		map[string]string{"Content-Type": "application/json"})
	status := resp.StatusCode
	payload := readBody(resp)
	resp.Body.Close()

	if status == http.StatusOK {
		t.Fatalf("a rename whose bytes could not follow answered 200 (%s). The metadata now names "+
			"a path the content store has no bytes for: the file lists correctly and reads as "+
			"missing.", payload)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("want 500 so the client knows the move failed, got %d %s", status, payload)
	}

	// The metadata must be back where it started, so the file is still readable under its
	// original name.
	get := h.do(http.MethodGet, "/api/files/download?path=movable.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "content" {
		t.Fatalf("the file must still be readable under its original name after a refused "+
			"rename, got %q", body)
	}
	// And it must not exist under the new name.
	gone := h.do(http.MethodGet, "/api/files/download?path=renamed.txt", nil, nil)
	gone.Body.Close()
	if gone.StatusCode == http.StatusOK {
		t.Fatal("the refused rename left the file under the new name as well")
	}
}

// TestMoveFailsWhenTheBytesCannotFollow is the same rule for a move into another folder.
func TestMoveFailsWhenTheBytesCannotFollow(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) {
		d.Content = failingMoveContent{ContentStore: d.Content, err: errContentMove}
	})
	h.uploadFile(t, "src.txt", "payload")
	h.mkdir(t, "dest")

	resp := h.do(http.MethodPost, "/api/files/move",
		strings.NewReader(`{"from":"src.txt","to":"dest/src.txt"}`),
		map[string]string{"Content-Type": "application/json"})
	status := resp.StatusCode
	payload := readBody(resp)
	resp.Body.Close()

	if status != http.StatusInternalServerError {
		t.Fatalf("a move whose bytes could not follow must fail loudly; got %d %s", status, payload)
	}

	get := h.do(http.MethodGet, "/api/files/download?path=src.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "payload" {
		t.Fatalf("the file must still be readable at its original path, got %q", body)
	}
}
