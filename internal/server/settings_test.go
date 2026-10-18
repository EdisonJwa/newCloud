package server_test

// Runtime settings (§10.1 管理配置变更).
//
// The settings table existed from the first migration with no Go code touching it, so every limit was
// a config file and a restart. These tests are about the two halves that matter: the change takes
// effect on the next request (not the next restart), and it is recorded with what it was before,
// because "who lowered the upload limit" is the question a support ticket asks.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestAdminSettingTakesEffectWithoutARestart(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.adminClient(t)

	// The configured cap is 64 KiB in the harness; a 100-byte body fits.
	if code := h.uploadStatus(t, "small.txt", strings.Repeat("x", 100)); code != http.StatusCreated {
		t.Fatalf("baseline upload: %d", code)
	}

	// Lower the cap to 8 bytes at runtime.
	resp := h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.max_file_bytes", `8`)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set setting: %d %s", resp.StatusCode, body)
	}

	// The NEXT request is refused, with no restart and no reconnect.
	if code := h.uploadStatus(t, "too-big.txt", strings.Repeat("x", 100)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an upload over the new cap returned %d; the setting did not take effect", code)
	}

	// And raising it restores service, which is what makes this a control rather than a one-way door.
	resp = h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.max_file_bytes", `65536`)
	resp.Body.Close()
	if code := h.uploadStatus(t, "fits-again.txt", strings.Repeat("x", 100)); code != http.StatusCreated {
		t.Fatalf("the upload was still refused after the cap was raised: %d", code)
	}

	// The change is audited with the previous value.
	var changes int
	for _, e := range h.meta.AuditLog() {
		if e.Action == "admin.setting.change" {
			changes++
			if e.Metadata["to"] == nil {
				t.Fatalf("the audit row does not record the new value: %v", e.Metadata)
			}
		}
	}
	if changes != 2 {
		t.Fatalf("want two admin.setting.change rows, got %d", changes)
	}
}

func TestDeletingAnOverrideRestoresTheConfiguredValue(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.adminClient(t)

	if resp := h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.max_file_bytes", `8`); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("set: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if code := h.uploadStatus(t, "x.txt", strings.Repeat("x", 100)); code == http.StatusCreated {
		t.Fatal("the override was not in force")
	}

	resp := h.adminDo(t, admin, http.MethodDelete, "/api/admin/settings/limits.max_file_bytes", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if code := h.uploadStatus(t, "y.txt", strings.Repeat("x", 100)); code != http.StatusCreated {
		t.Fatalf("deleting the override did not restore the configured cap: %d", code)
	}
}

func TestUnknownSettingIsRefusedByName(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.adminClient(t)

	resp := h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.teleport", `1`)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown key was accepted: %d", resp.StatusCode)
	}
	// The message has to name the keys that DO work, or the operator is left guessing.
	if !strings.Contains(body, "limits.max_file_bytes") {
		t.Fatalf("the refusal does not list the accepted keys: %s", body)
	}
}

func TestNonAdminCannotChangeSettings(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPut, "/api/admin/settings/limits.max_file_bytes",
		strings.NewReader(`1`), map[string]string{"Content-Type": "application/json"})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a non-admin changed a setting: %s", body)
	}
	// And nothing was written.
	var wrote bool
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.ReadSettings(ctx)
		_, wrote = got["limits.max_file_bytes"]
		return err
	})
	if wrote {
		t.Fatal("the refused request wrote the setting anyway")
	}
}

// uploadStatus uploads and returns the status code.
func (h *harness) uploadStatus(t *testing.T, name, content string) int {
	t.Helper()
	body := multipartBody(t, "", name, content)
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	code := resp.StatusCode
	resp.Body.Close()
	return code
}

var _ = json.Marshal

