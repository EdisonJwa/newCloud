package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/metrics"
)

// Metrics tests.
//
// The exposition format is written by hand, so the failure mode is a document that parses
// as nothing — a scraper silently ingests zero series and the dashboard goes flat without
// an error. These tests check the shape a scraper actually requires: HELP/TYPE lines before
// each family, one sample per line, and no duplicate series.

func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

// TestExpositionIsWellFormed is the baseline: every non-comment, non-blank line must be a
// `name{labels} value` sample with a numeric value.
func TestExpositionIsWellFormed(t *testing.T) {
	r := metrics.New()
	r.ObserveRequest("GET", 200, false)
	r.ObserveRequest("GET", 500, false)
	r.ObserveRequest("PUT", 409, true)
	r.ObserveUpload("ok")
	r.ObserveUpload("failed")
	r.ObserveThumbnail("ok")
	r.SetJobs(3, 1, 2)
	r.SetAudit(42, 0)
	r.ObserveAuditVerify(true)
	r.ObserveBlobGC(7, false)

	out := scrape(t, r)

	var samples int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		samples++
		// The last whitespace-separated field is the value.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("malformed sample line: %q", line)
		}
		if !strings.HasPrefix(fields[0], "newcloud_") {
			t.Fatalf("every series should be namespaced: %q", line)
		}
		// A value must be numeric; anything else makes the scraper drop the line.
		if _, err := parseFloat(fields[len(fields)-1]); err != nil {
			t.Fatalf("non-numeric value in %q: %v", line, err)
		}
	}
	if samples == 0 {
		t.Fatal("no samples were emitted")
	}
}

// TestNoDuplicateSeries: two lines with the same name and labels make a scraper reject the
// whole document.
func TestNoDuplicateSeries(t *testing.T) {
	r := metrics.New()
	// The same request twice must aggregate into one series, not two lines.
	r.ObserveRequest("GET", 200, false)
	r.ObserveRequest("GET", 200, false)

	out := scrape(t, r)

	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := line[:strings.LastIndex(line, " ")]
		if seen[key] {
			t.Fatalf("duplicate series: %q", key)
		}
		seen[key] = true
	}

	// And the count aggregated rather than being overwritten.
	if !strings.Contains(out, `newcloud_http_requests_total{method="GET",status_class="2xx"} 2`) {
		t.Fatalf("the two requests should aggregate to 2:\n%s", out)
	}
}

// TestStatusCodeClassesAggregate: the cardinality must stay bounded, which is the reason
// statuses are bucketed at all.
func TestStatusCodeClassesAggregate(t *testing.T) {
	r := metrics.New()
	for _, code := range []int{200, 201, 204} {
		r.ObserveRequest("GET", code, false)
	}
	r.ObserveRequest("GET", 404, false)
	r.ObserveRequest("GET", 503, false)

	snap := r.Snapshot()
	if snap.HTTPRequests["GET 2xx"] != 3 {
		t.Fatalf("want 3 successes, got %d", snap.HTTPRequests["GET 2xx"])
	}
	if snap.HTTPRequests["GET 4xx"] != 1 {
		t.Fatalf("want 1 client error, got %d", snap.HTTPRequests["GET 4xx"])
	}
	if snap.HTTPRequests["GET 5xx"] != 1 {
		t.Fatalf("want 1 server error, got %d", snap.HTTPRequests["GET 5xx"])
	}
}

// TestDAVConflictsAreCounted: the requirement names DAV conflict rate specifically.
func TestDAVConflictsAreCounted(t *testing.T) {
	r := metrics.New()

	r.ObserveRequest("PROPFIND", 207, true)
	r.ObserveRequest("PUT", 409, true)  // lock conflict
	r.ObserveRequest("PUT", 412, true)  // stale etag
	r.ObserveRequest("GET", 412, false) // a non-DAV 412 is not a DAV conflict

	snap := r.Snapshot()
	if snap.DAVRequests != 3 {
		t.Fatalf("want 3 DAV requests, got %d", snap.DAVRequests)
	}
	if snap.DAVConflicts != 2 {
		t.Fatalf("want 2 DAV conflicts, got %d", snap.DAVConflicts)
	}
}

