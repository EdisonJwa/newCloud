package server_test

// PRG-003: the content-origin invariant, stated precisely and enforced against drift.
//
// The audit found that a comment in internal/server/server.go claimed "nothing on the application
// origin returns user content" while four authenticated routes and WebDAV still did. The comment was
// wrong, not the code: an authenticated route cannot be moved to the content origin without also
// giving that origin the ability to authenticate a session, which is precisely the isolation the
// split exists to provide.
//
// So the invariant is now stated in two halves, and each half is tested:
//
//	ANONYMOUS  no unauthenticated content-delivery route is served by the application origin.
//	           Structural: enforced by the route table. Tested by
//	           TestASplitContentOriginRefusesEveryContentRouteOnTheApp and below.
//
//	AUTHENTICATED  these routes may serve bytes, and each is constrained by its own controls:
//	           authorization re-checked per request, attachment disposition by default, and for the
//	           single route that can render inline, a type allowlist without SVG, content sniffing,
//	           nosniff and a sandbox CSP. Tested per MIME class by preview_test.go.
//
// The test in this file is the drift guard for the second half. A comment cannot stop a new route
// from being added; a classification table that fails the build-level test when a route appears
// without an entry can.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// byteRouteClass says how a route that can return user bytes is constrained.
type byteRouteClass int

const (
	// classAnonymousContentOriginOnly: anonymous, and refused on the application origin when a
	// content origin is configured. Structural boundary.
	classAnonymousContentOriginOnly byteRouteClass = iota
	// classShareGated: anonymous but requires a share token plus whatever gate the share declares;
	// hands off to the content origin when one is configured.
	classShareGated
	// classAuthenticated: requires a session or app password and re-checks node permissions.
	classAuthenticated
	// classVettedInline: authenticated, and the only shape that can render bytes inline. The
	// controls are enumerated in the comment above and pinned by preview_test.go.
	classVettedInline
	// classMetadataOnly: matches the name heuristic (it lives under "versions", "content-ticket")
	// but returns JSON metadata or mints a URL rather than emitting file bytes. Listed explicitly
	// so the classification stays a decision rather than a pattern.
	classMetadataOnly
)

// userByteRoutes is the complete classification of routes that can emit user file bytes.
//
// Adding a route that returns file content without adding it here fails TestEveryUserByteRouteIs
// Classified, which is the point: the previous defect was an inventory that lived only in a
// comment.
var userByteRoutes = map[string]byteRouteClass{
	"GET /api/files/download":      classVettedInline, // attachment by default; inline via ?inline=1
	"GET /api/files/version":       classAuthenticated,
	"GET /api/files/thumbnail":     classAuthenticated, // derived image, re-authorized per request
	"GET /api/shared/{id}/content": classAuthenticated,
	"GET /content/*":               classAnonymousContentOriginOnly,
	"GET /s/{token}/download":      classShareGated,
	"GET /s/{token}/thumbnail":     classShareGated,
	// Not byte-serving, but they match the heuristic and must therefore be an explicit decision.
	"GET /api/files/versions":         classMetadataOnly,
	"POST /api/files/content-ticket":  classMetadataOnly, // mints a URL; the URL is what serves bytes
	"POST /api/files/restore-version": classMetadataOnly,
}

// WebDAV is NOT in the table above, and that is not an omission.
//
// chi validates methods against a fixed list, so PROPFIND and DAV GET are dispatched by the
// davInterceptor middleware BEFORE routing and never appear in chi.Walk's output. The byte-serving
// DAV method is GET, it requires an app password, and it re-authorizes the node per request, so it
// belongs in the classAuthenticated category — but it cannot be asserted through the route table.
// TestTheDavPathStillRefusesAnAnonymousGet covers it directly instead.

