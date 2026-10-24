package server_test

// The monitoring assets must match the metrics the server publishes.
//
// Three separate ways these can rot, none of which any test in internal/metrics can catch:
//
//  1. an alert rule naming a metric that does not exist — it sits in "inactive" forever and looks
//     like coverage on a dashboard;
//  2. an alert filtering on a label value the server never emits — same, and harder to see because
//     the metric name is right;
//  3. an alert linking to a runbook section that was renamed — the page arrives with a dead link,
//     which is exactly when nobody has time to go looking.
//
// promtool validates the rule *syntax*, which is necessary and covers none of these. This test
// covers the three, by reading the exposition the server produces rather than assuming it.

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAlertRulesNameOnlyMetricsThatExist.
func TestAlertRulesNameOnlyMetricsThatExist(t *testing.T) {
	root := repoRoot(t)

	rulesPath := filepath.Join(root, "deploy", "monitoring", "alerts.yml")
	rulesRaw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read alerts.yml: %v", err)
	}
	rules := string(rulesRaw)

	// The exposition from a live harness is the ground truth for both names and label values.
	h := newHarnessWithMetrics(t)
	// Drive one of each interesting outcome so families that only appear after an event are
	// present.
	//
	// Requests are the exception: that family is labelled by status class, so a class only exists
	// once a response of that class has been served. Calling ObserveRequest directly is right
	// here — this test is about which labels the exposition can contain, not about whether the
	// middleware is connected (TestRealTrafficAppearsInTheScrape covers that).
	h.metrics.SetJobs(1, 0, 0)
	h.metrics.SetAudit(1, 0)
	h.metrics.ObserveUpload("ok")
	h.metrics.ObserveThumbnail("ok")
	h.metrics.ObserveBlobGC(0, false)
	h.metrics.ObserveAuditVerify(true)
	for _, class := range []int{200, 404, 500} {
		h.metrics.ObserveRequest("GET", class, false)
	}
	// The auth kinds are the same shape as the request classes: a label value exists in the
	// exposition only after that kind has been observed. Both kinds the alert rules name must be
	// present here, or the guard reports a metric the server "never publishes" — which it did the
	// first time this ran after the break-glass rules were added.
	h.metrics.ObserveAuth("breakglass_ok")
	h.metrics.ObserveAuth("breakglass_denied")

	resp, err := http.Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	exposition := sb.String()
	if !strings.Contains(exposition, "newcloud_http_requests_total") {
		t.Fatal("the exposition is empty; this test would pass vacuously")
	}

	// 1. Every newcloud_* name in the rules must appear in the exposition.
	namesInRules := regexp.MustCompile(`newcloud_[a-z_]+`).FindAllString(rules, -1)
	if len(namesInRules) == 0 {
		t.Fatal("no metric names found in the rules")
	}
	// Metrics that are deliberately NOT published by this process.
	//
	// The guard's premise is "a rule on a metric the server never publishes is decorative".
	// That premise is right for everything the server measures, and wrong for a series a
	// sibling process owns: the backup runs from cron and publishes through node_exporter's
	// textfile collector, so the application cannot and should not emit it. Each exception
	// names where the series comes from — an exception without a source is how this guard
	// would rot into a rubber stamp.
	externallyPublished := map[string]string{
		"newcloud_backup_last_success_timestamp_seconds": "deploy/backup/backup-all.sh, via node_exporter's textfile collector",
		"newcloud_backup_last_drill_timestamp_seconds":   "the restore drill, via node_exporter's textfile collector",
	}
	var unknown []string
	seen := map[string]bool{}
	for _, name := range namesInRules {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, external := externallyPublished[name]; external {
			continue
		}
		if !strings.Contains(exposition, name+"{") && !strings.Contains(exposition, name+" ") {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("alert rules name metrics the server does not publish:\n  %s\n\n"+
			"A rule on a non-existent metric never fires and looks like coverage.",
			strings.Join(unknown, "\n  "))
	}

	// 2. Every label selector must use a value the exposition actually contains.
	selectorRe := regexp.MustCompile(`newcloud_[a-z_]+(?:\{[^}]*\})?`)
	for _, selector := range selectorRe.FindAllString(rules, -1) {
		labelRe := regexp.MustCompile(`(\w+)(?:=~|!~|=|!=)(?:"([^"]*)")`)
		for _, m := range labelRe.FindAllStringSubmatch(selector, -1) {
			label, value := m[1], m[2]
			// Only exact matches are checked; a regex or a negation has no single expected value.
			if strings.Contains(selector, "=~") || strings.Contains(selector, "!=") || strings.Contains(selector, "!~") {
				continue
			}
			if value == "" {
				continue
			}
			want := label + `="` + value + `"`
			if !strings.Contains(exposition, want) {
				t.Fatalf("a rule selects %s but the server never emits that value "+
					"(selector: %s)", want, selector)
			}
		}
	}

	// 3. Every runbook link must resolve to a heading in runbook.md.
	runbookRaw, err := os.ReadFile(filepath.Join(root, "deploy", "monitoring", "runbook.md"))
	if err != nil {
		t.Fatalf("read runbook.md: %v", err)
	}
	headings := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^## (.+)$`).
		FindAllStringSubmatch(string(runbookRaw), -1) {
		slug := strings.ToLower(m[1])
		slug = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(slug, "-")
		slug = strings.Trim(slug, "-")
		headings[slug] = true
	}
	if len(headings) == 0 {
		t.Fatal("runbook.md has no sections")
	}
	linkRe := regexp.MustCompile(`runbook\.md#([a-z0-9-]+)`)
	for _, m := range linkRe.FindAllStringSubmatch(rules, -1) {
		if !headings[m[1]] {
			t.Fatalf("an alert links to runbook.md#%s, which does not exist. "+
				"Sections present: %v", m[1], keys(headings))
		}
	}
}

// keys returns the map's keys, for a useful failure message.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
