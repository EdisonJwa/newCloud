package server

import (
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// clientIP extracts the peer address.
//
// X-Forwarded-For is deliberately not trusted here: this process cannot verify a
// header it did not set, and an attacker-supplied value would let them forge the
// address in the audit trail — which is the one field the trail exists to record
// accurately. The reverse proxy sets RemoteAddr, and the audit reads that.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// guessType returns a content type from a filename.
//
// For display only: the content origin forces attachment, so a misidentified type
// cannot become script execution.
func guessType(name string) string {
	if ext := path.Ext(name); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	return "application/octet-stream"
}

func contentTypeFor(n store.Node) string {
	if n.Mime != "" {
		return n.Mime
	}
	return guessType(n.Name)
}

// etagFor derives a strong ETag from a content address.
//
// Deriving it from the content rather than a counter means two identical uploads
// produce the same ETag, so a client that re-uploads does not force a re-download
// on every other client.
func etagFor(h [32]byte) string {
	return fmt.Sprintf(`"%x"`, h[:8])
}

// contentDisposition forces a download with a filename that survives non-ASCII.
//
// RFC 5987 encoding is required: without it a Chinese filename arrives as mojibake
// in every browser.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, percentEncode(name))
}

func percentEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var errNotImplemented = errors.New("not implemented")
