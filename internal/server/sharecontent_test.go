package server_test

// PRG-002: share downloads across the origin split.
//
// The reproduction the audit performed, and why it matters: the share landing page is served by the
// application hostname, its download button was a relative link, and the split listeners refuse
// content routes on that hostname. The button therefore 404'd. Pointing the button at the content
// hostname would not have fixed it either, because a password or recipient grant is a HOST-ONLY
// cookie that the content hostname never receives.
//
// The fix under test is a short-lived capability the application origin mints after the gate passes,
// bound to one share, one file and one purpose. These tests run the whole chain — landing origin,
// gate, redirect, content origin, bytes — rather than hand-crafting a request to the content
// handler, because a hand-crafted request is exactly what would not have caught the defect.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/server"
)

// createShare creates a share through the API and returns its token and id.
func createShare(t *testing.T, h *harness, body string) (string, string) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/shares/", strings.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode the share response: %v", err)
	}
	token, _ := out["token"].(string)
	sh, _ := out["share"].(map[string]any)
	id, _ := sh["id"].(string)
	if token == "" {
		t.Fatalf("no share token in the response: %v", out)
	}
	return token, id
}

// splitHarness builds a harness whose server believes a content origin exists: the topology the
// reference deployment now ships.
func splitHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessTuned(t, nil, func(d *server.Deps) { d.ContentSplit = true })
}

// appFetch issues a request against the application origin WITHOUT following redirects.
//
// Not following is the point: the behaviour under test IS the redirect, and a client that followed
// it would try to reach usercontent.example.test from the test process and fail for an unrelated
// reason. Using the handler directly also keeps the app/content distinction explicit at every call
// site instead of hiding it behind one base URL.
func appFetch(t *testing.T, h *harness, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.srvServer.Handler().ServeHTTP(rec, req)
	return rec
}

// contentFetch issues a request against the content origin's own handler, as a browser would after
// following the redirect to usercontent.example.com. No cookies are ever added: the content origin
// must not need them.
func contentFetch(t *testing.T, h *harness, rawURL string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	req.URL.Scheme = ""
	req.URL.Host = ""
	rec := httptest.NewRecorder()
	h.srvServer.ContentHandler().ServeHTTP(rec, req)
	return rec
}

// TestAnOpenShareDownloadSurvivesTheOriginSplit is the audit's PRG-002 probe, run the way a browser
// runs it: take the URL the landing page emits, follow it, and require the bytes.
func TestAnOpenShareDownloadSurvivesTheOriginSplit(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "report.txt", "the shared bytes")

	token, _ := createShare(t, h, `{"path":"report.txt","kind":"link"}`)

	// The link the ShareLanding component renders. It is RELATIVE on purpose: the landing page and
	// its gate live on the application origin, so the first hop has to land there.
	appPath := "/s/" + token + "/download"

	rec := appFetch(t, h, appPath)
	if rec.Code != http.StatusFound {
		t.Fatalf("the application origin answered the share download button with %d, want 302 to "+
			"the content origin. A 404 is the PRG-002 defect: the landing page is on this origin "+
			"and the bytes are on the other, so the two have to be connected. Body: %s",
			rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if location == "" {
		t.Fatal("the redirect carries no Location")
	}
	if !strings.HasPrefix(location, strings.TrimRight(h.cfg.Server.UserContentURL, "/")+"/") {
		t.Fatalf("the redirect points at %q, which is not the configured content origin", location)
	}
	if !strings.Contains(location, "ticket=") {
		t.Fatalf("the redirect carries no capability: %q. The content origin cannot see this "+
			"origin's cookies, so a bare redirect would only move the 404.", location)
	}

	// Follow it, arriving with no cookies at all — which is what a browser does, because the grant
	// cookie is host-only.
	content := contentFetch(t, h, location)
	if content.Code != http.StatusOK {
		t.Fatalf("the content origin refused the capability it was just handed: %d %s",
			content.Code, content.Body.String())
	}
	if body := content.Body.String(); body != "the shared bytes" {
		t.Fatalf("content origin served %q, want the shared file's bytes", body)
	}
}

// TestAPasswordShareDownloadSurvivesTheOriginSplit is the case the audit specifically called out:
// the hyperlink fix alone was insufficient because the grant is host-only.
func TestAPasswordShareDownloadSurvivesTheOriginSplit(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "guarded.txt", "behind a password")

	token, _ := createShare(t, h, `{"path":"guarded.txt","kind":"link","password":"correct horse"}`)
	appPath := "/s/" + token + "/download"

	// Without the password the application origin must refuse, not redirect: a redirect would hand
	// an unauthenticated visitor a capability, which is the gate bypassed.
	if rec := appFetch(t, h, appPath); rec.Code == http.StatusFound {
		t.Fatal("an unauthenticated visitor was redirected to the content origin, so the gate was " +
			"skipped and a capability was handed out without proof")
	}

	// Prove the password the way a browser does: the verify endpoint sets the host-only cookie.
	vr := h.do(http.MethodPost, "/s/"+token+"/verify",
		strings.NewReader(`{"password":"correct horse"}`),
		map[string]string{"Content-Type": "application/json"})
	vr.Body.Close()
	if vr.StatusCode != http.StatusOK {
		t.Fatalf("verify: %d", vr.StatusCode)
	}
	cookies := vr.Cookies()
	if len(cookies) == 0 {
		t.Fatal("verify issued no grant cookie")
	}

	// The same URL, with the cookie the browser would hold for the application origin.
	rec := appFetch(t, h, appPath, cookies...)
	if rec.Code != http.StatusFound {
		t.Fatalf("a gated visitor was answered %d, want 302 to the content origin (body: %s)",
			rec.Code, rec.Body.String())
	}

	// And the capability alone, with no cookie at all, opens the download on the content origin.
	content := contentFetch(t, h, rec.Header().Get("Location"))
	if content.Code != http.StatusOK {
		t.Fatalf("the content origin refused the gated capability: %d %s",
			content.Code, content.Body.String())
	}
	if body := content.Body.String(); body != "behind a password" {
		t.Fatalf("content origin served %q", body)
	}
}

