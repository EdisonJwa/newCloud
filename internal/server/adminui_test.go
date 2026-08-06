package server_test

// The admin UI's calls, exercised against the real server.
//
// The view was written against openapi.yaml and typechecked against the generated types, which
// catches a wrong shape but not a wrong *convention*: a query parameter named `state` instead of
// `status` typechecks fine if the spec says `status`, but the panel would have been written from
// the spec, so the risk is the opposite — the panel sends what the spec says and the handler
// reads something else. These tests close that by sending exactly the request the panel sends and
// asserting the server understands it.
//
// They are here rather than in the TypeScript because the contract lives on this side. A browser
// test would need a DOM runner for one request shape; this needs nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// TestAdminListShapesThePanelUsesAssertTheSameThing.
//
// The panel calls `GET /api/admin/users?status=pending` when the queue is selected and
// `GET /api/admin/users` (no filter) for "all". Both must work: the first is the approval queue,
// the second is what the "All" button sends.
func TestAdminListAcceptsTheShapeThePanelSends(t *testing.T) {
	h := newHarness(t)
	client, admin := h.adminClient(t)

	// One account in each state, so a filter that silently ignored the parameter would still
	// return everything and the test would catch it.
	mk := func(name, status string) store.User {
		u := h.meta.AddUser(store.User{
			Username: name, Email: name + "@example.test", Role: "user", Status: status,
		})
		if _, err := h.meta.EnsureRoot(context.Background(), u.ID); err != nil {
			t.Fatal(err)
		}
		return u
	}
	active := mk("active-one", store.StatusActive)
	pending := mk("pending-one", store.StatusPending)
	_ = active

	// 1. The queue: exactly the pending account.
	var queue struct {
		Users []store.User `json:"users"`
	}
	resp := h.adminGet(t, client, "/api/admin/users?status=pending")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue: want 200, got %d", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&queue)
	resp.Body.Close()
	if len(queue.Users) != 1 {
		t.Fatalf("the pending filter must return exactly the pending account, got %d", len(queue.Users))
	}
	if queue.Users[0].ID != pending.ID {
		t.Fatalf("the queue returned the wrong account: %s", queue.Users[0].Username)
	}

	// 2. No filter: everything, which is what "All" sends.
	resp = h.adminGet(t, client, "/api/admin/users")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("all: want 200, got %d", resp.StatusCode)
	}
	var all struct {
		Users []store.User `json:"users"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&all)
	resp.Body.Close()
	if len(all.Users) < 2 {
		t.Fatalf("an unfiltered list must include both accounts, got %d", len(all.Users))
	}

	// The admin itself is in there too, which is correct: it is an account.
	_ = admin
}

// TestAdminStatusPostAcceptsTheBodyThePanelSends.
//
// The panel sends {"status": "..."} — a bare object with one field. If the handler grew a wrapper
// (`{"user": {"status": ...}}`) this would fail while the spec and the types still agreed.
func TestAdminStatusPostAcceptsThePanelBody(t *testing.T) {
	h := newHarness(t)
	client, _ := h.adminClient(t)

	u := h.meta.AddUser(store.User{
		Username: "needs-approval", Email: "needs@example.test", Role: "user",
		Status: store.StatusPending,
	})
	if _, err := h.meta.EnsureRoot(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]string{"status": "active"})
	req, err := http.NewRequest(http.MethodPost,
		h.srv.URL+"/api/admin/users/"+u.ID.String()+"/status", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approval: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// And the account really is approved — a 200 that did not change the status would satisfy
	// a status-code-only assertion.
	var got store.User
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UserByID(ctx, u.ID)
		got = u
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusActive {
		t.Fatalf("the approval did not take effect: status is %q", got.Status)
	}
}
