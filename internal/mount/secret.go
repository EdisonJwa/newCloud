package mount

// The credential cipher for mount secrets (ADR-0014).
//
// Mount credentials are live secrets to external systems. They are encrypted with
// AES-256-GCM under a key from a file, so a metadata backup alone cannot talk to the
// remote, and the ciphertext in the database is authenticated: a tampered row fails to
// open rather than dialing with altered bytes.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Cipher seals and opens mount credentials.
type Cipher struct {
	aead cipher.AEAD
}

// LoadKeyFile reads the hex-encoded 32-byte key. The format matches the break-glass
// password file: one hex string, optionally newline-terminated, mode 0600 expected (a
// warning-level expectation rather than a refusal, because Windows reports modes
// differently and the operator's threat model is not the filesystem's).
func LoadKeyFile(path string) (*Cipher, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mount: read secret file: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("mount: secret file is not hex: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("mount: secret file must hold exactly 32 bytes (64 hex characters) of key")
	}
	return NewCipher(key)
}

// NewCipher builds the cipher from a raw key. The key is not retained beyond the AEAD.
func NewCipher(key []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts secret. The nonce is random per secret and prepended; GCM's tag rides
// at the tail. Re-sealing the same secret yields different bytes, which is correct —
// equality of ciphertexts would be a tracker across re-saves.
func (c *Cipher) Seal(secret string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(secret), nil), nil
}

// Open decrypts what Seal produced. An error here means the row or the key changed since
// the seal; the caller must treat the mount as broken, not fall back to anything.
func (c *Cipher) Open(sealed []byte) (string, error) {
	if len(sealed) < c.aead.NonceSize() {
		return "", errors.New("mount: ciphertext shorter than a nonce")
	}
	ns := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("mount: credential does not open (wrong key or tampered row): %w", err)
	}
	return string(plain), nil
}
