package server_test

// File preview is a download the browser renders in place. Moving from attachment to
// inline is the entire risk, and these tests exist to pin the boundary.
//
// The threat is not hypothetical: a user uploads an SVG or an HTML file, and with inline
// rendering on the application origin that file executes as the application, with the
// session cookie in reach. Stored XSS against every account that opens it.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// uploadWithType uploads content with a declared media type.
func uploadWithType(t *testing.T, h *harness, name, ctype, body string) {
	t.Helper()

	mb := multipartBody(t, "", name, body)
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", name, resp.StatusCode, readBody(resp))
	}
}

// inlineGet requests a preview.
func inlineGet(t *testing.T, h *harness, path string) *http.Response {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/download?inline=1&path="+path, nil, nil)
	return resp
}

// TestPreviewNeverInlinesActiveContent is the security test.
//
// SVG and HTML carry script. They must arrive as attachments even when the uploader
// declares an image type, even when the extension says "image", and even when the client
// explicitly asks for a preview.
func TestPreviewNeverInlinesActiveContent(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name  string
		ctype string
		body  string
		why   string
	}{
		{
			name:  "evil.svg",
			ctype: "image/svg+xml",
			body:  `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.cookie)</script></svg>`,
			why:   "SVG can carry script and event handlers",
		},
		{
			name:  "evil.html",
			ctype: "text/html",
			body:  `<script>fetch('/api/shares',{method:'POST'})</script>`,
			why:   "HTML is the execution primitive itself",
		},
		{
			name:  "evil.xhtml",
			ctype: "application/xhtml+xml",
			body:  `<script>alert(1)</script>`,
			why:   "XHTML is HTML with XML syntax",
		},
		{
			name:  "evil.xml",
			ctype: "text/xml",
			body:  `<?xml-stylesheet href="data:text/css,x" ?>`,
			why:   "XML can pull in a stylesheet",
		},
		{
			name:  "disguised.png",
			ctype: "image/png",
			body:  `<html><script>alert(1)</script></html>`,
			why:   "a client-claimed image type is not evidence",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uploadWithType(t, h, tc.name, tc.ctype, tc.body)

			resp := inlineGet(t, h, tc.name)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			cd := resp.Header.Get("Content-Disposition")
			if !strings.HasPrefix(cd, "attachment") {
				t.Fatalf("%s must download, not render (%s): got %q", tc.name, tc.why, cd)
			}
		})
	}
}

// TestPreviewServesImagesInline checks the feature itself works.
func TestPreviewServesImagesInline(t *testing.T) {
	h := newHarness(t)

	// A one-pixel PNG, as bytes.
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32)
	uploadWithType(t, h, "photo.png", "image/png", png)

	resp := inlineGet(t, h, "photo.png")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Fatalf("an image should render inline, got %q", cd)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("content type: %q", ct)
	}
}

// TestPreviewTextIsNeverServedAsMarkup: a .html file previewed as text must be plain text.
//
// The client renders text with textContent, so the bytes are inert. Serving it as
// text/html from this endpoint would undo that if anything ever loaded the URL directly.
func TestPreviewTextIsNeverServedAsMarkup(t *testing.T) {
	h := newHarness(t)

	uploadWithType(t, h, "notes.html", "text/html", `<h1>hello</h1><script>alert(1)</script>`)

	// The extension is refused for inline rendering entirely.
	resp := inlineGet(t, h, "notes.html")
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("html must download: %q", resp.Header.Get("Content-Disposition"))
	}

	// A text file keeps working, and is declared as text/plain whatever it contains.
	uploadWithType(t, h, "readme.md", "text/markdown", "# Title\n\nbody")
	resp2 := inlineGet(t, h, "readme.md")
	defer resp2.Body.Close()

	if cd := resp2.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Fatalf("markdown should preview inline, got %q", cd)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("text previews must be text/plain, got %q", ct)
	}
}

// TestPreviewHeadersCarrySandbox verifies the defence that does not depend on the type
// list being complete.
func TestPreviewHeadersCarrySandbox(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "shot.png", "image/png", "\x89PNG\r\n\x1a\n")

	resp := inlineGet(t, h, "shot.png")
	defer resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") {
		t.Fatalf("a preview must be sandboxed, got CSP %q", csp)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff missing: %q", got)
	}
}

// TestDownloadRemainsTheDefault: the client must opt in, or old behaviour changes.
func TestDownloadRemainsTheDefault(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "plain.png", "image/png", "\x89PNG\r\n\x1a\n")

	resp := h.do(http.MethodGet, "/api/files/download?path=plain.png", nil, nil)
	defer resp.Body.Close()

	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("a plain download must stay an attachment, got %q", cd)
	}
}

// TestPreviewSupportsRange: video seeking needs it, and the endpoint previously advertised
// Accept-Ranges while streaming the whole file.
func TestPreviewSupportsRange(t *testing.T) {
	h := newHarness(t)
	payload := "0123456789abcdefghij"
	uploadWithType(t, h, "clip.mp4", "video/mp4", payload)

	req, err := http.NewRequest(http.MethodGet,
		h.srv.URL+"/api/files/download?inline=1&path=clip.mp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
	req.Header.Set("Range", "bytes=4-8")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("want 206 for a range request, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "45678" {
		t.Fatalf("range body: %q", body)
	}
	if cr := resp.Header.Get("Content-Range"); !strings.Contains(cr, "bytes 4-8/") {
		t.Fatalf("Content-Range: %q", cr)
	}
}
