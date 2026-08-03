package store

import (
	"strings"
	"testing"
)

// stripTxControl tests.
//
// This function had a real bug that produced a confusing failure far from its cause: it
// tracked only anonymous `$$` dollar quotes, so a `CREATE FUNCTION ... $fn$ ... $fn$` body
// read as ordinary SQL. A line inside it that happened to be `BEGIN` (a PL/pgSQL block
// opener) was dropped as a transaction wrapper, corrupting the function, and the resulting
// migration error ("events_default_pkey" is an index) pointed nowhere near the cause.

func TestStripTxControlRemovesWrapper(t *testing.T) {
	in := "BEGIN;\nCREATE TABLE t (id int);\nCOMMIT;"
	got := stripTxControl(in)

	if strings.Contains(strings.ToUpper(got), "COMMIT") {
		t.Fatalf("COMMIT must be stripped, got:\n%s", got)
	}
	if strings.Contains(strings.ToUpper(got), "BEGIN") {
		t.Fatalf("BEGIN must be stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "CREATE TABLE") {
		t.Fatalf("the body must survive, got:\n%s", got)
	}
}

// TestStripTxControlKeepsDollarBody is the regression.
func TestStripTxControlKeepsDollarBody(t *testing.T) {
	in := `BEGIN;
CREATE OR REPLACE FUNCTION f() RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1;
END
$$;
COMMIT;`
	got := stripTxControl(in)

	// The PL/pgSQL BEGIN inside the anonymous dollar quote must survive.
	body := got[strings.Index(got, "LANGUAGE plpgsql"):]
	if !strings.Contains(body, "BEGIN") {
		t.Fatalf("the function body's BEGIN was stripped:\n%s", got)
	}
	// The wrapper's COMMIT must not.
	if strings.HasSuffix(strings.TrimSpace(got), "COMMIT;") {
		t.Fatalf("the trailing COMMIT must be stripped:\n%s", got)
	}
}

// TestStripTxControlKeepsNamedTagBody is the case that actually broke.
func TestStripTxControlKeepsNamedTagBody(t *testing.T) {
	in := `BEGIN;
CREATE OR REPLACE FUNCTION f() RETURNS void
LANGUAGE plpgsql AS $fn$
BEGIN
    PERFORM 1;
END
$fn$;
COMMIT;`
	got := stripTxControl(in)

	if !strings.Contains(got, "PERFORM 1") {
		t.Fatalf("the function body was lost:\n%s", got)
	}
	// Exactly one BEGIN should remain: the one inside the function body.
	if n := strings.Count(strings.ToUpper(got), "BEGIN"); n != 1 {
		t.Fatalf("want exactly one BEGIN (the PL/pgSQL block), got %d:\n%s", n, got)
	}
	if strings.HasSuffix(strings.TrimSpace(got), "COMMIT;") {
		t.Fatalf("the trailing COMMIT must be stripped:\n%s", got)
	}
}

// TestStripTxControlHandlesBlockDo: a DO block uses an anonymous $$ quote and often
// contains BEGIN.
func TestStripTxControlHandlesBlockDo(t *testing.T) {
	in := `BEGIN;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'x') THEN
        CREATE ROLE x LOGIN;
    END IF;
END
$$;
COMMIT;`
	got := stripTxControl(in)

	if !strings.Contains(got, "CREATE ROLE x LOGIN") {
		t.Fatalf("the DO block body was lost:\n%s", got)
	}
	if strings.HasSuffix(strings.TrimSpace(got), "COMMIT;") {
		t.Fatalf("the trailing COMMIT must be stripped:\n%s", got)
	}
}

// TestStripTxControlNestedDifferentTags: a $fn$ body may legally contain $$ text, and the
// state machine must not close early on it.
func TestStripTxControlNestedDifferentTags(t *testing.T) {
	in := `BEGIN;
CREATE FUNCTION f() RETURNS text AS $fn$
BEGIN
    RETURN '$$ is just text in here';
END
$fn$ LANGUAGE plpgsql;
COMMIT;`
	got := stripTxControl(in)

	if !strings.Contains(got, "RETURN '$$ is just text in here'") {
		t.Fatalf("the body was truncated at the inner $$:\n%s", got)
	}
	if strings.HasSuffix(strings.TrimSpace(got), "COMMIT;") {
		t.Fatalf("the trailing COMMIT must be stripped:\n%s", got)
	}
}
