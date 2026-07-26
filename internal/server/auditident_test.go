package server_test

// Audit rows must carry the actor's address.
//
// The address is denormalised into each row on purpose: an audit log has to show who did
// something *at the time*, and a join against `users` would rewrite history whenever an
// address changed. That makes forgetting to set it a silent data loss rather than a crash,
// which is why it needs a test.
//
// auth.logout and share.create were both writing a NULL address while every other event
// carried one. The API tolerated it (the column is nullable), so nothing failed — the rows
// were simply useless for the question an auditor asks.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// auditEvents fetches the activity feed for the harness user.
func auditEvents(t *testing.T, h *harness) []struct {
	Action     string `json:"action"`
	ActorEmail string `json:"actor_email"`
	ActorKind  string `json:"actor_kind"`
	ActorID    string `json:"actor_user_id"`
} {
	t.Helper()

	resp := h.do(http.MethodGet, "/api/audit/events", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit list: %d %s", resp.StatusCode, readBody(resp))
	}

	var payload struct {
		Events []struct {
			Action     string `json:"action"`
			ActorEmail string `json:"actor_email"`
			ActorKind  string `json:"actor_kind"`
			ActorID    string `json:"actor_user_id"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(readBody(resp)), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Events
}

// TestAuditIdentifiesTheActor checks every event caused by a signed-in user names them.
func TestAuditIdentifiesTheActor(t *testing.T) {
	h := newHarness(t)

	// Produce events from several code paths, including the two that used to omit the
	// address.
	uploadWithType(t, h, "note.txt", "text/plain", "hello")
	h.do(http.MethodGet, "/api/files/download?path=note.txt", nil, nil)

	created := h.do(http.MethodPost, "/api/shares/", jsonBody(t, map[string]any{
		"path": "note.txt", "kind": "link",
	}), map[string]string{"Content-Type": "application/json"})
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated && created.StatusCode != http.StatusOK {
		t.Fatalf("create share: %d %s", created.StatusCode, readBody(created))
	}

	events := auditEvents(t, h)
	if len(events) == 0 {
		t.Fatal("no audit events")
	}

	// A row with a user actor and no address is the defect this test exists for. Rows with
	// no actor at all (anonymous share access) legitimately have no address, so they are
	// excluded rather than failed.
	missing := 0
	checked := 0
	for _, ev := range events {
		if ev.ActorKind != "user" || ev.ActorID == "" {
			continue
		}
		checked++
		if ev.ActorEmail == "" {
			missing++
			t.Errorf("event %q has a user actor but no address", ev.Action)
		}
	}

	if checked == 0 {
		t.Fatal("no user-attributed events to check; the test is not exercising the path")
	}
	if missing > 0 {
		t.Errorf("%d of %d user events lost the actor address", missing, checked)
	}
}
