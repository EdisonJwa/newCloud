package auditverify

// Signed checkpoints (§10.2 签署审计检查点).
//
// An unsigned checkpoint file proves that the chain head matched AT THE MOMENT THE FILE WAS READ — as
// long as nobody can write to the anchor directory. An attacker who reaches the anchor location can
// replace yesterday's file with a head that matches a chain they also rewrote, and the directory still
// looks consistent: the checkpoints link to each other, and every link is forged.
//
// A signature closes that. The private key stays where the anchor is WRITTEN (the application, or an
// operator's workstation for a manual export), and the public key goes wherever the checkpoints are
// READ — an auditor's machine, a monitoring system, a CI job. Rewriting a checkpoint then requires the
// private key, which never lives next to the files it signs.
//
// What this deliberately is not: a defence against an attacker who holds the private key. If the
// signing key is stored on the same volume as the anchor, the signature proves only that the same
// attacker signed twice. The documentation says so, and the startup warning repeats it when both are
// configured to the same directory.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Signer holds the key that signs published checkpoints.
type Signer struct {
	priv ed25519.PrivateKey
}

// LoadSigner reads an Ed25519 private key.
//
// Two encodings are accepted because two kinds of people write these files: the seed is base64 or hex
// (what `newcloud audit anchor-keygen` writes, and what a secret manager stores), and the file may also
// be a PEM PKCS#8 document from openssl. Accepting both is cheaper than telling an operator to convert
// a key by hand at the moment they are trying to enable signing.
func LoadSigner(path string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auditverify: read signing key: %w", err)
	}
	text := strings.TrimSpace(string(raw))

	if strings.Contains(text, "BEGIN") {
		// PEM: strip the armour and decode the DER, which for Ed25519 PKCS#8 ends with the 32-byte
		// seed after a fixed prefix. Rather than parse ASN.1 by hand, accept the base64 body and
		// take the last 32 bytes — the layout PKCS#8 always has for this algorithm.
		body := strings.Join(strings.Split(strings.ReplaceAll(strings.ReplaceAll(text,
			"-----BEGIN PRIVATE KEY-----", ""), "-----END PRIVATE KEY-----", ""), "\n"), "")
		der, derr := base64.StdEncoding.DecodeString(body)
		if derr != nil {
			return nil, fmt.Errorf("auditverify: decode PEM key: %w", derr)
		}
		if len(der) < ed25519.SeedSize {
			return nil, fmt.Errorf("auditverify: PEM key is too short for Ed25519")
		}
		return &Signer{priv: ed25519.NewKeyFromSeed(der[len(der)-ed25519.SeedSize:])}, nil
	}

	var seed []byte
	if b, herr := hex.DecodeString(text); herr == nil && len(b) == ed25519.SeedSize {
		seed = b
	} else if b, berr := base64.StdEncoding.DecodeString(text); berr == nil && len(b) == ed25519.SeedSize {
		seed = b
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("auditverify: signing key must be a %d-byte Ed25519 seed in base64 "+
			"or hex (got %d bytes)", ed25519.SeedSize, len(text))
	}
	return &Signer{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// PublicKey returns the key an auditor needs.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// PublicKeyBase64 returns the public key in the form the verification command prints.
func (s *Signer) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(s.PublicKey())
}

// Sign returns the signature for a checkpoint's canonical bytes.
func (s *Signer) Sign(cp Checkpoint) (string, error) {
	body, err := cp.canonical()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, body)), nil
}

// GenerateSigner returns a new signing key.
func GenerateSigner() (*Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Signer{priv: priv}, nil
}

// SeedBase64 returns the private seed, for writing to a file or a secret manager.
func (s *Signer) SeedBase64() string {
	return base64.StdEncoding.EncodeToString(s.priv.Seed())
}

// canonical is the byte string a signature covers.
//
// It excludes the signature itself, obviously, and fixes the field order by using a struct rather than
// a map: two encoders must produce the same bytes from the same checkpoint or a valid signature fails
// for a reason nobody can see.
func (cp Checkpoint) canonical() ([]byte, error) {
	bare := struct {
		Day          string `json:"day"`
		Head         string `json:"head_hex"`
		Count        int64  `json:"count"`
		PublishedAt  string `json:"published_at"`
		PreviousHead string `json:"previous_head_hex,omitempty"`
	}{
		Day:          cp.Day.UTC().Format("2006-01-02"),
		Head:         cp.Head,
		Count:        cp.Count,
		PublishedAt:  cp.PublishedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		PreviousHead: cp.PreviousHead,
	}
	return json.Marshal(bare)
}

// VerifySignature checks one checkpoint against a public key.
func VerifySignature(pub ed25519.PublicKey, cp Checkpoint) error {
	if cp.Signature == "" {
		return fmt.Errorf("checkpoint for %s is NOT signed; an unsigned checkpoint proves nothing "+
			"against someone who can write to the anchor directory",
			cp.Day.UTC().Format("2006-01-02"))
	}
	sig, err := base64.StdEncoding.DecodeString(cp.Signature)
	if err != nil {
		return fmt.Errorf("checkpoint for %s has a malformed signature: %w",
			cp.Day.UTC().Format("2006-01-02"), err)
	}
	body, err := cp.canonical()
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, body, sig) {
		return fmt.Errorf("checkpoint for %s does not match its signature: the file was modified "+
			"after it was published", cp.Day.UTC().Format("2006-01-02"))
	}
	return nil
}

// VerifyCheckpointSet checks signatures and the chain of checkpoints.
//
// The chain check is separate from the signatures because they answer different questions: a signature
// says "this checkpoint is the one that was published", and the chain says "no checkpoint was REMOVED
// between this one and its predecessor". A deleted day is signed by nobody and leaves no trace except
// a gap in PreviousHead.
func VerifyCheckpointSet(pub ed25519.PublicKey, cps []Checkpoint) error {
	if len(cps) == 0 {
		return nil
	}
	for i := range cps {
		if err := VerifySignature(pub, cps[i]); err != nil {
			return err
		}
	}
	for i := 1; i < len(cps); i++ {
		if cps[i].PreviousHead != cps[i-1].Head {
			return fmt.Errorf("checkpoint for %s does not link to %s: a checkpoint was removed or "+
				"re-ordered", cps[i].Day.UTC().Format("2006-01-02"),
				cps[i-1].Day.UTC().Format("2006-01-02"))
		}
	}
	return nil
}

// ParsePublicKey reads a base64 or hex Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	text := strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(text); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b), nil
	}
	if b, err := hex.DecodeString(text); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b), nil
	}
	return nil, fmt.Errorf("auditverify: a public key must be %d bytes in base64 or hex",
		ed25519.PublicKeySize)
}