// TestEveryAllowListedKeyReachesAReadPoint is the check that keeps the allow-list honest.
//
// A settings table's characteristic failure is a key that stores fine and changes nothing: the operator
// sees "override" in the API and the product keeps behaving as configured. Each key is exercised
// through the behaviour it is supposed to control, so adding one to the list without wiring a read
// point fails here rather than in a support ticket.
func TestEveryAllowListedKeyReachesAReadPoint(t *testing.T) {
	t.Run("limits.upload_chunk_bytes", func(t *testing.T) {
		h := newHarness(t)
		admin, _ := h.adminClient(t)

		session := startUpload(t, h, "", "chunked.bin", 16)
		// At the configured 64 KiB cap this chunk is accepted.
		if resp, payload := sendChunk(t, h, session.ID, 0, 3, 16, "abcd"); resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("baseline chunk: %d %v", resp.StatusCode, payload)
		} else {
			resp.Body.Close()
		}

		resp := h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.upload_chunk_bytes", `2`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set chunk cap: %d", resp.StatusCode)
		}

		over, payload := sendChunk(t, h, session.ID, 4, 9, 16, "abcdef")
		defer over.Body.Close()
		if over.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("a chunk over the runtime cap returned %d, want 413: %v. The key is stored and "+
				"not read, which is the failure mode the allow-list exists to prevent.",
				over.StatusCode, payload)
		}
	})

	t.Run("limits.max_pending_upload_bytes", func(t *testing.T) {
		h := newHarness(t)
		admin, _ := h.adminClient(t)

		// No cap configured: a large session is accepted.
		start := func(size int64) int {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"name": "cap.bin", "size": size, "path": ""})
			resp := h.do(http.MethodPost, "/api/files/uploads", strings.NewReader(string(body)),
				map[string]string{"Content-Type": "application/json"})
			code := resp.StatusCode
			resp.Body.Close()
			return code
		}
		if code := start(1 << 20); code != http.StatusCreated {
			t.Fatalf("baseline session: %d", code)
		}

		resp := h.adminDo(t, admin, http.MethodPut,
			"/api/admin/settings/limits.max_pending_upload_bytes", `1024`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set pending cap: %d", resp.StatusCode)
		}

		// The NEXT request is refused without a restart — this is the property the config-only cap
		// did not have.
		if code := start(1 << 20); code != http.StatusInsufficientStorage {
			t.Fatalf("a session over the runtime staging cap returned %d, want 507", code)
		}
	})

	t.Run("limits.max_retained_version_bytes", func(t *testing.T) {
		// The worker reads this one, so the property is the effective value the worker resolves.
		h := newHarness(t)
		admin, _ := h.adminClient(t)

		resp := h.adminDo(t, admin, http.MethodPut,
			"/api/admin/settings/limits.max_retained_version_bytes", `777`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set retained-history cap: %d", resp.StatusCode)
		}
		if got := h.srvServer.MaxRetainedVersionBytesForTest(context.Background()); got != 777 {
			t.Fatalf("the worker would use %d, want the override 777", got)
		}
	})

	t.Run("limits.default_file_quota_bytes", func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t)
		admin, _ := h.adminClient(t)

		resp := h.adminDo(t, admin, http.MethodPut,
			"/api/admin/settings/limits.default_file_quota_bytes", `12345`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set default quota: %d", resp.StatusCode)
		}
		// The value the server would give a new account: the same helper the provisioning path uses,
		// called through the same cache. A new account cannot be created directly in the harness, so
		// the helper is what is checked — and it is the single place both provisioners call.
		got := h.srvServer.InitialQuota(ctx)
		if got == nil {
			t.Fatalf("a new account would get no quota at all, want the override 12345. The key is " +
				"stored and not read.")
		}
		if *got != 12345 {
			t.Fatalf("a new account would get quota %d, want the override 12345. The key is "+
				"stored and not read.", *got)
		}

		// And it does not touch an existing account: changing the default must not rewrite limits.
		var before, after int64
		_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			u, err := tx.UserByID(ctx, h.user.ID)
			before = u.QuotaBytes
			return err
		})
		h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.default_file_quota_bytes", `999`).Body.Close()
		_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			u, err := tx.UserByID(ctx, h.user.ID)
			after = u.QuotaBytes
			return err
		})
		if before != after {
			t.Fatalf("changing the default quota rewrote an existing account: %d -> %d", before, after)
		}
	})
}

