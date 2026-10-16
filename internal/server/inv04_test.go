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
	"github.com/EdisonJwa/newCloud/internal/store"
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

// TestShareDownloadIsAuditedOncePerLogicalDownload is §10.1 for the anonymous case.
//
// share.download was declared, had a method that wrote it, had tests for that method — and that
// method had no caller. So a public link served file content to anonymous visitors with no audit
// row at all, while the vocabulary said otherwise. The vocabulary scan could not notice: it looks for
// the action's name in non-test source, and the name was there, in unreachable code.
//
// The row is written once per CHARGED download. Range requests and retries join the existing session
// and must not add rows, or a video becomes hundreds of log entries per view and the real events
// become unfindable.
func TestShareDownloadIsAuditedOncePerLogicalDownload(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	audits := func() []store.AuditEntry {
		t.Helper()
		var out []store.AuditEntry
		for _, e := range h.meta.AuditLog() {
			if e.Action == "share.download" {
				out = append(out, e)
			}
		}
		return out
	}
	if n := len(audits()); n != 0 {
		t.Fatalf("the fixture already has %d share.download rows", n)
	}

	first := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil)
	body := readBody(first)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", first.StatusCode, body)
	}
	rows := audits()
	if len(rows) != 1 {
		t.Fatalf("an anonymous download through a public link wrote %d audit rows, want 1. "+
			"Without this row nobody can answer who took the file.", len(rows))
	}
	if rows[0].ShareID == nil {
		t.Fatal("the audit row does not identify the share")
	}
	if rows[0].ActorKind == "" || rows[0].ActorIP == "" {
		t.Fatalf("the row must record who and from where, got kind=%q ip=%q",
			rows[0].ActorKind, rows[0].ActorIP)
	}

	// A Range request continues the same paid download and must not be charged — or recorded — again.
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-1")
	rng, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rng.Body.Close()
	if n := len(audits()); n != 1 {
		t.Fatalf("a Range continuation added an audit row (%d total). One playback is one "+
			"download; recording every range buries the events that matter.", n)
	}
}

// TestADelegatedShareServes is SHARE-09's reachability half (转授权).
//
// A grantee holding `share` must be able to mint a link for somebody else's file, and that link must
// SERVE. Two independent defects made that impossible:
//
//   - the create endpoint resolved a PATH in the caller's own namespace, so a grantee's request failed
//     with "no such file" no matter what they held. The permission was storable, grantable, listable
//     and unreachable.
//   - the download path addressed the content store by the SHARE CREATOR's namespace. For an
//     owner-created share the creator and the node's owner are the same; for a delegated one the bytes
//     only exist under the node's owner, so the link 404'd even once it existed.
//
// The revocation half (a delegated link dying when the owner revokes the grant) is NOT asserted here.
// It currently passes, but a mutation that removes the creator-authorization check did not make it
// fail, so the mechanism is not identified yet and an assertion that passes for an unknown reason is
// not evidence. That is tracked as the next step rather than claimed here.
func TestADelegatedShareServes(t *testing.T) {
	h := newHarness(t)

	h.uploadFile(t, "delegated.txt", "delegated content")
	mate := h.meta.AddUser(store.User{Username: "mate", Email: "mate@example.test", Role: "user"})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	mateSession, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}

	nodes, err := h.meta.Resolve(context.Background(), h.user.ID, "delegated.txt")
	if err != nil {
		t.Fatal(err)
	}
	nodeID := nodes[len(nodes)-1].ID.String()

	// Ungranted: refused, and by the ACL rather than by the namespace.
	refused := h.doAs(t, mateSession, http.MethodPost, "/api/shares/",
		strings.NewReader(`{"node_id":"`+nodeID+`","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	refused.Body.Close()
	if refused.StatusCode == http.StatusCreated {
		t.Fatal("an ungranted account shared another user's file")
	}

	if code, body := h.grantAs(t, "delegated.txt", mate.Username,
		[]string{"read", "download", "share"}, nil); code != http.StatusCreated {
		t.Fatalf("grant: %d %+v", code, body)
	}

	create := h.doAs(t, mateSession, http.MethodPost, "/api/shares/",
		strings.NewReader(`{"node_id":"`+nodeID+`","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	createBody := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("a grantee holding share could not create a link by node id: %d %s",
			create.StatusCode, createBody)
	}
	token := createdToken(t, createBody)

	served := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil)
	servedBody := readBody(served)
	served.Body.Close()
	if served.StatusCode != http.StatusOK {
		t.Fatalf("a delegated share did not serve: %d %s", served.StatusCode, servedBody)
	}
	if servedBody != "delegated content" {
		t.Fatalf("the delegated share served %q", servedBody)
	}
}
