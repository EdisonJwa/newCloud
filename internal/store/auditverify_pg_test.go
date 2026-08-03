package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestPGAuditVerifyDetectsTampering is T-15 against the real database.
//
// The verifier is only worth having if it actually reports a rewritten row, and the
// only way to know that is to rewrite one. This test does exactly what an attacker
// with a database connection would do — change a column without recomputing the
// hash — and asserts the verifier refuses to bless it.
func TestPGAuditVerifyDetectsTampering(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}

	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.EnsureRoot(ctx, owner); err != nil {
			return err
		}
		for i := 0; i < 6; i++ {
			if err := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", ActorUserID: &owner, ActorEmail: "a@example.test",
				// The address is set deliberately: it round-trips through an `inet`
				// column, and omitting it here is what let a write/read hash
				// asymmetry survive every test while breaking verification in
				// production.
				ActorIP: "127.0.0.1", UserAgent: "test/1.0",
				Action: "file.write", TargetKind: "node", TargetID: uuid.New().String(),
				Result: "ok", Metadata: map[string]any{"i": i},
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// An intact chain verifies.
	//
	// This assertion carries more weight than the tamper case below: it is what
	// catches a write/read asymmetry such as PostgreSQL's `inet` prefix notation,
	// where the writer and the verifier hash different bytes for the same row and
	// every event looks tampered-with. The events written here carry a real actor
	// address and user agent for exactly that reason.
	report, err := auditverify.Run(context.Background(), s, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("freshly written chain must verify: %v", report.Err)
	}
	if report.Events != 6 {
		t.Fatalf("want 6 events, got %d", report.Events)
	}

	// Rewrite one row's payload without touching its hash. This is the attack the
	// chain exists to make visible, and it needs the migration role — which is why
	// the application must not run as that role (see deploy/).
	if _, err := pool.Exec(context.Background(),
		`UPDATE audit.events SET metadata = '{"i": 999}'::jsonb
		 WHERE id = (SELECT min(id) + 2 FROM audit.events)`); err != nil {
		t.Fatal(err)
	}

	report, err = auditverify.Run(context.Background(), s, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("a rewritten audit row must be reported as broken")
	}
}

// TestPGAuditVerifyDetectsTruncation covers the deletion shape that per-row
// verification alone cannot see: removing the tail leaves a chain that still links
// correctly, so the head must be compared against the newest row.
func TestPGAuditVerifyDetectsTruncation(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}

	owner := newUser(t, pool)
	s := store.NewPG(pool)
	s.EnableAudit()
	if err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.EnsureRoot(ctx, owner); err != nil {
			return err
		}
		for i := 0; i < 4; i++ {
			if err := tx.Audit(ctx, store.AuditEntry{
				ActorKind: "user", Action: "file.read", TargetKind: "node",
				TargetID: uuid.New().String(), Result: "ok",
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	head, err := s.AuditHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Delete the newest two rows. What remains is a valid prefix of the chain.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM audit.events WHERE id IN
			(SELECT id FROM audit.events ORDER BY id DESC LIMIT 2)`); err != nil {
		t.Fatal(err)
	}

	report, err := auditverify.Run(context.Background(), s, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The chain prefix is intact, so this is only detectable by noticing that the
	// head moved backwards.
	if report.OK() && string(report.Head) == string(head) {
		t.Fatal("truncation must be visible in the verified head")
	}
}
