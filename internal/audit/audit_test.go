package audit

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func sampleChain(n int) []Event {
	out := make([]Event, 0, n)
	prev := GenesisHash
	base := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		e := Event{
			Time:       base.Add(time.Duration(i) * time.Second),
			ActorKind:  ActorUser,
			ActorEmail: "a@example.com",
			ActorIP:    "203.0.113.7",
			UserAgent:  "test-agent",
			Action:     ActionFileRead,
			TargetKind: TargetNode,
			TargetID:   uuid.New().String(),
			Result:     ResultOK,
			Metadata:   map[string]any{"name": "report.pdf", "seq": i},
			PrevHash:   prev,
		}
		if err := e.Seal(); err != nil {
			panic(err)
		}
		prev = e.Hash
		out = append(out, e)
	}
	return out
}

func TestChainIsIntact(t *testing.T) {
	chain := sampleChain(25)
	bad, err := VerifyChain(chain)
	if err != nil {
		t.Fatalf("intact chain reported broken at %d: %v", bad, err)
	}
	if bad != -1 {
		t.Fatalf("want -1, got %d", bad)
	}
}

func TestTamperedFieldIsDetected(t *testing.T) {
	chain := sampleChain(10)
	chain[4].ActorEmail = "attacker@example.com" // forge who did it

	bad, err := VerifyChain(chain)
	if err == nil {
		t.Fatal("expected tamper detection")
	}
	if bad != 4 {
		t.Fatalf("want first bad index 4, got %d", bad)
	}
}

func TestDeletedEventIsDetected(t *testing.T) {
	chain := sampleChain(10)
	// Remove the middle event: the successor's prev_hash no longer matches.
	broken := append(append([]Event{}, chain[:4]...), chain[5:]...)

	bad, err := VerifyChain(broken)
	if err == nil {
		t.Fatal("expected deletion detection")
	}
	if bad != 4 {
		t.Fatalf("want break at index 4, got %d", bad)
	}
}

func TestReorderedEventsAreDetected(t *testing.T) {
	chain := sampleChain(6)
	chain[2], chain[3] = chain[3], chain[2]

	if _, err := VerifyChain(chain); err == nil {
		t.Fatal("expected reorder detection")
	}
}

func TestMetadataKeyOrderDoesNotAffectHash(t *testing.T) {
	base := Event{
		Time:       time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
		ActorKind:  ActorUser,
		Action:     ActionShareDownload,
		TargetKind: TargetShare,
		Result:     ResultOK,
		PrevHash:   GenesisHash,
	}

	a := base
	a.Metadata = map[string]any{"alpha": 1, "beta": "two", "gamma": true}
	if err := a.Seal(); err != nil {
		t.Fatal(err)
	}

	b := base
	b.Metadata = map[string]any{"gamma": true, "beta": "two", "alpha": 1}
	if err := b.Seal(); err != nil {
		t.Fatal(err)
	}

	if string(a.Hash) != string(b.Hash) {
		t.Fatal("hash must not depend on map iteration order")
	}
}

func TestHeadOfEmptyChainIsGenesis(t *testing.T) {
	if string(Head(nil)) != string(GenesisHash) {
		t.Fatal("empty chain head must be the genesis hash")
	}
}

// TestIPAddressFormatsSealIdentically is a regression guard for a bug that made
// every event with an actor address permanently unverifiable.
//
// The actor_ip column is PostgreSQL `inet`, which renders "127.0.0.1" as
// "127.0.0.1/32". The writer hashed the bare address it was given; the verifier
// hashed what the database returned. Both are "the same IP" but not the same bytes,
// so verification reported tampering on an intact log — the worst possible failure
// for a tamper-evident system, because it trains the operator to ignore the alarm.
func TestIPAddressFormatsSealIdentically(t *testing.T) {
	base := Event{
		Time:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ActorKind: ActorUser,
		Action:    ActionLoginSuccess,
		Result:    ResultOK,
		PrevHash:  GenesisHash,
	}

	write := base
	write.ActorIP = "127.0.0.1" // what the request handler has
	read := base
	read.ActorIP = "127.0.0.1/32" // what PostgreSQL hands back

	if err := write.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := read.Seal(); err != nil {
		t.Fatal(err)
	}
	if !equalBytes(write.Hash, read.Hash) {
		t.Fatal("the same address must seal to the same hash regardless of prefix notation")
	}

	// IPv6 too: PostgreSQL renders a bare IPv6 address with its prefix length as
	// well, and the two notations must converge.
	v6a, v6b := base, base
	v6a.ActorIP = "2001:db8::1"
	v6b.ActorIP = "2001:db8::1/128"
	if err := v6a.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := v6b.Seal(); err != nil {
		t.Fatal(err)
	}
	if !equalBytes(v6a.Hash, v6b.Hash) {
		t.Fatal("IPv6 must normalise the same way IPv4 does")
	}

	// A genuinely different address must not collide with either.
	other := base
	other.ActorIP = "127.0.0.2"
	if err := other.Seal(); err != nil {
		t.Fatal(err)
	}
	if equalBytes(other.Hash, write.Hash) {
		t.Fatal("different addresses must seal differently")
	}
}
