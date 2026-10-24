package server_test

// The admin panels' requests, exercised against the real server.
//
// Same reasoning as adminui_test.go: the views are written against openapi.yaml and typechecked
// against the generated types, which catches a wrong shape but not a wrong CONVENTION — a body
// wrapped in an extra object, a bare number where an object was expected. These tests send exactly
// what the panels send.
//
// The three panels exist because each uncovered a capability that was implemented and unreachable:
// groups could be created but not listed, quotas could be provisioned but not changed, and runtime
// limits could be overridden but not seen.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// adminDo sends one admin request and returns the status and decoded body.
func adminDo(t *testing.T, h *harness, c *http.Client, method, path string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// TestAdminGroupsPanelRoundTrip is the group screen's whole flow: create, add, list, remove.
func TestAdminGroupsPanelRoundTrip(t *testing.T) {
	h := newHarness(t)
	client, _ := h.adminClient(t)

	// What the "Create group" button sends.
	status, created := adminDo(t, h, client, http.MethodPost, "/api/admin/groups",
		map[string]any{"name": "engineering", "display_name": "Engineering"})
	if status != http.StatusCreated {
		t.Fatalf("create group: want 201, got %d (%v)", status, created)
	}
	groupID, _ := created["id"].(string)
	if groupID == "" {
		t.Fatalf("the created group has no id: %v", created)
	}

	// What the per-group "Add" form sends: a username, not an id — an operator types a name.
	status, withMember := adminDo(t, h, client, http.MethodPost,
		"/api/admin/groups/"+groupID+"/members",
		map[string]any{"username": h.user.Username, "role": "viewer"})
	if status != http.StatusOK {
		t.Fatalf("add member: want 200, got %d (%v)", status, withMember)
	}

	// The response is the group in the SAME shape the listing uses. Two shapes for one concept is
	// how a client ends up needing two parsers, and the panel redraws the group right after this.
	members, _ := withMember["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("add-member response carried %d members, want 1: %v", len(members), withMember)
	}
	first, _ := members[0].(map[string]any)
	if first["username"] != h.user.Username {
		t.Fatalf("member = %v, want an object naming %q", first, h.user.Username)
	}
	memberID, _ := first["id"].(string)
	if memberID == "" {
		t.Fatalf("the member has no id, so the remove button cannot work: %v", first)
	}

	// The listing, which is what the screen renders.
	status, listed := adminDo(t, h, client, http.MethodGet, "/api/admin/groups", nil)
	if status != http.StatusOK {
		t.Fatalf("list groups: want 200, got %d", status)
	}
	groups, _ := listed["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("list returned %d groups, want 1: %v", len(groups), listed)
	}

	// And removal, which is the only way to undo a membership.
	status, _ = adminDo(t, h, client, http.MethodDelete,
		"/api/admin/groups/"+groupID+"/members/"+memberID, nil)
	if status != http.StatusNoContent {
		t.Fatalf("remove member: want 204, got %d", status)
	}
	_, listed = adminDo(t, h, client, http.MethodGet, "/api/admin/groups", nil)
	groups, _ = listed["groups"].([]any)
	remaining, _ := groups[0].(map[string]any)["members"].([]any)
	if len(remaining) != 0 {
		t.Fatalf("the member was still listed after removal: %v", remaining)
	}
}

// TestAdminQuotaPanelBody pins the quota body: {"bytes": n}, not {"quota": n} and not a wrapper.
func TestAdminQuotaPanelBody(t *testing.T) {
	h := newHarness(t)
	client, _ := h.adminClient(t)

	status, out := adminDo(t, h, client, http.MethodPost,
		"/api/admin/users/"+h.user.ID.String()+"/quota",
		map[string]any{"bytes": 4 << 20})
	if status != http.StatusOK {
		t.Fatalf("set quota: want 200, got %d (%v)", status, out)
	}

	// A 200 that changed nothing would satisfy a status-only assertion, so the value is read back
	// through the listing the screen itself uses.
	_, listed := adminDo(t, h, client, http.MethodGet, "/api/admin/users", nil)
	users, _ := listed["users"].([]any)
	var found bool
	for _, raw := range users {
		u, _ := raw.(map[string]any)
		if u["id"] == h.user.ID.String() {
			found = true
			if got, _ := u["quota_bytes"].(float64); got != float64(4<<20) {
				t.Fatalf("quota_bytes = %v, want %d", u["quota_bytes"], 4<<20)
			}
		}
	}
	if !found {
		t.Fatal("the account disappeared from the listing")
	}
}

// TestAdminSettingsPanelBody pins the settings bodies, which are the least obvious ones:
//
//	GET    lists each key with its effective value and where that value came from
//	PUT    takes a BARE JSON NUMBER, not {"value": n}
//	DELETE removes the override
//
// A wrapper here would 400 against a spec that agreed with itself, because the client's type for
// the body is `number` and the handler decodes into an int64.
func TestAdminSettingsPanelBody(t *testing.T) {
	h := newHarness(t)
	client, _ := h.adminClient(t)

	const key = "limits.max_file_bytes"

	status, listed := adminDo(t, h, client, http.MethodGet, "/api/admin/settings", nil)
	if status != http.StatusOK {
		t.Fatalf("list settings: want 200, got %d", status)
	}
	settings, _ := listed["settings"].([]any)
	if len(settings) == 0 {
		t.Fatalf("no runtime settings listed: %v", listed)
	}
	var before any
	for _, raw := range settings {
		s, _ := raw.(map[string]any)
		if s["key"] == key {
			before = s["value"]
			if s["source"] != "config" {
				t.Fatalf("a fresh deployment should report source=config, got %v", s["source"])
			}
		}
	}
	if before == nil {
		t.Fatalf("the settings listing does not include %q: %v", key, listed)
	}

	// The override body, exactly as the panel sends it: a bare number.
	status, out := adminDo(t, h, client, http.MethodPut, "/api/admin/settings/"+key, 3<<20)
	if status != http.StatusOK {
		t.Fatalf("set setting: want 200, got %d (%v)", status, out)
	}

	// It is reported as an override afterwards, which is the whole reason the source column exists.
	_, listed = adminDo(t, h, client, http.MethodGet, "/api/admin/settings", nil)
	settings, _ = listed["settings"].([]any)
	for _, raw := range settings {
		s, _ := raw.(map[string]any)
		if s["key"] != key {
			continue
		}
		if got, _ := s["value"].(float64); got != float64(3<<20) {
			t.Fatalf("value = %v, want %d", s["value"], 3<<20)
		}
		if s["source"] != "override" {
			t.Fatalf("source = %v, want override — a value that happens to equal the default is a "+
				"different fact from one pinned by an override", s["source"])
		}
	}

	// And the reset, which returns the key to the configured value.
	status, out = adminDo(t, h, client, http.MethodDelete, "/api/admin/settings/"+key, nil)
	if status != http.StatusOK {
		t.Fatalf("reset setting: want 200, got %d (%v)", status, out)
	}
	_, listed = adminDo(t, h, client, http.MethodGet, "/api/admin/settings", nil)
	settings, _ = listed["settings"].([]any)
	for _, raw := range settings {
		s, _ := raw.(map[string]any)
		if s["key"] == key && s["source"] != "config" {
			t.Fatalf("after a reset the source is %v, want config", s["source"])
		}
	}
}

// TestAdminPanelsAreClosedToNonAdmins is the gate, stated for every panel.
//
// The screens are hidden from a non-admin in the UI, but hiding is presentation: the API is the
// gate, and it answers 404 rather than 403 so the surface is not even confirmed to exist.
func TestAdminPanelsAreClosedToNonAdmins(t *testing.T) {
	h := newHarness(t)

	// The harness user is an ordinary account with a live session.
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/admin/groups"},
		{http.MethodPost, "/api/admin/groups"},
		{http.MethodGet, "/api/admin/settings"},
		{http.MethodPut, "/api/admin/settings/limits.max_file_bytes"},
		{http.MethodDelete, "/api/admin/settings/limits.max_file_bytes"},
		{http.MethodPost, "/api/admin/users/" + h.user.ID.String() + "/quota"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body io.Reader
			if tc.method == http.MethodPost || tc.method == http.MethodPut {
				body = bytes.NewReader([]byte(`{"name":"x","bytes":1}`))
			}
			req, err := http.NewRequest(tc.method, h.srv.URL+tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("a non-admin got %d from %s %s; want 404 so the surface is not confirmed",
					resp.StatusCode, tc.method, tc.path)
			}
		})
	}
}
