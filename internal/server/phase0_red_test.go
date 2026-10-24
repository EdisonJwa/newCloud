//go:build phase0red

// Phase 0 server-level probes. Excluded from the normal suite; run with:
//
//	go test -tags phase0red ./internal/server -run TestPhase0 -count=1
//
// These tests fail against the current implementation until the owning phase
// changes the protocol. They are deliberately not skipped: the failure output is
// the evidence record.

package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// uploadPhase0 uploads one file into dir through the real HTTP surface.
func uploadPhase0(t *testing.T, h *harness, dir, name, body string) {
	t.Helper()
	mb := multipartBody(t, dir, name, body)
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s/%s: %d %s", dir, name, resp.StatusCode, readBody(resp))
	}
}

// phase0NodeID resolves one path to its node ID before it is deleted.
func phase0NodeID(t *testing.T, h *harness, rel string) uuid.UUID {
	t.Helper()
	nodes, err := h.meta.Resolve(context.Background(), h.user.ID, rel)
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	return nodes[len(nodes)-1].ID
}

// TestPhase0TreeRestoreAfterReuseReturnsOldBytes is the product-level P0: the
// restore is metadata-only, so after the name was reused and deleted again the
// old node is restored over the newer file's still-live path bytes.
func TestPhase0TreeRestoreAfterReuseReturnsOldBytes(t *testing.T) {
	h := harnessOn(t, "tree", nil)

	uploadPhase0(t, h, "", "reuse.txt", "OLD BYTES")
	oldID := phase0NodeID(t, h, "reuse.txt")

	if resp := h.do(http.MethodDelete, "/api/files/?path=reuse.txt", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete old: %d", resp.StatusCode)
	}
	uploadPhase0(t, h, "", "reuse.txt", "NEW BYTES")
	if resp := h.do(http.MethodDelete, "/api/files/?path=reuse.txt", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete new: %d", resp.StatusCode)
	}

	resp := h.do(http.MethodPost, "/api/files/restore",
		strings.NewReader(`{"id":"`+oldID.String()+`"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore old node = %d %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	code, body := downloadText(t, h, "reuse.txt")
	if code != http.StatusOK || body != "OLD BYTES" {
		t.Fatalf("restored file = %d %q, want 200 %q. The restore reported success but the "+
			"physical path still holds the newer file's bytes (P0 restore).", code, body, "OLD BYTES")
	}
}

// TestPhase0TreePurgeLeavesUnreferencedBytes: the metadata purge has no tree
// content collector, so "permanently deleted" files keep their bytes on disk.
func TestPhase0TreePurgeLeavesUnreferencedBytes(t *testing.T) {
	h := harnessOn(t, "tree", nil)

	uploadPhase0(t, h, "", "purge.txt", "KEEP ME?")
	if resp := h.do(http.MethodDelete, "/api/files/?path=purge.txt", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, perr := tx.PurgeTrash(ctx, time.Millisecond)
		return perr
	}); err != nil {
		t.Fatalf("purge trash: %v", err)
	}
	if _, err := h.meta.Resolve(context.Background(), h.user.ID, "purge.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("metadata still resolves purge.txt after purge: %v", err)
	}

	physical := filepath.Join(h.treeRoot, h.user.ID.String(), "purge.txt")
	if _, err := os.Stat(physical); err == nil {
		t.Fatalf("purge removed metadata but left physical bytes at %s; the file is "+
			"unreferenced and cannot be restored, yet still occupies disk (P0 purge).", physical)
	}
}

// failPutAt injects one deterministic Content.Put failure after earlier files in
// the same COPY have already committed.
type failPutAt struct {
	layout.ContentStore
	target string
}

func (c *failPutAt) Put(ctx context.Context, n layout.Node, r io.Reader, size int64) (layout.PutResult, error) {
	if n.Path == c.target {
		return layout.PutResult{}, fmt.Errorf("injected: phase0 copy failure at %s", c.target)
	}
	return c.ContentStore.Put(ctx, n, r, size)
}

// TestPhase0BatchCopyLeavesNoPartialDestination reproduces the DAV/batch parity
// gap: DAV rolls back a failed directory COPY, the batch engine returns a failed
// item while leaving the already-copied children live at the destination.
func TestPhase0BatchCopyLeavesNoPartialDestination(t *testing.T) {
	h := harnessOnTuned(t, "tree", nil, func(d *server.Deps) {
		d.Content = &failPutAt{ContentStore: d.Content, target: "dst/b.txt"}
	})

	h.mkdir(t, "src")
	uploadPhase0(t, h, "src", "a.txt", "A")
	uploadPhase0(t, h, "src", "b.txt", "B")

	status, out := postBatch(t, h, map[string]any{
		"op":    "copy",
		"items": []map[string]string{{"from": "src", "to": "dst"}},
	})
	if status == http.StatusOK {
		if out["failed"] == float64(0) {
			t.Fatalf("the injected copy failure was not reported: %v", out)
		}
	} else if status != http.StatusInternalServerError {
		t.Fatalf("batch copy: want a per-item result or 500, got %d (%v)", status, out)
	}

	// The correct batch contract is all-or-nothing for one directory item: a
	// failed item must not leave a live partial destination.
	if code, body := downloadText(t, h, "dst/a.txt"); code == http.StatusOK {
		t.Fatalf("a failed batch COPY left dst/a.txt live with %q; DAV rolls the partial "+
			"destination back, batch leaves it behind (parity defect).", body)
	}
}
