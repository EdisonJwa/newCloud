package server_test

// ID-03: JWKS key rotation, end to end.
//
// The requirement is "OIDC Discovery、JWKS 密钥轮换和验证失败策略" — discovery, key rotation, and a
// verification-failure policy. Rotation was previously untestable: the fake IdP had one fixed key
// and a hardcoded kid, so "the client follows rotation" could not be told apart from "the client
// cached the first JWKS and never needed another".
//
// These drive the real login flow, so they prove the whole path — discovery, JWKS fetch, signature
// verification, session minting — against a rotating IdP.

import (
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/fakeidp"
)

func rotationHarness(t *testing.T) *oidcHarness {
	t.Helper()
	return newOIDCHarness(t, fakeidp.User{
		Subject: "idp-subject-1", Username: "alice",
		Email: "alice@example.test", Name: "Alice Example",
		Groups: []string{"cloud-users"},
	})
}

// TestID03LoginSurvivesKeyRotation is the requirement proper.
//
// The IdP rotates its signing key between two logins. The second must succeed without restarting
// the server: go-oidc refetches the JWKS when it encounters an unknown kid, and this pins that
// the refetch happens. Against a single-key IdP this test cannot fail, which is why the rotation
// hook had to exist before the requirement could be called verified.
func TestID03LoginSurvivesKeyRotation(t *testing.T) {
	h := rotationHarness(t)

	h.login(t)
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("first login: %d", code)
	}

	if err := h.idp.RotateKey(); err != nil {
		t.Fatal(err)
	}

	// A second login as a different account, so a cached session cannot make it pass.
	h.idp.SetUser(fakeidp.User{
		Subject: "idp-subject-2", Username: "bob",
		Email: "bob@example.test", Name: "Bob Example",
		Groups: []string{"cloud-users"},
	})
	h.login(t)

	code, body := h.b.me()
	if code != http.StatusOK {
		t.Fatalf("login after key rotation must succeed without a restart, got %d", code)
	}
	if body["username"] != "bob" {
		t.Fatalf("the post-rotation login asserted the wrong account: %v", body)
	}
}

// TestID03ExistingSessionSurvivesRotation is the interaction with ID-07.
//
// A session is our own signed credential; verifying it never consults the IdP. IdP key rotation
// therefore must not disturb anyone who is already signed in — the alternative would log every
// user out on a routine IdP operation.
func TestID03ExistingSessionSurvivesRotation(t *testing.T) {
	h := rotationHarness(t)
	h.login(t)

	if err := h.idp.RotateKey(); err != nil {
		t.Fatal(err)
	}

	code, body := h.b.me()
	if code != http.StatusOK {
		t.Fatalf("an existing session must survive IdP key rotation, got %d", code)
	}
	if body["username"] != "alice" {
		t.Fatalf("the session changed identity across rotation: %v", body)
	}
}

// TestID03LoginSurvivesFullKeyWithdrawal.
//
// Retiring every previous key and rotating again leaves the client holding a JWKS that knows none
// of the current keys. A correct client refetches; one that trusts a stale cache stops working.
func TestID03LoginSurvivesFullKeyWithdrawal(t *testing.T) {
	h := rotationHarness(t)
	h.login(t)

	h.idp.RetireOldKeys()
	if err := h.idp.RotateKey(); err != nil {
		t.Fatal(err)
	}

	h.idp.SetUser(fakeidp.User{
		Subject: "idp-subject-3", Username: "carol",
		Email: "carol@example.test", Name: "Carol Example",
		Groups: []string{"cloud-users"},
	})
	h.login(t)

	code, body := h.b.me()
	if code != http.StatusOK {
		t.Fatalf("a login after full key withdrawal must succeed via a fresh JWKS, got %d", code)
	}
	if body["username"] != "carol" {
		t.Fatalf("wrong account after withdrawal: %v", body)
	}
}
