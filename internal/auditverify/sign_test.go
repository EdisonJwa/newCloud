package auditverify

// Signed checkpoints (§10.2).
//
// These tests are about the ONE property a signature adds over an unsigned file: an attacker who can
// write to the anchor directory but does not hold the private key cannot make a checkpoint verify. The
// tamper test is therefore the whole point — a signing feature whose tamper case passes is a
// decoration.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignedCheckpointsVerify(t *testing.T) {
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

	day := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := a.PublishWithCount(day, []byte("0123456789abcdef0123456789abcdef"), 42); err != nil {
		t.Fatal(err)
	}

	cps, err := a.Checkpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 {
		t.Fatalf("got %d checkpoints", len(cps))
	}
	if cps[0].Signature == "" {
		t.Fatal("the checkpoint was published unsigned despite a configured signer")
	}
	if err := VerifyCheckpointSet(signer.PublicKey(), cps); err != nil {
		t.Fatalf("a freshly signed checkpoint did not verify: %v", err)
	}
}

func TestAModifiedCheckpointFailsVerification(t *testing.T) {
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
	day := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	if err := a.PublishWithCount(day, []byte("0123456789abcdef0123456789abcdef"), 7); err != nil {
		t.Fatal(err)
	}

	// Rewrite the count on disk, the way somebody covering their tracks would: the checkpoint still
	// looks well-formed, still links to its predecessor, and is now a lie about how many events
	// existed.
	path := filepath.Join(dir, "2026-04-02.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := replaceOnce(t, string(raw), `"count": 7`, `"count": 3`)
	if tampered == string(raw) {
		t.Fatal("the tamper did not apply; this test would prove nothing")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	cps, err := a.Checkpoints()
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyCheckpointSet(signer.PublicKey(), cps)
	if err == nil {
		t.Fatal("a checkpoint edited after publication still verified; the signature covers " +
			"nothing useful")
	}
}

func TestUnsignedCheckpointIsRefusedWhenAVerifierExpectsSignatures(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Published WITHOUT a signer, as an older deployment would have.
	a, err := NewFileAnchor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
		[]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	cps, err := a.Checkpoints()
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyCheckpointSet(signer.PublicKey(), cps)
	if err == nil {
		t.Fatal("an unsigned checkpoint verified against a public key; a verifier that accepts " +
			"unsigned files can be bypassed by deleting the signature")
	}
	// The MESSAGE matters: "not signed" is an operator problem (the signer was never configured),
	// "signature mismatch" is an incident (somebody edited the file). Reporting the first as the
	// second sends the wrong person to look.
	if !strings.Contains(err.Error(), "NOT signed") {
		t.Fatalf("an unsigned checkpoint was reported as a mismatch rather than as unsigned: %v", err)
	}
}

func TestARemovedCheckpointBreaksTheChain(t *testing.T) {
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

	for i := 0; i < 3; i++ {
		day := time.Date(2026, 4, 5+i, 0, 0, 0, 0, time.UTC)
		head := make([]byte, 32)
		head[0] = byte(i + 1)
		if err := a.Publish(day, head); err != nil {
			t.Fatal(err)
		}
	}

	// Delete the middle day. Every remaining file is correctly signed by the real key; only the
	// PreviousHead link shows that something is missing.
	if err := os.Remove(filepath.Join(dir, "2026-04-06.json")); err != nil {
		t.Fatal(err)
	}
	cps, err := a.Checkpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 2 {
		t.Fatalf("expected two surviving checkpoints, got %d", len(cps))
	}
	if err := VerifyCheckpointSet(signer.PublicKey(), cps); err == nil {
		t.Fatal("a deleted checkpoint left the remaining set verifying; the chain is what detects " +
			"removal, because a missing file signs nothing")
	}
}

func TestSigningKeyRoundTripsThroughItsFileForm(t *testing.T) {
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signing-key")
	if err := os.WriteFile(path, []byte(signer.SeedBase64()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PublicKeyBase64() != signer.PublicKeyBase64() {
		t.Fatalf("the key loaded from disk has a different public half: %s vs %s",
			loaded.PublicKeyBase64(), signer.PublicKeyBase64())
	}
	// And a key file that is not a seed is refused rather than silently producing a different key.
	bad := filepath.Join(t.TempDir(), "bad-key")
	if err := os.WriteFile(bad, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(bad); err == nil {
		t.Fatal("a malformed key file was accepted")
	}
}

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + repl + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
