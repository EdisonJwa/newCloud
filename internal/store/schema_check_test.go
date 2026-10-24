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
	"os"
	"testing"
	"time"

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
