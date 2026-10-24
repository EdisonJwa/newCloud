package main

// Guards for the drill's data-loss and credential-handling behaviour.
//
// The 2026-10-24 review (docs/reference/review-2026-10-24.md, C4 and H1-ops) found two
// defects in this file:
//
//   - the live-vs-scratch guard compared DSNs as strings, so two spellings of one database
//     sailed past it into `DROP SCHEMA ... CASCADE` on the production metadata;
//   - pg_dump and pg_restore were handed the DSN in `--dbname=`, which puts the database
//     password in every local user's `ps` output for the duration of a dump, and into the
//     error text the binary prints on failure.
//
// Both are fixed; these tests are the part that keeps them fixed.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The DSN never travels as one string again. What reaches the tool is a set of libpq
// variables, and the assertions check both halves of that: the values are the ones the
// DSN named, and the sslmode spelling — the exact parameter that once defeated the string
// guard — survives the round trip.
func TestPGEnvFromDSNCarriesTheConnectionWithoutTheDSN(t *testing.T) {
	env, err := pgEnvFromDSN(
		"postgres://drill:secret@db.example.org:5433/newcloud?sslmode=disable&application_name=drill")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed env entry %q", kv)
		}
		got[parts[0]] = parts[1]
	}

	for key, want := range map[string]string{
		"PGHOST":     "db.example.org",
		"PGPORT":     "5433",
		"PGDATABASE": "newcloud",
		"PGUSER":     "drill",
		"PGPASSWORD": "secret",
		"PGSSLMODE":  "disable",
		"PGAPPNAME":  "drill",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}

	// The point of the exercise: no single entry, and no join of them, is the DSN, so a
	// failure message that echoes tool arguments cannot carry `secret` back out.
	joined := strings.Join(env, " ")
	if strings.Contains(joined, "postgres://") {
		t.Errorf("env still contains a DSN-shaped value: %s", joined)
	}
}

// A password-bearing DSN in an error message is the leak an operator pastes into a ticket.
func TestRedactDSNsStripsCredentials(t *testing.T) {
	in := `pg_dump: connection to server at "postgres://alice:hunter2@db.internal:5432/prod" failed`
	out := redactDSNs(in)
	if strings.Contains(out, "hunter2") {
		t.Errorf("password survived redaction: %s", out)
	}
	if !strings.Contains(out, "postgres://***:***@db.internal:5432/prod") {
		t.Errorf("host and database should survive; got %s", out)
	}
	// A DSN without credentials is left alone: redaction must not eat useful context.
	plain := "postgres://db.internal:5432/prod"
	if redactDSNs(plain) != plain {
		t.Errorf("credential-free DSN should pass through, got %s", redactDSNs(plain))
	}
}

// The guard itself. Two connections to the same database whose DSNs differ by exactly the
// query parameter the project's own documentation uses (`?sslmode=disable`) must be
// recognised as the same database and refused — this is the case that once reached
// `DROP SCHEMA public CASCADE`.
//
// Needs a database; without one it skips rather than passing vacuously.
func TestTheDrillRefusesARestoreTargetThatIsTheLiveDatabase(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; the guard needs two real connections")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	live, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect live: %v", err)
	}
	defer live.Close(ctx)

	// The same database, spelled differently. The old string comparison called these two
	// different databases; the server must call them the same one.
	var variant string
	if strings.Contains(dsn, "?") {
		variant = dsn + "&sslmode=disable"
	} else {
		variant = dsn + "?sslmode=disable"
	}

	shadow, err := pgx.Connect(ctx, variant)
	if err != nil {
		t.Fatalf("connect variant spelling: %v", err)
	}
	defer shadow.Close(ctx)

	err = refuseIfSameDatabase(ctx, live, shadow)
	if err == nil {
		t.Fatal("the guard accepted the live database as the restore target")
	}
	if !strings.Contains(err.Error(), "refusing to run") {
		t.Errorf("the refusal should say what to do; got: %v", err)
	}
}
