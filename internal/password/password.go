// Package password implements Argon2id hashing for human-chosen secrets.
//
// It exists as its own package because it is used by two subsystems that otherwise
// have nothing to do with each other: share passwords and the break-glass local
// administrator. Before it was extracted, the only copy lived inside the share
// service, which meant a login credential would have had to import the sharing layer
// — and the obvious workaround, a second copy of the KDF code, is how two places end
// up with different parameters and one of them silently weaker.
//
// The distinction this package encodes: a human-typed secret gets a slow KDF; a
// machine-generated 256-bit token does not. App passwords and session tokens are in
// the second category and must not use this — Argon2 on every WebDAV request would add
// tens of milliseconds for no security benefit.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Params configures the KDF. The zero value is not usable; use Default or Test.
type Params struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	KeyLen  uint32
}

// Default is the production cost: 64 MiB, 3 passes, 4 lanes.
//
// Chosen so a single verification costs tens of milliseconds on a server CPU — slow
// enough that offline guessing is impractical, fast enough that a login is not
// noticeably delayed.
func Default() Params {
	return Params{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32}
}

// Test is deliberately weak so a test suite does not spend its time hashing.
//
// It must never be selected outside `_test.go`; the config path that chooses it is the
// only place that knows which it is.
func Test() Params {
	return Params{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32}
}

// ErrMalformed is returned for a stored hash that cannot have been produced here.
var ErrMalformed = errors.New("password: malformed hash")

// saltLen is the stored salt prefix length. It is part of the format: an existing hash
// cannot be re-read if this changes.
const saltLen = 16

// Hash derives a storable representation: salt || key.
//
// The salt travels with the key rather than in a separate column. That keeps a hash a
// single opaque value, so it can move between columns, files and test fixtures without
// a second field nobody remembers to carry.
func Hash(secret string, p Params) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("password: empty secret")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(secret), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return append(salt, key...), nil
}

// Verify reports whether secret matches a stored hash.
//
// Comparison is constant time. The value being compared is a KDF output rather than
// the secret itself, so a timing leak would be hard to exploit — but the habit should
// not depend on that reasoning being right.
func Verify(secret string, stored []byte, p Params) bool {
	if len(stored) <= saltLen {
		return false
	}
	salt, want := stored[:saltLen], stored[saltLen:]
	got := argon2.IDKey([]byte(secret), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// VerifyHex decodes a hex-encoded hash and verifies it.
//
// The break-glass hash lives in a file, and a file is most easily produced and
// inspected in hex. Accepting the format here keeps the "how do I generate one"
// question answerable from the command line.
func VerifyHex(secret, hexHash string, p Params) (bool, error) {
	raw, err := decodeHex(hexHash)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return Verify(secret, raw, p), nil
}