// TestTrashCapIsEnforcedThroughTheAPI covers §4.3's retained-usage cap end to end.
//
// The store conformance suite proves the check; this proves the SERVER passes the configured cap to
// it, answers with the right status and code, and says "the trash is full" rather than "your quota is
// full" — the two resources are separate, and the wrong message sends the user to look at a quota bar
// that has room on it.
func TestTrashCapIsEnforcedThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.adminClient(t)

	// A cap smaller than one file, set at runtime.
	resp := h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.max_trash_bytes", `10`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set trash cap: %d", resp.StatusCode)
	}

	h.uploadFile(t, "big.bin", strings.Repeat("x", 100))

	del := h.do(http.MethodDelete, "/api/files/?path=big.bin", nil, nil)
	body := readBody(del)
	del.Body.Close()
	if del.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("deleting into a full trash returned %d %s, want 507", del.StatusCode, body)
	}
	if !strings.Contains(body, "trash_full") {
		t.Fatalf("the error code should name the trash, got %s", body)
	}

	// Raising the cap lets the same delete through, which proves the refusal was the cap and not
	// something else about the request.
	resp = h.adminDo(t, admin, http.MethodPut, "/api/admin/settings/limits.max_trash_bytes", `100000`)
	resp.Body.Close()
	// A successful delete is 204 No Content.
	if del := h.do(http.MethodDelete, "/api/files/?path=big.bin", nil, nil); del.StatusCode != http.StatusNoContent {
		code := del.StatusCode
		del.Body.Close()
		t.Fatalf("the delete was still refused after raising the cap: %d", code)
	}
}

// TestPendingUploadCapIsEnforcedAtSessionCreation: §4.3's cap on unfinished uploads.
//
// Declared sizes, not written bytes: the point is to stop a client reserving unbounded staging space.
// The status is 507 rather than 429 because "retry later" is wrong advice — the client has to finish
// or abort something before it can start another.
func TestPendingUploadCapIsEnforcedAtSessionCreation(t *testing.T) {
	h := newHarnessTuned(t, func(c *config.Config) {
		c.Limits.MaxPendingUploadBytes = 1000
	}, nil)

	start := func(name string, size int64) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": name, "size": size, "path": ""})
		resp := h.do(http.MethodPost, "/api/files/uploads", strings.NewReader(string(body)),
			map[string]string{"Content-Type": "application/json"})
		defer resp.Body.Close()
		return resp.StatusCode, readBody(resp)
	}

	if code, payload := start("first.bin", 600); code != http.StatusCreated {
		t.Fatalf("first session: %d %s", code, payload)
	}
	code, payload := start("second.bin", 600)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("a session over the staging allowance returned %d %s, want 507", code, payload)
	}
	if !strings.Contains(payload, "pending_uploads_full") {
		t.Fatalf("the error should name the staging allowance, got %s", payload)
	}
}

// TestAdminCanProvisionAnAccount is ID-09's management-API half: 自动化开户 without SCIM.
//
// The account has no IdP identity, which is the point — the store claims a pre-provisioned row by
// username on first login, so an operator can set role and quota before the person ever signs in.
func TestAdminCanProvisionAnAccount(t *testing.T) {
	h := newHarness(t)
	admin, _ := h.adminClient(t)

	resp := h.adminDo(t, admin, http.MethodPost, "/api/admin/users",
		`{"username":"provisioned","email":"p@example.test","role":"user","quota_bytes":2048}`)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %s", resp.StatusCode, body)
	}

	// The account exists with what was asked for, and an audit row says who created it.
	var found bool
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UserByUsername(ctx, "provisioned")
		found = err == nil && u.QuotaBytes == 2048 && u.Role == "user" && u.IsActive()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the created account does not match the request")
	}
	var audited bool
	for _, e := range h.meta.AuditLog() {
		if e.Action == "user.create" && e.Metadata["source"] == "admin" {
			audited = true
		}
	}
	if !audited {
		t.Fatal("creating an account through the API was not audited; an account that appears " +
			"without a record of who made it is how a backdoor looks")
	}

	// Re-running the provisioning must conflict rather than rewrite the account.
	again := h.adminDo(t, admin, http.MethodPost, "/api/admin/users",
		`{"username":"provisioned","email":"p@example.test","role":"admin"}`)
	againBody := readBody(again)
	again.Body.Close()
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("re-creating the same username returned %d (%s), want 409", again.StatusCode, againBody)
	}
	var role string
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		u, err := tx.UserByUsername(ctx, "provisioned")
		role = u.Role
		return err
	})
	if role != "user" {
		t.Fatalf("the refused create promoted the account to %q", role)
	}
}
