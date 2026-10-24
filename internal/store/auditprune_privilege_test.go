package store_test

// The prune preflight (2026-10-24 review, M17).
//
// `newcloud audit prune` used to run as the application role and fail part-way: the grants in
// migrations/0001_init.sql give newcloud_app INSERT and SELECT on audit.events and nothing
// else, so the DELETE cannot succeed — but only after the archive had been written. A
// destructive maintenance command that half-succeeds is worse than one that refuses.
//
// The preflight is only worth having if it detects the real role, so this test creates a role
// with exactly the grants the migrations give the application and connects as it.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// testDSN returns the test database DSN, or skips.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL tests")
	}
	return dsn
}

// poolAsRole returns a pool whose connections run as role via SET ROLE.
func poolAsRole(t *testing.T, dsn, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPrunePrivilegesDetectsTheApplicationRole(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("owner pool: %v", err)
	}
	t.Cleanup(owner.Close)
	if err := store.ApplySchema(ctx, owner); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	const role = "newcloud_app_probe"
	_, _ = owner.Exec(ctx, `DROP ROLE IF EXISTS `+role)
	if _, err := owner.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	// Exactly the grants migrations/0001_init.sql gives the application: no DELETE on the log
	// and no write at all on the marks.
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT USAGE ON SCHEMA audit TO ` + role,
		`GRANT SELECT, INSERT ON audit.events TO ` + role,
		`GRANT SELECT ON audit.prune_marks TO ` + role,
	} {
		if _, err := owner.Exec(ctx, grant); err != nil {
			t.Fatalf("%s: %v", grant, err)
		}
	}

	app := store.NewPG(poolAsRole(t, dsn, role))
	gotRole, missing, err := app.PrunePrivileges(ctx)
	if err != nil {
		t.Fatalf("PrunePrivileges as the app role: %v", err)
	}
	if gotRole != role {
		t.Fatalf("reported role %q, want %q", gotRole, role)
	}
	if len(missing) != 2 {
		t.Fatalf("missing = %v; want both DELETE on audit.events and INSERT on audit.prune_marks", missing)
	}
	joined := strings.Join(missing, " ")
	if !strings.Contains(joined, "audit.events") || !strings.Contains(joined, "audit.prune_marks") {
		t.Fatalf("missing = %v; the message must name the tables the role cannot touch", missing)
	}

	// The owner can prune, and the preflight must say so rather than warning on every run.
	ownerPG := store.NewPG(owner)
	ownerRole, ownerMissing, err := ownerPG.PrunePrivileges(ctx)
	if err != nil {
		t.Fatalf("PrunePrivileges as the owner: %v", err)
	}
	if len(ownerMissing) != 0 {
		t.Fatalf("role %q (the schema owner) reported missing privileges: %v", ownerRole, ownerMissing)
	}
}
