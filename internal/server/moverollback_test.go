package server_test

// A failed overwrite must leave both sides of it intact.
//
// The 2026-10-23 review reproduced the opposite for the batch MOVE: with limits.max_trash_bytes=1,
// moving a source over an existing destination answered 507 trash_full, and the source then answered
// 404 with its bytes deleted. The rollback treated the staged node like a COPY's duplicate, but a
// MOVE's stage is the caller's own source node relocated to a temporary name — so "clean it up"
// destroyed the file.
//
// These tests drive the real HTTP surface on both storage layouts. They assert the STATE of both
// files, not just the status code: a rollback that reports an error and loses a file passes every
// status-only check.

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

// childrenOf lists a directory and returns its children keyed by name.
//
// The caller passes the path form the LISTING returns, on purpose: that is what the file manager
// sends back, and it is the form this suite exists to exercise.
func childrenOf(t *testing.T, h *harness, dir string) map[string]map[string]any {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/?path="+url.QueryEscape(dir), nil, nil)
	body := decode(t, resp)
	raw, _ := body["children"].([]any)

	out := make(map[string]map[string]any, len(raw))
	for _, item := range raw {
		child, _ := item.(map[string]any)
		name, _ := child["name"].(string)
		out[name] = child
	}
	return out
}

// trashItems lists the trash.
func trashItems(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	body := decode(t, h.do(http.MethodGet, "/api/files/trash", nil, nil))
	raw, _ := body["items"].([]any)

	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		out = append(out, entry)
	}
	return out
}

// pathField reads a node's `path` from a listing entry, failing if it is absent.
func pathField(t *testing.T, entry map[string]any, what string) string {
	t.Helper()
	p, _ := entry["path"].(string)
	if p == "" {
		t.Fatalf("the listing returned no path for %s: %v", what, entry)
	}
	return p
}

// artifactsOnDisk returns every path under the tree layout's root that carries a reserved name.
//
// The store's own staging directory is skipped: it is named with the same prefix and is expected to
// exist. Everything else with a reserved name is an intermediate an operation failed to finish.
func artifactsOnDisk(t *testing.T, h *harness) []string {
	t.Helper()
	if h.treeRoot == "" {
		return nil
	}

	var found []string
	err := filepath.WalkDir(h.treeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(h.treeRoot, p)
		if rerr != nil {
			return rerr
		}
		// The store's own temp area, and the owner directories the store creates.
		if rel == ".newcloud-tmp" || strings.HasPrefix(rel, ".newcloud-tmp"+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		if strings.Contains(d.Name(), ".newcloud-") {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree store: %v", err)
	}
	return found
}

// TestAMoveOverwriteThatCannotTrashKeepsBothFiles is the P0 regression, end to end.
//
// The trigger is the reviewer's exact one: a legal but tiny trash cap, two non-empty files, and
// overwrite=true. The assertion set is what a user cares about — both files still exist, with their
// original bytes, under their original names, and nothing was quietly moved to the trash.
func TestAMoveOverwriteThatCannotTrashKeepsBothFiles(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, func(c *config.Config) { c.Limits.MaxTrashBytes = 1 })
			source := h.uploadFile(t, "incoming.txt", "SOURCE important user data")
			destination := h.uploadFile(t, "existing.txt", "DEST existing user data")

			status, out := postBatch(t, h, map[string]any{
				"op":        "move",
				"overwrite": true,
				"items":     []map[string]string{{"from": "/incoming.txt", "to": "/existing.txt"}},
			})
			if status != http.StatusInsufficientStorage {
				t.Fatalf("want 507 (the trash is full), got %d (%v)", status, out)
			}

			// Both files, under their own names, with their own bytes.
			if code, body := downloadText(t, h, "/existing.txt"); code != http.StatusOK ||
				body != "DEST existing user data" {
				t.Fatalf("the destination after a refused overwrite: %d %q; the destination must be "+
					"exactly as it was", code, body)
			}
			if code, body := downloadText(t, h, "/incoming.txt"); code != http.StatusOK ||
				body != "SOURCE important user data" {
				t.Fatalf("the SOURCE after a refused overwrite: %d %q. This is the data loss the "+
					"review reproduced: the rollback removed the source's bytes because it treated "+
					"the source node as a disposable copy.", code, body)
			}

			// The source is the SAME node, still live: a rollback that re-created it would lose its
			// id, its version history and every share that names it.
			src := h.resolveNode(t, "incoming.txt")
			if src.ID != source.ID {
				t.Fatalf("the source is now node %s, was %s: identity must survive a rollback",
					src.ID, source.ID)
			}
			if src.TrashedAt != nil {
				t.Fatal("the source node is in the trash after a refused overwrite")
			}
			if src.Size != source.Size {
				t.Fatalf("the source's size changed: %d -> %d", source.Size, src.Size)
			}
			if dst := h.resolveNode(t, "existing.txt"); dst.ID != destination.ID || dst.TrashedAt != nil {
				t.Fatalf("the destination node changed: %s (trashed=%v)", dst.ID, dst.TrashedAt != nil)
			}

			// No intermediate names left behind: the reserved sibling names are an implementation
			// detail, and one surviving in the listing is a swap that did not finish.
			for name := range childrenOf(t, h, "/") {
				if strings.Contains(name, ".newcloud-") {
					t.Fatalf("a %q intermediate is still visible after a failed overwrite", name)
				}
			}
			if left := artifactsOnDisk(t, h); len(left) != 0 {
				t.Fatalf("a failed overwrite left %v in the content store; those bytes hold quota "+
					"while nothing names them", left)
			}

			// Nothing was trashed: the operation never got that far, so a failure that pushed the
			// destination into the bin would be a second, silent loss.
			if items := trashItems(t, h); len(items) != 0 {
				t.Fatalf("a refused overwrite put %d item(s) in the trash: %v", len(items), items)
			}
		})
	}
}

