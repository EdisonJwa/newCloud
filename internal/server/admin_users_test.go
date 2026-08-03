package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/fakeidp"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// Account administration API tests.
//
// The headline case is the one round four left open: `pending` mode creates an account that
// nothing could approve. These tests drive the whole loop — provision, list, approve, log in
// — because a remedy that exists but is not reachable over the API has not really closed the
// dead end.

// adminClient returns a client acting as an administrator.
//
// It mints a session for a synthetic admin account rather than promoting the harness user,
// so a test cannot accidentally pass because the caller happened to be an owner.
func (h *harness) adminClient(t *testing.T) (*http.Client, store.User) {
	t.Helper()

	admin := h.meta.AddUser(store.User{
		Username: "ops-admin", Email: "ops@example.test", Role: "admin",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), admin.ID); err != nil {
		t.Fatal(err)
	}
	token, err := h.sessions.Mint(context.Background(), admin.ID, admin.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return h.cookieClient(token), admin
}

// cookieClient returns a client that presents one session cookie.
func (h *harness) cookieClient(token string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req = req.Clone(req.Context())
			req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: token})
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

func (h *harness) adminGet(t *testing.T, c *http.Client, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *harness) adminStatus(t *testing.T, c *http.Client, id, status string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"status": status})
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/admin/users/"+id+"/status",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestAdminListsPendingAccounts: the approval queue has to be visible.
func TestAdminListsPendingAccounts(t *testing.T) {
	h := newHarness(t)

	waiting := h.meta.AddUser(store.User{
		Username: "awaiting", Email: "awaiting@example.test", Role: "user", Status: "pending",
	})
	h.meta.AddUser(store.User{Username: "normal", Email: "normal@example.test", Role: "user"})

	client, _ := h.adminClient(t)

	resp := h.adminGet(t, client, "/api/admin/users?status=pending")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var listing struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Status   string `json:"status"`
		} `json:"users"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listing)
	if len(listing.Users) != 1 {
		t.Fatalf("the pending filter must select one account, got %d", len(listing.Users))
	}
	if listing.Users[0].Username != "awaiting" {
		t.Fatalf("wrong account: %s", listing.Users[0].Username)
	}
	if listing.Users[0].ID != waiting.ID.String() {
		t.Fatal("the id must match the account")
	}
}

// TestAdminApprovalUnblocksLogin is the dead end being closed.
//
// The account is provisioned pending, refused, approved through the API, and then admitted.
func TestAdminApprovalUnblocksLogin(t *testing.T) {
	// Build an OIDC harness in pending mode.
	h := newOIDCHarnessWith(t, fakeidp.User{
		Subject: "approve-me", Username: "approvee", Email: "approvee@example.test",
		Groups: []string{"cloud-users"},
	}, func(c *config.Config) {
		c.Auth.JIT = config.JITConfig{Mode: config.JITPending}
	})

	// First login is refused.
	if got := h.loginStatus(t); got != http.StatusForbidden {
		t.Fatalf("a pending account must be refused, got %d", got)
	}

	// The account exists, disabled.
	var target store.User
	for _, u := range h.meta.Users() {
		if u.Username == "approvee" {
			target = u
		}
	}
	if target.ID.String() == "" {
		t.Fatal("pending mode must create the account so it can be approved")
	}
	if target.Status != "pending" {
		t.Fatalf("want status pending, got %q", target.Status)
	}

	// An administrator approves it through the API.
	admin := h.meta.AddUser(store.User{
		Username: "approver", Email: "approver@example.test", Role: "admin",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), admin.ID); err != nil {
		t.Fatal(err)
	}
	token, err := h.sessions.Mint(context.Background(), admin.ID, admin.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"status": "active"})
	req, _ := http.NewRequest(http.MethodPost,
		h.url+"/api/admin/users/"+target.ID.String()+"/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: token})
	// The OIDC harness serves TLS with a self-signed certificate, so its own client is
	// the one that can reach it.
	client := h.b.followRedirects()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approval: want 200, got %d", resp.StatusCode)
	}

	var result struct {
		Changed bool `json:"changed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if !result.Changed {
		t.Fatal("the approval must report a change")
	}

	// Now the login succeeds. A successful callback redirects into the app rather than
	// answering 200, so "admitted" means "not refused": anything below 400 with a session
	// cookie is a completed login.
	if got := h.loginStatus(t); got >= 400 {
		t.Fatalf("an approved account must be admitted, got %d", got)
	}
	if code, _ := h.b.me(); code != http.StatusOK {
		t.Fatalf("an approved account must have a session, got %d", code)
	}
}

