package server

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// handleContent serves user-uploaded bytes from the content origin.
//
// Every response here is forced to download. The uploaded file may itself be a
// perfectly valid HTML document with a script tag; serving it as text/html from
// an origin that shares a cookie jar with the app would be stored XSS with extra
// steps. Attachment + nosniff + the sandbox CSP set in Caddy makes the browser
// treat it as inert bytes.
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="download"`)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	s.notImplemented(w, r, "content_download")
}

// ErrUnsafePath is returned for names that must never reach the filesystem or a
// URL, regardless of what the client claims.
var ErrUnsafePath = errors.New("unsafe path component")

// windowsReserved are device names Windows refuses, case-insensitively and with
// or without an extension. A file named "CON.txt" syncs to a Windows client and
// then cannot be opened, which users report as data loss.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// windowsIllegal are characters Windows forbids in a filename. The colon is the
// dangerous one: "C:evil" is a drive-relative path and "file:stream" addresses
// an NTFS alternate data stream, so a name containing it is never just a name.
const windowsIllegal = `<>:"|?*`

// SanitizeSegment validates a single path component.
//
// A name is a name, not a path: separators and traversal sequences are rejected
// rather than silently cleaned, because "cleaned" input that differs from what
// the user submitted is how two files end up with one name.
func SanitizeSegment(seg string) (string, error) {
	if seg == "" || seg == "." || seg == ".." {
		return "", ErrUnsafePath
	}
	if !utf8.ValidString(seg) {
		return "", ErrUnsafePath
	}
	if len(seg) > 255 {
		return "", ErrUnsafePath
	}
	for _, r := range seg {
		if r == 0 || r == '/' || r == '\\' {
			return "", ErrUnsafePath
		}
		if r < 0x20 || r == 0x7f {
			return "", ErrUnsafePath
		}
		if strings.ContainsRune(windowsIllegal, r) {
			return "", ErrUnsafePath
		}
		if unicode.IsControl(r) {
			return "", ErrUnsafePath
		}
	}
	// Windows strips trailing dots and spaces, so "report." and "report" would
	// collide on a Windows client.
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return "", ErrUnsafePath
	}
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return "", ErrUnsafePath
	}
	return seg, nil
}

// SplitPath validates a URL path into components.
func SplitPath(p string) ([]string, error) {
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "" {
		return nil, nil
	}
	parts := strings.Split(p, "/")
	for _, seg := range parts {
		if _, err := SanitizeSegment(seg); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

var _ io.Reader