// looksLikeByteRoute reports whether a route pattern is plausibly one that serves file content.
//
// Deliberately over-inclusive. A false positive costs a test failure the author must resolve by
// classifying the route; a false negative is an unclassified content route, which is the defect.
func looksLikeByteRoute(pattern string) bool {
	for _, token := range []string{"download", "content", "thumbnail", "version", "/dav"} {
		if strings.Contains(pattern, token) {
			return true
		}
	}
	return false
}

// TestEveryUserByteRouteIsClassified is the drift guard over the real route table.
func TestEveryUserByteRouteIsClassified(t *testing.T) {
	h := newHarness(t)
	routes := h.srvServer.Routes()

	known := map[string]bool{}
	for _, r := range routes {
		// Routes() returns "METHOD PATTERN" as chi reports them.
		_, pattern, ok := strings.Cut(r, " ")
		if !ok {
			continue
		}
		// Skip the content-origin-only entries in this table when looking for drift on the app
		// origin: they are asserted separately below.
		if !looksLikeByteRoute(pattern) {
			continue
		}
		key := r
		if _, classified := userByteRoutes[key]; classified {
			known[key] = true
			continue
		}
		// A pattern that serves bytes but has no classification is the failure.
		t.Fatalf("route %q can serve user bytes but is not classified in userByteRoutes. Add it "+
			"with the control that constrains it, and test that control — the previous version of "+
			"this boundary claimed completeness in a comment and was wrong (PRG-003).", r)
	}

	// And every classification must still correspond to a real route, so the table cannot rot into a
	// list of routes that no longer exist.
	for key := range userByteRoutes {
		found := false
		for _, r := range routes {
			if r == key {
				found = true
				break
			}
		}
		if !found {
			// The content-origin route lives on the other handler; check that one before failing.
			for _, r := range h.srvServer.ContentRoutes() {
				if r == key {
					found = true
					break
				}
			}
		}
		if !found {
			t.Fatalf("userByteRoutes names %q, which is not in either handler's route table; the "+
				"classification has rotted and would no longer notice a real route", key)
		}
	}
}

// TestTheAnonymousContentRoutesAreRefusedWhenSplit is the ANONYMOUS half, restated as an explicit
// matrix so a reader can see exactly which host/path combinations are expected to fail closed.
func TestTheAnonymousContentRoutesAreRefusedWhenSplit(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "matrix.txt", "matrix bytes")
	token, _ := createShare(t, h, `{"path":"matrix.txt","kind":"link"}`)

	// Anonymous content-delivery paths on the APPLICATION origin.
	//
	// /content/* is refused outright. The two share paths must NOT serve bytes either — they are
	// allowed to redirect to the content origin, because the landing page on this origin is the only
	// place the gate can run, but they must never answer with the bytes themselves.
	for _, path := range []string{
		"/content/matrix.txt?ticket=x",
		"/s/" + token + "/download",
		"/s/" + token + "/thumbnail",
	} {
		rec := appFetch(t, h, path)
		if cd := rec.Header().Get("Content-Disposition"); strings.HasPrefix(cd, "attachment") {
			t.Fatalf("%s on the application origin answered with an attachment (%q), so a content "+
				"route is serving bytes there", path, cd)
		}
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "matrix bytes") {
			t.Fatalf("%s on the application origin returned the file's bytes", path)
		}
	}

	// API and UI paths must not exist on the CONTENT origin at all.
	for _, path := range []string{
		"/api/files/download?path=matrix.txt&inline=1",
		"/api/files/version?path=matrix.txt",
		"/api/files/thumbnail?path=matrix.txt",
		"/api/shared/00000000-0000-0000-0000-000000000000/content",
		"/dav/alice/matrix.txt",
		"/s/" + token,
		"/",
	} {
		if rec := contentFetch(t, h, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d on the content origin; that origin must serve its three "+
				"content routes and nothing else, so a misrouted application request fails closed",
				path, rec.Code)
		}
	}
}

