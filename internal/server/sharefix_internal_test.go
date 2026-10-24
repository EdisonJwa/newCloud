package server

// Unit coverage for the two unexported helpers the share-surface fixes added
// (2026-10-24 review, H5 and H9). These are pure functions, so the interesting
// properties — what leaks and what charges — are cheapest to pin directly.

import (
	"strings"
	"testing"
)

// redactLogPath must leave ordinary paths alone and hide the token in /s/ URLs —
// both the bare /s/{token} and the deeper /s/{token}/download forms.
func TestRedactLogPathHidesShareTokens(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/files/", "/api/files/"},
		{"/dav/alice/docs", "/dav/alice/docs"},
		{"/healthz", "/healthz"},
		{"/s/", "/s/"},
		{"/s/lAJTiLbq7vdiYlAfYSbp9Q", "/s/***"},
		{"/s/lAJTiLbq7vdiYlAfYSbp9Q/download", "/s/***/download"},
		{"/s/lAJTiLbq7vdiYlAfYSbp9Q/thumbnail", "/s/***/thumbnail"},
		{"/s/lAJTiLbq7vdiYlAfYSbp9Q/recipient/verify", "/s/***/recipient/verify"},
		{"/settings/something", "/settings/something"},
	}
	for _, c := range cases {
		if got := redactLogPath(c.in); got != c.want {
			t.Errorf("redactLogPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// No input may leak the token body.
	if got := redactLogPath("/s/secret-token-value/download"); strings.Contains(got, "secret") {
		t.Errorf("token survived redaction: %q", got)
	}
}

// isPartialRange decides whether a Range rides an existing download session or pays
// for a new one. The whole-entity forms are the security boundary: `bytes=0-` was
// once treated as a continuation and re-served the complete file for free after a
// share's download limit was exhausted.
func TestIsPartialRangeSeparatesSeeksFromFullRefetches(t *testing.T) {
	const size = 10_000

	free := []string{
		"bytes=0-1023",  // bounded prefix
		"bytes=8192-",   // non-zero start, unbounded end
		"bytes=4096-",   // a resume
		"bytes=0-9998",  // ends one byte short of the entity
		"bytes=100-200", // interior window
		"bytes=-100",    // suffix shorter than the entity
	}
	for _, rng := range free {
		if !isPartialRange(rng, size) {
			t.Errorf("%q should be a continuation: it names less than the whole entity", rng)
		}
	}

	charged := []string{
		"",                    // no Range: a plain GET
		"bytes=0-",            // the whole entity — the H9 bypass
		"bytes=0-9999",        // the whole entity, bounded spelling
		"bytes=-10000",        // a suffix as long as the entity
		"bytes=0-99999",       // over-long end: still the whole entity
		"bytes=0-100,200-300", // multi-range: conservatively a fresh download
		"garbage",             // malformed: charge rather than guess
		"bytes=",              // malformed
		"items=0-5",           // not a bytes unit
	}
	for _, rng := range charged {
		if isPartialRange(rng, size) {
			t.Errorf("%q must be charged as a fresh download", rng)
		}
	}
}
