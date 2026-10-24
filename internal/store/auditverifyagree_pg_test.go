package store_test

// Does a chain that spans the audit hash-version change verify on BOTH verification paths?
//
// A note on what this test can and cannot catch. It seals the legacy row at runtime, with the same
// code that later verifies it, so it is blind to a change in the hashing itself: mutate canonical()
// and both sides move together. What it does catch is the thing that actually went wrong once — the
// two verifiers reading the same rows differently, one honouring the row's hash_version and the
// other not. That was a one-off CLI failure that has not reproduced since, and the honest reading is
// that the earlier hand-built fixture, not the product, was at fault.
//
// The store has two: `audit.VerifyChain` (reached through VerifyAuditChain) and `auditverify.Run`,
// which is what `newcloud audit verify` and the prune command use. They share the preimage but read
// events separately, and the previous round found them disagreeing on a hand-built legacy row
// without being able to say which one was wrong. This drives both over the same rows and reports
// which fails, so the answer stops being a guess.

import (
	"context"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestBothVerifiersAgreeOnAChainSpanningTheHashVersion(t *testing.T) {
	pool := testPool(t)
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DELETE FROM audit.events`); err != nil {
		t.Fatalf("clear events: %v", err)
	}
	// The audit table is shared with every other test in this package, and a leftover row from this
	// one is not inert: the next test that appends events finds a chain starting from a hash it did
	// not write, and fails for a reason that has nothing to do with what it is testing. That is
	// exactly what happened to TestTamperingWithTheAuthMethodBreaksTheChain.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM audit.events`); err != nil {
			t.Errorf("clean up audit events: %v", err)
		}
	})

	// A legacy event, sealed with the pre-versioning preimage and stored the way the old writer
	// stored it: NULL where the writer nullified empty values.
	legacy := audit.Event{
		Time:        time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		ActorKind:   "user",
		ActorEmail:  "legacy@example.test",
		Action:      "file.write",
		TargetKind:  "node",
		TargetID:    "legacy-event",
		Result:      "ok",
		PrevHash:    audit.GenesisHash,
		HashVersion: 1,
	}
	if err := legacy.Seal(); err != nil {
		t.Fatalf("seal legacy: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit.events (ts, actor_kind, actor_email, user_agent, action, target_kind,
		    target_id, result, metadata, prev_hash, hash, hash_version)
		VALUES ($1, 'user', 'legacy@example.test', '', 'file.write', 'node', 'legacy-event', 'ok',
		    '{}'::jsonb, $2, $3, 1)`,
		legacy.Time, legacy.PrevHash, legacy.Hash); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}

	// A current event, which chains onto it under version 2.
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorEmail: "current@example.test",
			Action: "file.write", TargetKind: "node", TargetID: "current-event", Result: "ok",
		})
	}); err != nil {
		t.Fatalf("append current: %v", err)
	}

	// What each path sees for the legacy row. If the two disagree here, the difference is in reading,
	// not in the preimage.
	rows, err := s.AuditRows(ctx, 0, 100)
	if err != nil {
		t.Fatalf("AuditRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2", len(rows))
	}
	t.Logf("legacy row as read: id=%d hash_version=%d email=%q ip=%q bytes=%v metadata=%v",
		rows[0].ID, rows[0].HashVersion, rows[0].ActorEmail, rows[0].ActorIP,
		rows[0].Bytes, rows[0].Metadata)

	storeErr := s.VerifyAuditChain(ctx, 0)

	report, runErr := auditverify.Run(ctx, s, 100)
	if runErr != nil {
		t.Fatalf("auditverify.Run: %v", runErr)
	}

	t.Logf("store path: %v | auditverify path: ok=%v broken=%d err=%v",
		storeErr, report.OK(), report.BrokenAt, report.Err)

	if (storeErr == nil) != report.OK() {
		t.Fatalf("the two verification paths disagree on the same rows: store=%v auditverify=%v "+
			"(broken at %d). Only one of them is what an operator runs.", storeErr, report.Err,
			report.BrokenAt)
	}
	if storeErr != nil {
		t.Fatalf("a chain with one version-1 and one version-2 event does not verify: %v", storeErr)
	}
}
