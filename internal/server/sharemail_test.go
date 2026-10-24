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

// Email shares over HTTP.
//
// The unit tests in internal/share cover the rules; these cover the wiring, which is
// where the earlier gap actually lived: the rules existed but no handler consulted them,
// so an email share behaved exactly like a link share for anyone who had the URL.

// emailShareFixture uploads a file and creates an email share for it.
func emailShareFixture(t *testing.T, h *harness, recipients ...string) (token string) {
	t.Helper()

	body := multipartBody(t, "", "secret.txt", "confidential")
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	payload := map[string]any{
		"path":       "secret.txt",
		"kind":       "email",
		"recipients": recipients,
	}
	req := h.do(http.MethodPost, "/api/shares/", jsonBody(t, payload),
		map[string]string{"Content-Type": "application/json"})
	if req.StatusCode != http.StatusCreated {
		t.Fatalf("create share: %d %s", req.StatusCode, readBody(req))
	}
	created := decode(t, req)

	tok, _ := created["token"].(string)
	if tok == "" {
		t.Fatal("no token returned")
	}
	return tok
}

// TestEmailSharePageRequiresVerification: the landing page must ask for an address, not
// expose the share.
func TestEmailSharePageRequiresVerification(t *testing.T) {
	h := newHarness(t)
	token := emailShareFixture(t, h, "alice@example.test")

	resp, err := http.Get(h.srv.URL + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	info := decode(t, resp)

	if info["requires_email"] != true {
		t.Fatalf("an email share must ask for verification, got %v", info)
	}
	// Nothing about the shared content may be present at this point.
	if _, leaked := info["children"]; leaked {
		t.Fatal("the unverified page must not include a file listing")
	}
	if _, leaked := info["root_name"]; leaked {
		t.Fatal("the unverified page must not reveal the target name")
	}
}

// TestEmailShareDownloadBlockedWithoutVerification is the security fix, stated as a test:
// holding the URL must not be enough.
func TestEmailShareDownloadBlockedWithoutVerification(t *testing.T) {
	h := newHarness(t)
	token := emailShareFixture(t, h, "alice@example.test")

	resp, err := http.Get(h.srv.URL + "/s/" + token + "/download")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("an email share must not be downloadable before the address is verified")
	}
}

// TestEmailShareVerificationFlow walks invite → code → verify → download.
func TestEmailShareVerificationFlow(t *testing.T) {
	h := newHarness(t)
	token := emailShareFixture(t, h, "alice@example.test")

	// Step 1: request a code. The response is deliberately uniform.
	resp, err := http.Post(h.srv.URL+"/s/"+token+"/recipient", "application/json",
		strings.NewReader(`{"email":"alice@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("code request: want 202, got %d", resp.StatusCode)
	}

	// Fetch the code from the mail path. No transport is configured in tests, so the
	// server records it; this is the only place a test may look at it.
	code := h.lastRecipientCode(t, "alice@example.test")
	if code == "" {
		t.Fatal("no verification code was generated for the invited address")
	}

	// Step 2: exchange it.
	jar := h.client(t)
	verifyResp, err := jar.Post(h.srv.URL+"/s/"+token+"/recipient/verify", "application/json",
		jsonBody(t, map[string]any{"email": "alice@example.test", "code": code}))
	if err != nil {
		t.Fatal(err)
	}
	verifyResp.Body.Close()
	if verifyResp.StatusCode != http.StatusOK {
		t.Fatalf("verify: want 200, got %d", verifyResp.StatusCode)
	}

	// Step 3: the download now works.
	dl, err := jar.Get(h.srv.URL + "/s/" + token + "/download")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download after verification: want 200, got %d", dl.StatusCode)
	}
	if got := readBody(dl); got != "confidential" {
		t.Fatalf("content: %q", got)
	}
}

// TestEmailShareUninvitedAddressRejected: the response must not reveal whether the
// address was on the list.
func TestEmailShareUninvitedAddressRejected(t *testing.T) {
	h := newHarness(t)
	token := emailShareFixture(t, h, "alice@example.test")

	resp, err := http.Post(h.srv.URL+"/s/"+token+"/recipient", "application/json",
		strings.NewReader(`{"email":"stranger@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Accepted regardless: whether an address was invited must not be observable from
	// an unauthenticated endpoint.
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want a uniform 202, got %d", resp.StatusCode)
	}
	if code := h.lastRecipientCode(t, "stranger@example.test"); code != "" {
		t.Fatal("no code may be generated for an address that was never invited")
	}
}

// TestLinkShareStillWorksWithoutVerification: the gate must not break plain links.
func TestLinkShareStillWorksWithoutVerification(t *testing.T) {
	h := newHarness(t)

	body := multipartBody(t, "", "open.txt", "public")
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	resp.Body.Close()

	req := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"open.txt","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	if req.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", req.StatusCode, readBody(req))
	}
	created := decode(t, req)
	token, _ := created["token"].(string)

	page, err := http.Get(h.srv.URL + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	info := decode(t, page)
	if info["requires_email"] == true {
		t.Fatal("a link share must not demand email verification")
	}

	dl, err := http.Get(h.srv.URL + "/s/" + token + "/download")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("link download: want 200, got %d", dl.StatusCode)
	}
}

