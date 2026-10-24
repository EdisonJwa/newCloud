package server_test

// Nextcloud client compatibility (ADR-0015), exercised through the real router.
//
// These tests go through server.Handler(), so they prove the WIRING as well as the
// behaviour: a compatibility endpoint that exists in a package but is not routed is the
// "implemented and not wired" defect this project keeps finding. The login-flow test runs
// the whole handshake — anonymous start, browser approval with a real session cookie,
// poll, and then a WebDAV request with the credential it returned — because each step is
// only useful if the next one accepts its output.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// issueAppPassword gives a test a credential without going through the UI.
func (h *harness) issueAppPassword(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	secret, _, err := auth.NewAppPasswords(h.meta).Issue(context.Background(), userID, "test", "rw", 0)
	if err != nil {
		t.Fatalf("issue app password: %v", err)
	}
	return secret
}

// doBasic sends a request authenticated with an App Password.
func (h *harness) doBasic(t *testing.T, method, path, user, pass string,
	body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, pass)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// doAnonymous sends a request with no credential at all.
func (h *harness) doAnonymous(t *testing.T, method, path string, body io.Reader,
	headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// doNoRedirect sends an anonymous request without following redirects. The flow page's
// "not signed in" answer IS a redirect, and the default client would follow it to the IdP
// (which a test harness deliberately does not have).
func (h *harness) doNoRedirect(t *testing.T, method, path string) *http.Response {
	t.Helper()
	client := *h.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequest(method, h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNextcloudStatusIsAnonymousAndCarriesTheProtocolVersion(t *testing.T) {
	h := newHarness(t)

	resp := h.doAnonymous(t, http.MethodGet, "/status.php", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status.php: want 200, got %d", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["installed"] != true {
		t.Fatalf("installed = %v, want true", body["installed"])
	}
	if body["maintenance"] != false || body["needsDbUpgrade"] != false {
		t.Fatalf("a serving instance must not report maintenance or a pending schema upgrade: %v", body)
	}
	if _, ok := body["versionstring"].(string); !ok {
		t.Fatalf("versionstring missing or not a string: %v", body)
	}
	if body["productname"] != "newCloud" {
		t.Fatalf("productname = %v", body["productname"])
	}
}

func TestNextcloudOCSRequiresTheCSRFHeaderAndACredential(t *testing.T) {
	h := newHarness(t)
	secret := h.issueAppPassword(t, h.user.ID)

	// No OCS-APIRequest header: this is exactly what a cross-site form submission looks
	// like, and Nextcloud refuses it with 412.
	resp := h.doBasic(t, http.MethodGet, "/ocs/v1.php/cloud/capabilities?format=json",
		"alice", secret, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("OCS without OCS-APIRequest: want 412, got %d", resp.StatusCode)
	}

	// Header but no credential.
	resp = h.doAnonymous(t, http.MethodGet, "/ocs/v1.php/cloud/capabilities?format=json", nil,
		map[string]string{"OCS-APIRequest": "true"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OCS without auth: want 401, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("a 401 must challenge, or the client shows a generic failure instead of asking " +
			"for the app password")
	}

	// A bad app password is 401 too, not 500.
	resp = h.doBasic(t, http.MethodGet, "/ocs/v1.php/cloud/capabilities?format=json",
		"alice", "wc_wrong", nil, map[string]string{"OCS-APIRequest": "true"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("OCS with a bad credential: want 401, got %d", resp.StatusCode)
	}
}

func TestNextcloudCapabilitiesSayOnlyWhatIsImplemented(t *testing.T) {
	h := newHarness(t)
	secret := h.issueAppPassword(t, h.user.ID)
	headers := map[string]string{"OCS-APIRequest": "true"}

	resp := h.doBasic(t, http.MethodGet, "/ocs/v1.php/cloud/capabilities?format=json",
		"alice", secret, nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("format=json answered %q", ct)
	}
	body := decode(t, resp)
	ocs, _ := body["ocs"].(map[string]any)
	meta, _ := ocs["meta"].(map[string]any)
	if meta["status"] != "ok" {
		t.Fatalf("meta.status = %v", meta["status"])
	}
	// OCS v1 spells success 100; a client that reads this and gets 200 concludes the
	// call failed.
	if code, _ := meta["statuscode"].(float64); int(code) != 100 {
		t.Fatalf("v1 statuscode = %v, want 100", meta["statuscode"])
	}
	data, _ := ocs["data"].(map[string]any)
	version, _ := data["version"].(map[string]any)
	if version["string"] == "" || version["string"] == nil {
		t.Fatalf("version.string missing: %v", version)
	}
	caps, _ := data["capabilities"].(map[string]any)
	core, _ := caps["core"].(map[string]any)
	if core["webdav-root"] != "remote.php/dav" {
		t.Fatalf("core.webdav-root = %v", core["webdav-root"])
	}
	files, _ := caps["files"].(map[string]any)
	if files["bigfilechunking"] != false {
		t.Fatalf("bigfilechunking = %v; advertising chunking this server cannot do makes every "+
			"large upload fail halfway", files["bigfilechunking"])
	}
	if _, advertised := files["chunked_upload"]; advertised {
		t.Fatalf("chunked_upload is advertised but no upload staging endpoint exists: %v", files)
	}

	// v2 answers with 200, and the XML default is still XML.
	resp = h.doBasic(t, http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json",
		"alice", secret, nil, headers)
	body = decode(t, resp)
	ocs, _ = body["ocs"].(map[string]any)
	meta, _ = ocs["meta"].(map[string]any)
	if code, _ := meta["statuscode"].(float64); int(code) != 200 {
		t.Fatalf("v2 statuscode = %v, want 200", meta["statuscode"])
	}

	resp = h.doBasic(t, http.MethodGet, "/ocs/v1.php/cloud/capabilities",
		"alice", secret, nil, headers)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Fatalf("default format answered %q, want XML", ct)
	}
	if !strings.Contains(string(raw), "<status>ok</status>") {
		t.Fatalf("XML envelope:\n%s", raw)
	}
}

func TestNextcloudOCSUserNamesTheNamespaceAndReportsQuota(t *testing.T) {
	h := newHarness(t)
	secret := h.issueAppPassword(t, h.user.ID)
	headers := map[string]string{"OCS-APIRequest": "true"}

	// A real upload, so "used" is measured rather than assumed.
	payload := strings.Repeat("x", 100)
	if resp := h.doBasic(t, http.MethodPut, "/remote.php/dav/files/alice/data.bin",
		"alice", secret, strings.NewReader(payload), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d", resp.StatusCode)
	}
	h.meta.SetQuota(h.user.ID, 1000)

	resp := h.doBasic(t, http.MethodGet, "/ocs/v1.php/cloud/user?format=json",
		"alice", secret, nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cloud/user: %d", resp.StatusCode)
	}
	body := decode(t, resp)
	ocs, _ := body["ocs"].(map[string]any)
	data, _ := ocs["data"].(map[string]any)

	// The id becomes the DAV path segment, so it must be the username, not the UUID.
	if data["id"] != "alice" {
		t.Fatalf("id = %v, want the namespace segment \"alice\"", data["id"])
	}
	if data["email"] != "alice@example.test" {
		t.Fatalf("email = %v", data["email"])
	}
	quota, _ := data["quota"].(map[string]any)
	if total, _ := quota["total"].(float64); int64(total) != 1000 {
		t.Fatalf("quota.total = %v, want 1000", quota["total"])
	}
	if used, _ := quota["used"].(float64); int64(used) != 100 {
		t.Fatalf("quota.used = %v, want 100", quota["used"])
	}
	if free, _ := quota["free"].(float64); int64(free) != 900 {
		t.Fatalf("quota.free = %v, want 900", quota["free"])
	}

	// Without a quota the protocol's value is -3 (unlimited), not 0: a client reading 0
	// free refuses to upload anything.
	h.meta.SetQuota(h.user.ID, 0)
	resp = h.doBasic(t, http.MethodGet, "/ocs/v1.php/cloud/user?format=json",
		"alice", secret, nil, headers)
	body = decode(t, resp)
	ocs, _ = body["ocs"].(map[string]any)
	data, _ = ocs["data"].(map[string]any)
	quota, _ = data["quota"].(map[string]any)
	if total, _ := quota["total"].(float64); int64(total) != -3 {
		t.Fatalf("an unlimited account reported total = %v, want -3", quota["total"])
	}
	if free, _ := quota["free"].(float64); int64(free) != -3 {
		t.Fatalf("an unlimited account reported free = %v, want -3", quota["free"])
	}
}

// TestNextcloudLoginFlowV2IssuesAWorkingCredential is the whole handshake.
func TestNextcloudLoginFlowV2IssuesAWorkingCredential(t *testing.T) {
	h := newHarness(t)

	const ua = "Nextcloud Desktop Client/3.13 (Windows)"
	resp := h.doAnonymous(t, http.MethodPost, "/index.php/login/v2", nil,
		map[string]string{"User-Agent": ua})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start login flow: %d", resp.StatusCode)
	}
	var start struct {
		Poll struct {
			Token    string `json:"token"`
			Endpoint string `json:"endpoint"`
		} `json:"poll"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		resp.Body.Close()
		t.Fatalf("decode start reply: %v", err)
	}
	resp.Body.Close()
	if start.Poll.Token == "" || start.Login == "" {
		t.Fatalf("start reply is missing fields: %+v", start)
	}
	if !strings.HasPrefix(start.Login, h.cfg.Server.PublicURL+"/login/v2/flow/") {
		t.Fatalf("login URL %q is not on the configured public URL", start.Login)
	}
	if !strings.HasPrefix(start.Poll.Endpoint, h.cfg.Server.PublicURL+"/login/v2/poll") {
		t.Fatalf("poll endpoint %q is not on the configured public URL", start.Poll.Endpoint)
	}

	loginPath := mustPath(t, start.Login)
	pollPath := mustPath(t, start.Poll.Endpoint)

	// Until the browser approves, the poll is 404 — the client's "keep waiting".
	pollForm := url.Values{"token": {start.Poll.Token}}.Encode()
	formHeaders := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	resp = h.doAnonymous(t, http.MethodPost, pollPath, strings.NewReader(pollForm), formHeaders)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("poll before approval: want 404, got %d", resp.StatusCode)
	}

	// Opening the login URL without a session sends the user to the IdP login, with the
	// flow as the post-login target.
	resp = h.doNoRedirect(t, http.MethodGet, loginPath)
	noSessionBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login page without a session: want 302, got %d: %s", resp.StatusCode, noSessionBody)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/api/auth/login?next=") {
		t.Fatalf("redirect = %q, want the OIDC login with the flow as next", loc)
	}

	// Signed in, the page names the client that is asking.
	resp = h.do(http.MethodGet, loginPath, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent page: %d", resp.StatusCode)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "Nextcloud Desktop Client/3.13") {
		t.Fatalf("consent page does not name the requesting client:\n%s", page)
	}
	if !strings.Contains(string(page), "alice") {
		t.Fatalf("consent page does not name the approving account:\n%s", page)
	}

	// Approving is a POST from the same origin.
	resp = h.do(http.MethodPost, loginPath, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d", resp.StatusCode)
	}
	done, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(done), "Access granted") {
		t.Fatalf("approval page does not confirm:\n%s", done)
	}

	// The poll now returns the credential, exactly once.
	resp = h.doAnonymous(t, http.MethodPost, pollPath, strings.NewReader(pollForm), formHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll after approval: %d", resp.StatusCode)
	}
	var creds struct {
		Server      string `json:"server"`
		LoginName   string `json:"loginName"`
		AppPassword string `json:"appPassword"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		resp.Body.Close()
		t.Fatalf("decode credential reply: %v", err)
	}
	resp.Body.Close()
	if creds.Server != h.cfg.Server.PublicURL {
		t.Fatalf("server = %q, want %q", creds.Server, h.cfg.Server.PublicURL)
	}
	if creds.LoginName != "alice" {
		t.Fatalf("loginName = %q", creds.LoginName)
	}
	if !strings.HasPrefix(creds.AppPassword, auth.AppPasswordPrefix) {
		t.Fatalf("appPassword %q is not an app password", creds.AppPassword)
	}

	// The credential the flow returned must actually work over WebDAV — this is the point
	// of the whole exchange.
	resp = h.doBasic(t, "PROPFIND", "/remote.php/dav/files/alice/", "alice",
		creds.AppPassword, nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("the issued credential did not work over WebDAV: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// It is labelled with the client, so the account can revoke the right one.
	list, err := auth.NewAppPasswords(h.meta).List(context.Background(), h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	labelled := false
	for _, p := range list {
		if p.Label == "Nextcloud Desktop Client/3.13" && p.Scope == "rw" {
			labelled = true
		}
	}
	if !labelled {
		t.Fatalf("no credential labelled with the client's name: %+v", list)
	}

	// A second poll must not mint a second credential.
	resp = h.doAnonymous(t, http.MethodPost, pollPath, strings.NewReader(pollForm), formHeaders)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second poll: want 404, got %d", resp.StatusCode)
	}
}

func TestNextcloudLoginFlowCannotBeTakenOverByAnotherAccount(t *testing.T) {
	h := newHarness(t)

	resp := h.doAnonymous(t, http.MethodPost, "/index.php/login/v2", nil,
		map[string]string{"User-Agent": "Nextcloud Android/3.30"})
	var start struct {
		Poll  struct{ Token, Endpoint string } `json:"poll"`
		Login string                           `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	loginPath := mustPath(t, start.Login)
	if r := h.do(http.MethodPost, loginPath, nil, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("alice approving: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// A second account opening the same browser link must not be able to claim the flow:
	// the client would receive a credential for the wrong user.
	bob := h.meta.AddUser(store.User{Username: "bob", Email: "bob@example.test", Role: "user"})
	if _, err := h.meta.EnsureRoot(context.Background(), bob.ID); err != nil {
		t.Fatal(err)
	}
	bobSession, err := h.sessions.Mint(context.Background(), bob.ID, bob.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+loginPath, nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: bobSession})
	resp, err = h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("bob approving alice's flow: want 409, got %d", resp.StatusCode)
	}

	// The credential still belongs to alice.
	pollPath := mustPath(t, start.Poll.Endpoint)
	resp = h.doAnonymous(t, http.MethodPost, pollPath,
		strings.NewReader(url.Values{"token": {start.Poll.Token}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll after the takeover attempt: %d", resp.StatusCode)
	}
	var creds struct {
		LoginName string `json:"loginName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if creds.LoginName != "alice" {
		t.Fatalf("the credential was issued for %q, want alice", creds.LoginName)
	}
}

// TestNextcloudAccountRemovalRevokesTheCredential covers the client's DELETE /ocs v2
// core/apppassword call when a user removes the account. Nextcloud documents that a client
// proceeds even when this fails — but leaving a working credential behind after "remove
// account" is a security bug from the user's point of view.
func TestNextcloudAccountRemovalRevokesTheCredential(t *testing.T) {
	h := newHarness(t)
	secret := h.issueAppPassword(t, h.user.ID)
	headers := map[string]string{"OCS-APIRequest": "true"}

	resp := h.doBasic(t, http.MethodDelete, "/ocs/v2.php/core/apppassword", "alice", secret,
		nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE core/apppassword: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The credential the request authenticated with is the one that must be gone.
	resp = h.doBasic(t, "PROPFIND", "/remote.php/dav/files/alice/", "alice", secret, nil,
		map[string]string{"Depth": "0"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the revoked credential still works: %d", resp.StatusCode)
	}
}

func TestNextcloudLoginFlowStartIsPostOnly(t *testing.T) {
	h := newHarness(t)

	resp := h.doAnonymous(t, http.MethodGet, "/index.php/login/v2", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET start: want 405, got %d", resp.StatusCode)
	}
}

// TestNextcloudLoginFlowPollRejectsAnUnknownToken also pins that a missing token is a
// 404, not a 500: a client that lost its token should restart, and a 500 would look like
// a server fault worth retrying forever.
func TestNextcloudLoginFlowPollRejectsAnUnknownToken(t *testing.T) {
	h := newHarness(t)

	resp := h.doAnonymous(t, http.MethodPost, "/login/v2/poll",
		strings.NewReader("token="+url.QueryEscape("not-a-real-token")),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown poll token: want 404, got %d", resp.StatusCode)
	}
}

// mustPath returns the path+query of an absolute URL, so a test can drive it against the
// httptest server even though the URL was built from the configured public origin.
func mustPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// A flow that is never completed must not leave a credential behind. The store sweeps
// expired rows, and nothing is minted until the poll, which is the property that makes
// an abandoned flow harmless.
func TestAnAbandonedLoginFlowCreatesNoCredential(t *testing.T) {
	h := newHarness(t)

	resp := h.doAnonymous(t, http.MethodPost, "/index.php/login/v2", nil,
		map[string]string{"User-Agent": "abandoning client"})
	var start struct {
		Poll struct {
			Token    string `json:"token"`
			Endpoint string `json:"endpoint"`
		} `json:"poll"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	// Abandon it: no approval, no poll.
	_ = start.Login
	list, err := auth.NewAppPasswords(h.meta).List(context.Background(), h.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("starting a login flow minted %d credential(s); the credential is created by "+
			"the poll, after the user approves", len(list))
	}
}
