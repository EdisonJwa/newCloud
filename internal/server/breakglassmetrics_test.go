package server_test

// §3.3 requires "使用后告警" — an alert after break-glass use. An alert needs a signal, and an
// audit row is not one: nothing scrapes the audit table. These tests pin that the counter exists,
// that it is incremented by real requests rather than by the handler calling itself, and that the
// two kinds stay distinct.
//
// The distinction matters for the operator: "it was used" pages someone, "someone is guessing at
// it" is a slower signal. Folding them into one counter would make the first alert unschedulable
// because the second fires routinely.

import (
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/server"
)

// breakglassWithMetrics wires the local administrator and a live registry.
func breakglassWithMetrics(t *testing.T) (*harness, *metrics.Registry) {
	t.Helper()
	reg := metrics.New()
	h, _ := breakglassHarnessTuned(t, []string{"127.0.0.0/8", "::1/128"},
		func(deps *server.Deps) { deps.Metrics = reg })
	h.metrics = reg
	return h, reg
}

// TestSuccessfulBreakglassLoginIsCounted.
func TestSuccessfulBreakglassLoginIsCounted(t *testing.T) {
	h, _ := breakglassWithMetrics(t)

	if code, body := h.loginBreakglass(t, "breakglass-admin", "correct horse battery"); code != 200 {
		t.Fatalf("setup: break-glass login should succeed, got %d %v", code, body)
	}

	scrape := h.scrape(t)
	if !hasNonZeroSeries(scrape, `newcloud_auth_events_total{kind="breakglass_ok"}`) {
		t.Fatalf("a successful break-glass login must be countable, or §3.3's \"使用后告警\" has "+
			"no signal to alert on:\n%s", scrape)
	}
	// The denial kind must NOT have moved: a counter that increments on both would make the
	// "someone is guessing" alert fire on every legitimate recovery.
	if hasNonZeroSeries(scrape, `newcloud_auth_events_total{kind="breakglass_denied"}`) {
		t.Fatalf("a successful login must not count as a denial:\n%s", scrape)
	}
}

// TestFailedBreakglassAttemptIsCountedSeparately.
func TestFailedBreakglassAttemptIsCountedSeparately(t *testing.T) {
	h, _ := breakglassWithMetrics(t)

	if code, _ := h.loginBreakglass(t, "breakglass-admin", "wrong password"); code == 200 {
		t.Fatal("setup: the wrong password must be refused")
	}

	scrape := h.scrape(t)
	if !hasNonZeroSeries(scrape, `newcloud_auth_events_total{kind="breakglass_denied"}`) {
		t.Fatalf("a refused attempt must be countable:\n%s", scrape)
	}
	if hasNonZeroSeries(scrape, `newcloud_auth_events_total{kind="breakglass_ok"}`) {
		t.Fatalf("a refused attempt must not count as a successful use:\n%s", scrape)
	}
}

// TestBreakglassCountersAreAbsentWithoutUse.
//
// The series only exists once the kind has been observed, which is why the alert uses `increase()`
// rather than the raw value: a rule on an absent series and a rule on a zero series behave
// differently, and the alert has to work on a fresh process that has never seen a break-glass use.
func TestBreakglassCountersAreAbsentWithoutUse(t *testing.T) {
	h, _ := breakglassWithMetrics(t)

	scrape := h.scrape(t)
	if strings.Contains(scrape, `newcloud_auth_events_total{kind="breakglass_ok"}`) {
		t.Fatalf("the counter exists before any use; the alert's increase() over a missing "+
			"series is what makes it correct on a fresh process:\n%s", scrape)
	}
}

// TestBreakglassMetricsAreAbsentWithoutARegistry.
//
// A deployment that runs no Prometheus passes no registry. The handlers must not panic, and the
// login must still work — metrics are an observer, never a dependency.
func TestBreakglassWorksWithoutARegistry(t *testing.T) {
	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})
	if code, body := h.loginBreakglass(t, "breakglass-admin", "correct horse battery"); code != 200 {
		t.Fatalf("break-glass must work with no metrics registry, got %d %v", code, body)
	}
	if code, _ := h.loginBreakglass(t, "breakglass-admin", "wrong"); code == 200 {
		t.Fatal("the refusal path must also work without a registry")
	}
}
