package server_test

// The content origin (§7.1, §12).
//
// /content/* was a stub that answered "not implemented", usercontent_url was required and never read,
// and ticket.PurposeDownload was declared and never minted: three pieces of one unfinished feature.
// These tests cover the property that makes the feature worth having — the application origin cannot
// serve user bytes, and the content origin serves them only to a ticket that names one path.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/server"
)

// mintContentTicket asks the app origin for a content URL.
func mintContentTicket(t *testing.T, h *harness, path string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	resp := h.do(http.MethodPost, "/api/files/content-ticket", strings.NewReader(string(body)),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestContentOriginServesOnlyATicketThatNamesThePath(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "secret.txt", "content bytes")

	code, out := mintContentTicket(t, h, "secret.txt")
	if code != http.StatusOK {
		t.Fatalf("mint: %d %v", code, out)
	}
	raw, _ := out["url"].(string)
	if !strings.Contains(raw, "/content/secret.txt?ticket=") {
		t.Fatalf("the URL does not address the content origin: %q", raw)
	}

	// The ticket is the credential: no cookie is sent to the content origin, which is exactly why
	// the app has to mint one.
	handler := h.srvServer.ContentHandler()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	// raw may be absolute; the handler cares only about the path and query.
	req.URL.Scheme = ""
	req.URL.Host = ""
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("content fetch: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "content bytes" {
		t.Fatalf("content body = %q", rec.Body.String())
	}
	// Forced to download, whatever the file is.
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q; the content origin must never render", cd)
	}
}

func TestContentEndpointRefusesWithoutOrWithAnotherPath(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "file.txt", "bytes")
	code, out := mintContentTicket(t, h, "file.txt")
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	raw := out["url"].(string)
	ticketParam := raw[strings.Index(raw, "ticket=")+len("ticket="):]

	handler := h.srvServer.ContentHandler()
	fetch := func(url string) int {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.URL.Scheme, req.URL.Host = "", ""
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := fetch("/content/file.txt"); code != http.StatusNotFound {
		t.Fatalf("content without a ticket returned %d; the ticket is the credential", code)
	}
	// A ticket names its path: presenting it for another file must not work, or it would be a
	// bearer credential for the whole account.
	h.uploadFile(t, "other.txt", "other bytes")
	if code := fetch("/content/other.txt?ticket=" + ticketParam); code != http.StatusNotFound {
		t.Fatalf("a ticket for file.txt served other.txt (%d)", code)
	}
	// The same ticket for its own path still works, so the refusal above is about the path and not
	// about the ticket being unusable.
	if code := fetch("/content/file.txt?ticket=" + ticketParam); code != http.StatusOK {
		t.Fatalf("the ticket stopped working for its own path: %d", code)
	}
}

func TestASplitContentOriginLeavesNothingToFetchOnTheApp(t *testing.T) {
	// The same server, told that a separate content listener exists.
	h := newHarnessTuned(t, nil, func(d *server.Deps) { d.ContentSplit = true })

	h.uploadFile(t, "hidden.txt", "bytes")

	// The application origin does not have the route at all, so an accidental link there cannot
	// serve bytes with the app's origin — which is the entire security property.
	//
	// Asserted as "not the file" rather than "404": the app serves its SPA for unknown paths, and a
	// 200 containing index.html is not a leak. What must not happen is the BYTES coming back, or an
	// attachment disposition, which is what the content handler always sets.
	resp := h.do(http.MethodGet, "/content/hidden.txt", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if strings.Contains(body, "bytes") && !strings.Contains(body, "<html") {
		t.Fatalf("the application origin served the file's bytes while a content origin was " +
			"configured; the separation is decorative if both origins can serve the same content")
	}
	if cd := resp.Header.Get("Content-Disposition"); strings.HasPrefix(cd, "attachment") {
		t.Fatalf("the application origin answered with an attachment (%q), so /content/* is "+
			"registered there despite the split", cd)
	}

	// The content handler still can, by ticket.
	code, out := mintContentTicket(t, h, "hidden.txt")
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, out["url"].(string), nil)
	req.URL.Scheme, req.URL.Host = "", ""
	rec := httptest.NewRecorder()
	h.srvServer.ContentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("content origin: %d", rec.Code)
	}
}