// TestUploadOutcomes: upload failure rate needs the failures, not just the successes.
func TestUploadOutcomes(t *testing.T) {
	r := metrics.New()
	r.ObserveUpload("ok")
	r.ObserveUpload("ok")
	r.ObserveUpload("too_large")

	snap := r.Snapshot()
	if snap.Uploads["ok"] != 2 || snap.Uploads["too_large"] != 1 {
		t.Fatalf("wrong upload counts: %v", snap.Uploads)
	}
}

// TestQueueDepthIsPublished: a backlog is the signal an operator needs before it is a
// problem.
func TestQueueDepthIsPublished(t *testing.T) {
	r := metrics.New()
	r.SetJobs(12, 2, 1)

	out := scrape(t, r)
	for _, want := range []string{
		`newcloud_jobs{state="pending"} 12`,
		`newcloud_jobs{state="running"} 2`,
		`newcloud_jobs{state="failed"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestHelpAndTypePrecedeEachFamily: without them a strict parser rejects the document.
func TestHelpAndTypePrecedeEachFamily(t *testing.T) {
	r := metrics.New()
	r.ObserveRequest("GET", 200, false)
	out := scrape(t, r)

	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "newcloud_http_requests_total{") {
			continue
		}
		// Walk back to find the HELP and TYPE for this family.
		var sawHelp, sawType bool
		for j := i - 1; j >= 0 && strings.HasPrefix(lines[j], "#"); j-- {
			if strings.HasPrefix(lines[j], "# HELP newcloud_http_requests_total ") {
				sawHelp = true
			}
			if strings.HasPrefix(lines[j], "# TYPE newcloud_http_requests_total ") {
				sawType = true
			}
		}
		if !sawHelp || !sawType {
			t.Fatalf("family lacks HELP/TYPE (help=%v type=%v)", sawHelp, sawType)
		}
		break
	}
}

// TestConcurrentObservationIsSafe: metrics are incremented from every request goroutine.
func TestConcurrentObservationIsSafe(t *testing.T) {
	r := metrics.New()
	done := make(chan struct{})

	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 500; j++ {
				r.ObserveRequest("GET", 200, false)
				r.ObserveUpload("ok")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	snap := r.Snapshot()
	if snap.HTTPRequests["GET 2xx"] != 4000 {
		t.Fatalf("want 4000 requests, got %d", snap.HTTPRequests["GET 2xx"])
	}
	if snap.Uploads["ok"] != 4000 {
		t.Fatalf("want 4000 uploads, got %d", snap.Uploads["ok"])
	}
}

// TestScrapeWhileWriting: a scrape must not block on the request path.
func TestScrapeWhileWriting(t *testing.T) {
	r := metrics.New()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				r.ObserveRequest("GET", 200, false)
			}
		}
	}()
	defer close(stop)

	for i := 0; i < 20; i++ {
		out := scrape(t, r)
		if !strings.Contains(out, "newcloud_http_requests_total") {
			t.Fatal("scrape lost the family mid-write")
		}
	}
}

// TestHandlerSetsContentType: a scraper chooses its parser from this header.
func TestHandlerSetsContentType(t *testing.T) {
	r := metrics.New()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("wrong content type: %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metrics must not be cached, got %q", rec.Header().Get("Cache-Control"))
	}
}

// TestBodyIsReadable guards against a handler that writes nothing.
func TestBodyIsReadable(t *testing.T) {
	r := metrics.New()
	resp, err := http.Get(servedURL(t, r))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("empty metrics body")
	}
}

// servedURL starts a test server returning the metrics.
func servedURL(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// parseFloat parses a sample value.
func parseFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }
