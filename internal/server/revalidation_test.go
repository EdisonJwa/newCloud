package server_test

// ID-06 / WC-GAP-001: bounded identity revalidation.
//
// The problem, stated exactly: role and group membership are written to the users row only during
// the OIDC callback, because that is the only place the IdP's claims are read. A person removed from
// the admin group who never signs in again keeps the stored role, and the session (and any WebDAV App
// Password) they already hold keeps exercising it. "Effective at the next login" is not a bound when
// the next login may never come.
//
// auth.revalidate_minutes converts that into a documented window: past it, a credential is refused
// and the only way back in is an interactive sign-in, which re-reads the claims. These tests pin both
// halves — that the bound applies, and that it does not apply to the accounts for which it would be
// wrong.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// harnessWithRevalidation builds a harness with the window configured.
func harnessWithRevalidation(t *testing.T, minutes int) *harness {
	t.Helper()
	return newHarnessTuned(t, func(c *config.Config) {
		c.Auth.RevalidateMinutes = minutes
	}, nil)
}

// linkIdPIdentity gives the harness account an IdP identity, which is what makes the revalidation
// rules apply to it. Accounts with no IdP identity are exempt by design.
func linkIdPIdentity(t *testing.T, h *harness, id uuid.UUID) {
	t.Helper()
	// UpsertIdPUser matches on (issuer, subject), so the existing row is refreshed in place rather
	// than duplicated — the account keeps its id, which the harness's session depends on.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer:   "https://idp.example.test",
			Subject:  "subject-" + id.String(),
			Username: h.user.Username,
			Email:    h.user.Email,
			Role:     "user",
		})
		return err
	}); err != nil {
		t.Fatalf("link IdP identity: %v", err)
	}
}

// loadUser reads the account row.
func loadUser(t *testing.T, h *harness, id uuid.UUID) store.User {
	t.Helper()
	var u store.User
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.UserByID(ctx, id)
		u = got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestAnIdPAccountWithNoConfirmationIsRefusedWhenAWindowIsSet.
//
// Nil IdPConfirmedAt is the migration case: the column arrives empty, and "we have never confirmed
// this identity" is the honest reading. Treating nil as "confirmed just now" would mean the window
// silently did not apply to every pre-existing account on the deployment that adopted it.
func TestAnIdPAccountWithNoConfirmationIsRefusedWhenAWindowIsSet(t *testing.T) {
	h := harnessWithRevalidation(t, 30)
	linkIdPIdentity(t, h, h.user.ID)

	// UpsertIdPUser stamps the confirmation, so clear it to model a row that predates the column.
	if err := h.meta.BackdateIdPConfirmation(h.user.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// A zero time is non-nil; the predicate must treat it as unconfirmed rather than as ancient
	// history that happens to be far in the past — which, arithmetically, is the same answer, so the
	// distinction is only about intent.
	u := loadUser(t, h, h.user.ID)
	if !h.srvServer.CredentialIsStaleForTest(u) {
		t.Fatal("an account confirmed at the zero time must be treated as stale")
	}
}

// TestARecentlyConfirmedAccountIsAllowed: the bound must not lock out everyone.
func TestARecentlyConfirmedAccountIsAllowed(t *testing.T) {
	h := harnessWithRevalidation(t, 30)
	linkIdPIdentity(t, h, h.user.ID) // UpsertIdPUser stamps confirmation as now

	resp := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a just-confirmed account was refused (%d); the window would log everyone out "+
			"on every request", resp.StatusCode)
	}
}

// TestAStaleIdPAccountIsRefused is the gap: the credential outlived its confirmation.
func TestAStaleIdPAccountIsRefused(t *testing.T) {
	h := harnessWithRevalidation(t, 30)
	linkIdPIdentity(t, h, h.user.ID)

	// Age the confirmation past the window without touching anything else. This is exactly the state
	// of an account whose group membership changed in the IdP while the session stayed open.
	if err := h.meta.BackdateIdPConfirmation(h.user.ID, time.Now().Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a credential whose confirmation aged past the window was still accepted (%d). "+
			"ID-06 asks for a bounded revocation window; this is the only mechanism that bounds it "+
			"without an IdP event feed.", resp.StatusCode)
	}
}

// TestTheWindowCanBeDisabled keeps the previous behaviour available and explicit.
func TestTheWindowCanBeDisabled(t *testing.T) {
	h := harnessWithRevalidation(t, 0)
	linkIdPIdentity(t, h, h.user.ID)
	if err := h.meta.BackdateIdPConfirmation(h.user.ID, time.Now().Add(-365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with revalidate_minutes=0 the old behaviour must hold (%d); a deployment that "+
			"has not opted into periodic re-authentication must not get it by default",
			resp.StatusCode)
	}
}

// TestBreakglassIsExemptFromTheWindow.
//
// Break-glass exists to be usable when the IdP is unavailable — which is exactly when no
// confirmation is possible. Expiring it would put the recovery credential behind the system that is
// broken, so the exemption is a property of the design rather than a convenience.
func TestBreakglassIsExemptFromTheWindow(t *testing.T) {
	h := harnessWithRevalidation(t, 30)
	if err := h.meta.MarkBreakglassForTest(h.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.meta.BackdateIdPConfirmation(h.user.ID, time.Now().Add(-365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	u := loadUser(t, h, h.user.ID)
	if !u.IsBreakglass {
		t.Fatal("the fixture did not mark the account break-glass")
	}
	if h.srvServer.CredentialIsStaleForTest(u) {
		t.Fatal("a break-glass account was treated as stale. It has no IdP to confirm against, " +
			"and expiring it would lock the recovery path behind the system it exists to recover from.")
	}
}

// TestAnAccountWithoutAnIdPIdentityIsExempt: a management-API account has no claims to re-derive.
func TestAnAccountWithoutAnIdPIdentityIsExempt(t *testing.T) {
	h := harnessWithRevalidation(t, 30)

	u := loadUser(t, h, h.user.ID)
	if u.IdPIssuer != "" || u.IdPSubject != "" {
		t.Fatalf("the fixture is wrong: expected no IdP identity, got %q/%q", u.IdPIssuer, u.IdPSubject)
	}
	if h.srvServer.CredentialIsStaleForTest(u) {
		t.Fatal("an account with no IdP identity was treated as stale. Its role comes from an " +
			"operator, not from a claim, so there is nothing to revalidate.")
	}
}