// TestAdminEndpointsAreInvisibleToNonAdmins: 404, not 403.
func TestAdminEndpointsAreInvisibleToNonAdmins(t *testing.T) {
	h := newHarness(t) // the harness user is a normal user

	for _, path := range []string{"/api/admin/users", "/api/admin/users?status=pending"} {
		resp := h.do(http.MethodGet, path, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s must be 404 for a non-admin, got %d", path, resp.StatusCode)
		}
	}

	// And the status change too.
	code, _ := h.adminStatus(t, h.srv.Client(), h.user.ID.String(), "disabled")
	_ = code
	// The plain client carries no session at all, so it is unauthenticated; use the
	// harness user's own cookie instead.
	req, _ := http.NewRequest(http.MethodPost,
		h.srv.URL+"/api/admin/users/"+h.user.ID.String()+"/status",
		bytes.NewReader([]byte(`{"status":"disabled"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("a non-admin must not reach the status endpoint, got %d", resp2.StatusCode)
	}
}

// TestAdminDisableRevokesTheLiveSession: disabling has to actually cut access off.
//
// A disabled account whose session still works is not disabled — the same lesson role
// downgrade taught.
func TestAdminDisableRevokesTheLiveSession(t *testing.T) {
	h := newHarness(t)

	target := h.meta.AddUser(store.User{
		Username: "troublemaker", Email: "trouble@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}
	targetToken, err := h.sessions.Mint(context.Background(), target.ID, target.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}

	// The target can act.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: targetToken})
	before, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	before.Body.Close()
	if before.StatusCode != http.StatusOK {
		t.Fatalf("the target should be able to act before being disabled, got %d", before.StatusCode)
	}

	// Disable.
	client, _ := h.adminClient(t)
	code, body := h.adminStatus(t, client, target.ID.String(), "disabled")
	if code != http.StatusOK {
		t.Fatalf("disable: want 200, got %d (%v)", code, body)
	}
	if changed, _ := body["changed"].(bool); !changed {
		t.Fatalf("disable must report a change, got %v", body)
	}

	// The same token no longer works.
	req2, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/auth/me", nil)
	req2.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: targetToken})
	after, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a disabled account's session must stop working, got %d", after.StatusCode)
	}
}

// TestAdminStatusChangeIsAudited: "who disabled this account" must be answerable.
func TestAdminStatusChangeIsAudited(t *testing.T) {
	h := newHarness(t)
	target := h.meta.AddUser(store.User{
		Username: "audited", Email: "audited@example.test", Role: "user",
	})

	client, _ := h.adminClient(t)
	if code, _ := h.adminStatus(t, client, target.ID.String(), "disabled"); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}

	var found bool
	for _, e := range h.meta.AuditEntries() {
		if e.Action == "user.status.change" {
			found = true
			if e.TargetID != target.ID.String() {
				t.Fatalf("audit names the wrong target: %s", e.TargetID)
			}
			if got, _ := e.Metadata["to"].(string); got != "disabled" {
				t.Fatalf("audit should record the new status, got %v", e.Metadata["to"])
			}
		}
	}
	if !found {
		t.Fatal("a status change must be audited")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAdminDeleteRequiresConfirmation: deletion is irreversible, so a slip must not destroy an
// account. The confirmation is the account's own username, checked inside the transaction
// against the row being deleted.
func TestAdminDeleteRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	target := h.meta.AddUser(store.User{
		Username: "confirmed", Email: "confirmed@example.test", Role: "user", Status: "disabled",
	})
	client, _ := h.adminClient(t)

	del := func(confirm string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"confirm_username": confirm})
		req, err := http.NewRequest(http.MethodDelete,
			h.srv.URL+"/api/admin/users/"+target.ID.String(), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, derr := client.Do(req)
		if derr != nil {
			t.Fatal(derr)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Wrong name: refused.
	if code, _ := del("someone-else"); code != http.StatusBadRequest {
		t.Fatalf("a wrong confirmation must be 400, got %d", code)
	}
	// Missing confirmation: refused.
	if code, _ := del(""); code != http.StatusBadRequest {
		t.Fatalf("a missing confirmation must be 400, got %d", code)
	}

	// The account survived both refusals.
	var still store.User
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UserByID(ctx, target.ID)
		still = u
		return err
	})
	if still.Status == store.StatusDeleted {
		t.Fatal("a refused deletion must not modify the account")
	}

	// Correct name: deleted, and the outcome is reported.
	code, out := del("confirmed")
	if code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d (%v)", code, out)
	}
	if deleted, _ := out["deleted"].(bool); !deleted {
		t.Fatalf("the response must confirm the deletion, got %v", out)
	}
	// The audit-retention decision is surfaced, because it is the part an operator is most
	// likely to wonder about.
	if _, ok := out["audit_rows_kept"]; !ok {
		t.Fatalf("the response must report how many audit rows were kept, got %v", out)
	}
}

// TestAdminDeleteRefusesAnActiveAccount: disable is the reversible step and must come first.
func TestAdminDeleteRefusesAnActiveAccount(t *testing.T) {
	h := newHarness(t)
	target := h.meta.AddUser(store.User{
		Username: "still-on", Email: "on@example.test", Role: "user",
	})
	client, _ := h.adminClient(t)

	body, _ := json.Marshal(map[string]string{"confirm_username": "still-on"})
	req, _ := http.NewRequest(http.MethodDelete,
		h.srv.URL+"/api/admin/users/"+target.ID.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("an active account must not be deletable: disable it first")
	}
}

// TestAdminDeleteIsInvisibleToNonAdmins.
func TestAdminDeleteIsInvisibleToNonAdmins(t *testing.T) {
	h := newHarness(t)
	target := h.meta.AddUser(store.User{
		Username: "protected", Email: "p@example.test", Role: "user", Status: "disabled",
	})

	body, _ := json.Marshal(map[string]string{"confirm_username": "protected"})
	req, _ := http.NewRequest(http.MethodDelete,
		h.srv.URL+"/api/admin/users/"+target.ID.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a non-admin must get 404 from the delete endpoint, got %d", resp.StatusCode)
	}
}
