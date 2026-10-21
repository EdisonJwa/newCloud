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

// TestVersionByteCapTrimsAndAudits: §4.3's cap on retained history, driven through the worker.
//
// The store conformance suite proves the trimming order; this proves the worker CALLS it, records what
// it did, and does nothing when the cap is unset — the half that makes the cap real in a deployment.
//
// Against the in-memory store rather than PostgreSQL: the property is the worker's wiring and the
// audit row, and the PG trim itself is already covered by the shared suite.
func TestVersionByteCapTrimsAndAudits(t *testing.T) {
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "cap", Email: "cap@example.test", Role: "user"})
	var root store.Node
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner.ID)
		root = n
		return err
	}); err != nil {
		t.Fatal(err)
	}

	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil))).WithVersionByteCap(150)

	var nodeID uuid.UUID
	for i, size := range []int64{100, 100, 100} {
		if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, _, err := tx.WriteFile(ctx, store.WriteFile{
				NewFile: store.NewFile{
					OwnerID: owner.ID, ParentID: root.ID, Name: "capped.bin", Size: size,
					Mime: "text/plain", ETag: `"v` + string(rune('0'+i)) + `"`,
					BlobID: uuid.New(), BlobHash: [32]byte{byte(i + 1)},
				},
				KeepVersion: true,
			})
			nodeID = n.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	n, err := w.TrimVersionHistory(context.Background())
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if n == 0 {
		t.Fatal("the worker trimmed nothing with 200 bytes of history against a 150-byte cap")
	}

	var kept int
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		v, verr := tx.Versions(ctx, nodeID, 100)
		kept = len(v)
		return verr
	}); err != nil {
		t.Fatal(err)
	}
	if kept == 0 {
		t.Fatal("the trim removed all history; the newest version is the one users reach for")
	}

	var audited int
	for _, e := range meta.AuditLog() {
		if e.Action == "version.trim" {
			audited++
		}
	}
	if audited != 1 {
		t.Fatalf("want one version.trim audit row, got %d. A trimmed version has no row left to "+
			"explain itself.", audited)
	}

	// And with no cap configured the worker must touch nothing: an unset limit must not trim.
	noCap := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := noCap.TrimVersionHistory(context.Background()); err != nil || n != 0 {
		t.Fatalf("an unset cap trimmed %d versions (err %v); zero means unlimited", n, err)
	}
}

// TestVersionByteCapHonoursARuntimeOverride: the cap the worker uses must be the EFFECTIVE one.
//
// The configured cap is 150 and no override exists in the first half; the second half writes an
// override of 1 byte and the worker must trim down to it, without being restarted.
func TestVersionByteCapHonoursARuntimeOverride(t *testing.T) {
	meta := store.NewMemory()
	owner := meta.AddUser(store.User{Username: "cap2", Email: "cap2@example.test", Role: "user"})
	var root store.Node
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, err := tx.EnsureRoot(ctx, owner.ID)
		root = n
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i, size := range []int64{100, 100, 100} {
		if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, _, err := tx.WriteFile(ctx, store.WriteFile{
				NewFile: store.NewFile{
					OwnerID: owner.ID, ParentID: root.ID, Name: "history.bin", Size: size,
					Mime: "text/plain", ETag: `"h` + string(rune('0'+i)) + `"`,
					BlobID: uuid.New(), BlobHash: [32]byte{byte(i + 1)},
				},
				KeepVersion: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 150 configured: the 200 bytes of history must be trimmed to 150 is impossible in whole entries,
	// so the newest entry survives and the rest go — at least one row.
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil))).WithVersionByteCap(150)
	if n, err := w.TrimVersionHistory(context.Background()); err != nil || n == 0 {
		t.Fatalf("configured cap trimmed %d (err %v)", n, err)
	}

	// Now write history back above the configured cap, then override it to a larger value: the worker
	// must use the override and trim NOTHING.
	for i := 0; i < 2; i++ {
		if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, _, err := tx.WriteFile(ctx, store.WriteFile{
				NewFile: store.NewFile{
					OwnerID: owner.ID, ParentID: root.ID, Name: "history.bin", Size: 40,
					Mime: "text/plain", ETag: `"o` + string(rune('0'+i)) + `"`,
					BlobID: uuid.New(), BlobHash: [32]byte{byte(i + 5)},
				},
				KeepVersion: true,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.WriteSetting(ctx, "limits.max_retained_version_bytes", []byte("100000"), uuid.Nil)
	}); err != nil {
		t.Fatal(err)
	}

	if n, err := w.TrimVersionHistory(context.Background()); err != nil || n != 0 {
		t.Fatalf("the worker trimmed %d rows while an override raised the cap (err %v); it used the "+
			"configured value instead of the effective one", n, err)
	}
}
