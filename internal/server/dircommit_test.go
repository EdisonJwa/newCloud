package server_test

// NC-R6-001 on the JSON API: REST mkdir and the batch COPY used to commit the metadata node and
// its success audit row, then discard a failed content-store Mkdir. On the tree layout the directory
// did not exist on disk while the API said it did.
//
// The failure is injected into the real layout through the ContentStore interface, so the handlers
// are the production ones. Both layouts run: blob must not regress, and the tree layout is where the
// missing directory was observable.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

var errInjectedMkdir = errors.New("injected: the content store cannot create a directory")

type failingMkdirContent struct {
	layout.ContentStore
	fail  atomic.Bool
	match func(layout.Node) bool
}

func (f *failingMkdirContent) Mkdir(ctx context.Context, n layout.Node) error {
	if f.fail.Load() && (f.match == nil || f.match(n)) {
		return errInjectedMkdir
	}
	return f.ContentStore.Mkdir(ctx, n)
}

// mkdirFaultHarness builds a harness on the named layout with a content store whose Mkdir can be
// made to fail after the fixture has been seeded.
func mkdirFaultHarness(t *testing.T, layoutName string) (*harness, *failingMkdirContent) {
	t.Helper()
	var f *failingMkdirContent
	h := harnessOnTuned(t, layoutName, nil, func(d *server.Deps) {
		f = &failingMkdirContent{ContentStore: d.Content}
		d.Content = f
	})
	return h, f
}

// requireNoLiveNode fails when rel resolves to a live node.
func requireNoLiveNode(t *testing.T, h *harness, rel string) {
	t.Helper()
	if _, err := h.meta.Resolve(context.Background(), h.user.ID, rel); err == nil {
		t.Fatalf("%q is a live node although the content store refused its directory. Metadata and "+
			"the disk disagree, and the client was told a directory exists that does not.", rel)
	}
}

// requireNoSuccessAudit fails when an "ok" file.write row named name appears after mark.
func requireNoSuccessAudit(t *testing.T, h *harness, mark int, name string) {
	t.Helper()
	for _, row := range h.meta.AuditEntries()[mark:] {
		if row.Action != "file.write" || row.Result != "ok" {
			continue
		}
		if got, _ := row.Metadata["name"].(string); got == name {
			t.Fatalf("an audit row claims a successful write of %q that never happened", name)
		}
	}
}

// TestRestMkdirReportsAContentStoreFailure is the review's REST probe.
func TestRestMkdirReportsAContentStoreFailure(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h, f := mkdirFaultHarness(t, layoutName)
			f.match = func(n layout.Node) bool { return n.Path == "newdir" }
			f.fail.Store(true)
			mark := len(h.meta.AuditEntries())

			resp := h.do(http.MethodPost, "/api/files/mkdir",
				strings.NewReader(`{"path":"/newdir"}`),
				map[string]string{"Content-Type": "application/json"})
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("mkdir whose directory could not be created = %d (%s), want 500. A 201 "+
					"here is the defect the review reproduced.", resp.StatusCode, readBody(resp))
			}
			requireNoLiveNode(t, h, "newdir")
			requireNoSuccessAudit(t, h, mark, "newdir")

			if layoutName == "tree" {
				p := filepath.Join(h.treeRoot, h.user.ID.String(), "newdir")
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("the tree layout has %q on disk after a refused mkdir: %v", p, err)
				}
			}
		})
	}
}

// TestBatchCopyReportsAContentStoreFailure is the JSON batch probe. It copies an empty collection,
// the shape whose failure the review saw as complete:true/failed:0/ok:true.
func TestBatchCopyReportsAContentStoreFailure(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h, f := mkdirFaultHarness(t, layoutName)
			h.mkdir(t, "source")
			f.match = func(n layout.Node) bool { return n.Path == "copied" }
			f.fail.Store(true)
			mark := len(h.meta.AuditEntries())

			status, out := postBatch(t, h, map[string]any{
				"op":    "copy",
				"items": []map[string]string{{"from": "source", "to": "copied"}},
			})
			if status != http.StatusInternalServerError {
				t.Fatalf("batch COPY whose directory could not be created = %d (%v), want 500. "+
					"A 200 with complete:true is the review's finding.", status, out)
			}
			if code, _ := out["error"].(string); code != "failed" {
				t.Fatalf("batch failure code = %q, want %q", code, "failed")
			}
			requireNoLiveNode(t, h, "copied")
			requireNoSuccessAudit(t, h, mark, "copied")
		})
	}
}

