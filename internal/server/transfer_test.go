package server_test

// Copy, move-in-bulk and ZIP over the JSON API.
//
// The gap these cover is a product gap rather than a protocol one: COPY and MOVE existed as WebDAV
// methods, so a test suite could show that the server "supports copy" while the file manager offered
// no way to do it. These tests ask the JSON API the questions the browser asks, which is the surface
// that was missing.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
)

// batchBody is the exact payload shape the file manager sends.
type batchBody struct {
	Op        string `json:"op"`
	Overwrite bool   `json:"overwrite"`
	Items     []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"items"`
}

// postBatch sends one batch request and returns the status and decoded body.
func postBatch(t *testing.T, h *harness, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/files/batch", jsonBody(t, body),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()

	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// downloadText reads a file through the API and returns its bytes as a string.
func downloadText(t *testing.T, h *harness, path string) (int, string) {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/download?path="+url.QueryEscape(path), nil, nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestBatchCopiesAFileInPlace is the simplest shape the review asked for: copy, and both exist.
func TestBatchCopiesAFileInPlace(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "source.txt", "the original bytes")

	status, out := postBatch(t, h, map[string]any{
		"op":    "copy",
		"items": []map[string]string{{"from": "source.txt", "to": "copy.txt"}},
	})
	if status != http.StatusOK {
		t.Fatalf("copy: want 200, got %d (%v)", status, out)
	}

	// Both files exist, with the same bytes: a copy that destroyed its source, or produced an empty
	// file, would still answer 200.
	if code, body := downloadText(t, h, "source.txt"); code != http.StatusOK || body != "the original bytes" {
		t.Fatalf("the source after a copy: %d %q", code, body)
	}
	if code, body := downloadText(t, h, "copy.txt"); code != http.StatusOK || body != "the original bytes" {
		t.Fatalf("the copy: %d %q", code, body)
	}
}

// TestBatchCopiesADirectoryRecursively is the outcome the DAV handler once got wrong: the destination
// directory was created and nothing was written into it, which reports success while losing the data
// the person asked to copy.
func TestBatchCopiesADirectoryRecursively(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "album")
	h.mkdir(t, "album/day-one")

	for _, f := range []struct{ dir, name, body string }{
		{"album", "cover.txt", "cover bytes"},
		{"album/day-one", "shot.txt", "shot bytes"},
	} {
		mb := multipartBody(t, f.dir, f.name, f.body)
		if rec := h.do(http.MethodPost, "/api/files/upload", mb,
			map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s/%s: %d %s", f.dir, f.name, rec.StatusCode, readBody(rec))
		}
	}

	status, out := postBatch(t, h, map[string]any{
		"op":    "copy",
		"items": []map[string]string{{"from": "album", "to": "album-copy"}},
	})
	if status != http.StatusOK {
		t.Fatalf("copy a directory: want 200, got %d (%v)", status, out)
	}

	// The nested file is the assertion that matters: a copy which only recreates the top directory
	// satisfies every listing check for the root.
	for _, want := range []struct{ path, body string }{
		{"album-copy/cover.txt", "cover bytes"},
		{"album-copy/day-one/shot.txt", "shot bytes"},
	} {
		if code, body := downloadText(t, h, want.path); code != http.StatusOK || body != want.body {
			t.Fatalf("copied %s: %d %q, want %q", want.path, code, body, want.body)
		}
	}
}

// TestBatchMovesAFile checks the operation that used to be rename-only.
func TestBatchMovesAFile(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "inbox")
	h.uploadFile(t, "note.txt", "movable")

	status, out := postBatch(t, h, map[string]any{
		"op":    "move",
		"items": []map[string]string{{"from": "note.txt", "to": "inbox/note.txt"}},
	})
	if status != http.StatusOK {
		t.Fatalf("move: want 200, got %d (%v)", status, out)
	}

	// Gone from the source: a move that copied instead of moving doubles the space used and leaves
	// the original where the person no longer expects it.
	if code, _ := downloadText(t, h, "note.txt"); code != http.StatusNotFound {
		t.Fatalf("the source still exists after a move: %d", code)
	}
	if code, body := downloadText(t, h, "inbox/note.txt"); code != http.StatusOK || body != "movable" {
		t.Fatalf("the destination after a move: %d %q", code, body)
	}
}

// TestBatchReportsEachItemsOutcome is the reason the endpoint answers a list.
//
// A multi-file operation is routinely partial: three of five move, and the two that do not fail for
// different reasons. One status code cannot say that, and guessing on the client's behalf means
// telling somebody a file moved when it did not.
func TestBatchReportsEachItemsOutcome(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "one.txt", "one")
	h.uploadFile(t, "two.txt", "two")
	h.uploadFile(t, "three.txt", "three")
	// Occupies the destination of the second item, so exactly that one must fail.
	h.uploadFile(t, "taken.txt", "already here")

	status, out := postBatch(t, h, map[string]any{
		"op": "copy",
		"items": []map[string]string{
			{"from": "one.txt", "to": "one-copy.txt"},
			{"from": "two.txt", "to": "taken.txt"},
			{"from": "three.txt", "to": "three-copy.txt"},
			{"from": "missing.txt", "to": "whatever.txt"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("a partially failing batch must still answer 200: got %d (%v)", status, out)
	}

	results, _ := out["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("want one result per item, got %d: %v", len(results), out)
	}

	want := []struct {
		ok   bool
		code string
	}{
		{true, ""},
		// The name exists and overwrite was not granted: the client needs this code to ask
		// "replace it?" rather than treating it as a hard failure.
		{false, "target_exists"},
		{true, ""},
		{false, "not_found"},
	}
	for i, w := range want {
		entry, _ := results[i].(map[string]any)
		if got, _ := entry["ok"].(bool); got != w.ok {
			t.Fatalf("item %d ok = %v, want %v (%v)", i, entry["ok"], w.ok, entry)
		}
		if code, _ := entry["code"].(string); code != w.code {
			t.Fatalf("item %d code = %q, want %q (%v)", i, code, w.code, entry)
		}
	}

	// The successful items really happened, and the failed one really did not.
	if code, _ := downloadText(t, h, "one-copy.txt"); code != http.StatusOK {
		t.Fatalf("the first item was reported ok but is not there: %d", code)
	}
	if _, body := downloadText(t, h, "taken.txt"); body != "already here" {
		t.Fatalf("a refused copy replaced the destination anyway: %q", body)
	}
}

// TestBatchOverwriteReplacesTheDestination is the "replace it?" half of the conflict protocol.
func TestBatchOverwriteReplacesTheDestination(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "incoming.txt", "new bytes")
	h.uploadFile(t, "target.txt", "old bytes")

	// Without overwrite the batch refuses, which is what makes the question askable.
	status, out := postBatch(t, h, map[string]any{
		"op":    "move",
		"items": []map[string]string{{"from": "incoming.txt", "to": "target.txt"}},
	})
	if status != http.StatusConflict {
		t.Fatalf("a single refused item should answer 409 like the dedicated route: got %d (%v)", status, out)
	}

	// With it, the destination is replaced by the source.
	status, out = postBatch(t, h, map[string]any{
		"op":        "move",
		"overwrite": true,
		"items":     []map[string]string{{"from": "incoming.txt", "to": "target.txt"}},
	})
	if status != http.StatusOK {
		t.Fatalf("overwrite move: want 200, got %d (%v)", status, out)
	}
	if code, body := downloadText(t, h, "target.txt"); code != http.StatusOK || body != "new bytes" {
		t.Fatalf("the replaced destination: %d %q", code, body)
	}
}

// TestBatchRefusesAFolderIntoItself guards the recursion the copy walk cannot survive.
func TestBatchRefusesAFolderIntoItself(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "box")
	mb := multipartBody(t, "box", "inside.txt", "x")
	if rec := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
		t.Fatalf("seed: %d %s", rec.StatusCode, readBody(rec))
	}

	for _, op := range []string{"copy", "move"} {
		t.Run(op, func(t *testing.T) {
			status, out := postBatch(t, h, map[string]any{
				"op":    op,
				"items": []map[string]string{{"from": "box", "to": "box/nested"}},
			})
			if status == http.StatusOK {
				if failed, _ := out["failed"].(float64); failed == 0 {
					t.Fatalf("%s into itself was accepted: %v", op, out)
				}
			}
			// Either way the tree must be intact, not half-copied.
			if code, body := downloadText(t, h, "box/inside.txt"); code != http.StatusOK || body != "x" {
				t.Fatalf("%s into itself damaged the source: %d %q", op, code, body)
			}
		})
	}
}

// TestZipStreamsTheSelection covers the archive itself.
func TestZipStreamsTheSelection(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "pack")
	for _, f := range []struct{ name, body string }{
		{"a.txt", "alpha"},
		{"b.txt", "beta"},
	} {
		mb := multipartBody(t, "pack", f.name, f.body)
		if rec := h.do(http.MethodPost, "/api/files/upload", mb,
			map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s: %d %s", f.name, rec.StatusCode, readBody(rec))
		}
	}

	resp := h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape("pack"), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip: want 200, got %d: %s", resp.StatusCode, readBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", ct)
	}
	// A ZIP offered inline would be rendered as a document by a browser that guessed at the bytes.
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "pack.zip") {
		t.Fatalf("Content-Disposition = %q, want an attachment named pack.zip", cd)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("the response is not a valid ZIP: %v", err)
	}

	got := map[string]string{}
	for _, f := range zr.File {
		rc, oerr := f.Open()
		if oerr != nil {
			t.Fatal(oerr)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(body)
	}

	// The directory entry is present so an empty folder survives extraction, and the two files
	// carry their bytes under a path that includes the folder they came from.
	if _, ok := got["pack/"]; !ok {
		t.Fatalf("no directory entry for the folder: %v", keysOf(got))
	}
	if got["pack/a.txt"] != "alpha" || got["pack/b.txt"] != "beta" {
		t.Fatalf("archive contents = %v", got)
	}
}

// TestZipRequiresASession keeps the new read route behind the same gate as the others.
//
// A route that serves many files at once is exactly the kind of convenience that ends up outside
// the authenticated group by accident, and "it only streams what the caller could already fetch
// one at a time" would be true right up until it was not.
func TestZipRequiresASession(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "private.txt", "secret")

	req, err := http.NewRequest(http.MethodGet,
		h.srv.URL+"/api/files/zip?path="+url.QueryEscape("private.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous zip request answered %d, want 401", resp.StatusCode)
	}
}

// TestZipRefusesAForeignNode is the authorization half: the archive must not be a way to read
// something the caller cannot download individually.
func TestZipRefusesAForeignNode(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "mine.txt", "mine")

	resp := h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape("nobody.txt"), nil, nil)
	body := readBody(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("zipping an absent path answered %d (%s), want 404", resp.StatusCode, body)
	}
	if strings.Contains(body, "mine") {
		t.Fatalf("the refusal leaked content: %s", body)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBatchAcceptsThePathsTheClientRenders pins the path form the file manager sends.
//
// The panel hands back the `path` field it was given, and the API renders that field with a leading
// slash. The blob layout resolves such a path either way, so without a deliberate check the
// normalisation in cleanRel could be removed and nothing would notice — until a deployment on the
// tree layout, where an absolute path is refused outright (layout.ErrUnsafePath).
func TestBatchAcceptsThePathsTheClientRenders(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "docs")
	mb := multipartBody(t, "docs", "a.txt", "alpha")
	if rec := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
		t.Fatalf("seed: %d %s", rec.StatusCode, readBody(rec))
	}

	status, out := postBatch(t, h, map[string]any{
		"op":    "copy",
		"items": []map[string]string{{"from": "/docs/a.txt", "to": "/docs/b.txt"}},
	})
	if status != http.StatusOK {
		t.Fatalf("copy with leading-slash paths: want 200, got %d (%v)", status, out)
	}
	if code, body := downloadText(t, h, "/docs/b.txt"); code != http.StatusOK || body != "alpha" {
		t.Fatalf("the copy at %q: %d %q", "/docs/b.txt", code, body)
	}

	// The same form through the archive route, whose members also reach the content store by path.
	resp := h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape("/docs"), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip with a leading-slash path: %d %s", resp.StatusCode, readBody(resp))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a ZIP: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 3 { // the folder, a.txt and b.txt
		t.Fatalf("archive members = %v, want the folder and its two files", names)
	}
}

/* ---------- both storage layouts ---------- */

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
			// and — worse — would hide the refusal that says so.
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

// TestOverwritingANonEmptyDirectoryWorksOnBothLayouts is P0-01 from the 2026-10-23 review.
//
// On the tree layout the batch COPY of a folder over an existing folder answered 500
// content_copy_failed and the destination then read 404, because the metadata switch happened before
// the physical moves: the old tree still occupied the destination path, so the install could not
// happen — and the metadata had already been changed to name a node that was never installed.
//
// The failure is layout-specific and was invisible to a blob-only suite. Both layouts run here.
func TestOverwritingANonEmptyDirectoryWorksOnBothLayouts(t *testing.T) {
	for _, layout := range []string{"blob", "tree"} {
		t.Run(layout, func(t *testing.T) {
			h := harnessOn(t, layout, nil)
			seedDir(t, h, "src", struct{ name, body string }{"new.txt", "new bytes"})
			seedDir(t, h, "dst", struct{ name, body string }{"old.txt", "old bytes"})

			status, out := postBatch(t, h, map[string]any{
				"op":        "copy",
				"overwrite": true,
				"items":     []map[string]string{{"from": "src", "to": "dst"}},
			})
			if status != http.StatusOK {
				t.Fatalf("copy over a non-empty directory: want 200, got %d (%v)", status, out)
			}

			// The replacement is complete and readable at its own path.
			if code, body := downloadText(t, h, "dst/new.txt"); code != http.StatusOK || body != "new bytes" {
				t.Fatalf("the replacement: %d %q, want \"new bytes\"", code, body)
			}
			// And the previous contents are gone from that path — the copy is a replacement, not a merge.
			if code, _ := downloadText(t, h, "dst/old.txt"); code != http.StatusNotFound {
				t.Fatalf("the replaced folder still serves its old contents: %d", code)
			}

			// The displaced original is in the trash under the reserved backup name, which is what makes
			// the replacement recoverable by hand.

			resp := h.do(http.MethodGet, "/api/files/trash", nil, nil)
			listing := decode(t, resp)
			items, _ := listing["items"].([]any)
			var replaced bool
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if name, _ := item["name"].(string); strings.Contains(name, ".newcloud-replaced-") {
					replaced = true
				}
			}
			if !replaced {
				t.Fatalf("the replaced folder is not in the trash; a replacement that cannot be undone "+
					"is a deletion: %v", items)
			}
		})
	}
}

// TestMovingOverANonEmptyDirectoryWorksOnBothLayouts is the MOVE half of P0-01.
func TestMovingOverANonEmptyDirectoryWorksOnBothLayouts(t *testing.T) {
	for _, layout := range []string{"blob", "tree"} {
		t.Run(layout, func(t *testing.T) {
			h := harnessOn(t, layout, nil)
			seedDir(t, h, "incoming", struct{ name, body string }{"moved.txt", "moved bytes"})
			seedDir(t, h, "target", struct{ name, body string }{"old.txt", "old bytes"})

			status, out := postBatch(t, h, map[string]any{
				"op":        "move",
				"overwrite": true,
				"items":     []map[string]string{{"from": "incoming", "to": "target"}},
			})
			if status != http.StatusOK {
				t.Fatalf("move over a non-empty directory: want 200, got %d (%v)", status, out)
			}
			if code, body := downloadText(t, h, "target/moved.txt"); code != http.StatusOK || body != "moved bytes" {
				t.Fatalf("the moved folder: %d %q", code, body)
			}
			if code, _ := downloadText(t, h, "incoming"); code != http.StatusNotFound {
				t.Fatalf("the source still exists after a move: %d", code)
			}
		})
	}
}

// TestCopyIsNotTruncatedByTheListingPageSize is P0-02.
//
// The recursive copy walked each directory with Tx.Children, which truncates at
// limits.max_profind_children and says nothing about it. With the limit set to 1, copying a folder
// of two files answered {"complete":true} and copied one of them: a data operation reporting success
// for a result that lost a file. The requirement is that traversal either covers the tree or fails.
func TestCopyIsNotTruncatedByTheListingPageSize(t *testing.T) {
	for _, layout := range []string{"blob", "tree"} {
		t.Run(layout, func(t *testing.T) {
			// The page size the LISTING endpoint uses, deliberately smaller than the folder.
			h := harnessOn(t, layout, func(c *config.Config) { c.Limits.MaxProfindChildren = 1 })
			seedDir(t, h, "pack",
				struct{ name, body string }{"a.txt", "AAA"},
				struct{ name, body string }{"b.txt", "BBB"},
			)

			status, out := postBatch(t, h, map[string]any{
				"op":    "copy",
				"items": []map[string]string{{"from": "pack", "to": "duplicate"}},
			})

			// Either the copy is refused outright, or it is complete. Both are acceptable answers;
			// "successful with a file missing" is not.
			if status != http.StatusOK {
				return
			}
			if complete, _ := out["complete"].(bool); !complete {
				return
			}
			for _, f := range []struct{ name, body string }{
				{"duplicate/a.txt", "AAA"},
				{"duplicate/b.txt", "BBB"},
			} {
				code, body := downloadText(t, h, f.name)
				if code != http.StatusOK || body != f.body {
					t.Fatalf("the copy reported complete but %s is %d %q. A transfer that silently "+
						"omits files is worse than one that fails: the client believes it has a copy.",
						f.name, code, body)
				}
			}
		})
	}
}

// TestZipIsNotTruncatedByTheListingPageSize is P0-03.
//
// Same traversal bug with a worse symptom: the archive streamed successfully, was a structurally
// valid ZIP, and was missing a member. No extraction tool reports that as an error.
func TestZipIsNotTruncatedByTheListingPageSize(t *testing.T) {
	for _, layout := range []string{"blob", "tree"} {
		t.Run(layout, func(t *testing.T) {
			h := harnessOn(t, layout, func(c *config.Config) { c.Limits.MaxProfindChildren = 1 })
			seedDir(t, h, "pack",
				struct{ name, body string }{"a.txt", "AAA"},
				struct{ name, body string }{"b.txt", "BBB"},
			)

			resp := h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape("pack"), nil, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return // an explicit refusal is an acceptable answer
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatalf("the response is not a valid ZIP: %v", err)
			}
			got := map[string]bool{}
			for _, f := range zr.File {
				got[f.Name] = true
			}
			for _, want := range []string{"pack/a.txt", "pack/b.txt"} {
				if !got[want] {
					t.Fatalf("the archive is missing %q (entries: %v). A truncated ZIP is still a "+
						"valid ZIP, so nothing downstream will notice the loss.", want, got)
				}
			}
		})
	}
}

// TestAFolderTooLargeToProveCompleteIsRefused is the other half of the traversal fix.
//
// "Copy the whole tree or fail" has two branches, and only the successful one is reachable from
// outside at realistic sizes. The budget is lowered here so the refusal itself is exercised: a
// folder bigger than the completeness budget must produce an explicit error, not a partial copy that
// claims to be complete.
func TestAFolderTooLargeToProveCompleteIsRefused(t *testing.T) {
	h := newHarness(t)
	seedDir(t, h, "pack",
		struct{ name, body string }{"a.txt", "AAA"},
		struct{ name, body string }{"b.txt", "BBB"},
		struct{ name, body string }{"c.txt", "CCC"},
	)

	restore := h.srvServer.SetTraversalLimitForTest(2)
	defer restore()

	// A copy: one item, so the batch answers the item's own status.
	status, out := postBatch(t, h, map[string]any{
		"op":    "copy",
		"items": []map[string]string{{"from": "pack", "to": "duplicate"}},
	})
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("copying a folder larger than the completeness budget: want 413, got %d (%v)",
			status, out)
	}
	// Nothing half-copied is visible: the refusal happens before the destination exists.
	if code, _ := downloadText(t, h, "duplicate/a.txt"); code != http.StatusNotFound {
		t.Fatalf("a refused copy left a partial destination behind: %d", code)
	}

	// An archive: refused before the first byte, so the client gets an error rather than a valid ZIP
	// with files missing.
	resp := h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape("pack"), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("archiving a folder larger than the completeness budget: want 413, got %d",
			resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "zip") {
		t.Fatalf("the refusal started an archive (Content-Type %q); a truncated ZIP is a valid ZIP "+
			"and no client would notice the loss", ct)
	}
}
