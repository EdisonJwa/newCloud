package auditverify

// The continuous verifier's signature check (§10.2).
//
// Verify (no key) can only ask "does the anchor match the chain". When the checkpoint file
// and the chain are rewritten TOGETHER — the attacker holds the database and the anchor
// directory — that comparison passes, because it was answered by the two things the attacker
// owns. The signature is what breaks that agreement: rewriting the file also requires the
// private key. These tests construct exactly that coordinated rewrite and require the keyed
// verifier to reject what the keyless one accepted.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// fakeRows is a Chain built from in-memory events, so a test can present a DIFFERENT chain
// than the one the checkpoint was published over.
type fakeRows struct {
	events []audit.Event
}

func (f *fakeRows) AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error) {
	var out []audit.Event
	for _, e := range f.events {
		if e.ID > afterID {
			out = append(out, e)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRows) AuditHead(ctx context.Context) ([]byte, error) {
	if len(f.events) == 0 {
		return audit.GenesisHash, nil
	}
	return f.events[len(f.events)-1].Hash, nil
}

func sealChain(t *testing.T, n int, action string) *fakeRows {
	t.Helper()
	out := &fakeRows{}
	prev := audit.GenesisHash
	for i := 1; i <= n; i++ {
		e := audit.Event{
			ID: int64(i), Time: time.Date(2026, 5, 1, 0, 0, i, 0, time.UTC),
			Action: audit.Action(action), Result: audit.ResultOK, PrevHash: append([]byte(nil), prev...),
		}
		if err := e.Seal(); err != nil {
			t.Fatal(err)
		}
		out.events = append(out.events, e)
		prev = e.Hash
	}
	return out
}

// TestVerifySignedRejectsACoordinatedRewrite is the case only the signature can catch.
func TestVerifySignedRejectsACoordinatedRewrite(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a, err := NewFileAnchor(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.WithSigner(signer)

	truth := sealChain(t, 10, "test.truth")
	sc := NewStoreChain(truth)
	head, count, err := sc.Head()
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := a.PublishWithCount(day, head, count); err != nil {
		t.Fatal(err)
	}

	// The attacker owns the database and the anchor directory. They replace the log with a
	// different one, then rewrite the checkpoint to name the replacement's head and count.
	lie := sealChain(t, 10, "test.lie")
	lieHead, lieCount, err := NewStoreChain(lie).Head()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, day.Format("2006-01-02")+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatal(err)
	}
	cp.Head = hex.EncodeToString(lieHead)
	cp.Count = lieCount
	body, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// The keyless verifier accepts: the file agrees with the chain, which is all it can check.
	res, err := Verify(a, NewStoreChain(lie))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("the coordinated rewrite should pass the keyless check (that is the gap): %v", res.Problems)
	}

	// The keyed verifier refuses: the rewritten file is not the one that was published.
	res, err = VerifySigned(a, NewStoreChain(lie), signer.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("a checkpoint rewritten without the private key verified; the continuous " +
			"path is blind to exactly the attacker who can rewrite both copies")
	}
}

// TestVerifySignedAcceptsHonestSignedCheckpoints keeps the fix from failing every honest
// signed deployment on its first anchor tick.
func TestVerifySignedAcceptsHonestSignedCheckpoints(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a, err := NewFileAnchor(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.WithSigner(signer)

	chain := sealChain(t, 5, "test.honest")
	head, count, err := NewStoreChain(chain).Head()
	if err != nil {
		t.Fatal(err)
	}
	for _, day := range []time.Time{
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
	} {
		if err := a.PublishWithCount(day, head, count); err != nil {
			t.Fatal(err)
		}
	}

	res, err := VerifySigned(a, NewStoreChain(chain), signer.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("honest signed checkpoints must verify: %v", res.Problems)
	}
	if res.Matched != 2 {
		t.Fatalf("matched %d checkpoints, want 2", res.Matched)
	}
}

// TestVerifyWithWrongKeyFails: a signature check against a key that did not sign must fail,
// which is what a key rotation produces until the verifier is reconfigured.
func TestVerifyWithWrongKeyFails(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	other, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a, err := NewFileAnchor(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.WithSigner(signer)
	chain := sealChain(t, 3, "test.rotate")
	head, count, err := NewStoreChain(chain).Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PublishWithCount(time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC), head, count); err != nil {
		t.Fatal(err)
	}

	res, err := VerifySigned(a, NewStoreChain(chain), other.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("checkpoints from another key verified; a key mismatch is silent")
	}
}
