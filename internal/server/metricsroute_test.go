package server_test

// /metrics over HTTP, and the request-level wiring behind it.
//
// The metrics package has eleven unit tests and they all pass. None of them goes through the
// router, and the test harness never supplied a registry — so the route registration, the
// middleware that feeds it, and the "nil means no route" branch had never been executed. A
// scrape endpoint that is defined but unmounted looks exactly like a working one from the
// package's own tests, and the gap only appears when someone points Prometheus at it.
//
// These tests mount the harness with a registry and drive real requests.

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/server"
)

// TestMetricsRouteIsServedWhenARegistryIsPresent.
//
// The route is unauthenticated by design (a scraper has no session cookie), so the check is that
// a plain GET works and that it answers with the Prometheus content type rather than JSON.
func TestMetricsRouteIsServedWhenARegistryIsPresent(t *testing.T) {
	h := newHarnessWithMetrics(t)

	resp, err := http.Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("the scrape endpoint must not answer as JSON; content type is %q", ct)
	}
}

// TestMetricsAreAbsentWhenNoRegistryIsConfigured.
//
// The field is documented as "nil means metrics are not exposed", and that is a real deployment
// choice — an operator who does not run Prometheus should not have a public endpoint publishing
// operational counts. The branch has to be tested, or "nil" is a comment rather than behaviour.
func TestMetricsAreAbsentWhenNoRegistryIsConfigured(t *testing.T) {
	h := newHarness(t) // the default harness passes no registry

	resp, err := http.Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 404 is not the observable answer here, and asserting it would have been wrong. The router
	// installs the app shell as its NotFound handler, so an unregistered path returns 200 with
	// index.html — correct for a single-page app, and the reason this check looks at the content
	// instead of the status.
	//
	// What matters is that the response is not metrics: a scraper pointed at a deployment with no
	// registry must fail loudly ("unexpected content type") rather than silently record zero
	// values or a parse error every scrape. Serving HTML is a clear failure.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/plain") {
		t.Fatalf("with no registry the path must not serve exposition text; content type is %q", ct)
	}
	if strings.Contains(string(body), "newcloud_http_requests_total") {
		t.Fatalf("with no registry the path still exposes metrics:\n%s", string(body))
	}
}

// TestRealTrafficAppearsInTheScrape is the wiring check.
//
// The registry's own tests call ObserveRequest directly, which proves the counter works but not
// that anything calls it. This drives traffic through the middleware and then reads the numbers
// back out of the served exposition, which is the only form of the claim a Prometheus scrape
// would actually test.
func TestRealTrafficAppearsInTheScrape(t *testing.T) {
	h := newHarnessWithMetrics(t)

	// A 200 and a 4xx through the real router.
	//
	// The 4xx comes from inside /api rather than from an unknown path: an unknown path reaches
	// the router's NotFound handler, which is the app shell, so it answers 200 HTML. Using one
	// would have asserted a 4xx that never happens — the first version of this test did exactly
	// that and failed.
	for _, path := range []string{"/api/files/", "/api/files/download?path=/nope/absent.txt"} {
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	scrape := h.scrape(t)

	// Two requests happened, so at least two observations must be present. Asserting the exact
	// counts would make the test brittle against the login/session middleware's own requests;
	// asserting "the series exists and is non-zero" is what proves the middleware is connected.
	// The series must be present *with the real method label and a non-zero count*.
	//
	// An earlier version only asked for `status_class="4xx"` anywhere in the text, which a
	// middleware recording the wrong method would still satisfy — the mutation "record every
	// request as method=MUTATED" passed it. Naming the series and requiring a non-zero value is
	// what makes this a wiring check rather than a smoke test.
	for _, want := range []string{
		`newcloud_http_requests_total{method="GET",status_class="2xx"}`,
		`newcloud_http_requests_total{method="GET",status_class="4xx"}`,
	} {
		if !hasNonZeroSeries(scrape, want) {
			t.Fatalf("no non-zero %s in the scrape (the middleware may be disconnected or "+
				"recording the wrong labels):\n%s", want, scrape)
		}
	}
}

// hasNonZeroSeries reports whether the exposition contains series with the given prefix and a
// value greater than zero.
func hasNonZeroSeries(scrape, prefix string) bool {
	for _, line := range strings.Split(scrape, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil && v > 0 {
			return true
		}
	}
	return false
}

// TestAuditGaugesAppearInTheScrape covers the gauges that are set rather than incremented.
//
// These are pushed by the background worker (audit chain length and head age). A typo in the
// exposition format would not fail the worker, so reading them back out of the served text is
// the only check that matters.
func TestAuditGaugesAppearInTheScrape(t *testing.T) {
	h := newHarnessWithMetrics(t)

	// Set the gauges the way the worker does.
	h.metrics.SetAudit(7, 0)

	scrape := h.scrape(t)
	for _, want := range []string{
		"newcloud_audit_events",
		"newcloud_audit_chain_head_age_seconds",
	} {
		if !strings.Contains(scrape, want) {
			t.Fatalf("the scrape is missing %s:\n%s", want, scrape)
		}
	}
	if !strings.Contains(scrape, "newcloud_audit_events 7") {
		t.Fatalf("the audit gauge did not carry the value it was given:\n%s", scrape)
	}
}

// scrape fetches and returns the exposition text.
func (h *harness) scrape(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape: want 200, got %d", resp.StatusCode)
	}
	var sb strings.Builder
	buf := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// newHarnessWithMetrics builds the harness with a live registry attached.
//
// Separate from newHarness because the default harness deliberately has none: most tests should
// run without the extra surface, and the "no registry" branch needs a harness that has not been
// given one.
func newHarnessWithMetrics(t *testing.T) *harness {
	t.Helper()
	reg := metrics.New()
	h := newHarnessTuned(t, nil, func(deps *server.Deps) {
		deps.Metrics = reg
	})
	h.metrics = reg
	return h
}
