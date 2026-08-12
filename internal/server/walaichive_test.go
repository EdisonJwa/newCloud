package server_test

// The deployment configuration must keep the RPO promise the documentation makes.
//
// restore-drill.md states "元数据（PG） RPO ≤ 15 分钟（WAL 归档连续）". Two settings are load-bearing
// for that number, and one of them was missing from deploy/docker-compose.yml:
//
//	archive_mode=on
//	    Without it there is no WAL to replay at all, and the RPO is the dump interval — 24 hours.
//
//	archive_timeout
//	    PostgreSQL archives a segment when it fills (16 MB) or when this fires, whichever comes
//	    first. With no timeout, a quiet deployment holds an unarchived segment until 16 MB of WAL
//	    accumulates, which on an idle system can be hours. The 15-minute RPO was therefore true
//	    only on a busy system — and a quiet Sunday night is exactly when nobody is watching and a
//	    backup matters most.
//
// This is checked as configuration rather than measured, because the failure is a missing setting
// rather than a wrong behaviour: a deployment with no archive_timeout behaves perfectly and simply
// archives less often than promised.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestComposeEnablesContinuousWalArchiving checks the settings the RPO claim depends on.
func TestComposeEnablesContinuousWalArchiving(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, "archive_mode=on") {
		t.Fatal("docker-compose.yml does not set archive_mode=on; without WAL archiving the " +
			"metadata RPO is the dump interval (24h), not the 15 minutes restore-drill.md claims")
	}

	// archive_timeout, with any unit. The value matters less than its presence, but a value above
	// the stated RPO would fail to meet it regardless of transfer time, so it is bounded.
	if !regexp.MustCompile(`archive_timeout=\d+(s|min)`).MatchString(text) {
		t.Fatal("docker-compose.yml does not set archive_timeout. PostgreSQL only archives a WAL " +
			"segment when it fills or when this fires, so without it an idle deployment can leave " +
			"a segment unarchived for hours and the 15-minute RPO becomes untrue exactly when it " +
			"is needed — on a quiet system nobody is watching.")
	}

	// The value is bounded by the stated RPO: archiving is one term of it, and transfer plus
	// restore are others, so the timeout must leave room. Parsed as an integer — comparing the
	// digit strings lexically makes "5" >= "15" true, which is how the first version of this
	// check failed on its own correct configuration.
	m := regexp.MustCompile(`archive_timeout=(\d+)(s|min)`).FindStringSubmatch(text)
	if m != nil {
		var n int
		fmtSscan(m[1], &n)
		limit := 300 // seconds
		if m[2] == "min" {
			limit = 15 * 60
		} else {
			limit = 300
		}
		got := n
		if m[2] == "min" {
			got = n * 60
		}
		if got >= limit {
			t.Fatalf("archive_timeout=%s leaves no room for transfer and restore inside a "+
				"15-minute RPO (%d seconds)", m[0], got)
		}
	}
}

// TestArchiveCommandIsIdempotent.
//
// PostgreSQL re-runs archive_command until it returns zero. A command that overwrites
// unconditionally is not idempotent but is usually harmless; one that fails when the file exists
// causes the archiver to retry forever while pg_wal grows without bound. The shipped command
// guards with `test ! -f`, which is the right shape — pinned here because the failure mode (disk
// fills days later) is far from the change that causes it.
func TestArchiveCommandIsIdempotent(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	text := string(raw)

	line := regexp.MustCompile(`archive_command=([^\n]*)`).FindStringSubmatch(text)
	if line == nil {
		t.Fatal("docker-compose.yml has no archive_command; with archive_mode=on but no command " +
			"the archiver never runs and WAL accumulates until the disk is full")
	}
	cmd := line[1]
	if !strings.Contains(cmd, "test ! -f") {
		t.Fatalf("archive_command does not guard against an already-archived segment:\n  %s\n\n"+
			"PostgreSQL retries a failing archive_command indefinitely. A command that errors on "+
			"an existing destination makes the archiver spin forever while pg_wal grows.", cmd)
	}
}

// TestPITRProcedureIsDocumented checks the restore procedure exists in executable detail.
//
// Before this round section 2 said only "如果 RPO 要求回放到故障点，继续应用 WAL 到目标时间" —
// a sentence, not a procedure. Nobody recovers a database from a sentence, and the two settings
// people get wrong (recovery_target_action and recovery.signal) were not mentioned at all.
func TestPITRProcedureIsDocumented(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "restore-drill.md"))
	if err != nil {
		t.Fatalf("read restore-drill.md: %v", err)
	}
	text := string(doc)

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"pg_basebackup", "a base backup is the starting point for PITR"},
		{"recovery_target_time", "the recovery target is what makes it point-in-time"},
		{"recovery_target_action", "without 'promote' the instance stays read-only after recovery"},
		{"recovery.signal", "PG12+ needs this file; recovery.conf no longer exists"},
		{"restore_command", "replay needs to know where the archive is"},
	} {
		if !strings.Contains(text, want.fragment) {
			t.Fatalf("restore-drill.md does not mention %q (%s). A PITR procedure is not "+
				"recoverable from a summary sentence.", want.fragment, want.why)
		}
	}
}

// fmtSscan is a tiny integer parse, kept local so the test needs no extra import.
func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}
