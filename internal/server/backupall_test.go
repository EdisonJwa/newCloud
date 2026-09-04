package server_test

// The backup order must be enforced by something executable, not only by a paragraph.
//
// Restore-drill.md states the order and explains why, and internal/server/backuporder_test.go
// proves the property it rests on. Both are documentation and tests — neither one actually runs a
// backup. For a while there were two scripts (pg-backup.sh, blob-backup.sh) that each did their
// half correctly and knew nothing about each other, so the order lived entirely in prose. An
// operator with a cron entry pointing at the wrong half, or at both in the wrong sequence,
// produced exactly the broken restores the paragraph warns about.
//
// backup-all.sh exists so the order is executable. This test keeps it that way.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBackupAllRunsTheDumpBeforeTheBlobs checks the orchestrating script's step order.
func TestBackupAllRunsTheDumpBeforeTheBlobs(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "backup-all.sh"))
	if err != nil {
		t.Fatalf("read backup-all.sh: %v", err)
	}
	text := string(script)

	// Only executable lines count. The header comment names both scripts while explaining the
	// order, so a whole-file search finds the explanation rather than the invocation — which is
	// exactly how the first version of this test passed against a script that ran the blob copy
	// first. Every non-comment, non-blank line is what the shell actually executes.
	var executable []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		executable = append(executable, line)
	}
	code := strings.Join(executable, "\n")

	// Both halves must be invoked; a script that runs one of them is not a backup.
	pgAt := strings.Index(code, "pg-backup.sh")
	blobAt := strings.Index(code, "blob-backup.sh")
	if pgAt < 0 {
		t.Fatal("backup-all.sh does not invoke pg-backup.sh")
	}
	if blobAt < 0 {
		t.Fatal("backup-all.sh does not invoke blob-backup.sh")
	}
	if blobAt < pgAt {
		t.Fatalf("backup-all.sh runs blob-backup.sh (offset %d) before pg-backup.sh (offset %d). "+
			"The order is load-bearing: content is stored before its row, so copying blobs first "+
			"lets a write in the window leave a row whose bytes the copy does not have — a restore "+
			"that reports a missing blob for a file the backup claims to have. See "+
			"deploy/backup/restore-drill.md §2.5.", blobAt, pgAt)
	}
}

// TestBackupAllFailsLoudly guards the shell settings that make a partial backup stop the run.
//
// Without `set -eu`, a failing pg_dump leaves the script continuing to the blob copy, which then
// reports success. The operator gets a manifest saying a backup completed and no database dump in
// it — the worst of both outcomes, because the manifest is the evidence they would check.
func TestBackupAllFailsLoudly(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "backup-all.sh"))
	if err != nil {
		t.Fatalf("read backup-all.sh: %v", err)
	}
	text := string(script)

	if !regexp.MustCompile(`(?m)^set -eu$`).MatchString(text) {
		t.Fatal("backup-all.sh must start with `set -eu`: a failed dump has to stop the run rather " +
			"than fall through to the blob copy and report success")
	}

	// The required settings must be demanded, not defaulted. A silent default for DATABASE_URL or
	// RESTIC_REPOSITORY turns a misconfiguration into a backup of nothing.
	for _, required := range []string{"DATABASE_URL", "RESTIC_REPOSITORY"} {
		pattern := `${` + required + `:?`
		if !strings.Contains(text, pattern) {
			t.Fatalf("backup-all.sh does not fail when %s is unset (want ${%s:?...}). A defaulted "+
				"backup target means a successful run that backed up nothing.", required, required)
		}
	}
}

// TestBackupScriptsAgreeOnTheDocumentedOrder cross-checks the three places the order appears.
//
// They can drift independently: the document, the orchestrator, and the drill. A reader trusting
// one and running another is the failure this catches.
func TestBackupScriptsAgreeOnTheDocumentedOrder(t *testing.T) {
	root := repoRoot(t)

	doc, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "restore-drill.md"))
	if err != nil {
		t.Fatalf("read restore-drill.md: %v", err)
	}
	if !strings.Contains(string(doc), "Take the pg_dump first, then copy the blobs.") {
		t.Fatal("restore-drill.md no longer states the backup order")
	}

	script, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "backup-all.sh"))
	if err != nil {
		t.Fatalf("read backup-all.sh: %v", err)
	}
	if !strings.Contains(string(script), "THE ORDER: database dump first, then the blob copy") {
		t.Fatal("backup-all.sh must state the order it implements, so the two cannot diverge " +
			"without someone noticing")
	}

	// The drill must dump first too: it reproduces the nightly job, so a drill that ordered the
	// opposite way would exercise a procedure nobody runs.
	drill, err := os.ReadFile(filepath.Join(root, "cmd", "newcloud", "drill.go"))
	if err != nil {
		t.Fatalf("read drill.go: %v", err)
	}
	drillText := string(drill)
	if !strings.Contains(drillText, "pg_dump") {
		t.Fatal("the drill no longer dumps the database; if it was replaced, this check is stale")
	}
}

// TestPgBackupUsesTheConfiguredDatabase.
//
// pg-backup.sh used to call a bare `pg_dump`, which takes its connection from the PG* environment
// variables and ignores DATABASE_URL completely. On a host without those variables it dumps
// "postgres" on localhost:5432 — or fails — while the operator believes they have a backup of
// their real database. The failure is quiet in the first case and confusing in the second.
//
// This was found by actually running backup-all.sh, which exports DATABASE_URL and got
// "connection refused on localhost:5432" while the database was on port 15437.
func TestPgBackupUsesTheConfiguredDatabase(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "pg-backup.sh"))
	if err != nil {
		t.Fatalf("read pg-backup.sh: %v", err)
	}
	text := string(script)

	if !strings.Contains(text, "--dbname=") {
		t.Fatalf("pg-backup.sh does not pass --dbname; a bare `pg_dump` reads the PG* environment " +
			"and ignores DATABASE_URL, so it dumps whatever those variables happen to point at — " +
			"or nothing.")
	}
	// The variable must be required rather than defaulted: a defaulted backup target is a
	// successful run that backed up the wrong database.
	if !strings.Contains(text, "${DATABASE_URL:") && !strings.Contains(text, "${DSN:") {
		t.Fatal("pg-backup.sh does not fail when no connection is configured")
	}
}