// TestAShareCapabilityIsBoundToItsShareAndFile pins the blast radius of a leaked URL.
//
// A capability travels in a query string, so it must be worthless anywhere except the exact share
// and file it was minted for. Otherwise "one leaked URL" becomes "the whole shared folder".
//
// Both shares are PASSWORD-protected, and that is essential to the test: an open share authorises
// with no credential at all, so presenting a capability for the wrong share would return 200 for a
// reason unrelated to the capability. With a gate, the only way to succeed is a capability that
// belongs to that share — so a 200 here means the binding failed.
func TestAShareCapabilityIsBoundToItsShareAndFile(t *testing.T) {
	h := splitHarness(t)
	for _, f := range []struct{ name, body string }{
		{"one.txt", "first child"},
		{"two.txt", "second child"},
	} {
		mb := multipartBody(t, "", f.name, f.body)
		if rec := h.do(http.MethodPost, "/api/files/upload", mb,
			map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
			t.Fatalf("seed %s: %d", f.name, rec.StatusCode)
		}
	}

	const pw = "correct horse battery"
	tokenA, _ := createShare(t, h, `{"path":"one.txt","kind":"link","password":"`+pw+`"}`)
	tokenB, _ := createShare(t, h, `{"path":"two.txt","kind":"link","password":"`+pw+`"}`)

	// Satisfy share A's gate and mint its capability.
	vr := h.do(http.MethodPost, "/s/"+tokenA+"/verify", strings.NewReader(`{"password":"`+pw+`"}`),
		map[string]string{"Content-Type": "application/json"})
	vr.Body.Close()
	if vr.StatusCode != http.StatusOK {
		t.Fatalf("verify A: %d", vr.StatusCode)
	}
	rec := appFetch(t, h, "/s/"+tokenA+"/download", vr.Cookies()...)
	if rec.Code != http.StatusFound {
		t.Fatalf("mint: %d (body: %s)", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	ticket := u.Query().Get("ticket")
	if ticket == "" {
		t.Fatal("no ticket in the redirect")
	}
	contentBase := strings.TrimRight(h.cfg.Server.UserContentURL, "/")

	// Same capability, DIFFERENT share. No cookie for B either, so the only way this could succeed
	// is the capability being accepted for the wrong subject.
	other := contentBase + "/s/" + tokenB + "/download?ticket=" + url.QueryEscape(ticket)
	if got := contentFetch(t, h, other); got.Code == http.StatusOK {
		t.Fatalf("a capability minted for share A opened share B (got %d); the share id is the "+
			"token subject, so this must be inert", got.Code)
	}

	// Same capability, same share, but a DIFFERENT file name: must not widen to the whole share.
	widened := contentBase + "/s/" + tokenA + "/download?ticket=" +
		url.QueryEscape(ticket) + "&name=two.txt"
	if got := contentFetch(t, h, widened); got.Code == http.StatusOK {
		t.Fatalf("a capability for one file fetched another (got %d); the file name is bound into "+
			"the claims precisely so a leaked URL is not a folder-wide grant", got.Code)
	}

	// The unmodified capability still works, so the refusals above are about binding and not about
	// the capability being unusable.
	if got := contentFetch(t, h, location); got.Code != http.StatusOK {
		t.Fatalf("the capability stopped working for its own share and file: %d", got.Code)
	}
}

// TestARevokedShareStopsAnOutstandingCapability is the revocation half of the acceptance criteria.
//
// A capability lives for minutes, so the question that matters is not "does it expire" but "does
// revoking the share stop a URL that is already in somebody's hands".
func TestARevokedShareStopsAnOutstandingCapability(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "revocable.txt", "revocable bytes")

	token, shareID := createShare(t, h, `{"path":"revocable.txt","kind":"link"}`)

	rec := appFetch(t, h, "/s/"+token+"/download")
	if rec.Code != http.StatusFound {
		t.Fatalf("mint: %d", rec.Code)
	}
	location := rec.Header().Get("Location")

	// Works while the share is live.
	if got := contentFetch(t, h, location); got.Code != http.StatusOK {
		t.Fatalf("the capability should work before revocation: %d", got.Code)
	}

	del := h.do(http.MethodDelete, "/api/shares/"+shareID, nil, nil)
	del.Body.Close()
	if del.StatusCode >= 400 {
		t.Fatalf("revoke: %d", del.StatusCode)
	}

	// The same URL must now fail: every request re-resolves the share, so revocation does not wait
	// for the capability to expire.
	if got := contentFetch(t, h, location); got.Code == http.StatusOK {
		t.Fatal("a capability still served bytes after its share was revoked; revocation that waits " +
			"for a token to expire is not revocation")
	}
}

// TestTheShareThumbnailAlsoSurvivesTheOriginSplit covers the preview route, which the audit's route
// inventory includes and which the first split version refused alongside the download.
func TestTheShareThumbnailAlsoSurvivesTheOriginSplit(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "picture.txt", "not an image")

	token, _ := createShare(t, h, `{"path":"picture.txt","kind":"link"}`)

	rec := appFetch(t, h, "/s/"+token+"/thumbnail")
	if rec.Code != http.StatusFound {
		t.Fatalf("the share thumbnail on the application origin = %d, want 302. Previews are "+
			"content and moved with the download, so this route needs the same hand-off.",
			rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "/thumbnail?ticket=") {
		t.Fatalf("the preview redirect does not target the content origin's thumbnail route: %q",
			location)
	}

	// The content origin accepts the capability. A non-image is refused by the decoder rather than
	// by authorization, so a 404 here would mean the capability itself was rejected.
	got := contentFetch(t, h, location)
	if got.Code == http.StatusNotFound {
		t.Fatalf("the content origin refused the preview capability: %d %s",
			got.Code, got.Body.String())
	}
}

// TestTheSplitStillRefusesContentPathsItHasNoCapabilityFor keeps the refusal that IS correct.
//
// /content/* is minted as an absolute URL for the content origin, so the application origin has no
// business serving it. The two share routes are the only ones that redirect.
func TestTheSplitStillRefusesContentPathsItHasNoCapabilityFor(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "secret.txt", "bytes")

	rec := appFetch(t, h, "/content/secret.txt?ticket=whatever")
	body := rec.Body.String()
	if strings.Contains(body, "bytes") && !strings.Contains(body, "<html") {
		t.Fatalf("the application origin served /content/*: %s", body)
	}
	if cd := rec.Header().Get("Content-Disposition"); strings.HasPrefix(cd, "attachment") {
		t.Fatalf("/content/* is still registered on the application origin (attachment %q)", cd)
	}
}
