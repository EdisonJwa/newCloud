package auditverify_test

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
)

// Anchor tests.
//
// The value of an anchor is entirely in the tampering cases: a verification that only
// ever sees an intact chain proves nothing. Each test below removes or rewrites
// something and requires the verifier to notice.

// fakeChain is a chain of events that a test can shorten or alter.
type fakeChain struct {
	events []audit.Event
}

func (f *fakeChain) AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error) {
	var out []audit.Event
	for _, e := range f.events {
		if e.ID > afterID {
			out = append(out, e)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeChain) AuditHead(ctx context.Context) ([]byte, error) {
	if len(f.events) == 0 {
		return audit.GenesisHash, nil
	}
	return f.events[len(f.events)-1].Hash, nil
}

// buildChain produces n correctly linked events.
func buildAnchorChain(t *testing.T, n int) *fakeChain {
	t.Helper()
	return buildAnchorChainWithAction(t, n, "test.event")
}

// sealEvent returns one sealed event linked to prev.
func sealEvent(t *testing.T, id int64, at time.Time, action string, prev []byte) audit.Event {
	t.Helper()
	e := audit.Event{
		ID: id, Time: at, Action: audit.Action(action), Result: "ok",
		PrevHash: append([]byte(nil), prev...),
	}
	if err := e.Seal(); err != nil {
		t.Fatal(err)
	}
	return e
}

// buildAnchorChainWithAction produces a linked chain whose events all share an action.
func buildAnchorChainWithAction(t *testing.T, n int, action string) *fakeChain {
	t.Helper()
	events := make([]audit.Event, 0, n)
	prev := audit.GenesisHash
	for i := 0; i < n; i++ {
		events = append(events, sealEvent(t, int64(i+1), time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC), action, prev))
		prev = events[len(events)-1].Hash
	}
	return &fakeChain{events: events}
}

func newAnchor(t *testing.T) *auditverify.FileAnchor {
	t.Helper()
	a, err := auditverify.NewFileAnchor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAnchorVerifyAcceptsIntactChain is the baseline: an unmodified log must pass, or
// the check would report a breach on every deployment.
func TestAnchorVerifyAcceptsIntactChain(t *testing.T) {
	a := newAnchor(t)
	chain := buildAnchorChain(t, 10)

	head, count, err := auditverify.NewStoreChain(chain).Head()
	if err != nil {
		t.Fatal(err)
	}
	if count != 10 {
		t.Fatalf("want 10 events, got %d", count)
	}
	if err := a.PublishWithCount(time.Now(), head, count); err != nil {
		t.Fatal(err)
	}

	res, err := auditverify.Verify(a, auditverify.NewStoreChain(chain))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("an intact chain must verify, got %v", res.Problems)
	}
	if res.Matched != 1 {
		t.Fatalf("want 1 matched checkpoint, got %d", res.Matched)
	}
}

// TestAnchorDetectsTruncation is the case the anchor exists for.
//
// The remaining events still chain perfectly — deleting the tail leaves a valid log. The
// only trace is that the published checkpoint names more events than now exist.
func TestAnchorDetectsTruncation(t *testing.T) {
	a := newAnchor(t)
	full := buildAnchorChain(t, 10)

	head, count, _ := auditverify.NewStoreChain(full).Head()
	if err := a.PublishWithCount(time.Now(), head, count); err != nil {
		t.Fatal(err)
	}

	// The attacker deletes the newest five events. What remains is still a valid chain.
	truncated := &fakeChain{events: full.events[:5]}

	res, err := auditverify.Verify(a, auditverify.NewStoreChain(truncated))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("a truncated chain must not verify against its anchor")
	}
	if res.Matched != 0 {
		t.Fatalf("no checkpoint should have matched, got %d", res.Matched)
	}
}

// TestAnchorDetectsRewrittenMiddle: deleting events from the middle and relinking the
// survivors produces a chain that is internally consistent but hashes differently at the
// checkpoint's count.
func TestAnchorDetectsRewrittenMiddle(t *testing.T) {
	a := newAnchor(t)
	full := buildAnchorChain(t, 10)

	head, count, _ := auditverify.NewStoreChain(full).Head()
	if err := a.PublishWithCount(time.Now(), head, count); err != nil {
		t.Fatal(err)
	}

	// Same length, different content: identical count, different hashes. A verifier
	// that compared only counts would pass this, which is why the head is compared.
	alt := buildAnchorChainWithAction(t, 10, "test.rewritten")

	res, err := auditverify.Verify(a, auditverify.NewStoreChain(alt))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("a rewritten chain must not verify against its anchor")
	}
}

// TestAnchorDetectsDeletedCheckpoint: the checkpoints link to each other, so removing
// one leaves a gap the next one points into.
func TestAnchorDetectsDeletedCheckpoint(t *testing.T) {
	a := newAnchor(t)
	chain := buildAnchorChain(t, 10)
	sc := auditverify.NewStoreChain(chain)
	head, count, _ := sc.Head()

	// Publish three days, so a middle one can be deleted.
	day1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	day3 := day1.AddDate(0, 0, 2)
	for _, d := range []time.Time{day1, day2, day3} {
		if err := a.PublishWithCount(d, head, count); err != nil {
			t.Fatal(err)
		}
	}

	// Remove the middle checkpoint, as an attacker covering their tracks would.
	path := filepath.Join(a.DirForTest(), "2026-01-02.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	res, err := auditverify.Verify(a, sc)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("a deleted checkpoint must be detected from the broken chain of links")
	}
}

// TestAnchorIsIdempotentPerDay: a restart must not overwrite the day it already
// published, or the record that makes a rewrite detectable is itself rewritten.
func TestAnchorIsIdempotentPerDay(t *testing.T) {
	a := newAnchor(t)
	chain := buildAnchorChain(t, 5)
	sc := auditverify.NewStoreChain(chain)
	head, count, _ := sc.Head()

	day := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := a.PublishWithCount(day, head, count); err != nil {
		t.Fatal(err)
	}

	cps, err := a.Checkpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 {
		t.Fatalf("want 1 checkpoint, got %d", len(cps))
	}
	if cps[0].Head != hex.EncodeToString(head) {
		t.Fatal("the published head must be readable back")
	}
}

// TestAnchorRejectsEmptyHead: publishing nothing would produce a checkpoint that can
// never match, which reads as tampering forever.
func TestAnchorRejectsEmptyHead(t *testing.T) {
	a := newAnchor(t)
	if err := a.Publish(time.Now(), nil); err == nil {
		t.Fatal("an empty head must be refused")
	}
}