// TestAuditExportIsScopedToTheCaller: a user exports their own events, not everyone's.
func TestAuditExportIsScopedToTheCaller(t *testing.T) {
	h := newHarness(t)

	// Produce an event belonging to this user.
	body := multipartBody(t, "", "note.txt", "x")
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	resp.Body.Close()

	export := h.do(http.MethodGet, "/api/audit?format=json", nil, nil)
	defer export.Body.Close()
	if export.StatusCode != http.StatusOK {
		t.Fatalf("export: want 200, got %d", export.StatusCode)
	}

	var payload struct {
		Events []struct {
			Action   string `json:"action"`
			ActorID  string `json:"actor_user_id"`
			Actor    string `json:"actor_kind"`
			PrevHash string `json:"prev_hash"`
			Hash     string `json:"hash"`
		} `json:"events"`
		Total int `json:"total_matched"`
	}
	if err := json.NewDecoder(export.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Events) == 0 {
		t.Fatal("the export should contain at least the upload that just happened")
	}

	// An ordinary user must only ever see their own actor id.
	for _, e := range payload.Events {
		if e.ActorID != "" && e.ActorID != h.user.ID.String() {
			t.Fatalf("export leaked another actor's event: %s", e.ActorID)
		}
	}
}

// TestAuditExportCSV: the CSV format exists because a person reads it.
func TestAuditExportCSV(t *testing.T) {
	h := newHarness(t)

	body := multipartBody(t, "", "csv.txt", "x")
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()}).Body.Close()

	resp := h.do(http.MethodGet, "/api/audit?format=csv", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content type: %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("the export must download as a file, got %q", cd)
	}
}

// TestAuditExportRejectsBadFilter: silently exporting a different range than requested is
// how someone draws the wrong conclusion from an audit.
func TestAuditExportRejectsBadFilter(t *testing.T) {
	h := newHarness(t)

	for _, q := range []string{
		"?since=not-a-date",
		"?until=13/40/2020",
		"?result=maybe",
		"?share=not-a-uuid",
	} {
		resp := h.do(http.MethodGet, "/api/audit"+q, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", q, resp.StatusCode)
		}
	}
}

