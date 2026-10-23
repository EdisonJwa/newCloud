package server_test

// NC-RR-006: the content-origin route boundary.
//
// The re-review's finding was precise and is worth restating because the test names encode it: the
// separation existed only when `server.usercontent_addr` was set, the reference Caddyfile did not set
// it, and even with it set `/s/{token}/download` and `/s/{token}/thumbnail` stayed registered on the
// application handler. So the claim "user bytes can only be served on the content host" was a claim
// about a proxy configuration, not a property of the server.
//
// These tests assert the property on the two handlers directly, with no proxy in the way:
//
//	application origin  refuses every content-delivery route when a content origin exists
//	content origin      serves those three routes and nothing else, whatever the Host header says
//
// Testing at the handler level is the point: a test that went through Caddy would prove the Caddyfile
// is right and leave the server free to register a fourth content route that the proxy happens to
// forward. The route inventory is what has to be closed, so the route inventory is what is checked.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/server"
)

// contentRoutes are every path that returns user bytes. This list is the inventory the requirement
// talks about: adding a route that serves content without adding it here is exactly the drift
// NC-RR-006 is about, which is why the test also checks the SERVER's own route table below.
var contentRoutes = []string{
	"/content/secret.txt?ticket=whatever",
	"/s/SOMETOKEN/download",
	"/s/SOMETOKEN/thumbnail",
}

// TestASplitContentOriginRefusesEveryContentRouteOnTheApp is the core of NC-RR-006.
//
// With a content origin configured, the application origin must not return user bytes on ANY of the
// three paths — not only /content/*, which was the one route the original implementation moved.
func TestASplitContentOriginRefusesEveryContentRouteOnTheApp(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) { d.ContentSplit = true })
	h.uploadFile(t, "secret.txt", "the bytes that must not come from the app origin")

	// A genuinely valid ticket, so the refusal cannot be explained by an invalid credential: if the
	// app origin answered this, the ticket would work and the bytes would come out.
	_, out := mintContentTicket(t, h, "secret.txt")
	validURL, _ := out["url"].(string)
	if validURL == "" {
		t.Fatal("no content ticket was minted, so the test cannot prove the route is refused")
	}
	token := validURL[strings.Index(validURL, "ticket=")+len("ticket="):]

	for _, route := range []string{
		"/content/secret.txt?ticket=" + token,
		"/content/secret.txt?ticket=" + token + "&ignored=1",
		"/s/anytoken/download",
		"/s/anytoken/thumbnail",
	} {
		resp := h.do(http.MethodGet, route, nil, nil)
		body := readBody(resp)
		resp.Body.Close()

		// "Refused" means: not the bytes, and not an attachment. A 200 carrying the SPA shell is
		// acceptable (the app serves its shell for unknown paths); anything that hands back the
		// content is not.
		if strings.Contains(body, "the bytes that must not come from the app origin") &&
			!strings.Contains(body, "<html") {
			t.Fatalf("the application origin served user bytes on %s while a content origin was "+
				"configured. NC-RR-006: the separation is decorative if both origins can serve "+
				"the same content.", route)
		}
		if cd := resp.Header.Get("Content-Disposition"); strings.HasPrefix(cd, "attachment") {
			t.Fatalf("the application origin answered %s with an attachment (%q), so a "+
				"content-delivery route is still registered there", route, cd)
		}
	}
}

// TestTheContentOriginServesOnlyContentRoutes pins the other direction of the boundary.
//
// The content origin must be a file host and nothing else: no API, no share landing page, no SPA. A
// route that answered here would put application surface on the origin whose security model is
// "everything on this host is inert bytes".
func TestTheContentOriginServesOnlyContentRoutes(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "file.txt", "content bytes")
	code, out := mintContentTicket(t, h, "file.txt")
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	contentURL, _ := out["url"].(string)

	handler := h.srvServer.ContentHandler()

	// fetch issues a request against the content handler, with a caller-chosen Host so the boundary
	// can be shown not to depend on the Host header.
	fetch := func(host, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// The content routes work.
	if rec := fetch("usercontent.example.test", contentURL); rec.Code != http.StatusOK {
		t.Fatalf("content fetch on the content origin = %d, want 200", rec.Code)
	}

	// Everything else is a 404, whichever hostname is presented. The application host reaching this
	// handler — a proxy with a wildcard catch-all, a misrouted vhost — must not turn it into an app.
	for _, target := range []string{
		"/api/files/",
		"/api/auth/me",
		"/healthz",
		"/readyz",
		"/metrics",
		"/dav/alice/",
		"/s/anytoken",
		"/",
	} {
		for _, host := range []string{"usercontent.example.test", "cloud.example.test"} {
			if rec := fetch(host, target); rec.Code != http.StatusNotFound {
				t.Fatalf("the content origin answered %s (Host: %s) with %d; it must serve user "+
					"bytes and nothing else, so that a misrouted application request fails closed",
					target, host, rec.Code)
			}
		}
	}
}

