package worker_test

// The audit-chain gauges must be real numbers, refreshed without anchoring.
//
// Until the 2026-10-24 review (H6) both `newcloud_audit_events` and
// `newcloud_audit_chain_head_age_seconds` were written only inside the once-a-day
// checkpoint publish — and the head age was passed as a literal 0. The critical
// `audit chain stalled` alert compared a constant and could never fire on any
// deployment, and the scrub alert depended on a counter that stayed zero whenever
// anchoring was not configured.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

// scrape returns the /metrics body of a registry.
func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func gauge(t *testing.T, body, name string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, name))
		}
	}
	t.Fatalf("metric %s not present in:\n%s", name, body)
	return ""
}

// A chain whose newest event is three hours old must publish an age near three hours
// — not the literal zero it once carried — and the event count must be real. And it
// must do so WITHOUT anchoring configured, which is the state of every deployment
// that has not set audit.anchor_dir.
func TestAuditHealthPublishesTheRealChainHeadAge(t *testing.T) {
	meta := store.NewMemory()
	// One audited write, so the chain has a newest event to age.
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: "user.create",
			TargetKind: "user", TargetID: "1", Result: "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := meta.BackdateNewestAudit(time.Now().Add(-3 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	reg := metrics.New()
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil))).WithMetrics(reg)

	// No EnableAnchoring: the gauges must not depend on it.
	w.AuditHealth(context.Background())

	body := scrape(t, reg)
	age := gauge(t, body, "newcloud_audit_chain_head_age_seconds")
	seconds := parseSeconds(t, age)
	if seconds < 2*3600 {
		t.Fatalf("head age = %gs for an event backdated 3h; the gauge is not reading the "+
			"newest event (a literal 0 here is the H6 defect)", seconds)
	}
	if got := gauge(t, body, "newcloud_audit_events"); got != "1" {
		t.Fatalf("audit events = %s, want 1", got)
	}
}

// An empty chain must publish a zero age and a zero count, not a stale or missing
// gauge: "there is nothing to age" is itself the state a dashboard needs to see.
func TestAuditHealthOnAnEmptyChainIsZeroAndPresent(t *testing.T) {
	meta := store.NewMemory()
	reg := metrics.New()
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil))).WithMetrics(reg)

	w.AuditHealth(context.Background())

	body := scrape(t, reg)
	if got := gauge(t, body, "newcloud_audit_chain_head_age_seconds"); got != "0.000" {
		t.Fatalf("empty chain head age = %s, want 0.000", got)
	}
	if got := gauge(t, body, "newcloud_audit_events"); got != "0" {
		t.Fatalf("empty chain event count = %s, want 0", got)
	}
}

func parseSeconds(t *testing.T, v string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscan(v, &f); err != nil {
		t.Fatalf("age %q is not a number: %v", v, err)
	}
	return f
}
