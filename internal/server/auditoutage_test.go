package server_test

// T-16 at the HTTP layer: what a client sees when the audit log cannot be written.
//
// The policy is fail-closed (deploy/audit-failure-policy.md): the change is refused and nothing is
// modified. What matters for a client is that this is distinguishable from "something went
// wrong" — a 500 sends an operator to inspect the data, and the data is fine.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestAuditOutageReturns503.
func TestAuditOutageReturns503(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "existing.txt", "original")

	// Everything from here on cannot be audited.
	h.meta.SetAuditFault(errors.New("no space left on device"))

	resp := h.do(http.MethodDelete, "/api/files?path=existing.txt", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an unauditable write must be 503, got %d", resp.StatusCode)
	}

	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "audit_unavailable" {
		t.Fatalf(`error code must be "audit_unavailable", got %q`, body.Error)
	}
	// The message has to say the change did not happen, or the client cannot tell whether to
	// retry the same operation or check whether it half-applied.
	if !strings.Contains(body.Message, "refused") && !strings.Contains(body.Message, "nothing") {
		t.Fatalf("the message must state that nothing changed, got %q", body.Message)
	}

	// And the file really is still there: fail-closed means no partial state.
	check := h.do(http.MethodGet, "/api/files/download?path=existing.txt", nil, nil)
	defer check.Body.Close()
	if check.StatusCode != http.StatusOK {
		t.Fatalf("the file must be untouched, got %d", check.StatusCode)
	}
}

// TestReadsWorkDuringAuditOutage is the other half of the policy: fail-closed applies to writes.
//
// Reads stay up deliberately. An operator investigating "writes are failing" needs the data and
// the audit log; turning this into a total outage would remove the tools needed to diagnose it.
func TestReadsWorkDuringAuditOutage(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "readable.txt", "content")

	h.meta.SetAuditFault(errors.New("audit: unavailable"))

	list := h.do(http.MethodGet, "/api/files?path=", nil, nil)
	list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("listing must keep working, got %d", list.StatusCode)
	}

	dl := h.do(http.MethodGet, "/api/files/download?path=readable.txt", nil, nil)
	dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download must keep working, got %d", dl.StatusCode)
	}
}

// TestUploadIsRefusedDuringAuditOutage: the upload path must also be fail-closed, not only delete.
func TestUploadIsRefusedDuringAuditOutage(t *testing.T) {
	h := newHarness(t)
	h.meta.SetAuditFault(errors.New("audit: unavailable"))

	mb := multipartBody(t, "", "blocked.txt", "should not land")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an unauditable upload must be 503, got %d", resp.StatusCode)
	}

	// Nothing landed.
	check := h.do(http.MethodGet, "/api/files?path=", nil, nil)
	defer check.Body.Close()
	var listing struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	_ = json.NewDecoder(check.Body).Decode(&listing)
	for _, e := range listing.Entries {
		if e.Name == "blocked.txt" {
			t.Fatal("the refused upload must not create a file")
		}
	}
}
