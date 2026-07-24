package auth

import (
	"testing"

	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// newTestIssuer builds a ticket issuer with a fixed 32-byte secret. The tests
// exercise formatting and verification, not entropy.
func newTestIssuer(t *testing.T) *ticket.Issuer {
	t.Helper()
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	is, err := ticket.NewIssuer(secret)
	if err != nil {
		t.Fatal(err)
	}
	return is
}
