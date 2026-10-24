package server_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Deleting a group is an administrative act with a defined outcome for its spaces.
//
// The schema decides what happens to them (memberships cascade, owner_group_id goes NULL); what
// this checks is the route: an admin can do it, a missing group is a 404 rather than a silent
// success, and a non-admin is refused.
func TestAdminCanDeleteAGroup(t *testing.T) {
	h := newHarness(t)
	client, _ := h.adminClient(t)

	status, created := adminDo(t, h, client, http.MethodPost, "/api/admin/groups",
		map[string]any{"name": "disposable-" + uuid.New().String()[:8]})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d %v", status, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create group returned no id: %v", created)
	}

	resp := h.adminDo(t, client, http.MethodDelete, "/api/admin/groups/"+id, "")
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNoContent {
		t.Fatalf("delete group: %d, want 204", code)
	}

	// Again: gone, and it says so.
	resp = h.adminDo(t, client, http.MethodDelete, "/api/admin/groups/"+id, "")
	code = resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("deleting a group twice answered %d, want 404", code)
	}

	// A non-admin must not be able to do it.
	plain := h.do(http.MethodDelete, "/api/admin/groups/"+uuid.New().String(), nil, nil)
	plain.Body.Close()
	if plain.StatusCode != http.StatusForbidden && plain.StatusCode != http.StatusNotFound {
		t.Fatalf("a non-admin delete answered %d, want 403 or 404", plain.StatusCode)
	}
}
