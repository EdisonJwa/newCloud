package server_test

// Shared harness helpers for tests that need a chosen storage layout or a byte-level
// assertion. The batch/ZIP tests that motivated them join this file in the next commit.

import (
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
)

// downloadText reads a file through the API and returns its bytes as a string.
func downloadText(t *testing.T, h *harness, path string) (int, string) {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/download?path="+url.QueryEscape(path), nil, nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// harnessOn builds a harness whose content store is the named layout.
//
// Every batch test until now ran on blob storage, which is exactly why a tree-only failure survived
// a green suite: the layouts disagree about who owns a path. Blob has EfficientRename, so a rename
// is a metadata update and a swap never touches the filesystem; tree does not, so an overwrite has to
// free the destination path before anything can be installed there.
func harnessOn(t *testing.T, layout string, mutate func(*config.Config)) *harness {
	t.Helper()
	return harnessOnTuned(t, layout, mutate, nil)
}

// harnessOnTuned is harnessOn with one more hook over the deps, so a test can wrap the content
// store (fault injection) without rebuilding the tree/blob wiring by hand.
func harnessOnTuned(
	t *testing.T, layoutName string, mutate func(*config.Config), extra func(*server.Deps),
) *harness {
	t.Helper()

	var tune func(*server.Deps)
	var treeRoot string
	if layoutName == "tree" {
		treeRoot = t.TempDir()
		ts, err := tree.New(treeRoot)
		if err != nil {
			t.Fatalf("tree store: %v", err)
		}
		// The store holds an os.Root handle on its directory. Without this the temporary directory
		// cannot be removed and the test fails in cleanup on Windows, which reads as a feature failure.
		t.Cleanup(func() { _ = ts.Close() })
		tune = func(d *server.Deps) {
			d.Content = ts
			// main.go hands the server NO blob store for this layout: the tree layout names content
			// by path, not by hash, so there is nothing to address a version by. A harness that kept
			// the blob store would let a test exercise version history that production cannot serve,
			// and ??worse ??would hide the refusal that says so.
			d.Blobs = nil
			if extra != nil {
				extra(d)
			}
		}
	} else if extra != nil {
		tune = extra
	}
	h := newHarnessTuned(t, mutate, tune)
	// The content store's directory, so a test can check the BYTES rather than only what the metadata
	// lists: an orphan tree left by a failed overwrite holds quota while nothing names it, and no
	// listing can show that.
	h.treeRoot = treeRoot
	return h
}

// seedDir uploads files into a directory, creating it first.
func seedDir(t *testing.T, h *harness, dir string, files ...struct{ name, body string }) {
	t.Helper()
	h.mkdir(t, dir)
	for _, f := range files {
		mb := multipartBody(t, dir, f.name, f.body)
		if rec := h.do(http.MethodPost, "/api/files/upload", mb,
			map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s/%s: %d %s", dir, f.name, rec.StatusCode, readBody(rec))
		}
	}
}
