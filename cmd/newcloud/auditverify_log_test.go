package main

// The verification command must not announce success for a chain it rejects.
//
// Found by running the real CLI against a real database: deleting one audit row and re-running
// `newcloud audit verify` failed with "chain is broken: prev_hash does not match the previous
// event (event 4)" — and printed `"msg":"audit chain verified","events":2` on stdout first, because
// the success line was unconditional.
//
// Exit codes are not what a log pipeline reads. The failure went to stderr, so a dashboard, a log
// search, or a person reading the output of a cron job would see success.

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/auditverify"
)

func TestBrokenChainIsNotLoggedAsVerified(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	observed := time.Now().UTC()
	broken := auditverify.Report{
		Events:     2,
		FirstEvent: &observed,
		LastEvent:  &observed,
		BrokenAt:   4,
		Err:        errors.New("prev_hash does not match the previous event"),
	}
	if broken.OK() {
		t.Fatal("the fixture is supposed to be a broken report")
	}

	logVerifyReport(log, broken)
	out := buf.String()

	if strings.Contains(out, "audit chain verified") {
		t.Fatalf("a broken chain was logged as verified:\n%s", out)
	}
	if !strings.Contains(out, "BROKEN") {
		t.Fatalf("the failure has to be visible in the log, not only in the exit code:\n%s", out)
	}
	// The position and the reason are what make the line actionable.
	if !strings.Contains(out, "broken_at=4") {
		t.Fatalf("the log should say where the chain broke:\n%s", out)
	}
	if !strings.Contains(out, "prev_hash") {
		t.Fatalf("the log should carry the reason:\n%s", out)
	}

	// And a healthy chain still says so, or the fix would just silence the good case.
	buf.Reset()
	logVerifyReport(log, auditverify.Report{Events: 7, Head: []byte{1, 2, 3, 4}})
	good := buf.String()
	if !strings.Contains(good, "audit chain verified") {
		t.Fatalf("an intact chain must still be reported as verified:\n%s", good)
	}
	if strings.Contains(good, "BROKEN") {
		t.Fatalf("an intact chain was reported as broken:\n%s", good)
	}
}
