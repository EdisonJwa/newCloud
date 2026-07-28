package server_test

// Recent files feed the home screen.
//
// Two things have to be right for the home screen to be trustworthy: the order (newest first,
// across the whole tree, not just the root) and the path (so clicking an entry lands in the
// right column). Both are easy to get subtly wrong, and both are invisible until a user
// clicks something and ends up in the wrong folder.

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"testing"
)

type recentItem struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	ModifiedAt string `json:"modified_at"`
	Preview    string `json:"preview"`
}

func getRecent(t *testing.T, h *harness) []recentItem {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/recent?limit=10", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recent: %d %s", resp.StatusCode, readBody(resp))
	}
	var payload struct {
		Items []recentItem `json:"items"`
	}
	body := readBody(resp)
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return payload.Items
}

// uploadPath uploads a file to a slash-separated path, creating nothing implicitly: the
// parent must already exist, exactly as the API requires.
func uploadPath(t *testing.T, h *harness, fullPath, content string) {
	t.Helper()
	dir, name := path.Split(fullPath)
	dir = strings.TrimSuffix(dir, "/")

	mb := multipartBody(t, dir, name, content)
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", fullPath, resp.StatusCode, readBody(resp))
	}
}

func mkdirPath(t *testing.T, h *harness, p string) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/files/mkdir", jsonBody(t, map[string]any{"path": p}),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir %s: %d %s", p, resp.StatusCode, readBody(resp))
	}
}

// TestRecentIsEmptyForANewAccount: no files, no items, and no error.
func TestRecentIsEmptyForANewAccount(t *testing.T) {
	h := newHarness(t)
	if items := getRecent(t, h); len(items) != 0 {
		t.Fatalf("want no recents, got %d", len(items))
	}
}

// TestRecentFindsFilesBelowTheRoot is the part a naive implementation gets wrong: the newest
// file may be several levels deep, and the home screen still has to find it.
func TestRecentFindsFilesBelowTheRoot(t *testing.T) {
	h := newHarness(t)

	mkdirPath(t, h, "docs")
	mkdirPath(t, h, "docs/2026")
	uploadPath(t, h, "docs/2026/buried.txt", "deep")

	items := getRecent(t, h)
	if len(items) != 1 {
		t.Fatalf("want 1 recent, got %d", len(items))
	}
	if items[0].Name != "buried.txt" {
		t.Fatalf("want buried.txt, got %q", items[0].Name)
	}
	if items[0].Path != "/docs/2026/buried.txt" {
		t.Fatalf("path must locate the file for the column view, got %q", items[0].Path)
	}
}

// TestRecentOrdersNewestFirst: the home screen is a "pick up where you left off" list, so the
// order is the feature.
func TestRecentOrdersNewestFirst(t *testing.T) {
	h := newHarness(t)
	uploadPath(t, h, "first.txt", "one")
	uploadPath(t, h, "second.txt", "two")
	// Re-touch the first one so it becomes the most recent without relying on name order.
	uploadPath(t, h, "first.txt", "one again")

	items := getRecent(t, h)
	if len(items) != 2 {
		t.Fatalf("want 2 recents, got %d", len(items))
	}
	if items[0].Name != "first.txt" {
		t.Fatalf("the touched file must come first, got %q", items[0].Name)
	}
}

// TestRecentExcludesDirectoriesAndTrash: the list is for files people can open.
func TestRecentExcludesDirectoriesAndTrash(t *testing.T) {
	h := newHarness(t)
	mkdirPath(t, h, "folder")
	uploadPath(t, h, "gone.txt", "bye")

	resp := h.do(http.MethodDelete, "/api/files/?path=gone.txt", nil, nil)
	resp.Body.Close()

	for _, item := range getRecent(t, h) {
		if strings.Contains(item.Path, "folder") {
			t.Error("a directory must not appear in recents")
		}
		if strings.Contains(item.Path, "gone.txt") {
			t.Error("a trashed file must not appear in recents")
		}
	}
}

// TestRecentCarriesPreviewKind: the home screen shows thumbnails, which only work when the
// server says the file is previewable.
func TestRecentCarriesPreviewKind(t *testing.T) {
	h := newHarness(t)
	uploadPath(t, h, "note.txt", "hello")

	items := getRecent(t, h)
	if len(items) != 1 {
		t.Fatalf("want 1 recent, got %d", len(items))
	}
	if items[0].Preview != "text" {
		t.Fatalf("want preview text, got %q", items[0].Preview)
	}
}
