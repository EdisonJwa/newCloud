package server

// Thumbnail endpoint.
//
// GET /api/files/thumbnail?path=... renders a small PNG from the stored image.
//
// Three things this handler must get right, none of which are about resizing:
//
//  1. Authorisation is the same check as reading the file. A thumbnail is the file, just
//     smaller; a separate path that forgot the permission check would be a way to read
//     images without the right to read them.
//  2. The response is cached hard. The URL is keyed by path, but the bytes are keyed by
//     content address, so a replacement changes the bytes and the ETag catches it.
//  3. Failure is cheap. A non-image, a corrupt file or an oversized one answers 415 rather
//     than 500: the client shows the type badge instead, which is the designed outcome.

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/thumbnail"
)

// maxThumbnailSource bounds how much of a file is read to decide whether it is an image.
const maxThumbnailSource = 64 << 10

// handleThumbnail renders or serves a cached thumbnail.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	rel := r.URL.Query().Get("path")

	// Thumbnails are classified by the outcome the caller sees, and "not an image" is not a
	// failure: a directory full of PDFs would otherwise look like a broken renderer. The
	// classification happens at each return rather than from a wrapped ResponseWriter, because
	// the router already installs one and nesting them makes Flush fragile.
	nodes, err := s.deps.Meta.Resolve(r.Context(), actor.ID(), rel)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	node := nodes[len(nodes)-1]
	if node.IsDir() {
		s.observeThumbnail("not_an_image")
		writeError(w, http.StatusBadRequest, "is_directory", "a directory has no thumbnail")
		return
	}

	// The same capability a download requires. A thumbnail is content.
	cap, err := s.deps.Authz.Authorize(actor, node.ID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !cap.Can(authz.PermRead) {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	// Refuse early when the stored type says this is not an image, so a request for a PDF
	// does not open and read it.
	if kind := previewKindForNode(node.Name, node.Mime); kind != previewImage {
		s.observeThumbnail("not_an_image")
		writeError(w, http.StatusUnsupportedMediaType, "not_an_image",
			"this file has no thumbnail")
		return
	}

	// The cache key is the node's ETag, which the metadata layer derives from the content
	// address. Keying on the path would serve a stale thumbnail after a file was replaced
	// under the same name.
	key := contentKey(node)
	if key == ([32]byte{}) {
		writeError(w, http.StatusUnsupportedMediaType, "not_an_image",
			"this file has no content address")
		return
	}

	rc, err := s.deps.Content.Open(r.Context(), layout.Node{
		Owner: actor.ID().String(), Path: rel,
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer rc.Close()

	png, err := s.thumbs.Generate(key, rc)
	if err != nil {
		switch {
		case errors.Is(err, thumbnail.ErrUnsupported), errors.Is(err, thumbnail.ErrTooLarge):
			// Not an error the client can fix, and not a server fault: the UI falls back to
			// a type badge.
			writeError(w, http.StatusUnsupportedMediaType, "not_an_image",
				"this file has no thumbnail")
		default:
			s.deps.Log.Warn("thumbnail generation failed",
				"path", rel, "etag", node.ETag, "error", err)
			writeError(w, http.StatusUnsupportedMediaType, "not_an_image",
				"this file has no thumbnail")
		}
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	// The ETag is derived from the source content address: the same image always yields the
	// same thumbnail, so this is exact and needs no invalidation.
	w.Header().Set("ETag", `"t`+hex8(key)+`"`)
	// A thumbnail never changes for a given address, so it can be cached for a long time.
	// The path is the only thing that could point elsewhere, and that changes the ETag.
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Disposition", "inline")

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatchesList(match, w.Header().Get("ETag")) {
		// A cache hit is still a working thumbnail, so it counts as success rather than as a
		// distinct outcome: the client got what it asked for.
		s.observeThumbnail("ok")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// HEAD must not write a body, but the headers above are still useful.
	if r.Method == http.MethodHead {
		s.observeThumbnail("ok")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.observeThumbnail("ok")
	_, _ = w.Write(png)
}

// observeThumbnail records a thumbnail outcome when metrics are configured.
func (s *Server) observeThumbnail(result string) {
	if s.deps.Metrics != nil {
		s.deps.Metrics.ObserveThumbnail(result)
	}
}

// hex8 renders the first four bytes of a hash, enough to be unique per file and short
// enough to read in a log.
func hex8(h [32]byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 4; i++ {
		out[i*2] = digits[h[i]>>4]
		out[i*2+1] = digits[h[i]&0x0F]
	}
	return string(out)
}

// etagMatchesList reports whether an If-None-Match header names this validator.
func etagMatchesList(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// contentKey derives a stable 32-byte key from the node's content-derived ETag.
//
// The ETag is what changes when the file changes, so it is the correct cache key. Keying on
// the path instead would keep serving the old thumbnail after a replacement.
func contentKey(n store.Node) [32]byte {
	if n.ETag == "" {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(n.ETag))
}