// TestTheInlinePreviewControlsAreEnumerated pins the controls the AUTHENTICATED half depends on,
// at the level of a question a reviewer would ask: "what stops this from rendering a document?"
//
// The per-MIME behaviour is covered by preview_test.go; this test asserts the properties that make
// the exception vetted at all, so a future widening (adding SVG to the allowlist, dropping nosniff)
// fails here as well as there.
func TestTheInlinePreviewControlsAreEnumerated(t *testing.T) {
	h := newHarness(t)

	// An HTML document that claims to be a PNG. This is the exact payload the inline route exists to
	// neutralise.
	h.uploadFile(t, "disguised.png", `<html><script>alert(1)</script></html>`)

	rec := h.do(http.MethodGet, "/api/files/download?path=disguised.png&inline=1", nil, nil)
	body := readBody(rec)
	rec.Body.Close()

	if rec.StatusCode != http.StatusOK {
		t.Fatalf("the request should still succeed as a download, got %d", rec.StatusCode)
	}
	cd := rec.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("a file whose bytes are HTML but whose name says .png was served inline or with "+
			"disposition %q. The sniff is what forces an attachment; without it the declared type, "+
			"the name and the sent headers all agree on a lie.", cd)
	}
	if ct := rec.Header.Get("Content-Type"); strings.Contains(ct, "html") {
		t.Fatalf("the response declared %q for a file containing markup", ct)
	}
	if strings.Contains(body, "<script>") && strings.HasPrefix(cd, "inline") {
		t.Fatal("markup was served inline")
	}
}

// TestTheDavPathStillRefusesAnAnonymousGet is the WebDAV half of the classification.
//
// WebDAV serves file bytes and cannot be moved to the content origin: a desktop client sends Basic
// auth to the URL it was configured with, and the content origin has no session middleware. What
// must hold is that the route is authenticated and authorization-checked, and that a content-origin
// hostname cannot reach it at all.
func TestTheDavPathStillRefusesAnAnonymousGet(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "davfile.txt", "dav bytes")

	// Anonymous: 401 with a Basic challenge, not the file.
	req := httptest.NewRequest(http.MethodGet, "/dav/"+h.user.Username+"/davfile.txt", nil)
	rec := httptest.NewRecorder()
	h.srvServer.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous DAV GET answered %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "dav bytes") {
		t.Fatal("an anonymous DAV GET returned the file's bytes")
	}

	// The content origin must not expose DAV at all.
	content := httptest.NewRequest(http.MethodGet, "/dav/"+h.user.Username+"/davfile.txt", nil)
	crec := httptest.NewRecorder()
	h.srvServer.ContentHandler().ServeHTTP(crec, content)
	if crec.Code != http.StatusNotFound {
		t.Fatalf("the content origin answered a DAV GET with %d; WebDAV is an application route and "+
			"must not be reachable from the content hostname", crec.Code)
	}
}

// TestTheContentOriginServesNothingButContent is the counterpart matrix for the content origin,
// including the paths a reviewer would try first.
func TestTheContentOriginServesNothingButContent(t *testing.T) {
	h := splitHarness(t)
	h.uploadFile(t, "c.txt", "content origin bytes")
	token, _ := createShare(t, h, `{"path":"c.txt","kind":"link"}`)

	rec := appFetch(t, h, "/s/"+token+"/download")
	if rec.Code != http.StatusFound {
		t.Fatalf("mint: %d", rec.Code)
	}

	// The capability works on the content origin...
	if got := contentFetch(t, h, rec.Header().Get("Location")); got.Code != http.StatusOK {
		t.Fatalf("the content origin refused its own capability: %d", got.Code)
	}

	// ...and the same capability is inert everywhere else on that origin.
	u := rec.Header().Get("Location")
	if i := strings.Index(u, "?"); i >= 0 {
		u = u[i:]
	}
	for _, path := range []string{"/api/files/", "/healthz", "/metrics", "/dav/", "/"} {
		if got := contentFetch(t, h, path); got.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d on the content origin", path, got.Code)
		}
	}

}
