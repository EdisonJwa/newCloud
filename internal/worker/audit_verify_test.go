package worker_test

// §10.1: 审计校验失败 — an audit verification failure must itself be an audit event.
//
// An anchor mismatch is the loudest signal this system can produce: the published checkpoint and
// the log disagree, so events were added, removed or rewritten. It was reported as a LOG LINE, which
// is the one place such a record is worthless — whoever can edit the log can edit the line saying the
// log was edited. The chain is the tamper-evident artifact, so the failure has to go into the chain.
//
// This test tampers with a real PostgreSQL chain rather than simulating the verdict, because the
// question is whether the plumbing reports what the verifier actually decided.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

// TestAnchorMismatchIsRecordedInTheAuditLog is the mismatch case.
func TestAnchorMismatchIsRecordedInTheAuditLog(t *testing.T) {
	pool := walTestPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	// Some real events to form a chain.
	for i := 0; i < 3; i++ {
		if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "system", Action: "test.seed", TargetKind: "test", TargetID: "x",
				Result: "ok",
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	w := worker.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := t.TempDir()
	if err := w.EnableAnchoring(dir, s); err != nil {
		t.Fatalf("enable anchoring: %v", err)
	}

	// The audit table is shared and append-only, so counts are compared as DELTAS: an absolute
	// count picks up every failure any earlier run of this test recorded, which is how the first
	// version reported "an honest chain produced 3 verification failures".
	before := auditRowsFor(t, s, audit.ActionAuditVerifyFailure)

	// Publish a checkpoint over the honest chain.
	if err := w.AnchorNow(ctx); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if failed := auditRowsFor(t, s, audit.ActionAuditVerifyFailure); failed != before {
		t.Fatalf("an honest chain produced %d new verification failures", failed-before)
	}

	// Tamper by DELETING an event, which is the class the checkpoint comparison detects: the
	// published count now exceeds what the chain holds, and the head no longer matches.
	//
	// It is deliberately not a content rewrite. A rewrite that leaves the stored hash column alone
	// is caught only by recomputing every hash — `auditverify.Run`, which the `newcloud audit
	// verify` command runs — and the worker does not do that walk on its scheduled tick. Choosing
	// an undetectable tamper here would have produced a test asserting a behaviour the system does
	// not have; the split is honest and is stated in verifyAndRecord.
	if _, err := pool.Exec(ctx,
		`DELETE FROM audit.events WHERE id = (SELECT max(id) FROM audit.events)`); err != nil {
		t.Fatal(err)
	}

	// A second pass publishes today's checkpoint again (a new day boundary is not needed: the
	// mismatch is against the EARLIER published one, which is exactly the comparison that catches
	// a rewrite).
	_ = w.AnchorNow(ctx)

	if failed := auditRowsFor(t, s, audit.ActionAuditVerifyFailure); failed <= before {
		t.Fatal("a tampered audit chain produced no audit.verify.failure event. §10.1 requires " +
			"审计校验失败 to be recorded; a log line is the one place that record is worthless, " +
			"because whoever rewrote the log can rewrite the line about it.")
	}
}

// TestAnchorMismatchNamesTheReason keeps the two failure kinds distinguishable.
//
// "The check could not run" and "the check ran and found tampering" are different events with
// different responses, and collapsing them under one reason makes an operational fault look like a
// security incident.
func TestAnchorMismatchNamesTheReason(t *testing.T) {
	pool := walTestPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: "test.reason", TargetKind: "test", TargetID: "y",
			Result: "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}

	w := worker.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.EnableAnchoring(t.TempDir(), s); err != nil {
		t.Fatal(err)
	}
	if err := w.AnchorNow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM audit.events WHERE id = (SELECT max(id) FROM audit.events)`); err != nil {
		t.Fatal(err)
	}
	_ = w.AnchorNow(ctx)

	rows, err := s.AuditRows(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range rows {
		if string(e.Action) != string(audit.ActionAuditVerifyFailure) {
			continue
		}
		found = true
		reason, _ := e.Metadata["reason"].(string)
		if reason != "mismatch" {
			t.Fatalf("the failure records reason %q, want \"mismatch\". A runner error means the "+
				"check could not run; a mismatch means it ran and found tampering. Reporting one "+
				"as the other turns an operational fault into a security incident, or hides a real "+
				"one.", reason)
		}
		// The evidence has to travel with the event, or the row says "something was wrong" and
		// leaves the operator to reconstruct it by hand.
		if _, ok := e.Metadata["problems"]; !ok {
			t.Fatal("the failure does not carry the problems the verifier reported")
		}
	}
	if !found {
		t.Fatal("no verification-failure event was recorded")
	}
}
