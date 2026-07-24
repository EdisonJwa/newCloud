package dav

import (
	"encoding/base64"
	"net"
	"net/http"
	"strings"
)

// basicAuth parses an Authorization header, rejecting non-Basic schemes.
//
// Treating a Bearer token as a username would turn every failed client into a
// credential probe.
func basicAuth(r *http.Request) (user, pass string, ok bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Basic "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", "", false
	}
	u, p, found := strings.Cut(string(raw), ":")
	if !found || u == "" {
		return "", "", false
	}
	return u, p, true
}

// clientIP extracts the peer address.
//
// X-Forwarded-For is deliberately ignored here: this handler must not trust a
// header it cannot verify. The reverse proxy sets the real peer, and audit
// records come from that.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
