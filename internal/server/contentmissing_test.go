package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