// TestAuditExportRecordsItself: reading the audit log is an auditable act.
func TestAuditExportRecordsItself(t *testing.T) {
	h := newHarness(t)

	first := h.do(http.MethodGet, "/api/audit?format=json", nil, nil)
	first.Body.Close()

	second := h.do(http.MethodGet, "/api/audit?format=json", nil, nil)
	defer second.Body.Close()

	var payload struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	if err := json.NewDecoder(second.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	var sawExport bool
	for _, e := range payload.Events {
		if e.Action == "audit.export" {
			sawExport = true
		}
	}
	if !sawExport {
		t.Fatal("exporting the audit log must itself appear in the log")
	}
}

// helpers

// TestAuditExportIncludesAnonymousEventsAboutOwnedShares guards the reason the owner
// scope exists.
//
// Filtering the export on actor id alone answered "what did I do" while the owner's real
// question is "what happened to my files". The anonymous download of a link, and the
// refused attempt from an address that was never invited, carry no actor account at all —
// so a pure actor filter hid exactly the rows worth reading.
func TestAuditExportIncludesAnonymousEventsAboutOwnedShares(t *testing.T) {
	h := newHarness(t)
	token := emailShareFixture(t, h, "alice@example.test")

	// An anonymous visitor asks for a code using an address that was never invited. The
	// server records a denial with no actor account.
	anon, err := http.Post(h.srv.URL+"/s/"+token+"/recipient", "application/json",
		strings.NewReader(`{"email":"stranger@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()

	resp := h.do(http.MethodGet, "/api/audit?format=json", nil, nil)
	defer resp.Body.Close()

	var payload struct {
		Events []struct {
			Action    string `json:"action"`
			ActorKind string `json:"actor_kind"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	var sawAnonymousDenial bool
	for _, e := range payload.Events {
		if e.Action == "share.recipient.denied" && e.ActorKind == "anonymous" {
			sawAnonymousDenial = true
		}
	}
	if !sawAnonymousDenial {
		t.Fatal("an owner must see anonymous attempts on their own share; " +
			"a pure actor filter would hide exactly that")
	}
}

// TestAuditExportExcludesOtherOwnersEvents: the scope must not become "everything".
func TestAuditExportExcludesOtherOwnersEvents(t *testing.T) {
	h := newHarness(t)

	// An event belonging to a different account and a share it owns.
	other := h.meta.AddUser(store.User{Username: "mallory", Email: "m@example.test", Role: "user"})
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		shareID := uuid.New()
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "anonymous", Action: "share.download",
			TargetKind: "share", TargetID: shareID.String(),
			ShareID: &shareID, Result: "ok",
			Metadata: map[string]any{"owner": other.ID.String()},
		})
	})

	resp := h.do(http.MethodGet, "/api/audit?format=json", nil, nil)
	defer resp.Body.Close()

	var payload struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	// The fabricated share has no row, so it belongs to nobody and must be excluded.
	for _, e := range payload.Events {
		if e.Action == "share.download" {
			t.Fatal("an event about a share the caller does not own must not be exported")
		}
	}
}

// TestShareCreateAcceptsThePayloadTheDialogSends is the create-dialog half of the email-share feature.
//
// The feature existed end to end — the handler accepted `kind: "email"` with `recipients`, the page
// demanded verification, the mailer sent codes — and nothing in the browser could produce one: the
// create dialog's type selector offered only link and drop box, and its request type was a
// hand-written copy of the schema that had no `recipients` field. That is why the gap survived
// testing: every Go test built its own payload with curl-shaped JSON, so the one caller that could
// not send it was the one nobody exercised.
//
// This asserts the exact body ShareDialog now submits. It is here rather than in the TypeScript
// because the contract is the server's: a wrong field name typechecks fine against a spec that
// agrees with itself and fails only when a real handler decodes it.
func TestShareCreateAcceptsThePayloadTheDialogSends(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "offer.txt", "terms")

	payload := map[string]any{
		"path":               "offer.txt",
		"kind":               "email",
		"recipients":         []string{"buyer@example.test", "counsel@example.test"},
		"expires_in_seconds": 3600,
	}

	resp := h.do(http.MethodPost, "/api/shares/", jsonBody(t, payload),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the dialog's create request was refused: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)

	// The confirmation panel reads the recipients back, so they have to be in the response.
	sh, _ := created["share"].(map[string]any)
	got, _ := sh["recipients"].([]any)
	if len(got) != 2 {
		t.Fatalf("recipients = %v, want the two the dialog sent", sh["recipients"])
	}

	// And the payload produced a share that is actually restricted, not merely labelled "email":
	// the landing page asks for an address before it shows anything.
	token, _ := created["token"].(string)
	page := h.do(http.MethodGet, "/s/"+token, nil, map[string]string{"Accept": "application/json"})
	info := decode(t, page)
	if info["requires_email"] != true {
		t.Fatalf("a share created with kind=email does not require verification: %v", info)
	}
	if _, leaked := info["children"]; leaked {
		t.Fatal("the unverified landing page listed children")
	}
}

// TestShareCreateRefusesAMalformedRecipient pins the server rule the dialog's shape check mirrors.
//
// The two checks are deliberately not identical — the server uses net/mail and is the authority, the
// client only refuses the obvious typo before a round trip — but a client that let a broken address
// through must produce a clear 400 rather than a share that silently invites nobody.
func TestShareCreateRefusesAMalformedRecipient(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "doc.txt", "content")

	resp := h.do(http.MethodPost, "/api/shares/",
		jsonBody(t, map[string]any{
			"path":       "doc.txt",
			"kind":       "email",
			"recipients": []string{"not-an-address"},
		}),
		map[string]string{"Content-Type": "application/json"})
	body := readBody(resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a malformed recipient answered %d (%s); want 400 so the dialog can say which "+
			"address is wrong", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invalid_recipient") {
		t.Fatalf("the refusal did not name the cause: %s", body)
	}
}
