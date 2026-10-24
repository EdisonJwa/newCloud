package server_test

// The trash says when it truncated (2026-10-24 review, M8).
//
// The trash has no cursor, so a client cannot page further once it hits the cap — but it must be
// able to tell a full page from a complete one. Before this, a trash larger than the cap was
// returned as if it were the whole thing, and a client offering "restore everything" would
// silently restore a subset while reporting success.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

func uploadForTrashTest(t *testing.T, h *harness, name string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = io.WriteString(fw, "bytes")
	_ = mw.Close()
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d", name, resp.StatusCode)
	}
}

func trashListing(t *testing.T, h *harness) (items int, truncated bool) {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/trash", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trash listing: %d", resp.StatusCode)
	}
	var out struct {
		Items     []map[string]any `json:"items"`
		Truncated *bool            `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Truncated == nil {
		t.Fatal("the trash listing has no `truncated` field, so a client cannot tell a full page " +
			"from a complete one")
	}
	return len(out.Items), *out.Truncated
}

func TestTheTrashReportsWhenItHitTheCap(t *testing.T) {
	// A cap of one and two trashed files: the page is complete-looking but is not.
	h := harnessOn(t, "blob", func(c *config.Config) { c.Limits.MaxProfindChildren = 1 })
	uploadForTrashTest(t, h, "one.txt")
	uploadForTrashTest(t, h, "two.txt")
	for _, name := range []string{"one.txt", "two.txt"} {
		resp := h.do(http.MethodDelete, "/api/files/?path="+name, nil, nil)
		resp.Body.Close()
	}

	items, truncated := trashListing(t, h)
	if !truncated {
		t.Fatalf("the trash holds more entries than the cap of 1 but reported truncated=false "+
			"(items=%d)", items)
	}
	if items != 1 {
		t.Fatalf("the page returned %d items, want the cap of 1", items)
	}
}

func TestTheTrashDoesNotClaimTruncationWhenItFits(t *testing.T) {
	// The same cap, one file: a warning here would be as wrong as silence in the other direction,
	// because a client that trusts it would page forever.
	h := harnessOn(t, "blob", func(c *config.Config) { c.Limits.MaxProfindChildren = 1 })
	uploadForTrashTest(t, h, "only.txt")
	resp := h.do(http.MethodDelete, "/api/files/?path=only.txt", nil, nil)
	resp.Body.Close()

	items, truncated := trashListing(t, h)
	if truncated {
		t.Fatal("the trash fits in one page and reported truncated=true")
	}
	if items != 1 {
		t.Fatalf("the trash listing returned %d items, want 1", items)
	}
}
