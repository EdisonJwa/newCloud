package dav

// ID-06 / WC-GAP-001 on the WebDAV path.
//
// This is the path that matters most and had no test. A browser session at least forces an OIDC
// round trip when it expires; a WebDAV App Password does not. A desktop client holds one for months,
// authenticates with HTTP Basic on every request, and never talks to the IdP again — so "a role
// change applies at the next login" means it applies never, for exactly the credential most likely to
// outlive a person's access.
//
// The handler cannot know the window itself (it has no config for it and must agree with the session
// path), so it takes an injected predicate. Two things therefore need proving: that a stale account
// is refused, and that the refusal is a Basic challenge rather than a bare error, because a client
// that is not challenged cannot re-authenticate and will simply fail forever.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// withRevalidation attaches a staleness predicate to the handler.
func (e *testEnv) withRevalidation(fn func(store.User) bool) *testEnv {
	e.h = e.h.WithRevalidation(fn)
	return e
}

// TestAStaleCredentialIsChallengedOnWebDAV is the gap.
//
// The account is active and the App Password is valid; only the IdP confirmation is old. A
// WebDAV-only user in that state previously kept full access indefinitely.
func TestAStaleCredentialIsChallengedOnWebDAV(t *testing.T) {
	e := newEnv(t)
	e.withRevalidation(func(store.User) bool { return true })

	rec := e.do(t, http.MethodGet, e.davPath("anything.txt"), nil, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a stale credential was accepted on WebDAV (%d). An App Password is the "+
			"credential that never revisits the IdP, so this is the path where an unbounded "+
			"revocation window actually bites.", rec.Code)
	}
	// The challenge is what makes the refusal recoverable. Without it a client has no way to
	// re-authenticate and will fail the same way forever.
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(strings.ToLower(got), "basic") {
		t.Fatalf("the refusal did not send a Basic challenge (WWW-Authenticate=%q); the client "+
			"cannot prompt or re-authenticate and simply breaks", got)
	}
}

// TestAFreshCredentialIsAcceptedOnWebDAV: the bound must not break ordinary clients.
func TestAFreshCredentialIsAcceptedOnWebDAV(t *testing.T) {
	e := newEnv(t)
	e.withRevalidation(func(store.User) bool { return false })

	if rec := e.do(t, http.MethodPut, e.davPath("fresh.txt"),
		strings.NewReader("ok"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("a confirmed credential must work normally, got %d", rec.Code)
	}
}

// TestNoPredicateMeansNoCheck: nil disables the bound.
//
// This is the wiring contract for a deployment that has not configured a window. A handler that
// treated a nil predicate as "everything is stale" would lock out every WebDAV client on upgrade.
func TestNoPredicateMeansNoCheck(t *testing.T) {
	e := newEnv(t) // no WithRevalidation call at all

	if rec := e.do(t, http.MethodPut, e.davPath("plain.txt"),
		strings.NewReader("ok"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("with no revalidation predicate configured, WebDAV must work as before, got %d",
			rec.Code)
	}
}

// TestThePredicateSeesTheAccountBeingChecked pins what the hook is given.
//
// The predicate receives the resolved account, not the credential. A handler that passed a
// fabricated user (or the zero value) would make every deployment look either permanently stale or
// permanently fresh, depending on which way the predicate defaults — both silently wrong.
func TestThePredicateSeesTheAccountBeingChecked(t *testing.T) {
	e := newEnv(t)

	var seen store.User
	e.withRevalidation(func(u store.User) bool {
		seen = u
		return false
	})

	if rec := e.do(t, http.MethodPut, e.davPath("seen.txt"),
		strings.NewReader("ok"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}
	if seen.ID != e.user.ID {
		t.Fatalf("the predicate was given account %s, want %s — it is checking the wrong row, so "+
			"the window applies to somebody else or to nobody", seen.ID, e.user.ID)
	}
}

// TestAStaleCredentialIsRefusedBeforeTheRequestTouchesAnything.
//
// The check must run at authentication, not after the operation has been authorized: a stale
// credential that reaches the store would still be able to probe paths and, on a write, change data
// before the refusal.
func TestAStaleCredentialIsRefusedBeforeTheRequestTouchesAnything(t *testing.T) {
	e := newEnv(t)
	e.withRevalidation(func(store.User) bool { return true })

	rec := e.do(t, "PROPFIND", e.davPath(""), nil, map[string]string{"Depth": "1"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a stale credential reached the metadata layer (%d); the refusal must happen at "+
			"authentication so the request cannot read or write anything first", rec.Code)
	}
}

// TestAStaleCredentialCannotDelete is the damaging case, asserted separately.
//
// A refused listing is an annoyance; a refused delete that had already run is data loss. Keeping it
// as its own test means a future refactor that moves the check later fails here specifically.
func TestAStaleCredentialCannotDelete(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPut, e.davPath("keeper.txt"),
		strings.NewReader("must survive"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}

	e.withRevalidation(func(store.User) bool { return true })

	if rec := e.do(t, http.MethodDelete, e.davPath("keeper.txt"), nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a stale credential got past authentication on DELETE (%d)", rec.Code)
	}

	// Now let it through and confirm the file is still there — the refusal must not have deleted it.
	e.withRevalidation(func(store.User) bool { return false })
	got := e.do(t, http.MethodGet, e.davPath("keeper.txt"), nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("the file was removed by the refused DELETE (%d)", got.Code)
	}
	if got.Body.String() != "must survive" {
		t.Fatalf("the content changed: %q", got.Body.String())
	}
}

var _ = context.Background
var _ = httptest.NewRecorder
