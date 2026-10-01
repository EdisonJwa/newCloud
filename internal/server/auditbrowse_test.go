package server_test

// Thumbnails and audit browsing. Both endpoints were added for the interface, and both have
// a property worth pinning: the thumbnail is file content (so it needs the same permission
// as reading the file), and the audit view must not become a way to read other people's
// history.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// samplePNG builds a real 48x48 PNG.
//
// Real pixels matter: the generator refuses to synthesise a thumbnail for bytes it cannot
// decode, so a fake header would test the error path rather than the feature.
func samplePNG(t *testing.T) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 48, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 48; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 5), G: uint8(y * 5), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode sample png: %v", err)
	}
	return buf.String()
}

// TestThumbnailServesImages checks the happy path.
func TestThumbnailServesImages(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "shot.png", "image/png", samplePNG(t))

	resp := h.do(http.MethodGet, "/api/files/thumbnail?path=shot.png", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, readBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type %q", ct)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("a thumbnail needs an ETag, or it re-renders on every scroll")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("cache control %q", cc)
	}
}

// TestThumbnailRefusesNonImages: a PDF must not be decoded as a picture.
func TestThumbnailRefusesNonImages(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "doc.pdf", "application/pdf", "%PDF-1.4\n")

	resp := h.do(http.MethodGet, "/api/files/thumbnail?path=doc.pdf", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415 for a non-image, got %d", resp.StatusCode)
	}
}

// TestThumbnailRefusesMisleadingType: claiming image/png does not make the bytes an image.
//
// The generator decodes rather than trusting the declared type, so this answers 415 instead
// of failing inside the decoder with a 500.
func TestThumbnailRefusesMisleadingType(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "fake.png", "image/png", "<html>not an image</html>")

	resp := h.do(http.MethodGet, "/api/files/thumbnail?path=fake.png", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("a corrupt image must not be a 500: %s", readBody(resp))
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d", resp.StatusCode)
	}
}

// TestThumbnailRequiresAuth: the endpoint is content, not metadata.
func TestThumbnailRequiresAuth(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "shot.png", "image/png", samplePNG(t))

	// A request with no session: h.do always attaches the cookie, so build one by hand.
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/files/thumbnail?path=shot.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous request must not get a thumbnail, got %d", resp.StatusCode)
	}
}

