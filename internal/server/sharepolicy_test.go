package server_test

// Share-policy administration (T1c, competitiveness-analysis): the three runtime settings that
// give a deployment a sharing posture — max expiry, default expiry, required password — and
// the guarantee that matters, that the POSTURE CANNOT BE ROUTED AROUND: not at creation, not
// by extending an existing share afterwards.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// setSharePolicy applies the three keys as runtime settings, the way an administrator would.
func setSharePolicy(t *testing.T, h *harness, kv map[string]int64) {
	t.Helper()
	client, _ := h.adminClient(t)
	for k, v := range kv {
		status, out := adminDo(t, h, client, http.MethodPut, "/api/admin/settings/"+k, v)
		if status != http.StatusOK {
			t.Fatalf("set %s=%d: %d %v", k, v, status, out)
		}
	}
}

// shareExpiry reads a share's expires_at out of the owner's share listing.
func shareExpiry(t *testing.T, h *harness, id string) *time.Time {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/shares", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list shares: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Items []struct {
			ID        string     `json:"id"`
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("shares body: %v (%s)", err, body)
	}
	for _, s := range out.Items {
		if s.ID == id {
			return s.ExpiresAt
		}
	}
	t.Fatalf("share %s not in the owner's listing", id)
	return nil
}

func TestSharePolicyCapsAndDefaultsExpiry(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "policy.txt", "bytes")
	setSharePolicy(t, h, map[string]int64{
		"limits.share_max_expiry_seconds":     3600,
		"limits.share_default_expiry_seconds": 600,
	})

	// Over the cap: refused, and the share must not exist afterwards.
	resp := h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"policy.txt","kind":"link","expires_in_seconds":7200}`), nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("over-cap expiry answered %d (%s), want 422", resp.StatusCode, body)
	}

	// No expiry chosen: the default is applied.
	resp = h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"policy.txt","kind":"link"}`), nil)
	body = readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("default-expiry create: %d %s", resp.StatusCode, body)
	}
	var created struct {
		Share struct {
			ID string `json:"id"`
		} `json:"share"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	if created.Share.ID == "" {
		t.Fatalf("create response carries no share id: %s", body)
	}
	exp := shareExpiry(t, h, created.Share.ID)
	if exp == nil {
		t.Fatal("the default expiry was not applied to a share created without one")
	}
	if d := time.Until(*exp); d > 10*time.Minute || d < 8*time.Minute {
		t.Fatalf("default expiry landed at %v from now, want ~10m", d)
	}

	// A caller's shorter choice is honoured — the default is a floor, not an override.
	resp = h.do(http.MethodPatch, "/api/shares/"+created.Share.ID,
		strings.NewReader(`{"expires_in_seconds":60}`), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shorter-than-default patch: %d", resp.StatusCode)
	}

	// The cap cannot be routed around by extending the existing share.
	resp = h.do(http.MethodPatch, "/api/shares/"+created.Share.ID,
		strings.NewReader(`{"expires_in_seconds":7200}`), nil)
	body = readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("extending past the cap answered %d (%s), want 422", resp.StatusCode, body)
	}
}

func TestSharePolicyRequiresAPasswordOnReadShares(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "secret-policy.txt", "bytes")
	setSharePolicy(t, h, map[string]int64{"limits.share_require_password": 1})

	resp := h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"secret-policy.txt","kind":"link"}`), nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a passwordless link under a require-password posture answered %d (%s), want 422",
			resp.StatusCode, body)
	}

	// With a password: allowed.
	resp = h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"secret-policy.txt","kind":"link","password":"let-me-in"}`), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a passworded link under the posture: %d", resp.StatusCode)
	}

	// A drop box is exempt: an upload-only link posted publicly is a deliberate shape the
	// password requirement exists to protect read access, not to defeat.
	resp = h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"/","kind":"request"}`), nil)
	body = readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a drop box under the posture answered %d (%s), want 201 (exempt)", resp.StatusCode, body)
	}
}

func TestSharePolicyOffByDefault(t *testing.T) {
	// The posture of a deployment that never decided one: everything allowed, exactly as
	// before the keys existed. This is the ratchet's other half — the off state must stay
	// off without an explicit act, or every upgrade silently changes sharing behaviour.
	h := newHarness(t)
	h.uploadFile(t, "open.txt", "bytes")

	resp := h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"open.txt","kind":"link","expires_in_seconds":999999}`), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("an unconfigured deployment refused an open-ended link: %d", resp.StatusCode)
	}
}

func TestSharePolicySettingsAreValidated(t *testing.T) {
	h := newHarness(t)
	client, _ := h.adminClient(t)

	status, out := adminDo(t, h, client, http.MethodPut,
		"/api/admin/settings/limits.share_require_password", 2)
	if status != http.StatusBadRequest {
		t.Fatalf("require_password=2 answered %d (%v), want 400", status, out)
	}

	status, _ = adminDo(t, h, client, http.MethodPut,
		"/api/admin/settings/limits.share_max_expiry_seconds", -1)
	if status != http.StatusBadRequest {
		t.Fatalf("a negative max expiry answered %d, want 400", status)
	}
}