// TestAMoveOverwriteWhoseStageFailsKeepsBothFiles covers the OTHER failure point.
//
// The trash cap is one way the switch can fail; an audit store that refuses the write is another,
// and it fails EARLIER — while the source is being relocated. The reviewer asked for named failure
// points rather than one trigger, because "the rollback is safe" is a claim about every path.
func TestAMoveOverwriteWhoseStageFailsKeepsBothFiles(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			source := h.uploadFile(t, "incoming.txt", "SOURCE important user data")
			h.uploadFile(t, "existing.txt", "DEST existing user data")

			// Fail-closed audit (T-16): a write that cannot be recorded is refused. That refusal
			// happens inside the relocation, i.e. before the swap has installed anything.
			h.meta.SetAuditFault(errors.New("audit storage is down"))
			defer h.meta.SetAuditFault(nil)

			status, out := postBatch(t, h, map[string]any{
				"op":        "move",
				"overwrite": true,
				"items":     []map[string]string{{"from": "/incoming.txt", "to": "/existing.txt"}},
			})
			if status != http.StatusServiceUnavailable {
				t.Fatalf("an unauditable overwrite must answer 503, got %d (%v)", status, out)
			}

			h.meta.SetAuditFault(nil)

			if code, body := downloadText(t, h, "/incoming.txt"); code != http.StatusOK ||
				body != "SOURCE important user data" {
				t.Fatalf("the source after a stage that could not be audited: %d %q", code, body)
			}
			if code, body := downloadText(t, h, "/existing.txt"); code != http.StatusOK ||
				body != "DEST existing user data" {
				t.Fatalf("the destination after a stage that could not be audited: %d %q", code, body)
			}
			if src := h.resolveNode(t, "incoming.txt"); src.ID != source.ID || src.TrashedAt != nil {
				t.Fatalf("the source node changed: %s (trashed=%v)", src.ID, src.TrashedAt != nil)
			}
		})
	}
}

// TestAMoveOverwriteOfADirectoryThatCannotTrashKeepsBothTrees is the same P0 with a directory on
// both sides.
//
// It is a separate case because the rollback's physical work differs: on the tree layout a directory
// move is a rename of a whole subtree, and "put the original back" has to succeed for a non-empty
// directory. A file-only regression test would pass while that path deleted a folder's contents.
func TestAMoveOverwriteOfADirectoryThatCannotTrashKeepsBothTrees(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, func(c *config.Config) { c.Limits.MaxTrashBytes = 1 })
			seedDir(t, h, "incoming", struct{ name, body string }{"moved.txt", "moved bytes"})
			seedDir(t, h, "target", struct{ name, body string }{"old.txt", "old bytes"})
			incoming := h.resolveNode(t, "incoming")

			status, out := postBatch(t, h, map[string]any{
				"op":        "move",
				"overwrite": true,
				"items":     []map[string]string{{"from": "/incoming", "to": "/target"}},
			})
			if status != http.StatusInsufficientStorage {
				t.Fatalf("want 507, got %d (%v)", status, out)
			}

			for _, want := range []struct{ path, body string }{
				{"/target/old.txt", "old bytes"},
				{"/incoming/moved.txt", "moved bytes"},
			} {
				if code, body := downloadText(t, h, want.path); code != http.StatusOK || body != want.body {
					t.Fatalf("%s after a refused overwrite: %d %q, want %q", want.path, code, body, want.body)
				}
			}
			if got := h.resolveNode(t, "incoming"); got.ID != incoming.ID || got.TrashedAt != nil {
				t.Fatalf("the source directory changed: %s (trashed=%v)", got.ID, got.TrashedAt != nil)
			}
			for name := range childrenOf(t, h, "/") {
				if strings.Contains(name, ".newcloud-") {
					t.Fatalf("a %q intermediate survived a failed overwrite", name)
				}
			}
			if left := artifactsOnDisk(t, h); len(left) != 0 {
				t.Fatalf("a failed overwrite left %v in the content store; nobody names those bytes, "+
					"so they would occupy quota until manually removed", left)
			}
			if items := trashItems(t, h); len(items) != 0 {
				t.Fatalf("a refused overwrite put %d item(s) in the trash: %v", len(items), items)
			}
		})
	}
}

