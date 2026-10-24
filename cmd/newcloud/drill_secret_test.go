package main

// The container path must not put the database password in a process argument.
//
// Locally the libpq variables ride the child's process environment, which other users cannot
// read. Inside DRILL_PG_CONTAINER the only way to set one used to be `docker exec -e
// PGPASSWORD=…`, and every -e flag is an argv entry of the docker process — world-readable
// in `ps` for the whole duration of a dump, which is the exact exposure the DSN work removed
// everywhere else. The password now travels on stdin; these tests pin that by inspecting the
// planned argv and stdin directly, with no Docker daemon involved.

import (
	"bytes"
	"strings"
	"testing"
)

func TestContainerArgvNeverCarriesThePassword(t *testing.T) {
	const password = "hunter2-but-real"
	env := []string{
		"PGHOST=db.internal", "PGPORT=5432", "PGDATABASE=newcloud",
		"PGUSER=drill", "PGPASSWORD=" + password, "PGSSLMODE=require",
	}

	tc := buildToolCommand("pg-container", "pg_dump",
		[]string{"--format=custom", "--compress=9", "--file=/tmp/drill/newcloud.dump"}, env)

	for _, a := range tc.argv {
		if strings.Contains(a, password) {
			t.Fatalf("argv element %q contains the password; it is readable in `ps` and in "+
				"`docker inspect` for as long as the tool runs", a)
		}
	}
	if got := string(tc.stdin); got != password {
		t.Fatalf("stdin payload is %q, want exactly the password", got)
	}
	// The shell wrapper must be the thing that reads it, and the tool must run after the
	// assignment so libpq sees it as an environment variable.
	if len(tc.argv) < 3 || tc.argv[len(tc.argv)-2] != "-c" {
		t.Fatalf("expected a `sh -c` invocation, got %v", tc.argv)
	}
	script := tc.argv[len(tc.argv)-1]
	if !strings.HasPrefix(script, `PGPASSWORD="$(cat)" pg_dump`) {
		t.Fatalf("the shell command does not read the password from stdin: %q", script)
	}
	// Non-secret connection settings still travel as -e flags, so the tool reaches the
	// right database without any of them being folded into the secret path.
	for _, want := range []string{"PGHOST=db.internal", "PGUSER=drill"} {
		found := false
		for _, a := range tc.argv {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("connection setting %q is missing from the argv: %v", want, tc.argv)
		}
	}
}

// Tool arguments are interpolated into a shell command, so quoting is what stands between a
// path and shell interpretation. A single quote in a path (a tmpdir name can carry one) must
// survive the round trip as data, not terminate the string and start code.
func TestShellQuoteKeepsArgumentsAsData(t *testing.T) {
	const tricky = `/tmp/newcloud-drill-*/it's a test/newcloud.dump`
	if got := shellQuote(tricky); got != `'`+strings.ReplaceAll(tricky, `'`, `'\''`)+`'` {
		t.Fatalf("shellQuote produced %q, which is not a single-quoted word", got)
	}

	tc := buildToolCommand("pg-container", "pg_restore",
		[]string{"--no-owner", tricky}, []string{"PGPASSWORD=x"})
	script := tc.argv[len(tc.argv)-1]
	// The escape idiom must appear around the embedded quote: "…/it" runs to the end of
	// the word, \' carries the quote, and "s a test/…" reopens it.
	if !strings.Contains(script, `it'\''s`) {
		t.Fatalf("the embedded single quote was not escaped: %q", script)
	}
	if bytes.Contains(tc.stdin, []byte("PGPASSWORD")) {
		t.Fatal("stdin must carry the password value only")
	}
}

// Without a container the local shape is unchanged: direct exec, env overlay, no stdin.
// The env approach is the correct one locally because a process environment is not argv.
func TestLocalInvocationKeepsTheEnvOverlay(t *testing.T) {
	env := []string{"PGHOST=db.internal", "PGPASSWORD=hunter2"}
	tc := buildToolCommand("", "pg_dump", []string{"--format=custom"}, env)
	if tc.overlayEnv == false {
		t.Fatal("the local invocation lost its env overlay; the DSN would not reach libpq")
	}
	if tc.stdin != nil {
		t.Fatal("the local invocation must not use stdin; the env path is correct there")
	}
	if len(tc.argv) != 2 || tc.argv[0] != "pg_dump" || tc.argv[1] != "--format=custom" {
		t.Fatalf("the local argv gained a wrapper: %v", tc.argv)
	}
}

// A container run with no password in the env (no secret anywhere in the DSN) keeps the
// direct exec, which fails more legibly than a shell wrapper.
func TestContainerWithoutPasswordStaysDirect(t *testing.T) {
	tc := buildToolCommand("pg-container", "pg_restore", []string{"--list", "/tmp/x.dump"},
		[]string{"PGHOST=db.internal"})
	for _, a := range tc.argv {
		if a == "sh" {
			t.Fatalf("no secret is in play, so the shell wrapper is unnecessary: %v", tc.argv)
		}
	}
	if tc.stdin != nil {
		t.Fatal("stdin should be untouched when there is no password")
	}
}
