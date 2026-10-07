package store_test

// Pruning the audit log must not make the log look tampered with.
//
// The chain proves that no event was altered or removed. Deleting the oldest rows leaves a first
// survivor whose prev_hash points at an event that is gone, and every later verification reports a
// break — the same answer it gives for an attacker who deleted rows. So a prune records a boundary
// (the last removed event's id and hash) and verification starts there.
//
// These tests cover the store half: the mark is written in the same transaction as the deletion, a
// second prune continues from the first, and a prune over an ALREADY BROKEN chain is refused — because
// a mark written over tampered history would vouch for it.

import (
	"context"
	"testing"
	"time"

	"strings"

	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestPruneRecordsABoundaryThatVerificationAccepts(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()

	owner := newUser(t, pool)
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := 0; i < 12; i++ {
			if err := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: &owner, ActorEmail: "a@example.test",
				Action: "file.read", TargetKind: "node", TargetID: "x", Result: "ok",
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Everything was written "now"; a cutoff in the future selects all of it.
	cutoff := time.Now().UTC().Add(time.Hour)
	res, err := s.PruneAuditBefore(ctx, cutoff, "/tmp/archive.jsonl")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Mark.Events != 12 {
		t.Fatalf("mark says %d events were archived, want 12", res.Mark.Events)
	}
	if len(res.Mark.HeadHash) != 32 {
		t.Fatalf("mark carries a %d-byte head, want a 32-byte digest", len(res.Mark.HeadHash))
	}

	// The log is empty now, and "verify from the boundary" reports an intact (empty) segment rather
	// than a broken chain.
	report, err := auditverify.RunFrom(ctx, s, 100, res.Mark.ThroughID, res.Mark.HeadHash)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("verification from the recorded boundary failed: %v (broken at %d)",
			report.Err, report.BrokenAt)
	}

	// And the mark is what verify reads.
	mark, err := s.LatestPruneMark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mark.ThroughID != res.Mark.ThroughID || mark.ArchivePath != "/tmp/archive.jsonl" {
		t.Fatalf("the stored mark does not match the prune: %+v", mark)
	}
}

func TestAppendsAfterAPruneStillVerify(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()
	owner := newUser(t, pool)

	write := func(n int) {
		t.Helper()
		if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			for i := 0; i < n; i++ {
				if err := tx.Audit(ctx, store.AuditEntry{
					ActorKind: "user", ActorUserID: &owner, ActorEmail: "a@example.test",
					Action: "file.read", TargetKind: "node", TargetID: "x", Result: "ok",
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	write(5)
	res, err := s.PruneAuditBefore(ctx, time.Now().UTC().Add(time.Hour), "archive-1.jsonl")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	// New events, written AFTER the boundary, must link to the archived head: that is what makes
	// the mark a checkpoint rather than a fresh start.
	write(4)

	report, err := auditverify.RunFrom(ctx, s, 100, res.Mark.ThroughID, res.Mark.HeadHash)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("events appended after a prune do not verify against the boundary: %v", report.Err)
	}
	if report.Events != 4 {
		t.Fatalf("verified %d events after the boundary, want the 4 written since", report.Events)
	}

	// A second prune continues from the first; the marks are cumulative boundaries, not competing
	// claims.
	second, err := s.PruneAuditBefore(ctx, time.Now().UTC().Add(time.Hour), "archive-2.jsonl")
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if second.Mark.ThroughID <= res.Mark.ThroughID {
		t.Fatalf("the second boundary (%d) did not move past the first (%d)",
			second.Mark.ThroughID, res.Mark.ThroughID)
	}
	latest, err := s.LatestPruneMark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ArchivePath != "archive-2.jsonl" {
		t.Fatalf("LatestPruneMark returned the older boundary: %+v", latest)
	}
}

func TestPruneRefusesWhenTheChainIsAlreadyBroken(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()
	owner := newUser(t, pool)

	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for i := 0; i < 6; i++ {
			if err := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: &owner, ActorEmail: "a@example.test",
				Action: "file.read", TargetKind: "node", TargetID: "x", Result: "ok",
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Break the LINK at the boundary itself: the event that would carry the mark no longer matches
	// its predecessor. (A rewrite in the MIDDLE is caught by verifying before pruning, which the
	// command does; the store's check is the last line of defence for the specific link the mark
	// would vouch for.)
	if _, err := pool.Exec(ctx, `
		UPDATE audit.events SET prev_hash = decode(repeat('00', 32), 'hex')
		WHERE id = (SELECT max(id) FROM audit.events)`); err != nil {
		t.Fatal(err)
	}

	_, err := s.PruneAuditBefore(ctx, time.Now().UTC().Add(time.Hour), "x.jsonl")
	if err == nil {
		t.Fatal("a prune over a broken chain succeeded; the mark would vouch for tampered history")
	}
	if !strings.Contains(err.Error(), "already broken") {
		t.Fatalf("the refusal should say the chain is broken, got: %v", err)
	}
}
