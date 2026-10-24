package server_test

// Restoring a node that a MOVE overwrite displaced (2026-10-24 review, M4).
//
// A MOVE onto an existing name displaces the destination into the trash under a reserved
// sibling name — "<rand>.newcloud-replaced-<name>" — and tx.Restore then brings it back with
// that name. A user who restores their file gets a name they never chose, which is bad on its
// own, and which also blocks the fix for the reserved names being addressable: refusing to
// address them (the M4 leak) is only safe once restore stops producing them.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestRestoreRecoversTheNameAfterAMoveOverwrite(t *testing.T) {
	h := newHarness(t)

	upload := func(name, content string) {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		_ = mw.WriteField("path", "")
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = io.WriteString(fw, content)
		_ = mw.Close()
		resp := h.do(http.MethodPost, "/api/files/upload", body,
			map[string]string{"Content-Type": mw.FormDataContentType()})
		resp.Body.Close()
	}
	upload("budget.txt", "the precious original")
	upload("replacement.txt", "the replacement")

	root, err := h.meta.Root(context.Background(), h.user.ID)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	var displacedID string
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		kids, kerr := tx.Children(ctx, root.ID, 100)
		if kerr != nil {
			return kerr
		}
		for _, k := range kids {
			if k.Name == "budget.txt" {
				displacedID = k.ID.String()
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if displacedID == "" {
		t.Fatal("the uploaded file is not in the root")
	}

	// The batch MOVE is the one that overwrites; /api/files/move refuses an occupied destination.
	status, out := postBatch(t, h, map[string]any{
		"op":        "move",
		"overwrite": true,
		"items": []map[string]string{{"from": "replacement.txt", "to": "budget.txt"}},
	})
	if status != http.StatusOK && status != http.StatusMultiStatus {
		t.Fatalf("move over an existing name: %d (%v)", status, out)
	}

	// The displaced original must be in the trash under an artefact name — if it is not, this
	// test is not exercising the case it claims to.
	var trashedName string
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.NodeIncludingTrashed(ctx, uuid.MustParse(displacedID))
		if nerr != nil {
			return nerr
		}
		if n.TrashedAt == nil {
			t.Fatal("the overwritten file was not displaced into the trash")
		}
		trashedName = n.Name
		return nil
	}); err != nil {
		t.Fatalf("read trashed node: %v", err)
	}
	if !strings.Contains(trashedName, ".newcloud-") {
		t.Fatalf("the displaced node is named %q; the test expected a reserved artefact name", trashedName)
	}

	resp := h.do(http.MethodPost, "/api/files/restore",
		strings.NewReader(`{"id":"`+displacedID+`"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: %d (%s)", resp.StatusCode, readBody(resp))
	}
	var body struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode restore response: %v", err)
	}
	resp.Body.Close()

	if strings.Contains(body.Name, ".newcloud-") {
		t.Fatalf("the restored file is named %q: the user is handed an internal artefact name "+
			"they cannot recognise, and once reserved names stop being addressable they cannot "+
			"reach it at all", body.Name)
	}
	if !strings.HasPrefix(body.Name, "budget") {
		t.Fatalf("the restored name %q does not recover the original name", body.Name)
	}

	// Readable at the name the response claims: a metadata-only rename would break this on the
	// tree layout, where the content path follows the name.
	got := h.do(http.MethodGet, "/api/files/download?path="+url.QueryEscape(body.Path), nil, nil)
	text := readBody(got)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("downloading the restored file at %q = %d (%s)", body.Path, got.StatusCode, text)
	}
	if text != "the precious original" {
		t.Fatalf("the restored file holds %q, want the overwritten original's bytes", text)
	}
}
