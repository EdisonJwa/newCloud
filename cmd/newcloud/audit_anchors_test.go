package main

// `audit verify` must compare the chain against the published anchors.
//
// It did not. The command verified the hash chain and stopped, so the one attack the chain
// cannot see — deleting the newest events — reported `audit chain verified` and exited 0.
// Measured on a real PostgreSQL: append 25 events, publish a checkpoint covering 20, delete the
// newest 5, run `audit verify` → "verified", exit 0. The operations guide told operators the
// weekly check "compares the recorded daily anchors", so the gap was in the product and in the
// documentation at once.
//
// These tests drive compareAnchors, the function the command calls, so they fail if the
// comparison is removed from `audit verify` again — not merely if the library misbehaves.

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/auditverify"
)

// stubChain is a Chain whose head is whatever the test says it is.
type stubChain struct {
	head  []byte
	count int64
	// heads is the head the chain reports at a given event count, which is how the verifier
	// asks "what did the chain look like when this checkpoint was published?".
	heads map[int64][]byte
}

func (c stubChain) Head() ([]byte, int64, error) { return c.head, c.count, nil }

func (c stubChain) HeadAtCount(n int64) ([]byte, error) {
	if h, ok := c.heads[n]; ok {
		return h, nil
	}
	return nil, errors.New("count out of range")
}

func anchorTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// publishCheckpoint writes one anchor covering count events at head.
func publishCheckpoint(t *testing.T, head []byte, count int64) string {
	t.Helper()
	dir := t.TempDir()
	anchor, err := auditverify.NewFileAnchor(dir)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := anchor.PublishWithCount(yesterday(), head, count); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return dir
}

// TestTruncatedChainFailsAuditVerify is the regression. Deleting the newest events used to
// report success; it must now fail, named as a truncation.
func TestTruncatedChainFailsAuditVerify(t *testing.T) {
	dir := publishCheckpoint(t, []byte{0xaa, 0xbb, 0xcc, 0xdd}, 20)

	// The chain now holds 15 events; the head it should have at the 20-event mark is gone.
	truncated := stubChain{head: []byte{1, 1, 1, 1}, count: 15, heads: map[int64][]byte{}}

	var buf bytes.Buffer
	err := compareAnchors(anchorTestLogger(&buf), dir, truncated, nil)
	if err == nil {
		t.Fatalf("a truncated chain passed audit verify; this is the attack the anchors exist to "+
			"catch\nlog:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "disagree with the chain") {
		t.Fatalf("the failure must name the disagreement, got: %v", err)
	}
}

// TestIntactChainPassesAuditVerify keeps the good case green, or the fix would be a check that
// only ever fails.
func TestIntactChainPassesAuditVerify(t *testing.T) {
	head := []byte{0x10, 0x20, 0x30, 0x40}
	dir := publishCheckpoint(t, head, 20)
	intact := stubChain{head: head, count: 20, heads: map[int64][]byte{20: head}}

	var buf bytes.Buffer
	if err := compareAnchors(anchorTestLogger(&buf), dir, intact, nil); err != nil {
		t.Fatalf("an intact chain failed verification: %v\nlog:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "anchors agree with the chain") {
		t.Fatalf("the success line should say the anchors were compared:\n%s", buf.String())
	}
}

// TestNoAnchorDirectoryWarnsInsteadOfClaimingAgreement: an unconfigured deployment must say the
// completeness claim is unproven. Silence here is what let the gap survive.
func TestNoAnchorDirectoryWarnsInsteadOfClaimingAgreement(t *testing.T) {
	var buf bytes.Buffer
	err := compareAnchors(anchorTestLogger(&buf), "", stubChain{head: []byte{1}, count: 1}, nil)
	if err != nil {
		t.Fatalf("an unconfigured anchor dir is a warning, not a failure: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no anchors were compared") {
		t.Fatalf("the missing-anchor case must be announced:\n%s", out)
	}
	if strings.Contains(out, "anchors agree with the chain") {
		t.Fatalf("nothing was compared, so agreement cannot be claimed:\n%s", out)
	}
}

// TestEmptyAnchorSetDoesNotClaimAgreement: a deployment on its first day has no checkpoints yet.
// "Zero checkpoints" is not "the chain is complete".
func TestEmptyAnchorSetDoesNotClaimAgreement(t *testing.T) {
	dir := t.TempDir() // no checkpoint published
	var buf bytes.Buffer
	err := compareAnchors(anchorTestLogger(&buf), dir, stubChain{head: []byte{1}, count: 1}, nil)
	if err != nil {
		t.Fatalf("an empty anchor set is not a verification failure: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "anchors agree with the chain") {
		t.Fatalf("an empty anchor set must not be reported as agreement:\n%s", out)
	}
	if !strings.Contains(out, "anchor set is empty") {
		t.Fatalf("an empty anchor set must say why it proves nothing:\n%s", out)
	}
}

// TestWrongSigningKeyIsRejected pins the signed path: a checkpoint signed by one key must not be
// accepted under another, or signing would be decorative.
func TestWrongSigningKeyIsRejected(t *testing.T) {
	signer, err := auditverify.GenerateSigner()
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	dir := t.TempDir()
	anchor, err := auditverify.NewFileAnchor(dir)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	anchor = anchor.WithSigner(signer)

	head := []byte{0xde, 0xad, 0xbe, 0xef}
	if err := anchor.PublishWithCount(yesterday(), head, 20); err != nil {
		t.Fatalf("publish: %v", err)
	}
	chain := stubChain{head: head, count: 20, heads: map[int64][]byte{20: head}}

	// Unsigned, the fixture agrees on head and count — which is exactly why the key exists.
	res, err := auditverify.VerifySigned(anchor, chain, nil)
	if err != nil || !res.OK() {
		t.Fatalf("fixture should agree unsigned: err=%v res=%+v", err, res)
	}

	other, err := auditverify.GenerateSigner()
	if err != nil {
		t.Fatalf("other signer: %v", err)
	}
	var buf bytes.Buffer
	err = compareAnchors(anchorTestLogger(&buf), dir, chain, other.PublicKey())
	if err == nil {
		t.Fatalf("a checkpoint signed by one key was accepted under another\nlog:\n%s", buf.String())
	}
}

// yesterday is the day a checkpoint covers: the worker anchors completed days only.
func yesterday() time.Time {
	return time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
}
