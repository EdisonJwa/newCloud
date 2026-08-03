// Package auditverify implements the independent audit-chain verifier.
//
// The requirement (§10.2) is blunt about why this has to exist as a command rather
// than as a function nobody calls: "a tamper-evident log that is never verified is
// just a log". Before this, `audit.VerifyChain` and `store.VerifyAuditChain` existed
// and were referenced only by tests, so the chain's integrity was an untested
// claim in production.
//
// It lives in its own package so the operator-facing output and the checking logic
// can be tested without a database, and so `newcloud audit verify` is a thin
// wrapper over something with unit tests.
package auditverify

import (
	"context"
	"fmt"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// Reader is the storage dependency: the rows of the chain, oldest first.
//
// It is deliberately not the whole store. A verifier that can also write is one
// compromised code path away from repairing a chain that should be reported broken.
type Reader interface {
	// AuditRows returns up to limit events starting after the given sequence
	// position, oldest first. A nil cursor starts at the beginning.
	AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error)
	// AuditHead returns the newest chain hash, or the genesis value when the log is
	// empty.
	AuditHead(ctx context.Context) ([]byte, error)
}

// Report is the outcome of a verification run.
type Report struct {
	// Events is how many rows were checked.
	Events int
	// Head is the hash of the last verified event, which an auditor compares
	// against the externally published anchor.
	Head []byte
	// FirstEvent and LastEvent bound the window that was checked, so a partial run
	// is not mistaken for a complete one.
	FirstEvent *time.Time
	LastEvent  *time.Time
	// BrokenAt is the sequence position of the first event that failed, or 0 when
	// the chain is intact.
	BrokenAt int64
	// Err is the failure reason, if any.
	Err error
}

// OK reports whether the chain verified.
func (r Report) OK() bool { return r.Err == nil }

// Run verifies the chain in batches.
//
// The whole log is never loaded: a year of events is large enough that holding it in
// memory would make the verifier itself an outage risk, and the batch boundary does
// not weaken the check because each event commits to its predecessor's hash.
func Run(ctx context.Context, r Reader, batch int) (Report, error) {
	if batch <= 0 {
		batch = 1000
	}

	var (
		after   int64
		prev    = audit.GenesisHash
		report  = Report{}
		checked int
	)

	for {
		events, err := r.AuditRows(ctx, after, batch)
		if err != nil {
			return report, fmt.Errorf("auditverify: read events: %w", err)
		}
		if len(events) == 0 {
			break
		}
		for i := range events {
			e := &events[i]
			// The chain is verified by recomputing each hash from its canonical
			// form and comparing, not by trusting the stored one.
			if !bytesEqual(e.PrevHash, prev) {
				report.Events = checked
				report.BrokenAt = e.ID
				report.Err = fmt.Errorf("prev_hash does not match the previous event (event %d)", e.ID)
				return report, nil
			}
			stored := e.Hash
			if err := e.Seal(); err != nil {
				report.BrokenAt = e.ID
				report.Err = fmt.Errorf("canonicalise event %d: %w", e.ID, err)
				return report, nil
			}
			if !bytesEqual(stored, e.Hash) {
				report.Events = checked
				report.BrokenAt = e.ID
				report.Err = fmt.Errorf("hash mismatch: the record was modified (event %d)", e.ID)
				return report, nil
			}
			prev = stored
			checked++

			if report.FirstEvent == nil {
				t := e.Time
				report.FirstEvent = &t
			}
			if e.ID > after {
				after = e.ID
			}
			t := e.Time
			report.LastEvent = &t
		}
	}

	report.Events = checked
	report.Head = prev

	// The head must agree with what the table says is newest. A truncation removes
	// the tail rather than rewriting a row, so no per-event check catches it: the
	// chain still verifies from genesis to wherever it was cut.
	head, err := r.AuditHead(ctx)
	if err != nil {
		return report, fmt.Errorf("auditverify: read head: %w", err)
	}
	if !bytesEqual(head, prev) {
		report.BrokenAt = after
		report.Err = fmt.Errorf("chain head mismatch: the log has been truncated or rows were removed")
	}
	return report, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
