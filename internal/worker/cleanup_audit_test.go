package worker_test

// §8.1: 对清理任务提供审计与可重复执行语义 — cleanup tasks must be auditable and safely repeatable.
//
// The destructive cleanups deleted user data and wrote nothing to the audit log. A file removed by
// the trash sweep, a version discarded by retention, and blobs reclaimed by the collector all left
// the audit table untouched, so the one place designed to answer "what happened to this file"
// could not answer it.
//
// These cases assert the auditable half. The repeatable half — that a second run is a no-op — is
// asserted in the same tests, because "auditable" is worthless if the audit says something
// different from what the second run did.

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newAuditUser creates an account directly, since the memory store's AddUser helper does not exist
// for PostgreSQL and the worker tests do not share the store package's fixtures.
func newAuditUser(t *testing.T, s store.Store) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "worker-" + uuid.New().String(),
			Username: "worker-" + uuid.New().String()[:8],
			Email:    "worker-" + uuid.New().String()[:8] + "@example.test",
			Role:     "user",
		})
		if err != nil {
			return err
		}
		id = u.ID
		return nil
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// newAuditBlob inserts a blob row and returns its id.
func newAuditBlob(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var sum [32]byte
	if _, err := rand.Read(sum[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO blobs (id, sha256, size, backend) VALUES ($1, $2, 0, 'local')`,
		id, sum[:]); err != nil {
		t.Fatalf("create blob: %v", err)
	}
	return id
}

// auditRowsFor counts audit events with one action.
func auditRowsFor(t *testing.T, s store.Store, action audit.Action) int {
	t.Helper()
	rows, err := s.AuditRows(context.Background(), 0, 10000)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	n := 0
	for _, e := range rows {
		if string(e.Action) == string(action) {
			n++
		}
	}
	return n
}

// TestPurgeTrashIsAuditedAndRepeatable is the trash half.
func TestPurgeTrashIsAuditedAndRepeatable(t *testing.T) {
	pool := walTestPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	// A trashed file old enough for a 30-day retention to take it.
	owner := newAuditUser(t, s)
	var root, fileID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner)
		if err != nil {
			return err
		}
		root = n.ID
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root, Name: "old.bin", Size: 16,
			Mime: "application/octet-stream", ETag: "o", BlobID: newAuditBlob(t, pool),
		})
		if err != nil {
			return err
		}
		fileID = f.ID
		return tx.Trash(ctx, fileID)
	}); err != nil {
		t.Fatal(err)
	}
	// Backdate so the retention window has passed. done through the table because the store has no
	// way to move its clock.
	if _, err := pool.Exec(ctx,
		`UPDATE nodes SET trashed_at = now() - interval '40 days' WHERE id = $1`, fileID); err != nil {
		t.Fatal(err)
	}

	w := worker.New(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithRetention(0, 30*24*time.Hour)

	before := auditRowsFor(t, s, audit.ActionTrashPurge)
	res, err := w.PurgeTrash(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.Nodes == 0 {
		t.Fatal("setup: nothing was purged, so this test would prove nothing")
	}

	after := auditRowsFor(t, s, audit.ActionTrashPurge)
	if after != before+1 {
		t.Fatalf("the purge wrote %d audit rows, want exactly 1. A file deleted by the retention "+
			"sweep left no record, so \"the file vanished on Thursday\" had no answer in the audit "+
			"log.", after-before)
	}

	// Repeatable: a second run finds nothing and must add no second row. An audit log that grows a
	// "deleted 0 items" entry every few minutes is one that operators stop reading.
	again, err := w.PurgeTrash(ctx)
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if again.Nodes != 0 {
		t.Fatalf("the second purge deleted %d nodes, so it is not idempotent", again.Nodes)
	}
	if got := auditRowsFor(t, s, audit.ActionTrashPurge); got != after {
		t.Fatalf("a purge that deleted nothing still wrote an audit row (%d -> %d)", after, got)
	}
}

// TestExpireVersionsIsAuditedAndRepeatable is the version-history half.
func TestExpireVersionsIsAuditedAndRepeatable(t *testing.T) {
	pool := walTestPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	s.EnableAudit()

	owner := newAuditUser(t, s)
	var fileID uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		root, err := tx.EnsureRoot(ctx, owner)
		if err != nil {
			return err
		}
		f, err := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: "versioned.bin", Size: 4,
			Mime: "application/octet-stream", ETag: "v1", BlobID: newAuditBlob(t, pool),
		})
		if err != nil {
			return err
		}
		fileID = f.ID
		_, uerr := tx.UpdateFile(ctx, store.UpdateFile{
			NodeID: f.ID, Size: 5, BlobID: newAuditBlob(t, pool), ETag: "v2", KeepVersion: true,
		})
		return uerr
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE versions SET created_at = now() - interval '400 days' WHERE node_id = $1`,
		fileID); err != nil {
		t.Fatal(err)
	}

	w := worker.New(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithRetention(90*24*time.Hour, 0)

	before := auditRowsFor(t, s, audit.ActionVersionExpire)
	n, err := w.ExpireVersions(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n == 0 {
		t.Fatal("setup: no version expired, so this test would prove nothing")
	}

	after := auditRowsFor(t, s, audit.ActionVersionExpire)
	if after != before+1 {
		t.Fatalf("version expiry wrote %d audit rows, want exactly 1. A version discarded by "+
			"retention is unrecoverable, and \"I could restore this last week\" is answered by "+
			"this event or by nothing.", after-before)
	}

	// Repeatable, and quiet when it does nothing.
	again, err := w.ExpireVersions(ctx)
	if err != nil {
		t.Fatalf("second expire: %v", err)
	}
	if again != 0 {
		t.Fatalf("the second expiry removed %d versions, so it is not idempotent", again)
	}
	if got := auditRowsFor(t, s, audit.ActionVersionExpire); got != after {
		t.Fatalf("an expiry that removed nothing still wrote an audit row (%d -> %d)", after, got)
	}
}
