// Package webassets embeds the browser client.
//
// The files are embedded rather than served from disk so the binary is the entire
// deployment artifact: there is no static directory to mount, no cache to warm and no
// way for the served markup to differ from what was reviewed. Editing the frontend
// requires a rebuild, which is the intended trade — a mismatch between the running
// binary and the assets on disk is a class of bug that should not exist here.
package webassets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed assets
var files embed.FS

// Handler serves the single-page application.
//
// It answers /assets/* from the embedded files and everything else with index.html,
// so a client-side route such as /s/<token> reaches the app rather than a 404. The
// router mounts it last, after every API route, so this fallback can never shadow an
// API endpoint.
type Handler struct {
	assets  http.Handler
	index   []byte
	modTime time.Time
	// hashes maps asset name to a content-derived ETag.
	hashes map[string]string
}

// New builds the asset handler.
func New() (*Handler, error) {
	sub, err := fs.Sub(files, "assets")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	// Precompute a hash per file. http.FileServer cannot emit a strong validator, and
	// without one a browser keeps serving an old script after an upgrade.
	hashes := map[string]string{}
	entries, herr := fs.ReadDir(sub, ".")
	if herr != nil {
		return nil, herr
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, rerr := fs.ReadFile(sub, e.Name())
		if rerr != nil {
			return nil, rerr
		}
		sum := sha256.Sum256(data)
		hashes[e.Name()] = `"` + hex.EncodeToString(sum[:8]) + `"`
	}

	return &Handler{
		assets: http.FileServer(http.FS(sub)),
		index:  index,
		hashes: hashes,
		// Fixed at construction: Last-Modified describes the binary, not the moment a
		// request happened to arrive.
		modTime: time.Now(),
	}, nil
}

// ServeHTTP implements the SPA fallback.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only GET and HEAD make sense for static content.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A missing asset is a real 404. Returning the HTML shell instead would make a
	// missing script look like a page that loads and then silently does nothing.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		// Caching an asset for five minutes is only safe if the browser can tell when the
		// bytes change. Without a validator it cannot: after an upgrade it keeps the old
		// script until the max-age expires, and the interface looks like the deploy did not
		// happen. The ETag below is a content hash, so a new binary invalidates it
		// immediately and an unchanged one costs a 304.
		w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		name := path.Base(r.URL.Path)
		if etag, ok := h.hashes[name]; ok {
			w.Header().Set("ETag", etag)
			// Answer a conditional request without touching the file. A 304 is what makes
			// revalidation cheap enough to be worth doing on every load.
			if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/assets/")
		h.assets.ServeHTTP(w, r2)
		return
	}

	// The shell is never cached: it decides which assets to load, so a stale copy
	// pins the browser to an old asset set.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Clickjacking is a real risk for a file manager: framing it and overlaying
	// invisible controls is how a user is tricked into deleting or sharing.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self'; "+
			"img-src 'self' data:; connect-src 'self'; form-action 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
	http.ServeContent(w, r, "index.html", h.modTime, strings.NewReader(string(h.index)))
}

// etagMatches reports whether an If-None-Match header names this validator.
//
// It handles the comma-separated list and the "*" wildcard, which a browser may send when
// revalidating a subresource.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
