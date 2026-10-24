package dav

// Nextcloud client path compatibility (ADR-0015).
//
// The official clients do not use /dav/<user>/: they sync against
// /remote.php/dav/files/<user>/ and some older ones against /remote.php/webdav/. The
// handler accepts all three shapes rather than rewriting them, because it builds the
// hrefs it returns from the request path — a rewrite would make every PROPFIND answer
// point at a URL the client never asked about. These tests pin the shapes, the hrefs the
// client will follow, the cross-account refusal, and the X-OC-Mtime contract.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const ncBase = "https://cloud.example.test"

func TestNextcloudFilesPathAnswersWithItsOwnHrefs(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, "MKCOL", ncBase+"/remote.php/dav/files/alice/docs", nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/alice/docs/a.txt",
		strings.NewReader("hello"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}

	rec = e.do(t, "PROPFIND", ncBase+"/remote.php/dav/files/alice/docs/", nil,
		map[string]string{"Depth": "1"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The href must be the path the client used — an absolute path, not a full URL.
	// The Nextcloud clients compare every href with the request URL's path and reject
	// the whole listing when one does not start with it, which a scheme+host prefix
	// does not.
	if !strings.Contains(body, "<D:href>/remote.php/dav/files/alice/docs/</D:href>") {
		t.Fatalf("the collection href is not the requested Nextcloud path:\n%s", body)
	}
	if !strings.Contains(body, "<D:href>/remote.php/dav/files/alice/docs/a.txt</D:href>") {
		t.Fatalf("the child href is not under the requested Nextcloud path:\n%s", body)
	}
}

func TestNextcloudLegacyWebdavPathAndFilesPathAreTheSameTree(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, http.MethodPut, ncBase+"/remote.php/webdav/legacy.txt",
		strings.NewReader("legacy"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT via /remote.php/webdav: %d %s", rec.Code, rec.Body.String())
	}

	rec := e.do(t, http.MethodGet, ncBase+"/remote.php/dav/files/alice/legacy.txt", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET via /remote.php/dav/files: %d", rec.Code)
	}
	if got := rec.Body.String(); got != "legacy" {
		t.Fatalf("bytes differ between the two Nextcloud paths: %q", got)
	}
}

func TestNextcloudPathNamingAnotherAccountIsRefused(t *testing.T) {
	e := newEnv(t)

	// The bytes would land in the CALLER's tree because the store resolves the relative
	// path under the authenticated account. Without the owner check the request would be
	// reported as writing to bob's namespace and succeed — a lie the client would act on.
	if rec := e.do(t, "PROPFIND", ncBase+"/remote.php/dav/files/bob/secret.txt", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("PROPFIND on another account's namespace answered %d, want 404", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/bob/secret.txt",
		strings.NewReader("x"), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT on another account's namespace answered %d, want 404", rec.Code)
	}
	if rec := e.do(t, "MKCOL", ncBase+"/remote.php/dav/files/bob/folder", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("MKCOL on another account's namespace answered %d, want 404", rec.Code)
	}

	// The native shape has the same rule; the handler's comment claimed it since the
	// beginning while the code discarded the owner segment.
	rec := e.do(t, "PROPFIND", ncBase+"/dav/bob/", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PROPFIND /dav/bob/ answered %d, want 404", rec.Code)
	}
}

func TestNextcloudPathRoutesUnknownDavSubtreesToNotFound(t *testing.T) {
	e := newEnv(t)

	// Chunked uploads are not implemented and are not advertised. A request to the
	// staging namespace must not be interpreted as a relative path inside the caller's
	// tree, which is what a loose prefix trim would do.
	for _, p := range []string{
		"/remote.php/dav/uploads/alice/123/0000001",
		"/remote.php/dav/versions/alice/versions/1",
		"/remote.php/dav/trashbin/alice/trash",
	} {
		rec := e.do(t, "PROPFIND", ncBase+p, nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("PROPFIND %s answered %d, want 404", p, rec.Code)
		}
	}
}

func TestNextcloudDavRootResolvesToTheCallersRoot(t *testing.T) {
	e := newEnv(t)

	rec := e.do(t, "PROPFIND", ncBase+"/remote.php/dav/", nil, map[string]string{"Depth": "0"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND /remote.php/dav/: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<D:resourcetype><D:collection/></D:resourcetype>") {
		t.Fatalf("the DAV root should be a collection:\n%s", rec.Body.String())
	}
}

func TestNextcloudUploadHonoursXOCMtime(t *testing.T) {
	e := newEnv(t)

	// 2020-09-13T12:26:40Z
	rec := e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/alice/dated.txt",
		strings.NewReader("dated"), map[string]string{"X-OC-Mtime": "1600000000"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-OC-MTime"); got != "accepted" {
		t.Fatalf("X-OC-MTime reply = %q, want \"accepted\"; the client treats its absence as "+
			"\"the server used its own clock\" and re-syncs the file", got)
	}

	rec = e.do(t, "PROPFIND", ncBase+"/remote.php/dav/files/alice/dated.txt", nil,
		map[string]string{"Depth": "0"})
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<D:getlastmodified>Sun, 13 Sep 2020 12:26:40 GMT</D:getlastmodified>") {
		t.Fatalf("the client's mtime was not stored:\n%s", body)
	}

	// An overwrite carries its own mtime, and the accepted header must follow it.
	rec = e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/alice/dated.txt",
		strings.NewReader("dated again"), map[string]string{"X-OC-Mtime": "1700000000"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("overwrite: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodHead, ncBase+"/remote.php/dav/files/alice/dated.txt", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", rec.Code)
	}
	if got := rec.Header().Get("Last-Modified"); !strings.Contains(got, "14 Nov 2023") {
		t.Fatalf("Last-Modified = %q, want the second upload's mtime", got)
	}

	// A malformed value must not be accepted and must not fail the upload: the file
	// exists, the server simply kept its own clock.
	rec = e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/alice/undated.txt",
		strings.NewReader("x"), map[string]string{"X-OC-Mtime": "not-a-time"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT with a malformed mtime: %d", rec.Code)
	}
	if got := rec.Header().Get("X-OC-MTime"); got != "" {
		t.Fatalf("a malformed mtime was reported as accepted (%q)", got)
	}
}

func TestNextcloudMoveAcceptsANextcloudDestination(t *testing.T) {
	e := newEnv(t)

	if rec := e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/alice/src.txt",
		strings.NewReader("move me"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d", rec.Code)
	}

	rec := e.do(t, "MOVE", ncBase+"/remote.php/dav/files/alice/src.txt", nil,
		map[string]string{
			"Destination": ncBase + "/remote.php/dav/files/alice/dst.txt",
			"Overwrite":   "T",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("MOVE with a Nextcloud Destination: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodGet, ncBase+"/remote.php/dav/files/alice/dst.txt", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after MOVE: %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	if string(b) != "move me" {
		t.Fatalf("moved content = %q", b)
	}

	// A Destination naming another account is the same refusal as a URL naming it, and
	// the source must be untouched.
	rec = e.do(t, "MOVE", ncBase+"/remote.php/dav/files/alice/dst.txt", nil,
		map[string]string{"Destination": ncBase + "/remote.php/dav/files/bob/stolen.txt"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("MOVE into another namespace answered %d, want 404", rec.Code)
	}
	rec = e.do(t, http.MethodGet, ncBase+"/remote.php/dav/files/alice/dst.txt", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the refused MOVE removed the source: GET answered %d", rec.Code)
	}
}

func TestNextcloudResponseHeadersCarryTheETag(t *testing.T) {
	e := newEnv(t)

	rec := e.do(t, http.MethodPut, ncBase+"/remote.php/dav/files/alice/etag.txt",
		strings.NewReader("etag"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("a Nextcloud client stores the ETag from the PUT response to avoid a second " +
			"PROPFIND; an empty one makes it re-list the folder after every upload")
	}
}

// Ensure the compatibility paths keep the method surface: OPTIONS is what a client uses
// to discover that WebDAV exists at all.
func TestNextcloudPathAnswersOptions(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodOptions, ncBase+"/remote.php/dav/files/alice/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS: %d", rec.Code)
	}
	if dav := rec.Header().Get("DAV"); !strings.Contains(dav, "1") {
		t.Fatalf("DAV header = %q", dav)
	}
}
