package server_test

// The limitations page must not lie (§10, documentation as evidence).
//
// docs/guides/limitations.md is the page an operator is told to trust over the README — "If a claim
// here conflicts with the README, this page wins". That authority is only worth something if the
// claims are true, and the failure mode is quiet: a capability gets implemented, the page keeps
// saying it is missing, and the next person either does not build it or does not use it.
//
// This round found exactly that. The audit-timeout policy was implemented and the page still listed
// it under "Current gaps" — a staleness I had created one round earlier and did not notice.
//
// Only claims that can be checked mechanically are asserted here; the rest of the page is prose and
// is not something a test can honestly adjudicate.

import (
	"net/http"
	"strings"
	"testing"
)

// TestLimitationsPageMatchesTheCode checks the specific factual claims that have a mechanical
// answer. Each one is a sentence in the document, not a paraphrase of it.
func TestLimitationsPageMatchesTheCode(t *testing.T) {
	h := newHarness(t)

	t.Run("REPORT is answered with an explicit 501", func(t *testing.T) {
		// The page says: "`REPORT` is answered with an explicit `501`."
		req, err := http.NewRequest("REPORT", h.srv.URL+"/dav/"+h.user.Username+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(h.user.Username, h.appPassword(t))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("the page says REPORT returns 501, the server returned %d. A client that "+
				"trusts the page will treat a different status as a different problem.",
				resp.StatusCode)
		}
	})

	t.Run("scim_token exists but there is no SCIM endpoint", func(t *testing.T) {
		// The page says: "A `scim_token` configuration key exists." and "No SCIM endpoint."
		//
		// The second half is the security-relevant one: a token that is accepted anywhere would be
		// a provisioning back door nobody documented.
		//
		// Asked of the ROUTE TABLE, not of an HTTP request. The first version probed
		// /scim/v2/Users and got 200 — because the SPA catch-all serves the shell for any
		// unmatched path, so "200" meant "no route", the opposite of what I was testing for. A
		// check that reads the same way to a pass and a failure is not a check.
		for _, r := range h.srvServer.Routes() {
			if strings.Contains(strings.ToLower(r), "scim") {
				t.Fatalf("route %q exists. The limitations page says there is no SCIM endpoint; if "+
					"one now exists it must be documented, not left to be discovered.", r)
			}
		}
	})
}

// appPassword issues one so a Basic-auth client can reach WebDAV.
func (h *harness) appPassword(t *testing.T) string {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/apppasswords/",
		strings.NewReader(`{"label":"limitations-check","scope":"rw"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("app password: %d %s", resp.StatusCode, readBody(resp))
	}
	secret, _ := decode(t, resp)["secret"].(string)
	if secret == "" {
		t.Fatal("no app password secret returned")
	}
	return secret
}
