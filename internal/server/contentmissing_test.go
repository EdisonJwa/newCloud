package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// A file whose metadata exists but whose bytes are gone is storage damage, and says so.
//
// It used to reach the client as a bare 500 with an empty body and leave nothing in the log. The
// three states are now distinguishable: 404 the file does not exist, 403 the caller may not read
// it, 500 the file exists, the caller may read it, and the bytes are missing.
func TestAMissingContentFileIsReportedAsStorageDamage(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) {
		d.Content = missingOpenContent{ContentStore: d.Content}
	})
	h.uploadFile(t, "ghost.txt", "bytes")

	resp := h.do(http.MethodGet, "/api/files/download?path=ghost.txt", nil, nil)
	code := resp.StatusCode
	body := readBody(resp)
	if code != http.StatusInternalServerError {
		t.Fatalf("a file whose content is missing answered %d, want 500: it exists and the caller "+
			"may read it, so 404 would send everyone looking at the wrong thing (%s)", code, body)
	}
	if !strings.Contains(body, "content_missing") {
		t.Fatalf("the error did not name the condition, so an operator cannot tell it from any "+
			"other fault: %s", body)
	}
}

// missingOpenContent makes Open fail the way a path-backed store does when the bytes are gone.
type missingOpenContent struct{ layout.ContentStore }

func (m missingOpenContent) Open(ctx context.Context, n layout.Node) (io.ReadSeekCloser, error) {
	return nil, layout.ErrNotFound
}

// The same condition on a route that opens content on its own.
//
// The mapping in writeStoreError was the reason this mattered for every route: layout.ErrNotFound
// meant "bytes missing", and it was answered 404 — the failure table's forbidden answer — so the
// archive, thumbnail, share and content-ticket routes all said "no such file" for a file the
// caller could see in a listing. This drives the thumbnail route, which is one of them.
func TestAMissingContentFileIsNotReportedAsAbsentOnOtherRoutes(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) {
		d.Content = missingOpenContent{ContentStore: d.Content}
	})
	h.uploadFile(t, "ghost.png", "not really a png")

	resp := h.do(http.MethodGet, "/api/files/thumbnail?path=ghost.png", nil, nil)
	code := resp.StatusCode
	body := readBody(resp)
	if code == http.StatusNotFound {
		t.Fatalf("the thumbnail route answered 404 for a file whose metadata exists: that is the " +
			"answer the failure table forbids, and it sends the user looking for a file that is " +
			"right there in their listing")
	}
	if code != http.StatusInternalServerError {
		t.Fatalf("thumbnail of a file with missing content answered %d, want 500 (%s)", code, body)
	}
	if !strings.Contains(body, "content_missing") {
		t.Fatalf("the error did not name the condition: %s", body)
	}
}

// The condition must be countable, or the alert on it is dead by configuration.
func TestContentMissingIsCounted(t *testing.T) {
	var reg *metrics.Registry
	h := newHarnessTuned(t, nil, func(d *server.Deps) {
		reg = metrics.New()
		d.Metrics = reg
		d.Content = missingOpenContent{ContentStore: d.Content}
	})
	h.uploadFile(t, "ghost2.txt", "bytes")

	first := h.do(http.MethodGet, "/api/files/download?path=ghost2.txt", nil, nil)
	first.Body.Close()
	if first.StatusCode != http.StatusInternalServerError {
		t.Fatalf("setup: expected the missing-content answer, got %d", first.StatusCode)
	}
	// The thumbnail route reports the same condition but does NOT reach this counter — measured, not
	// assumed: it answers 500 content_missing and counts nothing. The download path is the one the
	// alert depends on, and limitations.md records the gap rather than implying coverage.
	second := h.do(http.MethodGet, "/api/files/thumbnail?path=ghost2.txt", nil, nil)
	second.Body.Close()

	if got := reg.ContentMissing(); got != 1 {
		t.Fatalf("the download of a file with missing content counted %d, want 1: the alert on "+
			"this condition would be dead by configuration", got)
	}
}
