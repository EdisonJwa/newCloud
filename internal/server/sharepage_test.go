package server_test

// A share link is one URL with two audiences.
//
// A person clicks it and expects a page. The browser client then calls the same URL for
// JSON. The router serves the shell only as a fallback for unmatched paths, and
// /s/{token} is matched — so before the Accept-header split, opening a share link in a
// browser displayed raw JSON. These tests pin both halves of that contract.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// shareToken creates a plain link share and returns its token.
func shareToken(t *testing.T, h *harness) string {
	t.Helper()

	body := multipartBody(t, "", "shared.txt", "hello")
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	resp.Body.Close()

	req := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"shared.txt","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	if req.StatusCode != http.StatusCreated {
		t.Fatalf("create share: %d %s", req.StatusCode, readBody(req))
	}
	created := decode(t, req)
	token, _ := created["token"].(string)
	if token == "" {
		t.Fatal("no token")
	}
	return token
}

// TestShareLinkServesHTMLToABrowser is the regression: a click must produce a page.
func TestShareLinkServesHTMLToABrowser(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	// What a browser sends for a top-level navigation.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("a navigation must receive HTML, got %q", ct)
	}

	body := readBody(resp)
	if !strings.Contains(body, "<html") {
		t.Fatalf("the response is not an HTML document: %.120s", body)
	}
	// The shell must reference the embedded client, or the page renders nothing.
	if !strings.Contains(body, "/assets/app.js") {
		t.Fatalf("the shell does not load the client script: %.200s", body)
	}
}

// TestShareLinkServesJSONToAFetch keeps the other half working.
//
// The browser client calls this exact URL for its data; if the shell were returned
// instead, the share page would fail to load even though the HTML looked right.
func TestShareLinkServesJSONToAFetch(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	// What fetch() and any HTTP library sends when nothing is specified.
	req.Header.Set("Accept", "*/* or application/json")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("an API call must receive JSON, got %q", ct)
	}

	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info["kind"] != "link" {
		t.Fatalf("payload: %v", info)
	}
}

// TestShareLinkDefaultsToJSONWithoutAccept guards curl-style requests.
//
// Silently answering an API call with an HTML page is how a client ends up parsing a
// document as data, so the default must stay JSON.
func TestShareLinkDefaultsToJSONWithoutAccept(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Del("Accept")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("a request with no Accept header must get JSON, got %q", ct)
	}
}

// TestShareShellDoesNotLeakContent: the page shell is public, but it is not the data.
//
// A visitor must not learn anything about the share from the HTML alone — the token is
// the only thing the page is given, and the client asks for the rest.
func TestShareShellDoesNotLeakContent(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	req.Header.Set("Accept", "text/html")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := readBody(resp)
	if strings.Contains(body, "shared.txt") {
		t.Fatal("the public shell must not name the shared file")
	}
}

// TestUnknownShareTokenStillReturnsTheShell: an unknown token must not be distinguishable
// from a real one at the HTML layer either.
func TestUnknownShareTokenStillReturnsTheShell(t *testing.T) {
	h := newHarness(t)

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/s/doesnotexist", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The shell is served, and the client-side lookup reports the failure. Serving a 404
	// page here instead would make the HTML layer itself a token oracle.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want the shell, got %d", resp.StatusCode)
	}
}
