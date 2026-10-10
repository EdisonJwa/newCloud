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
