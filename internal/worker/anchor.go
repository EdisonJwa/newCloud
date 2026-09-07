package worker

// Daily audit anchor (INV-09).
//
// The hash chain proves the log is internally consistent; it cannot prove the log is
// complete. Someone with database access can delete the newest rows and the remaining
// chain still verifies perfectly. Publishing the head somewhere the application cannot
// write closes that, because a truncated chain no longer matches what was published.
//
// Until now audit.Anchor existed as an interface with no implementation and
// RecordAnchor was never called, so the project claimed tamper-evidence it did not have
// in production.

import (
	"context"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// anchorOnce publishes a checkpoint for yesterday if one is missing.
//
// Yesterday rather than today, and idempotent rather than unconditional: a checkpoint is
// only meaningful once the day it covers has closed, and a worker that restarts (or runs
// on several replicas) must not overwrite the same day's record with a different head.
func (w *Worker) anchorOnce(ctx context.Context) error {
	if w.anchor == nil {
		return nil
	}

	// Anchor the most recently completed day.
	day := time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)

	// Skip PUBLISHING if it is already published: rewriting it would erase the very record that
	// makes a later rewrite detectable.
	//
	// Only publishing is skipped. This used to `return nil` here, which also skipped the
	// verification below — so a worker that started after the day's checkpoint existed never
	// compared it against the log at all, and a running one checked at most once per day. An
	// anchor that is written but never compared proves nothing, and that was the state for
	// twenty-three hours out of twenty-four.
	existing, err := w.anchor.Checkpoints()
	if err != nil {
		return err
	}
	published := false
	for _, cp := range existing {
		if cp.Day.Equal(day) {
			published = true
			break
		}
	}
	if !published {
		return w.publishCheckpoint(ctx, day)
	}
	// Nothing to publish; verify what is there.
	w.verifyAndRecord(ctx)
	return nil
}

// publishCheckpoint writes one day's checkpoint and verifies the result.
func (w *Worker) publishCheckpoint(ctx context.Context, day time.Time) error {

	head, count, err := w.chain.Head()
	if err != nil {
		return err
	}
	if w.metrics != nil {
		// The head's age is the signal that matters operationally: a chain whose newest
		// event is days old means nothing is being written, which is either an idle
		// deployment or a broken audit path.
		w.metrics.SetAudit(count, 0)
	}

	// Publishing the *current* head for *yesterday's* date is slightly too inclusive:
	// it covers everything up to now, not up to midnight. That is the safe direction —
	// the checkpoint can only over-claim slightly, never under-claim — and it avoids
	// needing a "head as of midnight" query the store does not offer.
	if err := w.anchor.PublishWithCount(day, head, count); err != nil {
		return err
	}

	w.log.Info("audit chain anchored",
		"day", day.Format("2006-01-02"),
		"head", auditverify.Digest(head),
		"events", count)

	// Verify immediately after publishing: a checkpoint nobody compared is a write-only ritual.
	w.verifyAndRecord(ctx)
	return nil
}

// verifyAndRecord compares every published checkpoint against the chain and records the verdict.
//
// Called on EVERY tick, not only when a checkpoint was published. Verification is the part that
// turns an anchor into evidence, and tying it to the once-a-day publish meant a restarted worker
// never verified at all and a running one verified once a day — so tampering could go unnoticed for
// twenty-three hours, which is most of the value of the check gone.
//
// WHAT THIS DOES NOT DETECT. This compares published checkpoints against the chain: a deleted or
// truncated tail, a missing checkpoint in the middle, a broken link, a head that moved. It does NOT
// recompute each event's hash, so a REWRITE of an event's fields — an action changed from
// "file.delete" to "file.read", with the stored hash column left alone — passes here.
//
// That walk exists as auditverify.Run, and `newcloud audit verify` runs it. It is not on this tick
// because it reads every event in the table, which is not something a background timer should do on
// a deployment with millions of them. The split is deliberate: the cheap structural check runs
// continuously, the expensive full verification is an operator action. It is stated here because a
// reader would otherwise assume "verified" means every row was checked, and a security check that
// overstates its coverage is worse than one that is honestly narrow.
func (w *Worker) verifyAndRecord(ctx context.Context) {
	result, verr := w.VerifyAnchor()
	if verr != nil {
		w.log.Error("anchor verification failed to run", "error", verr)
		w.recordVerifyFailure(ctx, "runner_error", verr.Error(), nil)
	} else if !result.OK() {
		// This is the loudest thing this process can say: the published record and the
		// log disagree, which means events were added, removed or rewritten.
		w.log.Error("AUDIT ANCHOR MISMATCH: the chain does not match what was published",
			"problems", result.Problems)
		w.recordVerifyFailure(ctx, "mismatch", "", result.Problems)
	}
	if w.metrics != nil {
		w.metrics.ObserveAuditVerify(verr == nil && result.OK())
	}
}

// recordVerifyFailure writes the failure into the audit log itself (§10.1 审计校验失败).
//
// Two failure kinds are distinguished because they call for different investigations. A mismatch
// means the published record and the log disagree — events were added, removed or rewritten, which
// is the tampering case. A runner_error means the check could not be performed at all (an
// unreadable anchor directory, say), which is an operational fault and, on its own, says nothing
// about the log's integrity. Recording both under one reason code would make "we could not check"
// look like "we found something".
//
// The write is best-effort and never returns an error to the caller: this runs inside the anchor
// step, and a failure to record a failure must not stop the anchoring that follows. It is logged
// when it fails, which is the only remaining channel.
func (w *Worker) recordVerifyFailure(ctx context.Context, reason, detail string, problems []string) {
	err := w.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		meta := map[string]any{"reason": reason}
		if detail != "" {
			meta["error"] = detail
		}
		if len(problems) > 0 {
			// Bounded: a tampered log can produce a long problem list, and an audit row that is
			// megabytes wide is one nobody can read or index.
			if len(problems) > 10 {
				problems = append(problems[:10], "…")
			}
			meta["problems"] = problems
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: string(audit.ActionAuditVerifyFailure),
			TargetKind: "audit", TargetID: "chain",
			Result: string(audit.ResultError), Metadata: meta,
		})
	})
	if err != nil {
		w.log.Error("could not record an audit verification failure in the audit log",
			"error", err, "reason", reason)
	}
}

// EnableAnchoring wires the anchor and the chain it verifies.
//
// Separate from New so a deployment that has not configured an anchor directory simply
// does not call it, rather than having to pass nil-nil and hope. When it is not called,
// audit anchoring does not happen - and the startup log says so, because that is a
// decision an operator should see rather than infer.
func (w *Worker) EnableAnchoring(dir string, src auditverify.RowSource) error {
	a, err := auditverify.NewFileAnchor(dir)
	if err != nil {
		return err
	}
	w.anchor = a
	w.chain = auditverify.NewStoreChain(src)
	return nil
}

// AnchorNow publishes the pending checkpoint immediately.
//
// Exposed so the admin surface and the tests can force the step rather than waiting for
// the ticker.
func (w *Worker) AnchorNow(ctx context.Context) error { return w.anchorOnce(ctx) }

// VerifyAnchor checks every published checkpoint against the chain.
//
// This is the operation that turns the anchor from a write-only ritual into evidence:
// an anchor nobody ever compares against the log proves nothing.
func (w *Worker) VerifyAnchor() (auditverify.VerifyResult, error) {
	if w.anchor == nil || w.chain == nil {
		return auditverify.VerifyResult{}, nil
	}
	return auditverify.Verify(w.anchor, w.chain)
}