// TestBatchCopyOverwriteKeepsTheDestinationWhenTheStageFails covers the overwrite path: the staged
// replacement's directory cannot be created, so the old destination must be exactly as it was.
func TestBatchCopyOverwriteKeepsTheDestinationWhenTheStageFails(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h, f := mkdirFaultHarness(t, layoutName)
			h.mkdir(t, "source")
			h.mkdir(t, "dst")
			mb := multipartBody(t, "dst", "keep.txt", "ORIGINAL")
			if rec := h.do(http.MethodPost, "/api/files/upload", mb,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); rec.StatusCode != http.StatusCreated {
				t.Fatalf("seed keep.txt: %d %s", rec.StatusCode, readBody(rec))
			}
			dst := h.resolveNode(t, "dst")

			f.match = func(n layout.Node) bool {
				return strings.Contains(n.Path, ".newcloud-staged-")
			}
			f.fail.Store(true)

			status, out := postBatch(t, h, map[string]any{
				"op":        "copy",
				"overwrite": true,
				"items":     []map[string]string{{"from": "source", "to": "dst"}},
			})
			if status != http.StatusInternalServerError {
				t.Fatalf("batch COPY overwrite whose stage could not be created = %d (%v), want 500",
					status, out)
			}
			if after := h.resolveNode(t, "dst"); after.ID != dst.ID {
				t.Fatalf("a failed overwrite replaced the destination node (%s -> %s)", dst.ID, after.ID)
			}
			if code, body := downloadText(t, h, "dst/keep.txt"); code != http.StatusOK || body != "ORIGINAL" {
				t.Fatalf("the destination after a failed overwrite: %d %q, want the original bytes",
					code, body)
			}
		})
	}
}

// TestRestMkdirMetadataFailureLeavesNoLiveNode is the reverse direction. The seventh review
// showed that deleting the physical directory after a failed metadata transaction is unsafe
// (a concurrent winner's node and a concurrent writer's file were both reproduced as losses),
// so the documented outcome is an unreferenced directory and no live node. The retry adopts it.
func TestRestMkdirMetadataFailureLeavesNoLiveNode(t *testing.T) {
	h := harnessOn(t, "tree", nil)

	h.meta.SetAuditFault(errors.New("injected: audit unavailable"))
	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"/newdir"}`),
		map[string]string{"Content-Type": "application/json"})
	h.meta.SetAuditFault(nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("mkdir whose audit row could not be written = %d (%s), want 503",
			resp.StatusCode, readBody(resp))
	}
	requireNoLiveNode(t, h, "newdir")

	// The orphan is kept on purpose; only a cleanup protocol that can prove ownership for the
	// whole window may remove it. Nothing here may delete a writer's bytes.
	p := filepath.Join(h.treeRoot, h.user.ID.String(), "newdir")
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("the unreferenced directory is missing after a failed metadata commit "+
			"(fi=%v, err=%v); the safe policy keeps it for a retry", fi, err)
	}
	keep := filepath.Join(p, "other-writer.txt")
	if err := os.WriteFile(keep, []byte("IMPORTANT NEW DATA"), 0o600); err != nil {
		t.Fatalf("simulate a concurrent writer under the orphan: %v", err)
	}

	// The retried mkdir adopts the directory; it must not delete what is already inside.
	h.mkdir(t, "newdir")
	got, err := os.ReadFile(keep)
	if err != nil {
		t.Fatalf("the retry deleted a file another writer placed under the orphan: %v", err)
	}
	if string(got) != "IMPORTANT NEW DATA" {
		t.Fatalf("the concurrent writer's file was modified: %q", got)
	}
}
