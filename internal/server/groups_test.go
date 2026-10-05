package server_test

// Group sharing (SHARE-08) from an administrator's side.
//
// The store created groups, the authorizer expanded a group grant into its members, and the ACL API
// already accepted "share this with group:engineering". Nothing could create a group or put anybody
// in it, so a grant to a group answered "no user or group matches that name" for every group that
// could possibly exist — a sharing mode reachable in code and unreachable in the product.
//
// The test walks the whole path, because each half alone would pass while the feature did not work:
// admin creates the group and adds a member, the owner shares with the group, the member can read the
// file, and removing the member takes the access away again.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestGroupSharingWorksEndToEnd(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.adminClient(t)

	// The owner's file.
	h.uploadFile(t, "roadmap.txt", "secret plans")
	node := h.resolveNode(t, "roadmap.txt")

	// An account that will join the group.
	member := h.meta.AddUser(store.User{
		Username: "group-member", Email: "group-member@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), member.ID); err != nil {
		t.Fatal(err)
	}
	memberToken, err := h.sessions.Mint(context.Background(), member.ID, member.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	memberClient := h.cookieClient(memberToken)

	// 1. The administrator creates the group.
	create := h.adminDo(t, admin, http.MethodPost, "/api/admin/groups",
		`{"name":"engineering","display_name":"Engineering"}`)
	createBody := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create group: %d %s", create.StatusCode, createBody)
	}
	var group struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(createBody), &group); err != nil || group.ID == "" {
		t.Fatalf("no group id in %s", createBody)
	}

	// 2. Sharing with the group now resolves — the thing that could not happen before.
	grant := h.do(http.MethodPost, "/api/acl/",
		strings.NewReader(`{"path":"roadmap.txt","subject_kind":"group","subject":"engineering","permissions":["read","download"]}`),
		map[string]string{"Content-Type": "application/json"})
	grantBody := readBody(grant)
	grant.Body.Close()
	if grant.StatusCode != http.StatusCreated && grant.StatusCode != http.StatusOK {
		t.Fatalf("grant to a group: %d %s", grant.StatusCode, grantBody)
	}

	// 3. Membership decides access: before joining, the account cannot read the file.
	before := h.adminGet(t, memberClient, "/api/shared/"+node.ID.String())
	before.Body.Close()
	if before.StatusCode == http.StatusOK {
		t.Fatal("an account that is not in the group could read a file shared with it")
	}

	add := h.adminDo(t, admin, http.MethodPost, "/api/admin/groups/"+group.ID+"/members",
		`{"username":"group-member","role":"viewer"}`)
	addBody := readBody(add)
	add.Body.Close()
	if add.StatusCode != http.StatusOK {
		t.Fatalf("add member: %d %s", add.StatusCode, addBody)
	}

	after := h.adminGet(t, memberClient, "/api/shared/"+node.ID.String())
	after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Fatalf("a group member could not read the file shared with the group (%d). The grant is "+
			"the whole point of putting them in the group.", after.StatusCode)
	}

	// 4. Leaving the group takes the access away. Without this, membership would only ever add.
	del := h.adminDo(t, admin, http.MethodDelete, "/api/admin/groups/"+group.ID+"/members/"+member.ID.String(), "")
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("remove member: %d", del.StatusCode)
	}
	afterLeave := h.adminGet(t, memberClient, "/api/shared/"+node.ID.String())
	afterLeave.Body.Close()
	if afterLeave.StatusCode == http.StatusOK {
		t.Fatal("removing the member did not remove their access; the grant is expanded from " +
			"membership, so a stale membership is a stale permission")
	}

	// 5. Every change is audited.
	var creates, changes int
	for _, e := range h.meta.AuditLog() {
		switch e.Action {
		case "group.create":
			creates++
		case "group.member.change":
			changes++
			if e.Metadata["change"] == nil || e.Metadata["member"] == nil {
				t.Fatalf("the membership row does not say what changed: %v", e.Metadata)
			}
		}
	}
	if creates != 1 {
		t.Fatalf("want one group.create row, got %d", creates)
	}
	if changes != 2 {
		t.Fatalf("want an add and a remove recorded, got %d", changes)
	}
}

// adminDo issues a body-bearing request as the administrator.
func (h *harness) adminDo(t *testing.T, c *http.Client, method, path, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
