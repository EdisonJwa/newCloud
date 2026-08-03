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

	"github.com/EdisonJwa/newCloud/internal/auditverify"
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

	// Skip if it is already published: rewriting it would erase the very record that
	// makes a later rewrite detectable.
	existing, err := w.anchor.Checkpoints()
	if err != nil {
		return err
	}
	for _, cp := range existing {
		if cp.Day.Equal(day) {
			return nil
		}
	}

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

	// Verify immediately after publishing. An anchor that is written but never compared
	// against the log proves nothing, so the check runs on the same schedule as the write.
	result, verr := w.VerifyAnchor()
	if verr != nil {
		w.log.Error("anchor verification failed to run", "error", verr)
	} else if !result.OK() {
		// This is the loudest thing this process can say: the published record and the
		// log disagree, which means events were added, removed or rewritten.
		w.log.Error("AUDIT ANCHOR MISMATCH: the chain does not match what was published",
			"problems", result.Problems)
	}
	if w.metrics != nil {
		w.metrics.ObserveAuditVerify(verr == nil && result.OK())
	}

	w.log.Info("audit chain anchored",
		"day", day.Format("2006-01-02"),
		"head", auditverify.Digest(head),
		"events", count)
	return nil
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
