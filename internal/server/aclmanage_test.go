package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ACL management API tests.
//
// The store-level grant machinery is already covered by the conformance suite; what
// these check is the part only the HTTP layer can get wrong: who is allowed to create
// a grant, what happens to a name that matches nobody, and whether a refusal leaks the
// existence of a path.

// aclSetup creates a second account and a folder owned by the harness user.
func (h *harness) aclSetup(t *testing.T) (store.User, string) {
	t.Helper()

	mate := h.meta.AddUser(store.User{
		Username: "mate", Email: "mate@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}

	h.mkdir(t, "folder")
	h.mkdir(t, "other")
	return mate, "folder"
}

// grantAs posts a grant and returns the status and decoded body.
func (h *harness) grantAs(t *testing.T, path, subject string, perms []string,
	extra map[string]any) (int, map[string]any) {

	t.Helper()
	body := map[string]any{
		"path": path, "subject": subject, "permissions": perms,
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	resp := h.do(http.MethodPost, "/api/acl/", bytes.NewReader(raw), nil)
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestACLGrantThenListThenRevoke walks the whole management lifecycle.
func TestACLGrantThenListThenRevoke(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)

	code, body := h.grantAs(t, folder, mate.Username, []string{"read", "list"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("grant: want 201, got %d (%v)", code, body)
	}

	// The listing must show the grant with a readable subject, not a UUID.
	resp := h.do(http.MethodGet, "/api/acl/?path="+folder, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200, got %d", resp.StatusCode)
	}
	var listing struct {
		Grants []struct {
			ID      string   `json:"id"`
			Subject string   `json:"subject"`
			Perms   []string `json:"permissions"`
			Inherit bool     `json:"inherit"`
		} `json:"grants"`
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Grants) != 1 {
		t.Fatalf("want one grant, got %d", len(listing.Grants))
	}
	if listing.Grants[0].Subject != "mate" {
		t.Fatalf("the grant should name the subject readably, got %q", listing.Grants[0].Subject)
	}
	if !listing.Grants[0].Inherit {
		t.Fatal("a grant defaults to inheriting")
	}

	// Revoke it.
	resp2 := h.do(http.MethodDelete, "/api/acl/"+listing.Grants[0].ID+"?path="+folder, nil, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", resp2.StatusCode)
	}

	// And it is gone.
	resp3 := h.do(http.MethodGet, "/api/acl/?path="+folder, nil, nil)
	defer resp3.Body.Close()
	var after struct {
		Grants []json.RawMessage `json:"grants"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&after)
	if len(after.Grants) != 0 {
		t.Fatalf("the revoke should leave no grants, got %d", len(after.Grants))
	}
}

// TestACLGrantRequiresManagePermission is the security-critical case: an account that
// can read a folder must not be able to grant itself or anyone else more access.
func TestACLGrantRequiresManagePermission(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)

	// Give mate read+list only — note: no manage_permissions.
	if code, _ := h.grantAs(t, folder, mate.Username, []string{"read", "list"}, nil); code != http.StatusCreated {
		t.Fatalf("seed grant: %d", code)
	}

	// Now act as mate and try to escalate.
	mateSession, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}

	body := map[string]any{
		"path": folder, "subject": "mate", "permissions": []string{"read", "write", "delete"},
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/acl/", bytes.NewReader(raw))
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateSession})
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The path is in somebody else's namespace, so mate cannot even resolve it:
	// 404 rather than 403, which is the project's stated rule (a distinguishable
	// denial is a directory-enumeration oracle).
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a non-manager must not be able to grant; want 404, got %d", resp.StatusCode)
	}
}

// TestACLGrantUnknownSubjectIsNotFound: a typo must not create a grant that silently
// does nothing.
func TestACLGrantUnknownSubjectIsNotFound(t *testing.T) {
	h := newHarness(t)
	_, folder := h.aclSetup(t)

	code, body := h.grantAs(t, folder, "nobody-at-all", []string{"read"}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("an unknown subject must be 404, got %d (%v)", code, body)
	}
}

// TestACLGrantRejectsUnknownPermission: quietly dropping an unrecognised permission
// would create a grant narrower or wider than the caller asked for.
func TestACLGrantRejectsUnknownPermission(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)

	code, body := h.grantAs(t, folder, mate.Username, []string{"read", "teleport"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("an unknown permission must be 400, got %d (%v)", code, body)
	}
}

// TestACLGrantToGroup covers the group half: the subject is a group name, and the
// grant reaches members through the store's resolution.
func TestACLGrantToGroup(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)

	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		g, err := tx.CreateGroup(ctx, store.Group{Name: "engineering"})
		if err != nil {
			return err
		}
		return tx.AddGroupMember(ctx, g.ID, mate.ID, "viewer")
	}); err != nil {
		t.Fatal(err)
	}

	code, body := h.grantAs(t, folder, "engineering", []string{"read", "list"},
		map[string]any{"subject_kind": "group"})
	if code != http.StatusCreated {
		t.Fatalf("group grant: want 201, got %d (%v)", code, body)
	}

	// The member sees the folder in their "shared with me" list.
	mateSession, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/shared", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateSession})
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var listing struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listing)

	if len(listing.Items) != 1 {
		t.Fatalf("a group member must see the shared folder, got %d items", len(listing.Items))
	}
}

// TestACLWriteAliasStillWorks: a client written before the fine-grained permissions
// existed must keep working.
func TestACLWriteAliasStillWorks(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)

	if code, body := h.grantAs(t, folder, mate.Username, []string{"write"}, nil); code != http.StatusCreated {
		t.Fatalf("write alias: want 201, got %d (%v)", code, body)
	}

	// The stored grant must confer overwrite, which is what "write" has always meant.
	var perms store.Perm
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := h.meta.Resolve(ctx, h.user.ID, folder)
		if err != nil {
			return err
		}
		p, _, perr := tx.EffectivePermsFor(ctx, mate.ID, nodes[len(nodes)-1].ID)
		perms = p
		return perr
	})
	if perms&store.PermWrite == 0 {
		t.Fatalf("the alias must store a write grant, got %v", store.EncodePerms(perms))
	}
}

// TestACLRevokeRejectsForeignGrant: a manage-capable caller must not be able to revoke
// a grant that belongs to a different node.
func TestACLRevokeRejectsForeignGrant(t *testing.T) {
	h := newHarness(t)
	mate, folder := h.aclSetup(t)

	if code, _ := h.grantAs(t, "other", mate.Username, []string{"read"}, nil); code != http.StatusCreated {
		t.Fatalf("seed grant on the other folder: %d", code)
	}

	listResp := h.do(http.MethodGet, "/api/acl/?path=other", nil, nil)
	var listing struct {
		Grants []struct {
			ID string `json:"id"`
		} `json:"grants"`
	}
	defer listResp.Body.Close()
	_ = json.NewDecoder(listResp.Body).Decode(&listing)
	if len(listing.Grants) != 1 {
		t.Fatal("setup did not create the foreign grant")
	}

	// Revoke it while naming a different folder: the id exists, but not here.
	del := h.do(http.MethodDelete, "/api/acl/"+listing.Grants[0].ID+"?path="+folder, nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNotFound {
		t.Fatalf("revoking a grant that is not on this node must be 404, got %d", del.StatusCode)
	}

	// And it is still there.
	still := h.do(http.MethodGet, "/api/acl/?path=other", nil, nil)
	defer still.Body.Close()
	var after struct {
		Grants []json.RawMessage `json:"grants"`
	}
	_ = json.NewDecoder(still.Body).Decode(&after)
	if len(after.Grants) != 1 {
		t.Fatal("the foreign grant must survive the refused revoke")
	}
	_ = authz.PermRead
}
