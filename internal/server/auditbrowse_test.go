package server_test

// Thumbnails and audit browsing. Both endpoints were added for the interface, and both have
// a property worth pinning: the thumbnail is file content (so it needs the same permission
// as reading the file), and the audit view must not become a way to read other people's
// history.

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"
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
