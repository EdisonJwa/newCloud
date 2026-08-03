package password_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/password"
)

// The KDF parameters matter as much as the algorithm: a hash produced with one cost
// and verified with another fails, and the failure looks like a wrong password. These
// tests pin the properties the callers depend on.

func TestHashAndVerifyRoundTrip(t *testing.T) {
	h, err := password.Hash("correct horse battery staple", password.Test())
	if err != nil {
		t.Fatal(err)
	}
	if !password.Verify("correct horse battery staple", h, password.Test()) {
		t.Fatal("a hash must verify against the secret that produced it")
	}
	if password.Verify("wrong", h, password.Test()) {
		t.Fatal("a different secret must not verify")
	}
}

// TestHashIsSalted: two hashes of the same secret must differ, or the stored value
// becomes a lookup table entry.
func TestHashIsSalted(t *testing.T) {
	a, err := password.Hash("same secret", password.Test())
	if err != nil {
		t.Fatal(err)
	}
	b, err := password.Hash("same secret", password.Test())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("two hashes of the same secret must differ (salted)")
	}
	// And each still verifies.
	for i, h := range [][]byte{a, b} {
		if !password.Verify("same secret", h, password.Test()) {
			t.Fatalf("hash %d does not verify", i)
		}
	}
}

// TestVerifyRejectsMalformed: a truncated or empty stored value must be a clean
// refusal, not a panic and not a match.
func TestVerifyRejectsMalformed(t *testing.T) {
	for _, stored := range [][]byte{nil, {}, make([]byte, 16)} {
		if password.Verify("anything", stored, password.Test()) {
			t.Fatalf("a %d-byte stored value must not verify", len(stored))
		}
	}
}

// TestParametersAreNotInterchangeable: a hash made with one cost must not verify under
// another. This is the property that makes "just use cheap params in tests" safe —
// the test side cannot accidentally validate a production hash.
func TestParametersAreNotInterchangeable(t *testing.T) {
	prod, err := password.Hash("secret", password.Default())
	if err != nil {
		t.Fatal(err)
	}
	if password.Verify("secret", prod, password.Test()) {
		t.Fatal("a production hash must not verify under test parameters")
	}
	// It verifies under the parameters it was made with.
	if !password.Verify("secret", prod, password.Default()) {
		t.Fatal("a production hash must verify under production parameters")
	}
}

// TestVerifyHexAcceptsTrailingNewline: the break-glass file is written by hand or by a
// shell redirect, and both add a newline. Rejecting it would make the one account that
// works during an outage fail for an invisible reason.
func TestVerifyHexAcceptsTrailingNewline(t *testing.T) {
	h, err := password.Hash("emergency", password.Test())
	if err != nil {
		t.Fatal(err)
	}
	encoded := hex.EncodeToString(h)

	for _, variant := range []string{encoded, encoded + "\n", encoded + "\r\n", "  " + encoded + "  \n"} {
		ok, verr := password.VerifyHex("emergency", variant, password.Test())
		if verr != nil {
			t.Fatalf("variant %q: %v", strings.TrimSpace(variant), verr)
		}
		if !ok {
			t.Fatalf("variant %q did not verify", strings.TrimSpace(variant))
		}
	}
}

// TestVerifyHexRejectsGarbage: a corrupted file must be an error the caller can report,
// not a silent "wrong password".
func TestVerifyHexRejectsGarbage(t *testing.T) {
	if _, err := password.VerifyHex("x", "not-hex-at-all", password.Test()); err == nil {
		t.Fatal("non-hex input must be an error")
	}
}

// TestEmptySecretIsRefused: hashing "" would create a credential that anybody can
// satisfy.
func TestEmptySecretIsRefused(t *testing.T) {
	if _, err := password.Hash("", password.Test()); err == nil {
		t.Fatal("an empty secret must not be hashable")
	}
}
