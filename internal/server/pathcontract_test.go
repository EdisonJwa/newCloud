package server_test

// The path the listing hands out must be the path every other endpoint accepts.
//
// The file manager renders a node's `path` with a leading slash ("/docs/report.txt") and sends that
// value straight back — to download, inline preview, thumbnail, upload target, rename, archive and
// the copy/move batch. The blob layout resolves such a path either way because paths are metadata
// there; the TREE layout keeps bytes at the path and refuses an absolute one outright
// (layout.ErrUnsafePath). So the product listed a file and its own download endpoint answered 400
// invalid_name, for every normal download, preview and thumbnail on that layout (2026-10-23 review).
//
// This test does not construct the paths it requests: it asks the LISTING for them and passes back
// exactly what it got, which is the only way to catch a contract mismatch rather than a spelling
// mistake in the test. Both layouts run, because the layouts are where the contract differed.
//
// The names are deliberately awkward: a space, a non-ASCII folder and file name, and two levels of
// nesting. A fix that strips the first byte of the string, or that re-encodes the path, breaks on one
// of these.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// decodeJSONBody decodes a JSON response into v, closing the body.
func decodeJSONBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestTheListingPathRoundTripsThroughEveryContentRoute is the contract, on both layouts.
func TestTheListingPathRoundTripsThroughEveryContentRoute(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)

			const outer, inner = "Fotos 2026", "Fotos 2026/오늘"
			h.mkdir(t, outer)
			h.mkdir(t, inner)

			png := samplePNG(t)
			for _, f := range []struct{ name, body string }{
				{"사진.png", png},
				{"메모 최종.txt", "메모 내용"},
			} {
				mb := multipartBody(t, inner, f.name, f.body)
				if rec := h.do(http.MethodPost, "/api/files/upload", mb,
					map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
					t.Fatalf("seed %s: %d %s", f.name, rec.StatusCode, readBody(rec))
				}
			}

			// The paths as the API returns them. The premise of the test is that they are absolute;
			// if the API ever stops rendering them that way, this fails loudly instead of quietly
			// testing nothing.
			outerPath := pathField(t, childrenOf(t, h, "/")[outer], outer)
			if !strings.HasPrefix(outerPath, "/") {
				t.Fatalf("the listing rendered %q without a leading slash; this test's premise is "+
					"that the file manager sends an absolute-looking path back", outerPath)
			}
			innerPath := pathField(t, childrenOf(t, h, outerPath)["오늘"], "오늘")
			files := childrenOf(t, h, innerPath)
			photoPath := pathField(t, files["사진.png"], "사진.png")
			notePath := pathField(t, files["메모 최종.txt"], "메모 최종.txt")

			// 1. Download, both files, through the path the listing returned.
			if code, body := downloadText(t, h, photoPath); code != http.StatusOK || body != png {
				t.Fatalf("download %q: %d, %d bytes; the listing's own path must be downloadable",
					photoPath, code, len(body))
			}
			if code, body := downloadText(t, h, notePath); code != http.StatusOK || body != "메모 내용" {
				t.Fatalf("download %q: %d %q", notePath, code, body)
			}

			// 2. Inline preview: the same handler, and the one the text editor and media player use.
			inline := h.do(http.MethodGet,
				"/api/files/download?inline=1&path="+url.QueryEscape(photoPath), nil, nil)
			defer inline.Body.Close()
			if inline.StatusCode != http.StatusOK {
				t.Fatalf("inline preview %q: %d %s", photoPath, inline.StatusCode, readBody(inline))
			}
			if ct := inline.Header.Get("Content-Type"); ct != "image/png" {
				t.Fatalf("inline preview %q declared %q", photoPath, ct)
			}
			if cd := inline.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
				t.Fatalf("inline preview %q was not inline (%q)", photoPath, cd)
			}

			// 3. Thumbnail: the grid's preview, which the review reproduced failing the same way.
			thumb := h.do(http.MethodGet,
				"/api/files/thumbnail?path="+url.QueryEscape(photoPath), nil, nil)
			defer thumb.Body.Close()
			if thumb.StatusCode != http.StatusOK {
				t.Fatalf("thumbnail %q: %d %s", photoPath, thumb.StatusCode, readBody(thumb))
			}
			if ct := thumb.Header.Get("Content-Type"); ct != "image/png" {
				t.Fatalf("thumbnail %q declared %q", photoPath, ct)
			}

			// 4. Version history, addressed by the same path form.
			//
			// A second upload of the same name keeps the first bytes as a version. Whether that
			// version can be RESTORED is a layout question, not a path question: the blob layout
			// keeps the bytes at a content address, while the tree layout overwrote them at the path
			// and therefore refuses the restore instead of reporting a change it cannot make. Both
			// answers are checked, so the refusal cannot quietly turn into a success report.
			overwrite := multipartBody(t, inner, "메모 최종.txt", "새 내용")
			if rec := h.do(http.MethodPost, "/api/files/upload", overwrite,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + overwrite.Boundary()}); rec.StatusCode != http.StatusCreated {
				t.Fatalf("re-upload %q: %d %s", notePath, rec.StatusCode, readBody(rec))
			}

			versions := h.do(http.MethodGet,
				"/api/files/versions?path="+url.QueryEscape(notePath), nil, nil)
			var history struct {
				Versions []struct {
					Version int `json:"version"`
				} `json:"versions"`
			}
			decodeJSONBody(t, versions, &history)
			if len(history.Versions) == 0 {
				t.Fatalf("no version was kept after replacing %q", notePath)
			}

			restore := h.do(http.MethodPost, "/api/files/restore-version",
				jsonBody(t, map[string]any{"path": notePath, "version": history.Versions[0].Version}),
				map[string]string{"Content-Type": "application/json"})
			restore.Body.Close()

			if layoutName == "blob" {
				if restore.StatusCode != http.StatusOK {
					t.Fatalf("restore-version %d of %q: %d", history.Versions[0].Version, notePath, restore.StatusCode)
				}
				if code, body := downloadText(t, h, notePath); code != http.StatusOK || body != "메모 내용" {
					t.Fatalf("the restored version at %q: %d %q", notePath, code, body)
				}
			} else if restore.StatusCode != http.StatusConflict {
				t.Fatalf("restore-version on the tree layout answered %d; it must refuse (%d) rather "+
					"than flip the node's metadata over bytes it could not restore", restore.StatusCode,
					http.StatusConflict)
			}

			// What the note holds from here on: the restored version where that is possible, and the
			// bytes the re-upload wrote where the layout refused the restore.
			wantNote := "메모 내용"
			if layoutName != "blob" {
				wantNote = "새 내용"
			}

			// 5. Upload INTO the folder, naming it with the path the listing returned.
			mb := multipartBody(t, outerPath, "추가.txt", "added bytes")
			created := h.do(http.MethodPost, "/api/files/upload", mb,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
			var added struct {
				Path string `json:"path"`
			}
			decodeJSONBody(t, created, &added)
			if created.StatusCode != http.StatusCreated {
				t.Fatalf("upload into %q: %d", outerPath, created.StatusCode)
			}
			if code, body := downloadText(t, h, added.Path); code != http.StatusOK || body != "added bytes" {
				t.Fatalf("the file uploaded into %q: %d %q", outerPath, code, body)
			}

			// 6. A resumable upload into the same folder: the session stores the directory, and a
			// canonicalisation that happened only in the single-request upload would miss this one.
			session := startUpload(t, h, outerPath, "chunked.txt", 5)
			chunk, payload := sendChunk(t, h, session.ID, 0, 4, 5, "abcde")
			chunk.Body.Close()
			if chunk.StatusCode != http.StatusOK {
				t.Fatalf("chunk into %q: %d %v", outerPath, chunk.StatusCode, payload)
			}
			done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
			var chunked struct {
				Path string `json:"path"`
			}
			decodeJSONBody(t, done, &chunked)
			if done.StatusCode != http.StatusCreated {
				t.Fatalf("complete into %q: %d", outerPath, done.StatusCode)
			}
			if code, body := downloadText(t, h, chunked.Path); code != http.StatusOK || body != "abcde" {
				t.Fatalf("the resumable upload into %q: %d %q", outerPath, code, body)
			}

			// 7. Mkdir and rename, both taking the listing's path form.
			newDirPath := outerPath + "/새 폴더"
			mkd := h.do(http.MethodPost, "/api/files/mkdir",
				jsonBody(t, map[string]string{"path": newDirPath}),
				map[string]string{"Content-Type": "application/json"})
			mkd.Body.Close()
			if mkd.StatusCode != http.StatusCreated {
				t.Fatalf("mkdir %q: %d", newDirPath, mkd.StatusCode)
			}

			ren := h.do(http.MethodPost, "/api/files/rename",
				jsonBody(t, map[string]string{"path": notePath, "name": "메모.txt"}),
				map[string]string{"Content-Type": "application/json"})
			var renamed struct {
				Path string `json:"path"`
			}
			decodeJSONBody(t, ren, &renamed)
			if ren.StatusCode != http.StatusOK {
				t.Fatalf("rename %q: %d", notePath, ren.StatusCode)
			}
			if code, body := downloadText(t, h, renamed.Path); code != http.StatusOK || body != wantNote {
				t.Fatalf("the renamed file at %q: %d %q", renamed.Path, code, body)
			}

			// 8. Copy and move, using the absolute forms the panel sends.
			status, out := postBatch(t, h, map[string]any{
				"op":    "copy",
				"items": []map[string]string{{"from": photoPath, "to": innerPath + "/사본.png"}},
			})
			if status != http.StatusOK {
				t.Fatalf("copy using listing paths: %d (%v)", status, out)
			}
			if code, body := downloadText(t, h, innerPath+"/사본.png"); code != http.StatusOK || body != png {
				t.Fatalf("the copy made from %q: %d", photoPath, code)
			}

			// 9. Archive of the folder, by the path the listing returned.
			zipResp := h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape(innerPath), nil, nil)
			raw, _ := io.ReadAll(zipResp.Body)
			zipResp.Body.Close()
			if zipResp.StatusCode != http.StatusOK {
				t.Fatalf("archive %q: %d %s", innerPath, zipResp.StatusCode, string(raw))
			}
			zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatalf("the archive of %q is not a valid ZIP: %v", innerPath, err)
			}
			names := map[string]bool{}
			for _, f := range zr.File {
				names[f.Name] = true
			}
			if !names["오늘/사진.png"] {
				t.Fatalf("the archive of %q is missing its contents: %v", innerPath, names)
			}
		})
	}
}
