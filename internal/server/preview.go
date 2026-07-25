package server

// File preview.
//
// A preview is a download that the browser renders instead of saving. That single change
// — attachment to inline — is the whole risk, and it is why this file exists rather than a
// one-word parameter on the download handler.
//
// The threat is concrete: a user uploads an SVG or an HTML file. With inline rendering on
// the application origin, that file executes with the application's own origin, and the
// session cookie is right there. Stored XSS against every account that opens the file.
//
// Three defences, in order of how much they are relied on:
//
//  1. A type allow-list. Only types this server knows how to render inertly are previewed
//     in place; everything else downloads. HTML, SVG, XHTML and unknown types are never
//     inlined.
//  2. A response CSP of `sandbox`, which makes the browser treat the document as a unique
//     opaque origin with scripts disabled. This is the backstop for a type that slips
//     through the list — a format nobody expected to be active content.
//  3. `X-Content-Type-Options: nosniff`, so a claimed type cannot be reinterpreted by
//     sniffing the bytes.
//
// The application's own CSP (frame-ancestors 'none') does not help here: it applies to the
// app shell, not to a preview document served in a top-level tab or an iframe.

import (
	"bytes"
	"mime"
	"net/http"
	"path"
	"strings"
)

// previewKind classifies how a file should be presented.
type previewKind string

const (
	// previewNone: hand it to the browser as a download.
	previewNone previewKind = ""
	// previewImage: <img>, or a canvas for SVG-free formats.
	previewImage previewKind = "image"
	// previewVideo: <video>.
	previewVideo previewKind = "video"
	// previewAudio: <audio>.
	previewAudio previewKind = "audio"
	// previewPDF: the browser's own viewer, in an iframe.
	previewPDF previewKind = "pdf"
	// previewText: fetched and rendered as text by the client, never by the browser as
	// markup.
	previewText previewKind = "text"
)

// previewable lists the types that may be served inline.
//
// Every entry is a format a browser renders without executing script. SVG is deliberately
// absent and always will be: an SVG document can carry <script> and event handlers, and
// "SVG is an image" is exactly the mistaken reasoning that turns a file manager into an
// XSS vector. PDF is included because browsers sandbox their own viewer, and its CSP is
// still applied as a second layer.
var previewable = map[string]previewKind{
	// Images. No SVG: see above.
	"image/png":                previewImage,
	"image/jpeg":               previewImage,
	"image/gif":                previewImage,
	"image/webp":               previewImage,
	"image/avif":               previewImage,
	"image/bmp":                previewImage,
	"image/x-icon":             previewImage,
	"image/vnd.microsoft.icon": previewImage,

	// Video and audio: browsers decode these themselves.
	"video/mp4":       previewVideo,
	"video/webm":      previewVideo,
	"video/ogg":       previewVideo,
	"video/quicktime": previewVideo,
	"audio/mpeg":      previewAudio,
	"audio/ogg":       previewAudio,
	"audio/wav":       previewAudio,
	"audio/x-wav":     previewAudio,
	"audio/flac":      previewAudio,
	"audio/mp4":       previewAudio,

	// PDF, via the browser's viewer.
	"application/pdf": previewPDF,

	// Plain text and source. The client fetches these and inserts them with textContent,
	// so the browser never parses them as markup.
	"text/plain":             previewText,
	"text/markdown":          previewText,
	"text/csv":               previewText,
	"text/xml":               previewText,
	"application/json":       previewText,
	"application/xml":        previewText,
	"text/css":               previewText,
	"text/javascript":        previewText,
	"application/javascript": previewText,
	"application/x-yaml":     previewText,
	"text/yaml":              previewText,
}

