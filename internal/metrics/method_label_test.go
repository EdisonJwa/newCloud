package metrics

// The method label is client-controlled input, and a counter label fed straight from the
// wire grows one series per distinct spelling a client invents. These tests pin the
// allowlist behaviour: known methods keep their label, everything else collapses into a
// single OTHER series, and no observed label can be longer than the longest allowlist entry.

import (
	"strings"
	"testing"
)

// TestUnknownMethodsCollapseIntoOneSeries is the cardinality bound.
//
// Two different out-of-allowlist methods must land on the same series: an attacker (or a
// broken proxy) that sends a new verb per request must not be able to allocate process
// memory per request through the metrics endpoint.
func TestUnknownMethodsCollapseIntoOneSeries(t *testing.T) {
	r := New()
	r.ObserveRequest("get", 200, false)       // wrong case: not the same token as GET
	r.ObserveRequest("BREW", 200, false)      // RFC 2324, unfortunately real in the wild
	r.ObserveRequest("GET\u200b", 200, false) // a zero-width space is not a method
	r.ObserveRequest(strings.Repeat("X", 4096), 200, false)

	snap := r.Snapshot()
	if got := len(snap.HTTPRequests); got != 1 {
		t.Fatalf("%d distinct series from five unknown methods; the method label is still "+
			"client-controlled cardinality", got)
	}
	if _, ok := snap.HTTPRequests["OTHER 2xx"]; !ok {
		t.Fatalf("unknown methods did not land on the OTHER series: %v", snap.HTTPRequests)
	}
}

// TestAllowedMethodsKeepTheirLabels: the allowlist must not swallow the methods the
// routers dispatch on, or the error-rate query loses its breakdown.
func TestAllowedMethodsKeepTheirLabels(t *testing.T) {
	allowed := []string{
		"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS",
		"PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK",
		"REPORT", "SEARCH",
	}
	for _, m := range allowed {
		r := New()
		r.ObserveRequest(m, 200, false)
		snap := r.Snapshot()
		if _, ok := snap.HTTPRequests[m+" 2xx"]; !ok {
			t.Fatalf("%q was folded away; the per-method breakdown lost a method it needs", m)
		}
	}
}

// TestMethodLabelNeverExceedsTheAllowlistMaximum bounds the label width structurally: even
// a method accepted by the allowlist can never widen the series name, and a hostile one
// cannot get a long string into the exposition document at all.
func TestMethodLabelNeverExceedsTheAllowlistMaximum(t *testing.T) {
	const longest = len("PROPPATCH")
	for _, m := range []string{"GET", "PROPFIND", "PROPPATCH", "NOT-A-METHOD",
		strings.Repeat("LONG", 1024)} {
		if got := len(normaliseMethod(m)); got > longest {
			t.Fatalf("normalised method label is %d characters (max allowed %d)", got, longest)
		}
	}
}
