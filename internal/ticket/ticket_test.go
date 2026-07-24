package ticket

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newIssuer(t *testing.T) *Issuer {
	t.Helper()
	// 32 bytes of a fixed value; the test is about formatting, not entropy.
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	is, err := NewIssuer(secret)
	if err != nil {
		t.Fatal(err)
	}
	return is
}

func TestRoundTrip(t *testing.T) {
	is := newIssuer(t)
	c := Claims{
		Subject: "user-123",
		ID:      "jti-abc",
		Expiry:  time.Now().Add(time.Hour),
		Extra:   map[string]string{"scope": "ro"},
	}
	tok, err := is.Mint(PurposeSession, c)
	if err != nil {
		t.Fatal(err)
	}

	got, err := is.Verify(PurposeSession, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != c.Subject || got.ID != c.ID {
		t.Fatalf("claims mismatch: %+v", got)
	}
	if got.Extra["scope"] != "ro" {
		t.Fatalf("extra lost: %+v", got.Extra)
	}
}

// TestCrossPurposeReplay is the reason domain separation exists: a token minted
// for a one-shot download must not be usable as a login session, even though
// both are HMAC-signed by the same master secret.
func TestCrossPurposeReplay(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(PurposeDownload, Claims{Subject: "blob-1", Expiry: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := is.Verify(PurposeSession, tok); err == nil {
		t.Fatal("download token must not verify as a session")
	}
	if _, err := is.Verify(PurposeAppPassword, tok); err == nil {
		t.Fatal("download token must not verify as an app password")
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(PurposeSession, Claims{Subject: "user-1", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected format: %q", tok)
	}

	// Swap the subject for another user while keeping the original MAC.
	forged, err := is.Mint(PurposeSession, Claims{Subject: "user-2", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	forgedParts := strings.Split(forged, ".")
	parts[1] = forgedParts[1]
	tampered := strings.Join(parts, ".")

	if _, err := is.Verify(PurposeSession, tampered); err == nil {
		t.Fatal("payload substitution must fail MAC verification")
	}
}

func TestExpiry(t *testing.T) {
	is := newIssuer(t)
	is.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	tok, err := is.Mint(PurposeSession, Claims{
		Subject: "u",
		Issued:  is.now(),
		Expiry:  is.now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := is.Verify(PurposeSession, tok); err != nil {
		t.Fatalf("fresh token should verify: %v", err)
	}

	is.now = func() time.Time { return time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC) }
	if _, err := is.Verify(PurposeSession, tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

// TestSeparatorInSubjectCannotForgeFields guards the framing: the claims
// encoding uses 0x1f as a separator, so a subject containing it must not be
// able to inject an Extra entry.
func TestSeparatorInSubjectCannotForgeFields(t *testing.T) {
	is := newIssuer(t)
	// Attempt to inject "admin=true" as if it were an Extra field.
	tok, err := is.Mint(PurposeSession, Claims{
		Subject: "user\x1fadmin=true",
		Expiry:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := is.Verify(PurposeSession, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Extra["admin"]; ok {
		t.Fatal("separator injection forged an Extra claim")
	}
}

func TestShortSecretRejected(t *testing.T) {
	if _, err := NewIssuer([]byte("too-short")); err == nil {
		t.Fatal("short master secret must be rejected")
	}
}

func TestGarbageRejected(t *testing.T) {
	is := newIssuer(t)
	for _, s := range []string{"", "a", "a.b", "a.b.c.d", "session.!!!.???"} {
		if _, err := is.Verify(PurposeSession, s); err == nil {
			t.Fatalf("garbage token %q accepted", s)
		}
	}
}
