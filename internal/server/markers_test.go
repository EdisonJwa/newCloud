package server_test

// Provenance marks on file rows.
//
// The listing reports, per file, how many active links point at it and whether earlier
// versions exist. This is the audit capability made visible where people actually look, so
// it has to be right: a stale or wrong count would be worse than no mark at all, because it
// would be believed.
//
// The batch call behind it exists to avoid two queries per row. These tests pin the
// semantics — revoked shares do not count, unrequested nodes do not appear — and that the
// listing surfaces them.

import (
	"encoding/json"
	"net/http"
	"testing"
)

type listedFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	ID          string `json:"id"`
	ShareCount  int    `json:"share_count"`
	HasVersions bool   `json:"has_versions"`
}

func listRoot(t *testing.T, h *harness) []listedFile {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", resp.StatusCode, readBody(resp))
	}
	var payload struct {
		Children []listedFile `json:"children"`
	}
	if err := json.Unmarshal([]byte(readBody(resp)), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Children
}

func findFile(files []listedFile, name string) (listedFile, bool) {
	for _, f := range files {
		if f.Name == name {
			return f, true
		}
	}
	return listedFile{}, false
}

// TestListingMarksSharedFiles: a file with one link reports one.
func TestListingMarksSharedFiles(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "shared.txt", "text/plain", "hello")

	created := h.do(http.MethodPost, "/api/shares/", jsonBody(t, map[string]any{
		"path": "shared.txt", "kind": "link",
	}), map[string]string{"Content-Type": "application/json"})
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated && created.StatusCode != http.StatusOK {
		t.Fatalf("create share: %d %s", created.StatusCode, readBody(created))
	}

	file, ok := findFile(listRoot(t, h), "shared.txt")
	if !ok {
		t.Fatal("shared.txt missing from the listing")
	}
	if file.ShareCount != 1 {
		t.Fatalf("want share_count 1, got %d", file.ShareCount)
	}
}

// TestListingOmitsMarksForUnsharedFiles: the common case must stay unmarked.
func TestListingOmitsMarksForUnsharedFiles(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "private.txt", "text/plain", "hello")

	file, ok := findFile(listRoot(t, h), "private.txt")
	if !ok {
		t.Fatal("private.txt missing from the listing")
	}
	if file.ShareCount != 0 || file.HasVersions {
		t.Fatalf("an untouched file must carry no marks, got shares=%d versions=%v",
			file.ShareCount, file.HasVersions)
	}
}

// TestListingCountsOnlyActiveShares: a revoked link must stop counting.
//
// This is the case that makes the mark trustworthy. If revocation were not reflected, the
// row would keep claiming the file is shared after it had been unshared.
func TestListingCountsOnlyActiveShares(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "revoked.txt", "text/plain", "hello")

	created := h.do(http.MethodPost, "/api/shares/", jsonBody(t, map[string]any{
		"path": "revoked.txt", "kind": "link",
	}), map[string]string{"Content-Type": "application/json"})
	var payload struct {
		Share struct {
			ID string `json:"id"`
		} `json:"share"`
	}
	json.Unmarshal([]byte(readBody(created)), &payload)
	created.Body.Close()

	if payload.Share.ID == "" {
		t.Fatal("could not read the created share id")
	}

	before, _ := findFile(listRoot(t, h), "revoked.txt")
	if before.ShareCount != 1 {
		t.Fatalf("want 1 before revoking, got %d", before.ShareCount)
	}

	revoked := h.do(http.MethodDelete, "/api/shares/"+payload.Share.ID, nil, nil)
	revoked.Body.Close()

	after, _ := findFile(listRoot(t, h), "revoked.txt")
	if after.ShareCount != 0 {
		t.Fatalf("a revoked share must stop counting, got %d", after.ShareCount)
	}
}

// TestListingMarksVersionedFiles: overwriting a file leaves history, and the row says so.
func TestListingMarksVersionedFiles(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "doc.txt", "text/plain", "first")

	// A second upload to the same path keeps the previous content as a version.
	uploadWithType(t, h, "doc.txt", "text/plain", "second content")

	file, ok := findFile(listRoot(t, h), "doc.txt")
	if !ok {
		t.Fatal("doc.txt missing from the listing")
	}
	if !file.HasVersions {
		t.Fatal("an overwritten file should report has_versions")
	}
}