// textExtensions widen the text classification to source files whose registered media
// type is unhelpful or absent. Extension is used only to decide "show as text", never to
// decide "render as markup", so a wrong entry here cannot create script execution.
var textExtensions = map[string]bool{
	".md": true, ".markdown": true, ".rst": true, ".txt": true, ".log": true,
	".csv": true, ".tsv": true,
	".json": true, ".jsonl": true, ".yaml": true, ".yml": true, ".toml": true,
	".ini": true, ".conf": true, ".env": true,
	".go": true, ".rs": true, ".py": true, ".rb": true, ".php": true,
	".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true,
	".css": true, ".scss": true, ".less": true,
	".html": true, ".htm": true, ".xml": true, ".svg": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".java": true, ".kt": true, ".swift": true, ".sh": true, ".bash": true,
	".ps1": true, ".sql": true, ".proto": true, ".graphql": true,
	".dockerfile": true, ".makefile": true, ".gitignore": true,
	".lua": true, ".pl": true, ".r": true, ".scala": true, ".dart": true,
	".vue": true, ".svelte": true,
}

// classifyPreview decides how a node should be presented.
//
// The stored MIME type is preferred, but it came from the uploading client and is
// therefore a claim, not a fact. The extension is consulted as a second opinion, and the
// two disagreeing is resolved conservatively: anything that would be inlined on the
// strength of a client-supplied type alone is re-checked against the extension.
func classifyPreview(name, storedMIME string) previewKind {
	ext := strings.ToLower(path.Ext(name))

	// HTML and SVG are never inlined, whatever the claimed type says. Browsers infer these
	// from the extension too, so a file named "x.svg" served as image/png would still be
	// parsed as SVG by some clients.
	switch ext {
	case ".html", ".htm", ".xhtml", ".svg", ".svgz", ".xml", ".xsl", ".xslt":
		// XML belongs here with the rest: it can carry an <?xml-stylesheet?> processing
		// instruction, and a browser fetches that. The text-editor role is served by
		// previewText for extensions that are unambiguously source, not by this path.
		return previewNone
	case ".pdf":
		return previewPDF
	}

	// Normalise the stored type: drop parameters like "; charset=utf-8".
	declared := storedMIME
	if idx := strings.IndexByte(declared, ';'); idx >= 0 {
		declared = declared[:idx]
	}
	declared = strings.ToLower(strings.TrimSpace(declared))

	if declared == "" || declared == "application/octet-stream" {
		declared = strings.ToLower(guessType(name))
		if idx := strings.IndexByte(declared, ';'); idx >= 0 {
			declared = declared[:idx]
		}
	}

	if kind, ok := previewable[declared]; ok {
		// The client claimed a previewable type; confirm nothing about the name says
		// otherwise. An executable-looking extension behind an image/png claim is a
		// mismatch worth refusing.
		if kind == previewImage && isExecutableExtension(ext) {
			return previewNone
		}
		return kind
	}

	if textExtensions[ext] {
		return previewText
	}
	return previewNone
}

// isExecutableExtension reports whether an extension is something a user runs.
//
// It matters only in the mismatch case above: an uploader who names a program ".png" and
// declares image/png should get a download, not a broken image tag that some viewer might
// still launch.
func isExecutableExtension(ext string) bool {
	switch ext {
	case ".exe", ".dll", ".msi", ".bat", ".cmd", ".com", ".scr", ".ps1",
		".sh", ".app", ".dmg", ".pkg", ".deb", ".rpm", ".jar", ".apk":
		return true
	}
	return false
}

// previewHeaders sets the response headers for an inline render.
//
// The Content-Type is taken from the classification rather than from the stored value: a
// stored type is a client claim, and the classification is the decision this server is
// willing to stand behind.
func previewHeaders(w http.ResponseWriter, name, storedMIME string, kind previewKind) {
	ctype := safeContentType(name, storedMIME, kind)

	h := w.Header()
	h.Set("Content-Type", ctype)
	// nosniff stops the browser from second-guessing the declared type, which is the
	// mechanism that would otherwise let an HTML-looking file execute as HTML.
	h.Set("X-Content-Type-Options", "nosniff")
	// sandbox is the backstop: even if the type were wrong, the document gets an opaque
	// origin with scripts disabled and no access to the application's storage.
	h.Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; media-src 'self'; "+
			"style-src 'unsafe-inline'; sandbox; frame-ancestors 'self'")
	h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	h.Set("Content-Disposition", inlineDisposition(name))
}

