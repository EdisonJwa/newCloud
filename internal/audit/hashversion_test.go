package audit

// The versioned preimage (2026-10-24 review, M6).
//
// `request_id` and `auth_method` were stored in separate columns and never included in the hash, so
// a database writer could rewrite how an actor authenticated — session, app password or
// break-glass — and the chain would still verify. A field that is recorded but not covered is
// worse than an unrecorded one, because it reads as evidence.
//
// Versioning is what makes the fix possible without rewriting history: rows written before the
// change keep verifying under version 1, and new rows are sealed under version 2.

import (
	"bytes"
	"testing"
	"time"
)

// baseEvent is the shared fixture: everything the two versions agree on.
func baseEvent() Event {
	bytes := int64(42)
	return Event{
		Time:       time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		ActorKind:  "user",
		ActorEmail: "alice@example.test",
		ActorIP:    "203.0.113.9",
		UserAgent:  "test",
		Action:     ActionFileWrite,
		TargetKind: "node",
		TargetID:   "11111111-1111-1111-1111-111111111111",
		Result:     ResultOK,
		Bytes:      &bytes,
	}
}

// The two fields are invisible to a version-1 hash, which is the defect, and covered by a
// version-2 one, which is the fix.
func TestTheAuthMethodIsOutsideTheVersionOneHashAndInsideTheSecond(t *testing.T) {
	a := baseEvent()
	a.HashVersion = 1
	a.RequestID = "req-a"
	a.AuthMethod = "session"

	b := a
	b.RequestID = "req-b"
	b.AuthMethod = "breakglass"

	if err := a.Seal(); err != nil {
		t.Fatalf("seal a: %v", err)
	}
	if err := b.Seal(); err != nil {
		t.Fatalf("seal b: %v", err)
	}
	if !bytes.Equal(a.Hash, b.Hash) {
		t.Fatal("a version-1 hash changed when the request id and auth method changed; that would " +
			"mean the version-1 preimage has been altered, and every existing log would stop " +
			"verifying")
	}

	a.HashVersion = 2
	b.HashVersion = 2
	if err := a.Seal(); err != nil {
		t.Fatalf("seal a v2: %v", err)
	}
	if err := b.Seal(); err != nil {
		t.Fatalf("seal b v2: %v", err)
	}
	if bytes.Equal(a.Hash, b.Hash) {
		t.Fatal("a version-2 hash did not change when the auth method changed: session and " +
			"break-glass are interchangeable to a database writer again")
	}

	// And the same event under the two versions must differ, or the column would be decorative.
	c := baseEvent()
	c.RequestID, c.AuthMethod = "req-a", "session"
	c.HashVersion = 1
	if err := c.Seal(); err != nil {
		t.Fatalf("seal c v1: %v", err)
	}
	d := c
	d.HashVersion = 2
	if err := d.Seal(); err != nil {
		t.Fatalf("seal d v2: %v", err)
	}
	if bytes.Equal(c.Hash, d.Hash) {
		t.Fatal("version 1 and version 2 produced the same hash: the version is not reaching the " +
			"preimage, so a reader cannot tell which rules were in force")
	}
}

// A zero version means version 1: every row written before the column existed carries it, and
// treating it as "version 2" would invalidate the entire history on upgrade.
func TestAZeroVersionHashesAsVersionOne(t *testing.T) {
	zero := baseEvent()
	zero.RequestID, zero.AuthMethod = "req-a", "session"
	if err := zero.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	one := zero
	one.HashVersion = 1
	if err := one.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !bytes.Equal(zero.Hash, one.Hash) {
		t.Fatal("an event with no recorded version did not hash as version 1; an upgrade would " +
			"report every existing log as tampered with")
	}
}