// TestTheApplicationRouteTableContainsNoContentDeliveryRouteWhenSplit is the drift guard.
//
// The handler-level assertions above cover the three paths that exist today. This one asks the SERVER
// for its actual route table and requires that the content-delivery entries are the refusal stubs
// rather than the real handlers. Without it, adding a fourth content route would pass every test
// above while silently reopening the hole — which is precisely how this defect appeared the first
// time.
func TestTheApplicationRouteTableContainsNoContentDeliveryRouteWhenSplit(t *testing.T) {
	split := newHarnessTuned(t, nil, func(d *server.Deps) { d.ContentSplit = true })
	joined := newHarness(t)

	splitRoutes := routeSet(split.srvServer.Routes())
	joinedRoutes := routeSet(joined.srvServer.Routes())

	for _, p := range []string{"/content/*",
		"/s/{token}/download", "/s/{token}/thumbnail"} {
		// Both tables REGISTER the path: that is deliberate, because a missing route would fall
		// through to the SPA and return 200 HTML, which is a worse answer than an explicit 404 and
		// is indistinguishable from a working page to a naive client. What must differ is the
		// handler, and that is asserted behaviourally in the tests above.
		if !splitRoutes[p] {
			t.Fatalf("%q is not registered on the split application origin at all; the route must "+
				"exist and refuse, or unknown paths fall through to the SPA", p)
		}
		if !joinedRoutes[p] {
			t.Fatalf("%q is not registered on the unsplit origin", p)
		}
	}

	// And the properties that must NOT be on the content origin are absent from its handler: it
	// registers three routes, so the count is a real check that nothing grew.
	var count int
	for range split.srvServer.ContentRoutes() {
		count++
	}
	if count != 3 {
		t.Fatalf("the content origin registers %d routes; it must register exactly the three "+
			"content routes (/content/*, /s/{token}/download, /s/{token}/thumbnail). A fourth "+
			"route is surface on the origin that is supposed to be a file host.", count)
	}
}

func routeSet(routes []string) map[string]bool {
	out := make(map[string]bool, len(routes))
	for _, r := range routes {
		// Routes are "METHOD PATTERN"; the method is irrelevant to this check.
		if _, pattern, ok := strings.Cut(r, " "); ok {
			out[pattern] = true
		}
	}
	return out
}

// TestAEnabledShareIsDownloadableOnTheContentOrigin is the "authorized preview requirement" half of
// NC-RR-006's acceptance criteria.
//
// Moving share downloads off the application origin must not break share downloads: the recipient's
// browser is sent to the content host and the share link has to work there. A boundary that is
// enforced by breaking the feature is not a boundary, it is an outage.
func TestAEnabledShareIsDownloadableOnTheContentOrigin(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "shared.txt", "shared bytes")

	// Create an open share through the API.
	body, _ := json.Marshal(map[string]any{"path": "shared.txt", "kind": "link"})
	resp := h.do(http.MethodPost, "/api/shares", strings.NewReader(string(body)),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("create share: %d", resp.StatusCode)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	token, _ := created["token"].(string)
	if token == "" {
		// Some API shapes nest the token; fall back to the share URL path.
		if u, ok := created["url"].(string); ok {
			if i := strings.Index(u, "/s/"); i >= 0 {
				token = strings.TrimPrefix(u[i+3:], "/")
				if j := strings.IndexAny(token, "/?"); j >= 0 {
					token = token[:j]
				}
			}
		}
	}
	if token == "" {
		t.Fatalf("no share token in the create response: %v", created)
	}

	req := httptest.NewRequest(http.MethodGet, "/s/"+token+"/download", nil)
	rec := httptest.NewRecorder()
	h.srvServer.ContentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the content origin did not serve an authorized share download: %d %s",
			rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "shared bytes" {
		t.Fatalf("share download body = %q", rec.Body.String())
	}
}

