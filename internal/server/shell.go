package server

// Serving the browser shell from paths that are also API endpoints.
//
// A share link is one URL with two audiences. A person clicks it and expects a page; the
// client code then calls the same URL for JSON. The router can only serve the shell as a
// fallback for unmatched paths, and /s/{token} is matched — so the page handler has to
// decide, from the request itself, which representation to produce.
//
// The decision is made on the Accept header, which exists for exactly this purpose. A
// navigation sends `text/html`; fetch() and API clients do not, and a JSON response is the
// correct default for them.

import (
	"net/http"
	"strings"
)

// wantsHTML reports whether the client asked for a document.
//
// The check is deliberately narrow: only a request that explicitly lists text/html gets
// the shell. `Accept: */*` — the default for curl and most HTTP libraries — must keep
// receiving JSON, because silently answering an API call with an HTML page is how a
// client ends up parsing a page as data.
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	// Only the media ranges before any q-value matter here; a browser lists text/html in
	// them, and nothing else in this product's traffic does.
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(part)
		if idx := strings.IndexByte(media, ';'); idx >= 0 {
			media = strings.TrimSpace(media[:idx])
		}
		if strings.EqualFold(media, "text/html") {
			return true
		}
	}
	return false
}

// serveAppShell writes the embedded client.
func (s *Server) serveAppShell(w http.ResponseWriter, r *http.Request) {
	if s.ui == nil {
		// Built without the client: answer with JSON rather than an empty 200, which would
		// look like a blank page with no explanation.
		writeError(w, http.StatusNotFound, "not_found", "no browser client is embedded in this build")
		return
	}
	s.ui.ServeHTTP(w, r)
}