// TestACopyOverwriteThatCannotTrashCleansItsStage is the disposable half, over HTTP.
//
// A COPY's stage really is a duplicate: after a failed overwrite it must not survive in the
// listing, or the user finds a folder they never created holding a full copy of the source.
func TestACopyOverwriteThatCannotTrashCleansItsStage(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, func(c *config.Config) { c.Limits.MaxTrashBytes = 1 })
			seedDir(t, h, "src", struct{ name, body string }{"new.txt", "new bytes"})
			seedDir(t, h, "dst", struct{ name, body string }{"old.txt", "old bytes"})

			status, out := postBatch(t, h, map[string]any{
				"op":        "copy",
				"overwrite": true,
				"items":     []map[string]string{{"from": "/src", "to": "/dst"}},
			})
			if status != http.StatusInsufficientStorage {
				t.Fatalf("want 507, got %d (%v)", status, out)
			}

			if code, body := downloadText(t, h, "/dst/old.txt"); code != http.StatusOK ||
				body != "old bytes" {
				t.Fatalf("the destination after a refused copy overwrite: %d %q", code, body)
			}
			if code, body := downloadText(t, h, "/src/new.txt"); code != http.StatusOK ||
				body != "new bytes" {
				t.Fatalf("the source after a refused copy overwrite: %d %q", code, body)
			}
			for name := range childrenOf(t, h, "/") {
				if strings.Contains(name, ".newcloud-staged-") {
					t.Fatalf("the discarded copy's stage is still visible as %q; a failed overwrite "+
						"must not leave a folder the user never created", name)
				}
			}
		})
	}
}

// TestTheDisplacedDestinationCanBeRestoredAndRead is the trash half of the same contract.
//
// The review's symptom was "the restore reported success and the bytes were not there" — a restored
// node that cannot be downloaded is worse than a refused restore, because it looks recovered. This
// walks the whole path a user would: overwrite, find the displaced copy in the trash, restore it,
// download it, compare the bytes.
func TestTheDisplacedDestinationCanBeRestoredAndRead(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			seedDir(t, h, "box",
				struct{ name, body string }{"incoming.txt", "SOURCE important user data"},
				struct{ name, body string }{"existing.txt", "DEST existing user data"},
			)

			status, out := postBatch(t, h, map[string]any{
				"op":        "move",
				"overwrite": true,
				"items":     []map[string]string{{"from": "/box/incoming.txt", "to": "/box/existing.txt"}},
			})
			if status != http.StatusOK {
				t.Fatalf("overwrite: want 200, got %d (%v)", status, out)
			}

			var displaced map[string]any
			for _, item := range trashItems(t, h) {
				if name, _ := item["name"].(string); strings.Contains(name, ".newcloud-replaced-") {
					displaced = item
					break
				}
			}
			if displaced == nil {
				t.Fatal("the displaced destination is not in the trash; an overwrite that cannot be " +
					"undone is a deletion")
			}
			id, _ := displaced["id"].(string)
			if id == "" {
				t.Fatalf("the trash entry has no id: %v", displaced)
			}

			resp := h.do(http.MethodPost, "/api/files/restore",
				jsonBody(t, map[string]string{"id": id}),
				map[string]string{"Content-Type": "application/json"})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("restore: %d %s", resp.StatusCode, readBody(resp))
			}
			resp.Body.Close()

			// Ask the listing where the restored node actually is rather than assuming the name:
			// the review's failure was a node whose metadata looked restored and whose bytes were
			// gone, so the check has to go through the same path a browser would.
			var restoredPath string
			for name, child := range childrenOf(t, h, "/box") {
				if childID, _ := child["id"].(string); childID == id {
					restoredPath = pathField(t, child, name)
					break
				}
			}
			if restoredPath == "" {
				t.Fatal("the restored node is not listed in its directory")
			}
			if code, body := downloadText(t, h, restoredPath); code != http.StatusOK ||
				body != "DEST existing user data" {
				t.Fatalf("the restored node downloaded as %d %q; a restore that hands back unreadable "+
					"bytes is worse than a refusal, because it looks recovered", code, body)
			}

			// The fourth review's exact workflow finishes by RENAMING the restored node to a name of
			// the user's choosing, once the original name is free. That has to go through the API: a
			// bare metadata rename cannot move bytes on a path-backed layout for ANY node, so the
			// supported path is the rename endpoint, which moves them. If this fails on the tree
			// layout, "the old target is recoverable" is true only until somebody renames it.
			ren := h.do(http.MethodPost, "/api/files/rename",
				jsonBody(t, map[string]string{"path": restoredPath, "name": "restored-old.txt"}),
				map[string]string{"Content-Type": "application/json"})
			var renamed struct {
				Path string `json:"path"`
			}
			decodeJSONBody(t, ren, &renamed)
			if ren.StatusCode != http.StatusOK {
				t.Fatalf("renaming the restored node: %d", ren.StatusCode)
			}
			if code, body := downloadText(t, h, renamed.Path); code != http.StatusOK ||
				body != "DEST existing user data" {
				t.Fatalf("the restored node after a rename: %d %q; the rename must carry the bytes "+
					"with it, or the recovered file becomes unreadable at the name it now has",
					code, body)
			}
		})
	}
}
