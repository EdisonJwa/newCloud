package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// A group space: created by a group's manager, discoverable by its members, openable by them, and
// invisible to everyone else.
func TestGroupSpacesAreCreatedDiscoveredAndUsed(t *testing.T) {
	h := newHarness(t)

	// A group with the owner as manager and a second account as viewer.
	mate, _ := h.aclSetup(t)
	var groupName string
	adminClient, _ := h.adminClient(t)
	groupName = "team-" + uuid.New().String()[:8]
	status, created := adminDo(t, h, adminClient, http.MethodPost, "/api/admin/groups",
		map[string]any{"name": groupName})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d (%v)", status, created)
	}
	groupID, _ := created["id"].(string)

	// The owner manages it; mate views it.
	for _, m := range []struct {
		id   string
		role string
	}{{h.user.ID.String(), "manager"}, {mate.ID.String(), "viewer"}} {
		r := h.adminDo(t, adminClient, http.MethodPost, "/api/admin/groups/"+groupID+"/members",
			`{"user_id":"`+m.id+`","role":"`+m.role+`"}`)
		code := r.StatusCode
		body := readBody(r)
		if code != http.StatusCreated && code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("add %s: %d (%s)", m.role, code, body)
		}
	}

	// The manager creates the space.
	resp := h.do(http.MethodPost, "/api/spaces",
		strings.NewReader(`{"name":"team-files","group":"`+groupName+`"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create space: %d (%s)", resp.StatusCode, readBody(resp))
	}
	var space struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&space); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	// The member discovers it without being told about it.
	mateSession, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/spaces", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateSession})
	listResp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("list spaces: %v", err)
	}
	var listing struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode spaces: %v", err)
	}
	listResp.Body.Close()
	if len(listing.Items) != 1 || listing.Items[0].ID != space.ID {
		t.Fatalf("the member sees %+v, want the space its group owns", listing.Items)
	}

	// And can open it, by id: the grant the create route made is what allows this.
	openReq, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/nodes/"+space.ID+"/children", nil)
	openReq.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateSession})
	openResp, err := h.srv.Client().Do(openReq)
	if err != nil {
		t.Fatalf("open space: %v", err)
	}
	openCode := openResp.StatusCode
	openResp.Body.Close()
	if openCode != http.StatusOK {
		t.Fatalf("the member could not open the space it can see: %d", openCode)
	}

	// An account outside the group sees neither.
	// A second, unrelated account: aclSetup cannot be called twice (it creates a folder), so this
	// one is made directly.
	var stranger store.User
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, uerr := tx.UpsertIdPUser(ctx, store.UpsertUser{
			Issuer: "https://idp.test", Subject: "spaces-stranger",
			Username: "spaces-stranger", Email: "spaces-stranger@example.test", Role: "user",
		})
		stranger = u
		return uerr
	}); err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	strangerSession, err := h.sessions.Mint(context.Background(), stranger.ID, stranger.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	sReq, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/spaces", nil)
	sReq.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: strangerSession})
	sResp, err := h.srv.Client().Do(sReq)
	if err != nil {
		t.Fatalf("stranger list: %v", err)
	}
	var strangerList struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(sResp.Body).Decode(&strangerList)
	sResp.Body.Close()
	if len(strangerList.Items) != 0 {
		t.Fatalf("an account outside the group sees %d space(s)", len(strangerList.Items))
	}
}