// safeContentType picks the type to declare.
//
// Text previews are always served as text/plain: the client renders them with
// textContent, and declaring text/html for a .html file would invite the browser to
// execute it if it ever fetched the URL directly.
func safeContentType(name, storedMIME string, kind previewKind) string {
	switch kind {
	case previewText:
		return "text/plain; charset=utf-8"
	case previewPDF:
		return "application/pdf"
	}

	declared := storedMIME
	if idx := strings.IndexByte(declared, ';'); idx >= 0 {
		declared = declared[:idx]
	}
	declared = strings.TrimSpace(declared)
	if declared == "" || declared == "application/octet-stream" {
		declared = guessType(name)
	}
	if idx := strings.IndexByte(declared, ';'); idx >= 0 {
		declared = declared[:idx]
	}
	return strings.TrimSpace(declared)
}

// inlineDisposition is the attachment header with inline substituted.
//
// The filename parameters are kept: a "save as" from the preview still produces the right
// name, and RFC 5987 encoding is what makes a non-ASCII name survive.
func inlineDisposition(name string) string {
	return "inline" + strings.TrimPrefix(contentDisposition(name), "attachment")
}

// looksLikeActiveContent reports whether the bytes begin with markup a browser would
// treat as an active document.
//
// This is the check that no header can substitute for. A client may declare image/png,
// name the file .png, and send HTML: the declared type is a claim, and the extension is
// part of the same claim. Only the bytes are evidence.
//
// The sniff is deliberately one-sided. A false positive costs a preview (the file
// downloads instead); a false negative costs an XSS on the application origin. Guessing in
// the safe direction is the whole point.
func looksLikeActiveContent(head []byte) bool {
	// Strip a UTF-8 BOM and leading whitespace: a browser does, so a payload can hide
	// behind either.
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF})
	head = bytes.TrimLeft(head, " \t\r\n")

	lower := bytes.ToLower(head)

	switch {
	case bytes.HasPrefix(lower, []byte("<!doctype html")),
		bytes.HasPrefix(lower, []byte("<html")),
		bytes.HasPrefix(lower, []byte("<head")),
		bytes.HasPrefix(lower, []byte("<body")),
		bytes.HasPrefix(lower, []byte("<script")),
		bytes.HasPrefix(lower, []byte("<iframe")),
		bytes.HasPrefix(lower, []byte("<svg")),
		bytes.HasPrefix(lower, []byte("<math")),
		bytes.HasPrefix(lower, []byte("<?xml")),
		bytes.HasPrefix(lower, []byte("<!")):
		return true
	}

	// A bare tag anywhere near the start is enough: browsers tolerate leading comments and
	// whitespace before the document element.
	return bytes.HasPrefix(lower, []byte("<")) && bytes.Contains(lower, []byte(">"))
}

// sniffableBytes is how much of the stream the check reads.
const sniffableBytes = 512

// previewKindForNode is the exported-in-package helper the handlers use.
func previewKindForNode(name, storedMIME string) previewKind {
	return classifyPreview(name, storedMIME)
}

// previewFromHeader reports whether the client asked for the inline representation.
//
// The default stays a download. A preview is an explicit request, so a client that does
// not know about the parameter keeps the behaviour it had before this existed.
func previewFromHeader(r *http.Request) bool {
	switch r.URL.Query().Get("inline") {
	case "1", "true", "yes":
		return true
	}
	return false
}

// sniffMediaType is used by tests to confirm the declared type is one a browser treats
// inertly.
func sniffMediaType(ctype string) (string, bool) {
	media, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		return "", false
	}
	return media, true
}
