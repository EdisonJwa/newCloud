// Package webassets embeds the browser client.
//
// The client is a React + TypeScript application built by Vite into ./dist. The build output
// is embedded rather than served from disk so the binary is the entire deployment artifact:
// there is no static directory to mount, no cache to warm, and no way for the served markup
// to differ from what was reviewed.
//
// This package previously embedded three hand-written files and the frontend had no build
// step at all. That changed with the redesign, and the trade is worth stating: the build now
// needs Node, but the runtime dependency count is still zero — the output is static JS and
// CSS, and nothing from npm ships inside the binary except compiled bytes. A single
// `go build` still produces a deployable artifact because the build output is committed.
package webassets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var files embed.FS

// Handler serves the single-page application.
//
// It answers /assets/* from the embedded build and everything else with index.html, so a
// client-side route such as /s/<token> reaches the app rather than a 404. The router mounts it
// last, after every API route, so this fallback can never shadow an API endpoint.
type Handler struct {
	assets map[string][]byte
	index  []byte
	// hashes maps asset path to a content-derived ETag.
	hashes map[string]string
}

// New builds the asset handler.
func New() (*Handler, error) {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		return nil, err
	}

	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}

	h := &Handler{
		assets: map[string][]byte{},
		index:  index,
		hashes: map[string]string{},
	}

	// Load every asset once. Vite emits a small, fixed set of hashed files, so holding them
	// in memory removes the per-request filesystem walk entirely.
	err = fs.WalkDir(sub, "assets", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(sub, p)
		if readErr != nil {
			return readErr
		}
		h.assets[p] = data
		sum := sha256.Sum256(data)
		h.hashes[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}

	return h, nil
}

// ServeHTTP implements the SPA fallback.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only GET and HEAD make sense for static content.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A missing asset is a real 404. Returning the HTML shell instead would make a missing
	// script look like a page that loads and then silently does nothing.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")

		data, ok := h.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}

		// The file names Vite emits already contain a content hash, so the URL changes when
		// the bytes do; that justifies an immutable cache. The ETag is a second, independent
		// guarantee for anything that revalidates anyway.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", contentTypeOf(name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("ETag", h.hashes[name])

		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, h.hashes[name]) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
		return
	}

	// The shell is never cached: it names the hashed assets, so a stale copy would pin the
	// browser to an old bundle.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Clickjacking is a real risk for a file manager: framing it and overlaying fake controls
	// is how a user is tricked into deleting or sharing something.
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(h.index)
}

// contentTypeOf maps an asset extension to a media type.
//
// The set is closed: only what Vite emits is served, so nothing here can be talked into
// returning text/html for a file someone uploaded.
func contentTypeOf(name string) string {
	switch path.Ext(name) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// etagMatches reports whether an If-None-Match header names this validator.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
