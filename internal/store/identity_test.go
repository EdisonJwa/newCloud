package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestUpsertUserClaimsPreprovisionedAccount covers the case that broke the first
// real end-to-end run: an account created ahead of time (by SCIM, or by an admin)
// has a username but no IdP identity. The first login must claim that row rather
// than fail on the unique username index.
//
// The original implementation used a single INSERT ... ON CONFLICT on the IdP
// identity, which raised 23505 on users_username_key and returned a 500 to the
// user on their very first sign-in.
func TestUpsertUserClaimsPreprovisionedAccount(t *testing.T) {
	s := store.NewMemory()

	// Pre-provision without any IdP identity.
	pre := s.AddUser(store.User{
		Username: "dana", Email: "dana@example.test", Role: "user",
	})

	got, err := upsert(t, s, store.UpsertUser{
		Issuer: "https://id.example.test", Subject: "idp-dana",
		Username: "dana", Email: "dana@example.test",
		DisplayName: "Dana", Role: "user",
	})
	if err != nil {
		t.Fatalf("first login of a pre-provisioned account must succeed: %v", err)
	}
	if got.ID != pre.ID {
		t.Fatalf("the existing account must be claimed, got a new id %s (want %s)", got.ID, pre.ID)
	}
	if got.IdPSubject != "idp-dana" {
		t.Fatalf("the IdP identity must be bound to the account, got %q", got.IdPSubject)
	}

	// A returning login resolves by identity and stays the same row.
	again, err := upsert(t, s, store.UpsertUser{
		Issuer: "https://id.example.test", Subject: "idp-dana",
		Username: "dana", Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != pre.ID {
		t.Fatalf("returning login changed the account: %s -> %s", pre.ID, again.ID)
	}
}

// TestUpsertUserRejectsUsernameHijack: a different IdP subject must not be able to
// claim an account by presenting its username. That would be an account takeover
// with access to every file the victim owns.
func TestUpsertUserRejectsUsernameHijack(t *testing.T) {
	s := store.NewMemory()

	first, err := upsert(t, s, store.UpsertUser{
		Issuer: "https://id.example.test", Subject: "genuine",
		Username: "erin", Email: "erin@example.test", Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = upsert(t, s, store.UpsertUser{
		Issuer: "https://id.example.test", Subject: "attacker",
		Username: "erin", Role: "admin",
	})
	if err == nil {
		t.Fatal("a different subject must not claim an existing username")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}

	// The victim's record is untouched.
	var after store.User
	_ = s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UserByID(ctx, first.ID)
		after = u
		return uerr
	})
	if after.IdPSubject != "genuine" {
		t.Fatalf("the account identity was changed: %q", after.IdPSubject)
	}
	if after.Role != "user" {
		t.Fatalf("the account role was escalated: %q", after.Role)
	}
}

// TestUpsertUserDoesNotReactivateDisabledAccount: "disable" must survive a login.
func TestUpsertUserDoesNotReactivateDisabledAccount(t *testing.T) {
	s := store.NewMemory()

	u, err := upsert(t, s, store.UpsertUser{
		Issuer: "https://id.example.test", Subject: "frank",
		Username: "frank", Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.DisableUser(u.ID)

	if _, err := upsert(t, s, store.UpsertUser{
		Issuer: "https://id.example.test", Subject: "frank",
		Username: "frank", Role: "user",
	}); err == nil {
		t.Fatal("a disabled account must not be re-enabled by signing in")
	}
}

func upsert(t *testing.T, s *store.Memory, in store.UpsertUser) (store.User, error) {
	t.Helper()
	var out store.User
	err := s.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, in)
		out = u
		return uerr
	})
	return out, err
}