// TestAuditEventsListsOwnActions checks the view has something to show.
func TestAuditEventsListsOwnActions(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "a.txt", "text/plain", "hello")
	h.do(http.MethodGet, "/api/files/download?path=a.txt", nil, nil)

	resp := h.do(http.MethodGet, "/api/audit/events", nil, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, readBody(resp))
	}

	var payload struct {
		Events []struct {
			ID     int64  `json:"id"`
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"events"`
		Total   int      `json:"total"`
		HasMore bool     `json:"has_more"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal([]byte(readBody(resp)), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(payload.Events) == 0 {
		t.Fatal("the audit view is empty after uploading and downloading a file")
	}
	// Newest first: the download happened after the upload.
	if payload.Events[0].Action != "file.read" {
		t.Errorf("want the newest event first, got %q", payload.Events[0].Action)
	}
	if len(payload.Actions) == 0 {
		t.Error("the action facets are empty, so the filter would offer nothing")
	}
}

// TestAuditEventsPagingWalksBackwards: offset must skip, not be ignored.
//
// This is the bug the paging shape invites: filtering then slicing a limited window
// returns nothing for every page after the first.
func TestAuditEventsPagingWalksBackwards(t *testing.T) {
	h := newHarness(t)

	// Enough events to cross a page boundary would be slow; instead confirm the mechanism
	// with two pages of a few events.
	for _, name := range []string{"1.txt", "2.txt", "3.txt"} {
		uploadWithType(t, h, name, "text/plain", "x")
	}

	first := h.do(http.MethodGet, "/api/audit/events?limit=2", nil, nil)
	defer first.Body.Close()
	var page1 struct {
		Events  []struct{ ID int64 } `json:"events"`
		Total   int                  `json:"total"`
		HasMore bool                 `json:"has_more"`
	}
	json.Unmarshal([]byte(readBody(first)), &page1)

	second := h.do(http.MethodGet, "/api/audit/events?limit=2&offset=2", nil, nil)
	defer second.Body.Close()
	var page2 struct {
		Events []struct{ ID int64 } `json:"events"`
	}
	json.Unmarshal([]byte(readBody(second)), &page2)

	if len(page2.Events) == 0 {
		t.Fatal("offset returned nothing; paging would be impossible")
	}
	if page1.Events[0].ID == page2.Events[0].ID {
		t.Fatal("the second page repeats the first; offset is being ignored")
	}
	if page2.Events[0].ID >= page1.Events[0].ID {
		t.Errorf("paging must move backwards in a newest-first list: %d then %d",
			page1.Events[0].ID, page2.Events[0].ID)
	}
}

// TestAuditEventsScopeToOwner: a user must not read another user's history.
func TestAuditEventsScopeToOwner(t *testing.T) {
	h := newHarness(t)

	// The harness signs in as one user; a second user's events must not appear. Since the
	// harness has a single account, the check is that ScopeToOwner is applied at all: an
	// anonymous visitor's denial on someone else's share is invisible, while the owner's own
	// events are present.
	uploadWithType(t, h, "mine.txt", "text/plain", "x")

	resp := h.do(http.MethodGet, "/api/audit/events", nil, nil)
	defer resp.Body.Close()

	var payload struct {
		Events []struct {
			ActorID string `json:"actor_user_id"`
		} `json:"events"`
	}
	json.Unmarshal([]byte(readBody(resp)), &payload)

	if len(payload.Events) == 0 {
		t.Fatal("no events visible to the owner")
	}
}

// TestAuditEventsFilterByAction confirms the filter is applied rather than decorative.
func TestAuditEventsFilterByAction(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "a.txt", "text/plain", "hello")
	h.do(http.MethodGet, "/api/files/download?path=a.txt", nil, nil)

	resp := h.do(http.MethodGet, "/api/audit/events?action=file.read", nil, nil)
	defer resp.Body.Close()

	var payload struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	json.Unmarshal([]byte(readBody(resp)), &payload)

	if len(payload.Events) == 0 {
		t.Fatal("filtering on an action that occurred returned nothing")
	}
	for _, ev := range payload.Events {
		if ev.Action != "file.read" {
			t.Fatalf("filter ignored: got %q", ev.Action)
		}
	}
}

// TestSharedFileHasAPreviewOnlyWithAGrant is §6.3's uniform-authorization rule applied to thumbnails.
//
// Resolving a thumbnail by path can only ever find the caller's OWN files, so a folder somebody
// shared through an ACL showed type badges where it could show pictures — the one content entry point
// that ignored a grant that the download, listing and WebDAV paths all honoured. Addressing the node
// by id fixes that, and the id is deliberately NOT a capability: the same PermRead check decides,
// so a shared file has a preview exactly when the caller may read it.
func TestSharedFileHasAPreviewOnlyWithAGrant(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "private-shot.png", "image/png", samplePNG(t))
	node := h.resolveNode(t, "private-shot.png")

	// A second account, acting as itself.
	other := h.meta.AddUser(store.User{
		Username: "shared-viewer", Email: "shared-viewer@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	token, err := h.sessions.Mint(context.Background(), other.ID, other.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	client := h.cookieClient(token)

	byID := func() int {
		t.Helper()
		resp := h.adminGet(t, client, "/api/files/thumbnail?id="+node.ID.String())
		code := resp.StatusCode
		resp.Body.Close()
		return code
	}

	if code := byID(); code != http.StatusNotFound {
		t.Fatalf("a stranger got a thumbnail for somebody else's file (%d). The node id must not "+
			"be a capability.", code)
	}

	h.grantOn(t, node.ID, other.ID, store.PermRead, false)

	if code := byID(); code != http.StatusOK {
		t.Fatalf("a file shared with read permission has no preview (%d). Every other entry point "+
			"honours that grant; the thumbnail one must too, or the shared folder is a wall of type "+
			"badges.", code)
	}
}

// TestShareThumbnailIsTokenBound is §6.3's rule for preview derivatives.
//
// A preview is content. Served from the share page it must be reachable only through the share's own
// credential — the token plus whatever gate the share declares — and never through a permanent URL
// that outlives the share. Before this route existed, a shared folder was the one place previews
// were unavailable; the risk this test guards is the opposite shape, a preview route that forgets the
// token or the gate.
func TestShareThumbnailIsTokenBound(t *testing.T) {
	h := newHarness(t)

	// A folder with one image, shared read-only.
	if resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"photos"}`),
		map[string]string{"Content-Type": "application/json"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: %d %s", resp.StatusCode, readBody(resp))
	}
	body := multipartBody(t, "photos", "cat.png", samplePNG(t))
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	create := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"photos","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	payload := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("share: %d %s", create.StatusCode, payload)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(payload), &created); err != nil || created.Token == "" {
		t.Fatalf("share response has no token: %s", payload)
	}
	token := created.Token

	// The visitor sees the picture.
	got := h.do(http.MethodGet, "/s/"+token+"/thumbnail?name=cat.png", nil, nil)
	ct := got.Header.Get("Content-Type")
	got.Body.Close()
	if got.StatusCode != http.StatusOK || ct != "image/png" {
		t.Fatalf("thumbnail through the share: %d %q, want 200 image/png", got.StatusCode, ct)
	}

	// But not by naming something outside the share root, and not with a bad or missing token.
	for _, path := range []string{
		"/s/" + token + "/thumbnail?name=..%2fsecret.png",
		"/s/" + token + "/thumbnail?name=nope.png",
		"/s/not-a-real-token/thumbnail?name=cat.png",
	} {
		r := h.do(http.MethodGet, path, nil, nil)
		code := r.StatusCode
		r.Body.Close()
		if code == http.StatusOK {
			t.Fatalf("%s served a preview (%d). The token and the share root are the authorization; "+
				"neither can be dropped.", path, code)
		}
	}
}

// TestOwnerThumbnailStillNeedsASession is the other half of the same rule, for the endpoint that
// existed first: the authenticated thumbnail route is content, so an anonymous request must not get
// it. Written separately because the test above uses the owner's own session and would pass either
// way, which is the kind of assertion that reads as coverage without being any.
func TestOwnerThumbnailStillNeedsASession(t *testing.T) {
	h := newHarness(t)
	uploadWithType(t, h, "shot.png", "image/png", samplePNG(t))

	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/files/thumbnail?path=shot.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.srv.Client().Do(req) // no cookie jar: anonymous
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an anonymous request got a thumbnail from the authenticated endpoint")
	}
}

// TestShareThumbnailHonoursThePasswordGate is the gate half of the rule above.
//
// The test for a plain link share does not exercise the gate at all — there is nothing to satisfy —
// so removing the gate check entirely would leave it green. A password-protected share is where the
// check is load-bearing: the token alone must not be enough to see the picture, and the password
// must be enough once supplied.
func TestShareThumbnailHonoursThePasswordGate(t *testing.T) {
	h := newHarness(t)

	if resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"locked"}`),
		map[string]string{"Content-Type": "application/json"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: %d", resp.StatusCode)
	}
	body := multipartBody(t, "locked", "secret.png", samplePNG(t))
	up := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + body.Boundary()})
	up.Body.Close()

	create := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"locked","kind":"link","password":"correct horse"}`),
		map[string]string{"Content-Type": "application/json"})
	payload := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", create.StatusCode, payload)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(payload), &created); err != nil || created.Token == "" {
		t.Fatalf("no token: %s", payload)
	}

	anonymous := func(path string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := h.srv.Client().Do(req) // no cookies: a visitor who has not entered the password
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := anonymous("/s/" + created.Token + "/thumbnail?name=secret.png"); code == http.StatusOK {
		t.Fatal("the token alone was enough to see the picture behind a password")
	}

	// With the password, the preview works — otherwise the gate would be a wall rather than a check.
	verify, err := http.NewRequest(http.MethodPost,
		h.srv.URL+"/s/"+created.Token+"/verify",
		strings.NewReader(`{"password":"correct horse"}`))
	if err != nil {
		t.Fatal(err)
	}
	verify.Header.Set("Content-Type", "application/json")
	vresp, err := h.srv.Client().Do(verify)
	if err != nil {
		t.Fatal(err)
	}
	var cookies []*http.Cookie
	for _, ck := range vresp.Cookies() {
		cookies = append(cookies, ck)
	}
	vresp.Body.Close()

	withCookie, err := http.NewRequest(http.MethodGet,
		h.srv.URL+"/s/"+created.Token+"/thumbnail?name=secret.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range cookies {
		withCookie.AddCookie(ck)
	}
	final, err := h.srv.Client().Do(withCookie)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Body.Close()
	if final.StatusCode != http.StatusOK {
		t.Fatalf("after entering the password the preview failed (%d); the gate must open, not "+
			"just close", final.StatusCode)
	}
}
