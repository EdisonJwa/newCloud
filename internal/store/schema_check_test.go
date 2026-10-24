package store_test

// CheckSchema is the guard that stops a binary from serving against a database it does
// not understand (2026-10-24 review, H8). The interesting cases are the two directions:
//
//   - the database is BEHIND the binary: fatal, because the code will query columns
//     that do not exist, and the failure would surface as a 500 on a random request;
//   - the database is AHEAD of the binary: a rollback in progress. Reported, not fatal.
//
// Needs a real PostgreSQL (TEST_DATABASE_URL) because the whole question is what the
// server thinks of its own schema.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func schemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; the schema check needs a real database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A freshly migrated database has nothing behind and nothing ahead.
func TestCheckSchemaIsCleanAfterMigrate(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()

	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	behind, ahead, err := store.CheckSchema(ctx, pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(behind) != 0 || len(ahead) != 0 {
		t.Fatalf("behind=%v ahead=%v; a migrated database must report neither",
			behind, ahead)
	}
}

// A database missing an applied row is "behind", and the answer names the migration so
// the operator knows what to run rather than what to google.
func TestCheckSchemaReportsAMigrationThatIsBehind(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()

	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	// Find the newest applied migration and remove its row, which is exactly the state
	// a binary deployed ahead of `migrate` sees.
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1`).Scan(&name); err != nil {
		t.Fatalf("read newest migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE name = $1`, name); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`INSERT INTO schema_migrations (name) VALUES ($1) ON CONFLICT DO NOTHING`, name)
	})

	behind, _, err := store.CheckSchema(ctx, pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(behind) != 1 || behind[0] != name {
		t.Fatalf("behind = %v, want [%s]", behind, name)
	}
}

// A marker for a migration the binary does not embed is "ahead": a rollback, or a
// database touched by a newer release. Warned about, never treated as fatal.
func TestCheckSchemaReportsAnUnknownAppliedMigrationAsAhead(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()

	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	const future = "9999_from_the_future.sql"
	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_migrations (name) VALUES ($1) ON CONFLICT DO NOTHING`, future); err != nil {
		t.Fatalf("insert future marker: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM schema_migrations WHERE name = $1`, future)
	})

	behind, ahead, err := store.CheckSchema(ctx, pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(behind) != 0 {
		t.Fatalf("behind = %v; an unknown extra marker is not a missing migration", behind)
	}
	if len(ahead) != 1 || ahead[0] != future {
		t.Fatalf("ahead = %v, want [%s]", ahead, future)
	}
}

// A migration run must WAIT for another one rather than proceed beside it.
//
// The first version of this test started three runs at once and required all to succeed. It
// passed with the advisory lock removed: whichever run won applied everything before the
// others reached their first statement, so the race never happened and the test proved
// nothing. Holding the lock from a second connection makes the wait observable, which is the
// property that matters (2026-10-24 review, M11).
func TestMigrateWaitsForTheAdvisoryLock(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, store.MigrationLockID); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- store.ApplySchema(ctx, pool) }()

	select {
	case aerr := <-done:
		_, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.MigrationLockID)
		t.Fatalf("ApplySchema returned while the migration lock was held elsewhere (%v); "+
			"two runs can then apply the same migration", aerr)
	case <-time.After(750 * time.Millisecond):
		// Blocked, which is the point.
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.MigrationLockID); err != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case aerr := <-done:
		if aerr != nil {
			t.Fatalf("ApplySchema after the lock was released: %v", aerr)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("ApplySchema never completed after the lock was released")
	}
}

// A referential-integrity failure is a fault, not a 404.
//
// Inserting a node whose blob_id names no blobs row means the metadata would reference content
// that does not exist. Answering 404 ("no such file") sends an operator looking at the client
// while the storage is damaged; the project's own failure table promises an error instead
// (2026-10-24 review, M18).
func TestAMissingBlobRowIsNotReportedAsNotFound(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	pg := store.NewPG(pool)
	pg.EnableAudit()

	var userID uuid.UUID
	if err := pg.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "fk-probe",
			Username: "fk-probe", Email: "fk-probe@example.test", Role: "user",
		})
		userID = u.ID
		return uerr
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := pg.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, rerr := tx.EnsureRoot(ctx, userID)
		return rerr
	}); err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	var err error
	root, rerr := pg.Root(ctx, userID)
	if rerr != nil {
		t.Fatalf("root: %v", rerr)
	}
	if terr := pg.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: userID, ParentID: root.ID, Name: "dangling.bin",
			Size: 1, Mime: "application/octet-stream", ETag: `"x"`,
			BlobID:   uuid.New(), // no such blobs row
			BlobHash: [32]byte{1},
		})
		return cerr
	}); terr != nil {
		err = terr
	}

	if err == nil {
		t.Fatal("inserting a node that references a missing blob row succeeded")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a missing blob row was reported as not-found (%v); that reads as 'no such "+
			"file' to a client and hides a storage fault", err)
	}
}