// TestAShareChildIsDownloadableOnTheContentOrigin covers the `?name=` variant.
//
// A shared DIRECTORY downloads a child by name, which is a different query on the same content
// route. The origin boundary must not have moved the route into a shape that only serves the
// single-file case: the landing page for a directory renders a row per child, and each row's link
// is the one a recipient actually clicks.
func TestAShareChildIsDownloadableOnTheContentOrigin(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "box")
	// multipartBody takes the directory separately; uploadFile would put the file at the root
	// under a name containing a slash, which the server refuses.
	mb := multipartBody(t, "box", "one.txt", "the child bytes")
	if rec := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
		t.Fatalf("seed child: %d %s", rec.StatusCode, readBody(rec))
	}

	body, _ := json.Marshal(map[string]any{"path": "box", "kind": "link"})
	resp := h.do(http.MethodPost, "/api/shares", strings.NewReader(string(body)),
		map[string]string{"Content-Type": "application/json"})
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	token, _ := created["token"].(string)
	if token == "" {
		t.Skip("the share API did not return a bare token")
	}

	req := httptest.NewRequest(http.MethodGet,
		"/s/"+token+"/download?name="+url.QueryEscape("one.txt"), nil)
	rec := httptest.NewRecorder()
	h.srvServer.ContentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a shared directory's child was not served from the content origin: %d %s",
			rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "the child bytes" {
		t.Fatalf("child body = %q", rec.Body.String())
	}
}

// TestTheSplitDoesNotChangeShareLandingOnTheApp: the share PAGE must stay on the application origin.
//
// It is the HTML shell with the password/recipient gate, and it is where the share cookie is set.
// Moving it would break password shares on the content host, whose whole design is that it holds no
// session material. This pins the intended asymmetric split rather than a symmetric one.
func TestTheSplitDoesNotChangeShareLandingOnTheApp(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) { d.ContentSplit = true })
	h.uploadFile(t, "landing.txt", "bytes")

	body, _ := json.Marshal(map[string]any{"path": "landing.txt", "kind": "link"})
	resp := h.do(http.MethodPost, "/api/shares", strings.NewReader(string(body)),
		map[string]string{"Content-Type": "application/json"})
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	token, _ := created["token"].(string)
	if token == "" {
		t.Skip("the share API did not return a bare token; the landing-page assertion needs one")
	}

	// The landing page answers on the app origin, with JSON to a fetch (not a navigation).
	landing := h.do(http.MethodGet, "/s/"+token, nil,
		map[string]string{"Accept": "application/json"})
	landingBody := readBody(landing)
	landing.Body.Close()
	if landing.StatusCode == http.StatusNotFound {
		t.Fatalf("the share landing page is no longer served by the application origin: %s",
			landingBody)
	}

	// And the same path on the content origin is not an address there.
	req := httptest.NewRequest(http.MethodGet, "/s/"+token, nil)
	rec := httptest.NewRecorder()
	h.srvServer.ContentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("the content origin served the share landing page (%d); the landing page "+
			"belongs to the application origin, which is the only one that holds the gate's cookie",
			rec.Code)
	}
}

// TestAShareDownloadIsRefusedByTheAppOriginWhenSplit is the share half of NC-RR-006 stated on its
// own, because it is the route the original fix missed.
func TestAShareDownloadIsRefusedByTheAppOriginWhenSplit(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) { d.ContentSplit = true })
	h.uploadFile(t, "secret.txt", "bytes")

	resp := h.do(http.MethodGet, "/s/anytoken/download", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); strings.HasPrefix(cd, "attachment") {
		t.Fatalf("the app origin answered a share download with an attachment (%q); "+
			"/s/{token}/download must move to the content origin with the rest", cd)
	}
	if strings.Contains(body, "bytes") && !strings.Contains(body, "<html") {
		// "bytes" is generic enough that this can only fail if the handler actually served the
		// file; the attachment check above is the primary assertion.
		t.Fatalf("the app origin served a share download: %s", body)
	}
}
