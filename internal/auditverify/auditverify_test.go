package auditverify_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
)

// memReader is an in-memory chain, so the verifier can be tested without a
// database. It deliberately keeps the ability to corrupt a row: the whole point of
// the verifier is the corrupted case, and a test that only ever exercises a valid
// chain proves nothing.
type memReader struct {
	events []audit.Event
	// auditHeadOverride simulates the case the head check exists for: the rows were
	// truncated but some external record (or a stale read) still names the old tip.
	auditHeadOverride []byte
}

func (m *memReader) AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error) {
	var out []audit.Event
	for _, e := range m.events {
		if e.ID > afterID {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memReader) AuditHead(ctx context.Context) ([]byte, error) {
	if m.auditHeadOverride != nil {
		return m.auditHeadOverride, nil
	}
	if len(m.events) == 0 {
		return audit.GenesisHash, nil
	}
	return m.events[len(m.events)-1].Hash, nil
}

// buildChain produces a valid chain of n events.
func buildChain(t *testing.T, n int) *memReader {
	t.Helper()
	r := &memReader{}
	prev := audit.GenesisHash
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < n; i++ {
		e := audit.Event{
			ID:         int64(i + 1),
			Time:       base.Add(time.Duration(i) * time.Minute),
			ActorKind:  audit.ActorUser,
			ActorEmail: "alice@example.test",
			Action:     audit.ActionFileWrite,
			TargetKind: audit.TargetNode,
			TargetID:   uuid.New().String(),
			Result:     audit.ResultOK,
			Metadata:   map[string]any{"name": "a.txt", "seq": i},
			PrevHash:   prev,
		}
		if err := e.Seal(); err != nil {
			t.Fatal(err)
		}
		prev = e.Hash
		r.events = append(r.events, e)
	}
	return r
}

func TestIntactChainVerifies(t *testing.T) {
	r := buildChain(t, 25)

	report, err := auditverify.Run(context.Background(), r, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("intact chain must verify: %v", report.Err)
	}
	if report.Events != 25 {
		t.Fatalf("want 25 events checked, got %d", report.Events)
	}
	if report.FirstEvent == nil || report.LastEvent == nil {
		t.Fatal("the report must bound the verified window")
	}
}

// TestModifiedRowIsDetected is T-15: an edited audit row must break verification.
func TestModifiedRowIsDetected(t *testing.T) {
	r := buildChain(t, 10)
	// Rewrite what the row says without recomputing its hash, which is exactly what
	// an attacker with database access would try first.
	r.events[4].Metadata = map[string]any{"name": "innocent.txt", "seq": 4}

	report, err := auditverify.Run(context.Background(), r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("a modified row must be detected")
	}
	if report.BrokenAt != r.events[4].ID {
		t.Fatalf("want the report to name event %d, got %d", r.events[4].ID, report.BrokenAt)
	}
}

// TestTruncationIsDetected: deleting the tail leaves a chain that still verifies
// link by link, so only a head comparison can catch it.
func TestTruncationIsDetected(t *testing.T) {
	r := buildChain(t, 10)
	full := r.events[len(r.events)-1].Hash

	// Remove the last three rows. Every remaining event still chains correctly.
	r.events = r.events[:7]
	// The stored head still names the old tip, as an external anchor would.
	r.auditHeadOverride = full

	report, err := auditverify.Run(context.Background(), r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("truncating the log must be detected via the head")
	}
}

// TestDeletedMiddleRowIsDetected covers the other deletion shape: a row removed
// from the middle breaks the link from its successor.
func TestDeletedMiddleRowIsDetected(t *testing.T) {
	r := buildChain(t, 10)
	r.events = append(r.events[:5], r.events[6:]...)

	report, err := auditverify.Run(context.Background(), r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("a removed row must be detected")
	}
}

func TestEmptyChainVerifies(t *testing.T) {
	r := &memReader{}
	report, err := auditverify.Run(context.Background(), r, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("an empty log is a valid chain: %v", report.Err)
	}
	if report.Events != 0 {
		t.Fatalf("want 0 events, got %d", report.Events)
	}
}
