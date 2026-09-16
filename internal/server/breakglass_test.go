package server_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/password"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// Break-glass tests.
//
// The security properties are the point: this is the account that bypasses the IdP, so
// every test below is about a way it could be *too* powerful rather than about the
// happy path working.

// breakglassHarness wires a server with the local admin enabled.
func breakglassHarness(t *testing.T, allowedCIDRs []string) (*harness, string) {
	t.Helper()
	return breakglassHarnessTuned(t, allowedCIDRs, nil)
}

// breakglassHarnessTuned additionally tunes the deps, so a test can attach a metrics registry.
func breakglassHarnessTuned(t *testing.T, allowedCIDRs []string, tune func(*server.Deps)) (*harness, string) {
	t.Helper()

	// Write a real Argon2 hash with the production parameters, because the server
	// verifies with exactly those. Using the test parameters here would produce a
	// hash the server cannot check, and the test would fail for the wrong reason.
	hash, err := password.Hash("correct horse battery", password.Params{
		Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass.hex")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The override is applied before the server is built: mutating a harness field
	// afterwards would change a copy the server has already captured.
	h := newHarnessTuned(t, func(c *config.Config) {
		c.Auth.AllowLocalAdmin = true
		c.Auth.LocalAdmin = config.LocalAdminConfig{
			Username: "breakglass-admin", PasswordFile: path, AllowedCIDRs: allowedCIDRs,
		}
	}, tune)
	return h, path
}

// loginBreakglass posts a credential and returns the status.
func (h *harness) loginBreakglass(t *testing.T, username, secret string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": secret})
	resp, err := http.Post(h.srv.URL+"/api/auth/breakglass", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestBreakglassLoginWorks proves the feature is no longer fictional.
func TestBreakglassLoginWorks(t *testing.T) {
	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	code, body := h.loginBreakglass(t, "breakglass-admin", "correct horse battery")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", code, body)
	}
	if body["role"] != "admin" {
		t.Fatalf("the break-glass account is an administrator, got %v", body["role"])
	}
	// The response states this explicitly because it is a property of the account, not
	// a policy a client should have to infer.
	if body["content_access"] != false {
		t.Fatalf("break-glass must not read user content, got %v", body["content_access"])
	}

	// The account was provisioned as a real row, so it can be seen and disabled like
	// any other.
	var found bool
	for _, u := range h.meta.Users() {
		if u.Username == "breakglass" {
			found = true
			if u.Role != "admin" {
				t.Fatalf("stored role should be admin, got %q", u.Role)
			}
		}
	}
	if !found {
		t.Fatal("the break-glass account must exist as a user row")
	}
}

// TestBreakglassWrongPasswordIsRefused.
func TestBreakglassWrongPasswordIsRefused(t *testing.T) {
	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	code, _ := h.loginBreakglass(t, "breakglass-admin", "wrong")
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

// TestBreakglassWrongUsernameIsRefused: only the configured name is accepted, even
// with the right password. Otherwise the account could be found by brute force over
// usernames.
func TestBreakglassWrongUsernameIsRefused(t *testing.T) {
	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	code, _ := h.loginBreakglass(t, "administrator", "correct horse battery")
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

// TestBreakglassRespectsCIDR is the restriction that keeps the account off the
// internet. The test client connects from 127.0.0.1, so an allow-list that excludes
// loopback must refuse.
func TestBreakglassRespectsCIDR(t *testing.T) {
	h, _ := breakglassHarness(t, []string{"10.0.0.0/8"})

	code, _ := h.loginBreakglass(t, "breakglass-admin", "correct horse battery")
	if code != http.StatusForbidden {
		t.Fatalf("an address outside the allow-list must be refused, got %d", code)
	}
}

// TestBreakglassDisabledIsInvisible: a deployment that never enabled it must not
// advertise the endpoint at all.
func TestBreakglassDisabledIsInvisible(t *testing.T) {
	h := newHarness(t) // feature off

	code, _ := h.loginBreakglass(t, "breakglass-admin", "correct horse battery")
	if code != http.StatusNotFound {
		t.Fatalf("a disabled feature must answer 404, got %d", code)
	}
}

// TestBreakglassCannotReadUserContent is the most important assertion here.
//
// The admin branch of the authorizer grants read+delete on metadata and never content
// reads, and the break-glass account goes through the same branch. If that ever
// changes, this account becomes a way around every other user's privacy.
func TestBreakglassCannotReadUserContent(t *testing.T) {
	h, _ := breakglassHarness(t, []string{"127.0.0.0/8", "::1/128"})

	if code, _ := h.loginBreakglass(t, "breakglass-admin", "correct horse battery"); code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}

	var bg store.User
	for _, u := range h.meta.Users() {
		if u.Username == "breakglass" {
			bg = u
		}
	}
	if bg.ID.String() == "" {
		t.Fatal("breakglass account not provisioned")
	}

	// The harness user's file.
	mb := multipartBody(t, "", "secret.txt", "private")
	upResp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: want 201, got %d", upResp.StatusCode)
	}

	// Resolve the victim's file and ask the authorizer, as the break-glass actor would.
	nodes, err := h.meta.Resolve(context.Background(), h.user.ID, "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	fileID := nodes[len(nodes)-1].ID

	actor := authz.NewUserActor(bg.ID, "", authz.RoleAdmin, authz.ScopeRead|authz.ScopeWrite)
	cap, err := h.authz.Authorize(actor, fileID)
	if err != nil {
		// Denied outright is also acceptable; it is the strongest form of "cannot
		// read content".
		return
	}
	if cap.Can(authz.PermDownload) || cap.Can(authz.PermReadVersions) {
		t.Fatalf("an administrator must not be able to download user content, got %v", cap.Perms())
	}
}

// TestBreakglassAccountGetsTheConfiguredQuota is the path the first fix missed.
//
// limits.default_file_quota_bytes was applied to IdP-provisioned accounts and not to the
// break-glass one, which is the only local account and the one an operator is most likely to use
// while checking that a deployment behaves as configured. Found by running a real server with a
// 4 KiB default: the break-glass account came up with the schema's 10 GiB, and a 9 KiB upload was
// accepted against it.
//
// The policy now lives in one function on the server, so the next provisioner cannot pick it up
// selectively.
func TestBreakglassAccountGetsTheConfiguredQuota(t *testing.T) {
	const quota = int64(4096)

	hash, err := password.Hash("correct horse battery", password.Params{
		Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass.hex")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarnessTuned(t, func(c *config.Config) {
		c.Auth.AllowLocalAdmin = true
		c.Auth.LocalAdmin = config.LocalAdminConfig{
			Username: "breakglass", PasswordFile: path,
			AllowedCIDRs: []string{"127.0.0.1/32"},
		}
		c.Limits.DefaultFileQuotaBytes = quota
	}, nil)

	if code, body := h.loginBreakglass(t, "breakglass", "correct horse battery"); code != http.StatusOK {
		t.Fatalf("break-glass login: %d %v", code, body)
	}

	// The slice is captured once: Users() rebuilds it on every call, so indexing it repeatedly
	// re-reads a fresh (and freshly ordered) copy.
	users := h.meta.Users()
	var local *store.User
	for i := range users {
		if users[i].Username == "breakglass" {
			local = &users[i]
		}
	}
	if local == nil {
		t.Fatal("the break-glass login created no account")
	}
	if local.QuotaBytes != quota {
		t.Fatalf("the break-glass account has a quota of %d bytes; limits.default_file_quota_bytes "+
			"is %d. The setting is the per-user default for every provisioner, not only for IdP "+
			"logins - a local account that quietly gets ten gibibytes while the config says four "+
			"kibibytes is how an operator concludes the quota feature does not work.",
			local.QuotaBytes, quota)
	}
}
