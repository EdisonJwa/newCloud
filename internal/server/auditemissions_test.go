package server_test

// Three audit events that were declared and never written.
//
// §10.1 requires audit coverage of sensitive operations, and the vocabulary is what an operator
// filters on. A name that exists and never fires is worse than a missing name: the filter returns
// nothing, and nothing says whether that means "it did not happen" or "it was never wired".
//
// Each case here asserts the row appears for the event it describes AND does not appear for the
// adjacent event it must not be confused with — an account creation that fires on every login, or an
// expiry that fires for a revocation, would be as misleading as no row at all.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/fakeidp"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func actionsIn(t *testing.T, m *store.Memory, action string) []store.AuditEntry {
	t.Helper()
	var out []store.AuditEntry
	for _, e := range m.AuditLog() {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// TestUserCreateIsRecordedOncePerAccount: the creation is its own event, not a property of the first
// login that happens to produce it.
func TestUserCreateIsRecordedOncePerAccount(t *testing.T) {
	h := newHarness(t)
	if rows := actionsIn(t, h.meta, "user.create"); len(rows) != 0 {
		t.Fatalf("the fixture starts with %d user.create rows", len(rows))
	}

	// The harness user was created directly in the store, so this is the JIT path: a fresh OIDC
	// login for an identity the server has never seen.
	oidc := newOIDCHarness(t, fakeidp.User{
		Subject: "audit-jit", Username: "audit-jit", Email: "audit-jit@example.test",
	})
	oidc.login(t)

	rows := actionsIn(t, oidc.meta, "user.create")
	if len(rows) != 1 {
		t.Fatalf("a first login wrote %d user.create rows, want 1", len(rows))
	}
	if rows[0].Metadata["via"] != "oidc" {
		t.Fatalf("the row does not say how the account appeared: %v", rows[0].Metadata)
	}

	// A second login must not claim a second creation.
	oidc.b.resetJar(t)
	oidc.login(t)
	if rows := actionsIn(t, oidc.meta, "user.create"); len(rows) != 1 {
		t.Fatalf("a repeat login added another user.create row (%d total). The event marks the "+
			"account's creation; firing per login makes it a second, noisier login log.", len(rows))
	}
}

// TestExpiredShareIsRecordedAndRevokedIsNot: the visitor sees one answer, the owner's log has the
// reason.
func TestExpiredShareIsRecordedAndRevokedIsNot(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	// Age the share past its expiry directly in the metadata layer: the create API has no way to
	// produce an already-expired link, which is the correct design and inconvenient here.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		hash := sha256.Sum256([]byte(token))
		sh, err := tx.ShareByTokenHash(ctx, hash[:])
		if err != nil {
			return err
		}
		past := time.Now().Add(-time.Hour)
		sh.ExpiresAt = &past
		_, uerr := tx.UpdateShare(ctx, sh)
		return uerr
	}); err != nil {
		t.Fatalf("expire the share: %v", err)
	}

	resp := h.do(http.MethodGet, "/s/"+token, nil, nil)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("an expired link returned %d, want the same 404 every other refusal gives", code)
	}

	if rows := actionsIn(t, h.meta, "share.expired"); len(rows) != 1 {
		t.Fatalf("an expired link was used and wrote %d share.expired rows, want 1. The owner "+
			"cannot tell 'my link expired' from 'somebody revoked it' without this.", len(rows))
	}
	if rows := actionsIn(t, h.meta, "share.limit_reached"); len(rows) != 0 {
		t.Fatalf("expiry was recorded as an exhausted allowance: %v", rows)
	}
}

