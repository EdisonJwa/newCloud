package server_test

// A create-only link must not disclose the folder's inventory (2026-10-24 review,
// H10). The download route already refuses this grant; the listing must agree with
// it — names, sizes and ETags are information about content the owner withheld.

import (
	"net/http"
	"strings"
	"testing"
)

func TestACreateOnlyLinkDoesNotListTheFolder(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "drop")
	uploadPath(t, h, "drop/confidential.pdf", "bytes the visitor must not learn exist")

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"drop","kind":"link","permissions":["create"]}`),
		map[string]string{"Content-Type": "application/json"})
	created := decode(t, resp)
	token, _ := created["token"].(string)
	if token == "" {
		t.Fatalf("share was not created: %v", created)
	}

	page, err := http.Get(h.srv.URL + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("landing: want 200, got %d", page.StatusCode)
	}

	var payload struct {
		HideContents bool             `json:"hide_contents"`
		CanUpload    bool             `json:"can_upload"`
		CanRead      bool             `json:"can_read"`
		Children     []map[string]any `json:"children"`
	}
	decodeJSONBody(t, page, &payload)

	if !payload.HideContents {
		t.Error("a create-only link must present hide_contents=true")
	}
	if payload.CanRead {
		t.Error("can_read must be false for a create-only grant")
	}
	if len(payload.Children) != 0 {
		t.Errorf("a create-only link listed %d entries; the grant stops at uploading",
			len(payload.Children))
	}
}
