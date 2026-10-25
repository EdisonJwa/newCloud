package config

import "testing"

// PlainHTTP is the switch the whole plain-HTTP deployment hangs on (cookies drop their
// prefixes, the demo's -no-tls mode wires it through public_url), so the scheme sniffing
// is pinned here rather than left as an implementation detail.
func TestPlainHTTP(t *testing.T) {
	for _, tc := range []struct {
		publicURL string
		want      bool
	}{
		{"https://cloud.example.com", false},
		{"http://localhost:8080", true},
		{"HTTP://LocalHost:8080", true}, // scheme comparison is case-insensitive
		{"  http://localhost:8080  ", true},
		{"httpx://cloud.example.com", false},
		{"", false},
		{"cloud.example.com", false},
	} {
		s := ServerConfig{PublicURL: tc.publicURL}
		if got := s.PlainHTTP(); got != tc.want {
			t.Errorf("PlainHTTP(%q) = %v, want %v", tc.publicURL, got, tc.want)
		}
	}
}
