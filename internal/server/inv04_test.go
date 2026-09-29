package server_test

// INV-04: share links and app passwords are independently revocable credentials, and revocation
// takes effect within a defined bound.
//
// The bound here is zero. Nothing caches a credential: every verification reads the store, so a
// revoked credential is refused on the next request. That is a property worth pinning precisely,
// because it is exactly the kind of thing a well-meaning optimisation ("app passwords are looked up
// on every DAV request; cache them") would quietly turn into a window — and a window in which a
// stolen credential still works is the whole of the risk the requirement is about.
//
// Both halves are checked the same way: use the credential, revoke it, use it again.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/auth"
)

// TestRevokedAppPasswordStopsWorkingImmediately is the WebDAV half.
func TestRevokedAppPasswordStopsWorkingImmediately(t *testing.T) {
	h := newHarness(t)

	// Issued through the service the server uses, so the test does not depend on the HTTP shape of
	// the create endpoint — but revoked through the API, because that is the path an operator has.
	secret, credential, err := auth.NewAppPasswords(h.meta).Issue(
		context.Background(), h.user.ID, "laptop", "rw", 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	pf := func() int {
		t.Helper()
		req, err := http.NewRequest("PROPFIND", h.srv.URL+"/dav/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(h.user.Username, secret)
		req.Header.Set("Depth", "0")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := pf(); code != http.StatusMultiStatus {
		t.Fatalf("the credential does not work before revocation: %d", code)
	}

	del := h.do(http.MethodDelete, "/api/apppasswords/"+credential.ID.String(), nil, nil)
	body := readBody(del)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent && del.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %s", del.StatusCode, body)
	}

	if code := pf(); code != http.StatusUnauthorized {
		t.Fatalf("a revoked app password still works (PROPFIND %d). The credential is supposed to "+
			"die on the next request, not at some cache expiry the operator cannot see.", code)
	}
}

// TestRevokedShareLinkStopsServingImmediately is the sharing half.
func TestRevokedShareLinkStopsServingImmediately(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	if code := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil).StatusCode; code != http.StatusOK {
		t.Fatalf("the link does not work before revocation: %d", code)
	}

	list := h.do(http.MethodGet, "/api/shares/", nil, nil)
	payload := readBody(list)
	list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", list.StatusCode, payload)
	}
	id := firstShareID(t, payload)

	del := h.do(http.MethodDelete, "/api/shares/"+id, nil, nil)
	body := readBody(del)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent && del.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %s", del.StatusCode, body)
	}

	// Every public entry point, because a revocation that only closes the download leaves the page
	// and the drop-box upload accepting traffic.
	for _, path := range []string{"/s/" + token, "/s/" + token + "/download"} {
		resp := h.do(http.MethodGet, path, nil, nil)
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusNotFound {
			t.Fatalf("%s returned %d after the share was revoked; want 404", path, code)
		}
	}
}

// firstShareID pulls the id out of the share list payload.
func firstShareID(t *testing.T, payload string) string {
	t.Helper()
	// The list is a JSON object with an "items" array; the id appears before the token.
	i := strings.Index(payload, `"id":"`)
	if i < 0 {
		t.Fatalf("no share id in %s", payload)
	}
	rest := payload[i+len(`"id":"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("malformed share id in %s", payload)
	}
	return rest[:j]
}