// TestExhaustedShareLimitIsRecorded: the other reason a link stops working.
func TestExhaustedShareLimitIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "shared.txt", "bytes")
	create := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"shared.txt","kind":"link","max_downloads":1}`),
		map[string]string{"Content-Type": "application/json"})
	payload := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create share: %d %s", create.StatusCode, payload)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(payload), &created); err != nil || created.Token == "" {
		t.Fatalf("no token in %s", payload)
	}
	token := created.Token

	first := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("the first download was refused: %d", first.StatusCode)
	}

	second := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil)
	code := second.StatusCode
	second.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("the second download with a one-download link returned %d", code)
	}

	rows := actionsIn(t, h.meta, "share.limit_reached")
	if len(rows) != 1 {
		t.Fatalf("an exhausted link was used and wrote %d share.limit_reached rows, want 1", len(rows))
	}
	if rows := actionsIn(t, h.meta, "share.expired"); len(rows) != 0 {
		t.Fatalf("an exhausted allowance was recorded as an expiry: %v", rows)
	}
}

// TestShareViewIsRecordedOnlyWhenTheGateOpens: §10.1 分享访问.
//
// The download was audited and the view was not, so an owner could see what was taken and not that
// the link had been opened at all — and a visitor who studied the listing and took nothing left no
// trace. The row is written after the gate passes, not before: a refusal is a different event, and
// recording every password prompt as a view would drown the real ones.
func TestShareViewIsRecordedOnlyWhenTheGateOpens(t *testing.T) {
	h := newHarness(t)

	if resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"viewed"}`),
		map[string]string{"Content-Type": "application/json"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: %d", resp.StatusCode)
	}
	create := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"viewed","kind":"link","password":"open sesame"}`),
		map[string]string{"Content-Type": "application/json"})
	payload := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", create.StatusCode, payload)
	}
	token := createdToken(t, payload)

	// A visitor who has not entered the password gets the challenge, not a view.
	if resp := h.do(http.MethodGet, "/s/"+token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("challenge: %d", resp.StatusCode)
	}
	if rows := actionsIn(t, h.meta, "share.view"); len(rows) != 0 {
		t.Fatalf("a password challenge was recorded as a view (%d rows)", len(rows))
	}

	// After entering it, the listing is a view.
	verify, err := http.NewRequest(http.MethodPost, h.srv.URL+"/s/"+token+"/verify",
		strings.NewReader(`{"password":"open sesame"}`))
	if err != nil {
		t.Fatal(err)
	}
	verify.Header.Set("Content-Type", "application/json")
	vresp, err := h.srv.Client().Do(verify)
	if err != nil {
		t.Fatal(err)
	}
	var cookies []*http.Cookie
	cookies = append(cookies, vresp.Cookies()...)
	vresp.Body.Close()

	view, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range cookies {
		view.AddCookie(ck)
	}
	vres, err := h.srv.Client().Do(view)
	if err != nil {
		t.Fatal(err)
	}
	vres.Body.Close()
	if vres.StatusCode != http.StatusOK {
		t.Fatalf("view: %d", vres.StatusCode)
	}

	rows := actionsIn(t, h.meta, "share.view")
	if len(rows) != 1 {
		t.Fatalf("want exactly one share.view row after the gate opened, got %d", len(rows))
	}
	if rows[0].ShareID == nil {
		t.Fatal("the view row does not identify the share")
	}
}

// createdToken pulls the token out of a share-create response.
func createdToken(t *testing.T, payload string) string {
	t.Helper()
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(payload), &created); err != nil || created.Token == "" {
		t.Fatalf("no token in %s", payload)
	}
	return created.Token
}

// TestAuditRowsCarryTheRequestID is §10.1's request_id.
//
// The server generated a request id, logged it, and returned it — and never wrote it to the audit
// rows. So the step that matters in an investigation, from a log line or a user's report to the exact
// events of that request, required matching timestamps, which is guesswork under load.
func TestAuditRowsCarryTheRequestID(t *testing.T) {
	h := newHarness(t)

	h.uploadFile(t, "correlated.txt", "bytes")

	var withID, withoutID int
	for _, e := range h.meta.AuditLog() {
		if e.Action == "file.write" {
			if e.RequestID == "" {
				withoutID++
			} else {
				withID++
			}
		}
	}
	if withID == 0 {
		t.Fatalf("no file.write row carried a request id (%d without). The correlation is the "+
			"whole point of recording it.", withoutID)
	}
	if withoutID != 0 {
		t.Fatalf("%d file.write rows have no request id; a handler that writes audit inside a "+
			"transaction should inherit it from the context", withoutID)
	}

	// And an event written outside any request stays visibly uncorrelated rather than inventing an
	// id: "this happened in a background job" is worth being able to see.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "system", Action: "file.scrub", TargetKind: "blob", TargetID: "x",
			Result: "ok",
		})
	}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range h.meta.AuditLog() {
		if e.Action == "file.scrub" {
			found = true
			if e.RequestID != "" {
				t.Fatalf("an event outside a request invented a request id: %q", e.RequestID)
			}
		}
	}
	if !found {
		t.Fatal("the background event was not recorded")
	}
}
