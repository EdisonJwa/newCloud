package server_test

// Deleting frees the path; restoring brings the bytes back (ADR-0013 phase 0 / 2026-10-24 review).
//
// Trashing is a metadata operation, so on the tree layout a deleted file used to keep occupying the
// path it had: the name stayed unavailable until the retention sweep, and the retained copy sat at
// a path derived from a name that no longer existed in the namespace. That is why no-replace
// publish could not be enabled on this layout.
//
// The delete now moves the content to a reserved sibling name before the row is trashed, and the
// restore route (which already recovers a usable name from those reserved names) moves it back. The
// two halves are one behaviour, so this test exercises both: a delete that frees the path is only
// correct if the file can still come back.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestDeleteFreesThePathAndRestoreBringsTheBytesBack(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "keep.txt")
	_, _ = io.WriteString(fw, "the only copy")
	_ = mw.Close()
	up := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	up.Body.Close()
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", up.StatusCode)
	}

	root, err := h.meta.Root(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	var id string
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		kids, kerr := tx.Children(ctx, root.ID, 100)
		if kerr != nil {
			return kerr
		}
		for _, k := range kids {
			if k.Name == "keep.txt" {
				id = k.ID.String()
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if id == "" {
		t.Fatal("the uploaded file is not in the root")
	}

	del := h.do(http.MethodDelete, "/api/files/?path=keep.txt", nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", del.StatusCode)
	}

	// The trash holds it. Whether the row keeps its name depends on the layout — on the blob layout
	// the path is a metadata mapping, so nothing has to move and the name is unaffected; the tree
	// layout is covered by TestDeleteReleasesTheTreeLivePath in internal/dav.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.NodeIncludingTrashed(ctx, uuid.MustParse(id))
		if nerr != nil {
			return nerr
		}
		if n.TrashedAt == nil {
			t.Error("the deleted file is not in the trash")
		}
		return nil
	}); err != nil {
		t.Fatalf("read trashed node: %v", err)
	}

	// And it comes back, with its own name and its bytes.
	resp := h.do(http.MethodPost, "/api/files/restore",
		bytes.NewReader([]byte(`{"id":"`+id+`"}`)),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: %d (%s)", resp.StatusCode, readBody(resp))
	}
	var out struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	if out.Name != "keep.txt" {
		t.Fatalf("the restored name is %q, want the name the user deleted", out.Name)
	}
	got := h.do(http.MethodGet, "/api/files/download?path="+out.Path, nil, nil)
	text := readBody(got)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("download the restored file: %d (%s)", got.StatusCode, text)
	}
	if text != "the only copy" {
		t.Fatalf("the restored file holds %q", text)
	}
}
