package server_test

// Historical version content (§8.1 版本下载).
//
// The API could list a version and restore it but not hand back its bytes, so a version history was
// a list of promises: an auditor could see that v3 existed and could not check what it contained.
// Restoring to read is destructive — it overwrites the current file, which is the thing the operator
// was probably trying to avoid.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestVersionContentCanBeDownloaded(t *testing.T) {
	h := newHarness(t)

	h.uploadFile(t, "doc.txt", "first version")
	// A second upload with different bytes keeps the first as a version.
	h.uploadFile(t, "doc.txt", "second version")

	list := h.do(http.MethodGet, "/api/files/versions?path=doc.txt", nil, nil)
	payload := readBody(list)
	list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("versions: %d %s", list.StatusCode, payload)
	}

	resp := h.do(http.MethodGet, "/api/files/version?path=doc.txt&version=1", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("version content: %d %s", resp.StatusCode, body)
	}
	if body != "first version" {
		t.Fatalf("version 1 returned %q. A historical download that returns the CURRENT bytes is "+
			"worse than no download: it is a wrong answer with the right shape.", body)
	}

	// The current file is untouched by reading history.
	current := h.do(http.MethodGet, "/api/files/download?path=doc.txt", nil, nil)
	now := readBody(current)
	current.Body.Close()
	if now != "second version" {
		t.Fatalf("reading a version changed the file: %q", now)
	}

	// The read is audited, because it is access to file content.
	var audited bool
	for _, e := range h.meta.AuditLog() {
		if e.Action == "file.version.read" {
			audited = true
			if e.Metadata["version"] == nil {
				t.Fatal("the audit row does not say which version was read")
			}
		}
	}
	if !audited {
		t.Fatal("no file.version.read row: an audited system that cannot say who read an old " +
			"version cannot reconstruct a leak")
	}
}

func TestVersionContentRefusesWhatItCannotServe(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "doc.txt", "one")
	h.uploadFile(t, "doc.txt", "two")

	for _, tc := range []struct {
		path string
		want int
		why  string
	}{
		{"/api/files/version?path=doc.txt&version=99", http.StatusNotFound, "no such version"},
		{"/api/files/version?path=doc.txt&version=0", http.StatusBadRequest, "not a version"},
		{"/api/files/version?path=doc.txt", http.StatusBadRequest, "missing version"},
		{"/api/files/version?path=nope.txt&version=1", http.StatusNotFound, "no such file"},
	} {
		resp := h.do(http.MethodGet, tc.path, nil, nil)
		code := resp.StatusCode
		resp.Body.Close()
		if code != tc.want {
			t.Fatalf("%s (%s): got %d, want %d", tc.path, tc.why, code, tc.want)
		}
	}

	// Another account must not reach the bytes through the version route.
	other := h.meta.AddUser(store.User{Username: "v-eve", Email: "v-eve@example.test", Role: "user"})
	token, err := h.sessions.Mint(t.Context(), other.ID, other.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	resp := h.adminGet(t, h.cookieClient(token), "/api/files/version?path=doc.txt&version=1")
	code := resp.StatusCode
	resp.Body.Close()
	if code == http.StatusOK {
		t.Fatal("another account read a version's bytes; the version route must ask the same " +
			"question as every other content entry point")
	}
}

// TestSharedFileVersionsNeedTheVersionRight is the permission half, and the reason ?id= exists.
//
// The read-only grant and the version-history grant are separate, and a share that allows reading a
// file does not automatically allow reading what it used to contain. That distinction is only
// reachable for a file the caller does not own — resolving by path can only find their own files,
// where they hold every right — so the ACL case is the one that proves the check runs.
func TestSharedFileVersionsNeedTheVersionRight(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "shared-doc.txt", "old text")
	h.uploadFile(t, "shared-doc.txt", "new text")
	node := h.resolveNode(t, "shared-doc.txt")

	viewer := h.meta.AddUser(store.User{
		Username: "version-viewer", Email: "version-viewer@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(t.Context(), viewer.ID); err != nil {
		t.Fatal(err)
	}
	token, err := h.sessions.Mint(t.Context(), viewer.ID, viewer.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	client := h.cookieClient(token)

	get := func() int {
		t.Helper()
		resp := h.adminGet(t, client,
			"/api/files/version?id="+node.ID.String()+"&version=1")
		code := resp.StatusCode
		resp.Body.Close()
		return code
	}

	// Reading the file is not reading its history.
	h.grantOn(t, node.ID, viewer.ID, store.PermRead, false)
	if code := get(); code == http.StatusOK {
		t.Fatal("a read-only grant exposed an old version of the file. The history of a shared " +
			"document is not part of reading the document.")
	}

	h.grantOn(t, node.ID, viewer.ID, store.PermRead|store.PermReadVersions, false)
	if code := get(); code != http.StatusOK {
		t.Fatalf("a grant that includes version history was refused (%d)", code)
	}

	// And the LIST is reachable too. Reading version 1 is useless if the client cannot learn that
	// version 1 exists: the list is how a caller discovers the numbers this endpoint takes.
	listResp := h.adminGet(t, client, "/api/files/versions?id="+node.ID.String())
	listBody := readBody(listResp)
	code := listResp.StatusCode
	listResp.Body.Close()
	if code != http.StatusOK {
		t.Fatalf("the granted viewer cannot list versions (%d): %s", code, listBody)
	}
	if !strings.Contains(listBody, `"version":1`) {
		t.Fatalf("the version list does not name version 1: %s", listBody)
	}
}
