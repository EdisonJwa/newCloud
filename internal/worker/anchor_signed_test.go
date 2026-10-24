package worker_test

// The hourly anchor check verifies checkpoint SIGNATURES when the worker holds the signing
// key (§10.2). Until this, the continuous path compared the published checkpoints against the
// chain but never asked whether the checkpoint files were the ones that were published — the
// check `newcloud audit verify-anchors` performs — so an attacker who could rewrite the anchor
// directory and the log together sailed through every hourly tick and was caught only if a
// human ran the CLI with the public key.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
	"github.com/EdisonJwa/newCloud/internal/auditverify"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/worker"
)

// staticRows is the chain the anchor verifies against, held in memory so a test can swap the
// log for a forged one without a database.
type staticRows struct{ events []audit.Event }

func (s *staticRows) AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error) {
	var out []audit.Event
	for _, e := range s.events {
		if e.ID > afterID {
			out = append(out, e)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *staticRows) AuditHead(ctx context.Context) ([]byte, error) {
	if len(s.events) == 0 {
		return audit.GenesisHash, nil
	}
	return s.events[len(s.events)-1].Hash, nil
}

func sealedRows(t *testing.T, n int, action string) *staticRows {
	t.Helper()
	out := &staticRows{}
	prev := audit.GenesisHash
	for i := 1; i <= n; i++ {
		e := audit.Event{
			ID: int64(i), Time: time.Date(2026, 6, 1, 0, 0, i, 0, time.UTC),
			Action: audit.Action(action), Result: audit.ResultOK,
			PrevHash: append([]byte(nil), prev...),
		}
		if err := e.Seal(); err != nil {
			t.Fatal(err)
		}
		out.events = append(out.events, e)
		prev = e.Hash
	}
	return out
}

// TestContinuousAnchorVerifyChecksSignatures drives the worker's own verification path: an
// honest signed checkpoint verifies, and a checkpoint rewritten without the key fails it.
func TestContinuousAnchorVerifyChecksSignatures(t *testing.T) {
	signer, err := auditverify.GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	truth := sealedRows(t, 5, "test.truth")
	meta := store.NewMemory()
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.WithAnchorSigner(signer)
	dir := t.TempDir()
	if err := w.EnableAnchoring(dir, truth); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// AnchorNow publishes (signed, because the signer is configured) and then verifies.
	if err := w.AnchorNow(ctx); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	res, err := w.VerifyAnchor()
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("an honest signed checkpoint failed the continuous check: %v", res.Problems)
	}

	// The coordinated rewrite: the log is replaced and the checkpoint file is edited to
	// name the replacement, without the private key. The chain comparison alone accepts
	// this — file and log agree — which is precisely what the signature check is for.
	lie := sealedRows(t, 5, "test.lie")
	lieHead, lieCount, err := auditverify.NewStoreChain(lie).Head()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cp auditverify.Checkpoint
	for _, e := range entries {
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if jerr := json.Unmarshal(raw, &cp); jerr != nil {
			continue
		}
		cp.Head = hex.EncodeToString(lieHead)
		cp.Count = lieCount
		body, merr := json.Marshal(cp)
		if merr != nil {
			t.Fatal(merr)
		}
		if werr := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}

	res, err = w.VerifyAnchor()
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("a rewritten checkpoint passed the continuous verification; the hourly " +
			"check is exactly the window the signature closes")
	}
	// The failure must be ATTRIBUTED to the signature: "the chain does not match" is a
	// different investigation from "this file is not the one that was published".
	signatureProblem := false
	for _, p := range res.Problems {
		if strings.Contains(p, "signature") || strings.Contains(p, "NOT signed") {
			signatureProblem = true
		}
	}
	if !signatureProblem {
		t.Fatalf("the failure was reported, but not as a signature problem: %v", res.Problems)
	}
}

// TestContinuousAnchorVerifyWithoutKeyIsUnchanged pins the nil case: a deployment that never
// configured a key keeps the keyless behaviour, not a wall of "not signed" failures.
func TestContinuousAnchorVerifyWithoutKeyIsUnchanged(t *testing.T) {
	truth := sealedRows(t, 4, "test.unsigned")
	meta := store.NewMemory()
	w := worker.New(meta, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := t.TempDir()
	if err := w.EnableAnchoring(dir, truth); err != nil {
		t.Fatal(err)
	}
	if err := w.AnchorNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := w.VerifyAnchor()
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("an unsigned deployment must keep verifying without a key: %v", res.Problems)
	}
}
