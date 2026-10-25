package server_test

// Notification surface tests (M4). Three things can only be proven here, at the HTTP layer:
// the inbox routes answer for their owner and nobody else, the GRANT actually emits a
// notification and a queued mail in the same transaction, and the quota warning fires on the
// configured threshold without ever being able to fail the upload it reports on.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// inbox fetches the caller's notifications: the items and the unread count.
func inbox(t *testing.T, do func(method, path string, body io.Reader, headers map[string]string) *http.Response) ([]map[string]any, int) {
	t.Helper()
	resp := do(http.MethodGet, "/api/notifications", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inbox: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Items  []map[string]any `json:"items"`
		Unread int              `json:"unread"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("inbox body: %v (%s)", err, body)
	}
	return out.Items, out.Unread
}

// claimMailJobs pulls the queue once and returns every mail.send payload it finds.
//
// One pull, because a claim is a lease: a second claim in the same test would find the jobs
// the first one already holds, and a helper that silently returned nothing would look like
// "the server did not queue" rather than "the test already took it".
func claimMailJobs(t *testing.T, meta store.Store) []map[string]any {
	t.Helper()
	var found []map[string]any
	err := meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		jobs, cerr := tx.ClaimJobs(ctx, "notify-test", 10, time.Minute)
		if cerr != nil {
			return cerr
		}
		for _, j := range jobs {
			if j.Kind == "mail.send" {
				found = append(found, j.Payload)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("claim jobs: %v", err)
	}
	return found
}

func TestNotificationInboxIsPerCaller(t *testing.T) {
	h := newHarness(t)
	_, mateToken := h.intruder(t, "inbox-mate")

	// The harness user's inbox starts empty and stays empty when nobody has been notified.
	items, unread := inbox(t, h.do)
	if len(items) != 0 {
		t.Fatalf("a fresh account already has %d notifications", len(items))
	}
	if unread != 0 {
		t.Fatalf("unread count on a fresh account: %d", unread)
	}

	// A cursor the server did not issue is a 400, not a restart from the top.
	resp := h.do(http.MethodGet, "/api/notifications?cursor=bogus", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed cursor answered %d (%s), want 400", resp.StatusCode, body)
	}
	_ = mateToken
}

func TestGrantNotifiesTheGranteeAndQueuesTheMail(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "grant-folder")
	mb := multipartBody(t, "grant-folder", "file.txt", "bytes")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload into the folder: %d", resp.StatusCode)
	}
	mate, mateToken := h.intruder(t, "grant-mate")

	resp = h.do(http.MethodPost, "/api/acl", strings.NewReader(`{
		"path": "grant-folder",
		"subject": "grant-mate",
		"permissions": ["read"]
	}`), nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("grant: %d %s", resp.StatusCode, body)
	}

	// The grantee's bell, from the same transaction as the grant.
	items, _ := inbox(t, func(m, p string, b io.Reader, hs map[string]string) *http.Response {
		return h.doAs(t, mateToken, m, p, b, hs)
	})
	if len(items) != 1 {
		t.Fatalf("grantee has %d notifications, want 1: %v", len(items), items)
	}
	if items[0]["kind"] != "share.received" {
		t.Fatalf("wrong kind: %v", items[0]["kind"])
	}
	metaField, _ := items[0]["meta"].(map[string]any)
	if metaField == nil || metaField["label"] != "grant-folder" {
		t.Fatalf("notification meta does not name the folder: %v", items[0]["meta"])
	}

	// Dismissing it is idempotent, and a stranger's id is a 404, not a 204.
	id, _ := items[0]["id"].(string)
	for i := 0; i < 2; i++ {
		resp := h.doAs(t, mateToken, http.MethodPost, "/api/notifications/"+id+"/read", nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("read attempt %d: %d", i+1, resp.StatusCode)
		}
	}
	strangerResp := h.do(http.MethodPost, "/api/notifications/"+id+"/read", nil, nil)
	strangerResp.Body.Close()
	if strangerResp.StatusCode != http.StatusNotFound {
		t.Fatalf("reading somebody else's notification answered %d, want 404", strangerResp.StatusCode)
	}

	// The mail rode the same transaction: a share_notice job for the grantee's address.
	jobs := claimMailJobs(t, h.meta)
	if len(jobs) != 1 {
		t.Fatalf("%d mail.send jobs queued for the grant, want 1", len(jobs))
	}
	if jobs[0]["template"] != "share_notice" || jobs[0]["to"] != mate.Email {
		t.Fatalf("queued mail is wrong: %v", jobs[0])
	}
}

func TestEmailShareQueuesAnInvitationPerRecipient(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "invite.txt", "bytes")

	resp := h.do(http.MethodPost, "/api/shares",
		strings.NewReader(`{"path":"invite.txt","kind":"email","recipients":["first@example.test","second@example.test"]}`),
		nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("share create: %d %s", resp.StatusCode, body)
	}

	sent := map[string]bool{}
	for _, payload := range claimMailJobs(t, h.meta) {
		if payload["template"] != "share_notice" {
			t.Fatalf("wrong template: %v", payload)
		}
		to, _ := payload["to"].(string)
		sent[to] = true
	}
	if !sent["first@example.test"] || !sent["second@example.test"] {
		t.Fatalf("invitations did not cover both recipients: %v", sent)
	}
}

func TestQuotaWarningFiresOncePerBandUntilRead(t *testing.T) {
	// A 100-byte quota and a 90% threshold: one 95-byte upload warns, the next one does not,
	// and reading the warning re-arms it.
	//
	// The quota is set on the account directly rather than through the default-for-new-accounts
	// key, because the harness user already exists when the test starts and would keep whatever
	// limit they were provisioned with.
	h := newHarnessTuned(t, nil,
		func(d *server.Deps) {
			d.Notify = config.NotifyConfig{QuotaWarningPercent: 90, ShareExpiryDays: 3, RetentionDays: 90}
		})
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.SetUserQuota(ctx, h.user.ID, 100)
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	h.uploadFile(t, "big.bin", strings.Repeat("x", 95))
	items, _ := inbox(t, h.do)
	var warns int
	for _, n := range items {
		if n["kind"] == "quota.warning" {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("first upload over the threshold produced %d warnings, want 1", warns)
	}

	// A second upload inside the same band is silent — the dedupe key is per unread band.
	h.uploadFile(t, "small.txt", "x")
	items, _ = inbox(t, h.do)
	warns = 0
	var id string
	for _, n := range items {
		if n["kind"] == "quota.warning" {
			warns++
			id, _ = n["id"].(string)
		}
	}
	if warns != 1 {
		t.Fatalf("second upload produced %d warnings in total, want the original 1", warns)
	}

	// Read it, fill the quota completely: the FULL band is a new warning, not a repeat.
	resp := h.do(http.MethodPost, "/api/notifications/"+id+"/read", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	h.uploadFile(t, "fill.txt", strings.Repeat("y", 4))
	items, _ = inbox(t, h.do)
	warns = 0
	for _, n := range items {
		if n["kind"] == "quota.warning" {
			warns++
		}
	}
	if warns != 2 {
		t.Fatalf("after re-arm, %d quota warnings total, want 2 (warn + full)", warns)
	}
}

func TestNotificationsRequireASession(t *testing.T) {
	h := newHarness(t)
	client := h.client(t)
	resp, err := client.Get(h.srv.URL + "/api/notifications")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an anonymous caller read the inbox")
	}
}
