package store_test

// M6, end to end: rewriting how an actor authenticated must break the chain.
//
// The fields were stored in their own columns and never hashed, so this tampering was invisible:
// verify passed with "session" changed to "break-glass", and break-glass is exactly the credential
// an incident review would look for. Version 2 of the preimage appends request_id and auth_method,
// and the verifier uses each row's recorded version, so old rows still verify under the old rules.

import (
	"context"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestTamperingWithTheAuthMethodBreaksTheChain(t *testing.T) {
	pool := testPool(t)
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()

	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorEmail: "m6@example.test",
			Action: "file.write", TargetKind: "node", TargetID: "m6-probe",
			Result: "ok", AuthMethod: "session",
		})
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	var id int64
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT id, hash_version FROM audit.events WHERE target_id = 'm6-probe' ORDER BY id DESC LIMIT 1`).
		Scan(&id, &version); err != nil {
		t.Fatalf("read the new event: %v", err)
	}
	if version < 2 {
		t.Fatalf("the new event was sealed under version %d; new rows must be written under the "+
			"version that covers the auth method", version)
	}

	if err := s.VerifyAuditChain(ctx, 0); err != nil {
		t.Fatalf("the chain does not verify before tampering: %v", err)
	}

	// A database writer rewrites how the actor authenticated. This is the attack M6 describes.
	if _, err := pool.Exec(ctx,
		`UPDATE audit.events SET auth_method = 'breakglass' WHERE id = $1`, id); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	t.Cleanup(func() {
		// Put the row back so this test cannot break its neighbours.
		_, _ = pool.Exec(context.Background(),
			`UPDATE audit.events SET auth_method = $2 WHERE id = $1`, id, "session")
	})

	if err := s.VerifyAuditChain(ctx, 0); err == nil {
		t.Fatal("the chain still verified after the auth method was rewritten: the field is " +
			"recorded but not covered, which is the M6 defect")
	}
}
