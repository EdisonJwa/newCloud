package store_test

// ReadAudit — the query behind the audit browse and export — had no test of its own.
//
// The server-side export tests all run against the in-memory backend, so the PostgreSQL SELECT and its
// Scan ran only in production. That is how a column added to the scan and not to the SELECT reached a
// live server as "number of field descriptions must equal number of destinations": the browse endpoint
// answered 500 for every request, and nothing in the suite noticed.
//
// This test writes through the writer and reads through the reader, on the backend that actually runs
// in production, and checks the fields a person would notice missing.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestReadAuditReturnsWhatTheWriterStored(t *testing.T) {
	pool := testPool(t)
	if err := resetSchema(t, pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPG(pool)
	s.EnableAudit()
	ctx := context.Background()
	owner := newUser(t, pool)

	shareID := uuid.New() // any uuid; the column is a plain reference in this test
	size := int64(4096)

	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &owner, ActorEmail: "reader@example.test",
			ActorIP: "203.0.113.7", UserAgent: "Mozilla/5.0 test",
			Action: "file.download", TargetKind: "node", TargetID: "n-1",
			ShareID: &shareID, Result: "ok", Bytes: &size,
			Metadata:   map[string]any{"path": "/reports/q1.pdf"},
			RequestID:  "req-abc-123",
			AuthMethod: "app_password",
		})
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	rows, err := s.ReadAudit(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read %d rows, want 1", len(rows))
	}
	r := rows[0]

	if r.Action != "file.download" || r.Result != "ok" {
		t.Fatalf("action/result round-trip: %q/%q", r.Action, r.Result)
	}
	if r.ActorEmail != "reader@example.test" || r.UserAgent != "Mozilla/5.0 test" {
		t.Fatalf("actor fields round-trip: %+v", r)
	}
	// PostgreSQL's inet column renders a bare address with its prefix length. That is expected here
	// — normaliseIP is what makes both sides hash the same — so the raw column is compared in that
	// form rather than against the string that went in.
	if r.ActorIP != "203.0.113.7/32" {
		t.Fatalf("actor ip round-trip: %q", r.ActorIP)
	}
	if r.ActorUserID == nil || *r.ActorUserID != owner {
		t.Fatalf("actor id did not round-trip: %v", r.ActorUserID)
	}
	if r.ShareID == nil || *r.ShareID != shareID {
		t.Fatalf("share id did not round-trip: %v", r.ShareID)
	}
	if r.Bytes == nil || *r.Bytes != size {
		t.Fatalf("bytes did not round-trip: %v", r.Bytes)
	}
	if got := r.Metadata["path"]; got != "/reports/q1.pdf" {
		t.Fatalf("metadata did not round-trip: %v", r.Metadata)
	}
	// The two columns added for §10.1. When one is added to the Scan and not to the SELECT, this
	// line is the difference between a failing test and a 500 on every browse request.
	if r.RequestID != "req-abc-123" {
		t.Fatalf("request id did not round-trip: %q", r.RequestID)
	}
	if r.AuthMethod != "app_password" {
		t.Fatalf("auth method did not round-trip: %q", r.AuthMethod)
	}
	if len(r.Hash) != 32 || len(r.PrevHash) != 32 {
		t.Fatalf("hashes are %d and %d bytes", len(r.Hash), len(r.PrevHash))
	}
	if time.Since(r.Time) > time.Hour {
		t.Fatalf("timestamp is implausible: %v", r.Time)
	}
}
