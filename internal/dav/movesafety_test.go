package dav

// What a DAV MOVE/COPY is allowed to do to the namespace when it is not going to succeed.
//
// The fourth independent audit (2026-10-23) reproduced three failures that all came from the same
// place: `handleCopyMove` performed the destination overwrite (a committed `TrashCapped`) BEFORE the
// move it was part of had been validated, and a MOVE's overwrite was still "trash the destination,
// then move the source", which discards the destination's physical bytes on the tree layout.
//
//	P0-DAV-001  MOVE onto its own URI answered 204 and put the file in the trash.
//	P0-DAV-002  MOVE into its own subtree trashed the child, then the move failed with 409.
//	P0-DAV-003  after a successful MOVE overwrite the displaced target was in the trash, and on the
//	            tree layout its bytes were gone: restoring the node gave a name with no content.
//
// These tests pin the acceptance criteria: a refused MOVE has NO side effects, a same-resource MOVE
// is refused rather than obeyed, and a successful overwrite keeps the displaced destination readable
// through the trash. Every scenario runs against the real blob and tree stores, because the third
// failure is layout-specific and a blob-only suite passes it.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// nodeID resolves a path and returns the node's id, failing the test if it cannot.
func nodeID(t *testing.T, e *testEnv, rel string) [16]byte {
	t.Helper()
	var id [16]byte
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := e.meta.Resolve(ctx, e.user.ID, rel)
		if err != nil {
			return err
		}
		id = nodes[len(nodes)-1].ID
		return nil
	}); err != nil {
		t.Fatalf("resolve %q: %v", rel, err)
	}
	return id
}

// nodeAt returns the node row, including a trashed one.
func nodeAt(t *testing.T, e *testEnv, id [16]byte) store.Node {
	t.Helper()
	var n store.Node
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.NodeIncludingTrashed(ctx, id)
		n = got
		return err
	}); err != nil {
		t.Fatalf("read node %s: %v", id, err)
	}
	return n
}

// trashedNodes lists the trash.
func trashedNodes(t *testing.T, e *testEnv) []store.Node {
	t.Helper()
	var out []store.Node
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, err := tx.Trashed(ctx, e.user.ID, 1000)
		out = items
		return err
	}); err != nil {
		t.Fatalf("list trash: %v", err)
	}
	return out
}

// childNameOf finds the live child of the root with the given id.
func childNameOf(t *testing.T, e *testEnv, id [16]byte) string {
	t.Helper()
	root, err := e.meta.Resolve(context.Background(), e.user.ID, "")
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	var name string
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		kids, kerr := tx.Children(ctx, root[len(root)-1].ID, 1000)
		for _, k := range kids {
			if k.ID == id {
				name = k.Name
			}
		}
		return kerr
	}); err != nil {
		t.Fatalf("list root: %v", err)
	}
	return name
}

// childNameIn finds the live child of one directory with the given id.
func childNameIn(t *testing.T, e *testEnv, parentID, id [16]byte) string {
	t.Helper()
	var name string
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		kids, kerr := tx.Children(ctx, parentID, 1000)
		for _, k := range kids {
			if k.ID == id {
				name = k.Name
			}
		}
		return kerr
	}); err != nil {
		t.Fatalf("list children of %s: %v", parentID, err)
	}
	return name
}

// TestMoveOntoItselfKeepsTheFile is P0-DAV-001.
//
// RFC 4918 §9.9.4 allows a server to refuse a MOVE whose destination is the source, and obeying it is
// nonsense: the operation would have to delete the resource it is moving. The old code found the
// destination "already exists" (it is the source), overwrote it — trashing the user's own node — and
// then performed a move that was a no-op, answering 204. The file was gone and the client was told it
// had succeeded.
func TestMoveOntoItselfKeepsTheFile(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("keep.txt"),
			strings.NewReader("MUST SURVIVE"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}
		id := nodeID(t, e, "keep.txt")

		for _, dest := range []string{
			e.davPath("keep.txt"),
			// Same resource, different encoding: the check has to be by identity after resolution,
			// not by comparing the two strings a client sent.
			"https://cloud.example.test/dav/alice/keep%2Etxt",
		} {
			rec := e.do(t, "MOVE", e.davPath("keep.txt"), nil, map[string]string{"Destination": dest})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: MOVE onto itself (Destination %q) answered %d, want 403. Obeying it "+
					"would delete the file the request is moving.", mode, dest, rec.Code)
			}
			if got := e.do(t, http.MethodGet, e.davPath("keep.txt"), nil, nil); got.Code != http.StatusOK ||
				got.Body.String() != "MUST SURVIVE" {
				t.Fatalf("%s: GET after a refused self-MOVE = %d %q", mode, got.Code, got.Body.String())
			}
			if now := nodeID(t, e, "keep.txt"); now != id {
				t.Fatalf("%s: the node changed identity across a refused self-MOVE: %s -> %s",
					mode, id, now)
			}
			if n := nodeAt(t, e, id); n.TrashedAt != nil {
				t.Fatalf("%s: a refused self-MOVE trashed the file", mode)
			}
			if items := trashedNodes(t, e); len(items) != 0 {
				t.Fatalf("%s: a refused self-MOVE put %d item(s) in the trash: %v", mode, len(items), items)
			}
		}

		// A collection onto itself is the same situation.
		if rec := e.do(t, "MKCOL", e.davPath("folder"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: mkcol: %d", mode, rec.Code)
		}
		dirID := nodeID(t, e, "folder")
		if rec := e.do(t, "MOVE", e.davPath("folder"), nil,
			map[string]string{"Destination": e.davPath("folder")}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: collection MOVE onto itself answered %d, want 403", mode, rec.Code)
		}
		if n := nodeAt(t, e, dirID); n.TrashedAt != nil {
			t.Fatalf("%s: a refused collection self-MOVE trashed the folder", mode)
		}
	})
}

// TestMoveIntoItsOwnSubtreeIsRefusedWithoutSideEffects is P0-DAV-002.
//
// The destination's parent lies inside the source collection, so the operation can never be
// performed. The old code trashed the existing destination child first — a committed transaction —
// and only then let the metadata Move fail with ErrCycle. The client saw 409 and lost a folder.
func TestMoveIntoItsOwnSubtreeIsRefusedWithoutSideEffects(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		for _, dir := range []string{"folder", "folder/child", "folder/child/deeper"} {
			if rec := e.do(t, "MKCOL", e.davPath(dir), nil, nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: mkcol %s: %d", mode, dir, rec.Code)
			}
		}
		if rec := e.do(t, http.MethodPut, e.davPath("folder/child/precious.txt"),
			strings.NewReader("KEEP ME"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}
		folderID := nodeID(t, e, "folder")
		childID := nodeID(t, e, "folder/child")
		fileID := nodeID(t, e, "folder/child/precious.txt")

		for _, dest := range []string{
			e.davPath("folder/child"),
			e.davPath("folder/child/deeper"),
			// A destination *inside* the source is the same cycle whether or not it already exists.
			e.davPath("folder/child/deeper/moved"),
		} {
			rec := e.do(t, "MOVE", e.davPath("folder"), nil, map[string]string{"Destination": dest})
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s: MOVE into its own subtree (Destination %q) answered %d, want 409",
					mode, dest, rec.Code)
			}

			// The requirement is not the status code: it is that the refused operation changed
			// nothing. The child, its file and the source collection are all still there, with their
			// identities and bytes.
			for _, p := range []string{"folder", "folder/child", "folder/child/deeper"} {
				if got := e.do(t, "PROPFIND", e.davPath(p), nil,
					map[string]string{"Depth": "0"}); got.Code != http.StatusMultiStatus {
					t.Fatalf("%s: PROPFIND %s after a refused cycle MOVE = %d, want 207",
						mode, p, got.Code)
				}
			}
			if got := e.do(t, http.MethodGet, e.davPath("folder/child/precious.txt"), nil, nil); got.Code != http.StatusOK ||
				got.Body.String() != "KEEP ME" {
				t.Fatalf("%s: the file inside the refused destination is %d %q", mode, got.Code, got.Body.String())
			}
			if nodeID(t, e, "folder/child") != childID {
				t.Fatalf("%s: the destination child changed identity", mode)
			}
			if nodeID(t, e, "folder") != folderID {
				t.Fatalf("%s: the source collection changed identity", mode)
			}
			for _, id := range [][16]byte{folderID, childID, fileID} {
				if n := nodeAt(t, e, id); n.TrashedAt != nil {
					t.Fatalf("%s: node %s was trashed by a refused cycle MOVE", mode, id)
				}
			}
			if items := trashedNodes(t, e); len(items) != 0 {
				t.Fatalf("%s: a refused cycle MOVE put %d item(s) in the trash: %v",
					mode, len(items), items)
			}
		}

		// A collection that is not inside its own subtree still moves: the guard must not be a
		// blanket refusal of collection moves.
		if rec := e.do(t, "MKCOL", e.davPath("other"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: mkcol other: %d", mode, rec.Code)
		}
		if rec := e.do(t, "MOVE", e.davPath("folder/child"), nil,
			map[string]string{"Destination": e.davPath("other/child")}); rec.Code != http.StatusCreated {
			t.Fatalf("%s: a legitimate collection MOVE answered %d (%s)", mode, rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("other/child/precious.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "KEEP ME" {
			t.Fatalf("%s: the legitimate MOVE lost the bytes: %d %q", mode, got.Code, got.Body.String())
		}
	})
}

// TestMoveCanChangeTheSpellingOfItsOwnName is the other side of P0-DAV-001's fix.
//
// A same-node MOVE is refused when the destination is the same name. A MOVE that changes only the
// SPELLING of the name — "keep.txt" to "KEEP.TXT", which the store's name index folds together and
// Windows Explorer sends as an ordinary rename — is a legitimate rename: no other resource is
// destroyed, so refusing it would break a normal client workflow. The branch must relocate the node,
// not trash it.
func TestMoveCanChangeTheSpellingOfItsOwnName(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("keep.txt"),
			strings.NewReader("CASE SURVIVES"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}
		id := nodeID(t, e, "keep.txt")

		rec := e.do(t, "MOVE", e.davPath("keep.txt"), nil,
			map[string]string{"Destination": e.davPath("KEEP.TXT")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: case-only rename answered %d (%s), want 204", mode, rec.Code, rec.Body.String())
		}

		got := e.do(t, http.MethodGet, e.davPath("KEEP.TXT"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "CASE SURVIVES" {
			t.Fatalf("%s: the renamed file = %d %q", mode, got.Code, got.Body.String())
		}
		if now := nodeID(t, e, "KEEP.TXT"); now != id {
			t.Fatalf("%s: a rename changed the node's identity: %s -> %s", mode, id, now)
		}
		if items := trashedNodes(t, e); len(items) != 0 {
			t.Fatalf("%s: a case-only rename put %d item(s) in the trash: %v", mode, len(items), items)
		}
	})
}

// TestCopyIntoItsOwnSubtreeIsRefusedWithoutSideEffects is the COPY half of the cycle guard.
//
// The audit named MOVE, but the same preflight applies: without it a directory COPY into its own
// subtree creates the destination inside the source and then walks the copy it just created, until
// the depth guard fails and the rollback trashes a half-populated tree. The refusal is now decided
// before any of that happens.
func TestCopyIntoItsOwnSubtreeIsRefusedWithoutSideEffects(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, "MKCOL", e.davPath("box"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: mkcol: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("box/inside.txt"),
			strings.NewReader("inside bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}

		rec := e.do(t, "COPY", e.davPath("box"), nil,
			map[string]string{"Destination": e.davPath("box/nested")})
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: COPY into its own subtree answered %d, want 409", mode, rec.Code)
		}
		if got := e.do(t, http.MethodGet, e.davPath("box/inside.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "inside bytes" {
			t.Fatalf("%s: the source after a refused self-COPY: %d %q", mode, got.Code, got.Body.String())
		}
		var names []string
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			root, err := e.meta.Resolve(ctx, e.user.ID, "box")
			if err != nil {
				return err
			}
			kids, kerr := tx.Children(ctx, root[len(root)-1].ID, 1000)
			for _, k := range kids {
				names = append(names, k.Name)
			}
			return kerr
		}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		for _, n := range names {
			if n == "nested" {
				t.Fatalf("%s: a refused self-COPY created the destination directory", mode)
			}
		}
		if items := trashedNodes(t, e); len(items) != 0 {
			t.Fatalf("%s: a refused self-COPY put %d item(s) in the trash: %v", mode, len(items), items)
		}
	})
}

// TestAnUnauditableMoveHasNoSideEffects is the audit-store half of "a refused MOVE changes nothing".
//
// Every write in this system is fail-closed on the audit log (T-16): if the event cannot be recorded,
// the change is refused. The refusal happens inside the relocation transaction, which is the first
// thing a MOVE does — so the destination's bytes, the source's bytes and both node identities have to
// survive it. A 503 with a trashed destination would be the same class of defect as P0-DAV-002.
func TestAnUnauditableMoveHasNoSideEffects(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("source.txt"),
			strings.NewReader("SOURCE important user data"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed source: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("target.txt"),
			strings.NewReader("DEST existing user data"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed target: %d", mode, rec.Code)
		}
		srcID := nodeID(t, e, "source.txt")
		dstID := nodeID(t, e, "target.txt")

		e.meta.SetAuditFault(errors.New("audit storage is down"))
		rec := e.do(t, "MOVE", e.davPath("source.txt"), nil,
			map[string]string{"Destination": e.davPath("target.txt")})
		e.meta.SetAuditFault(nil)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: an unauditable MOVE answered %d (%s), want 503", mode, rec.Code, rec.Body.String())
		}
		for _, want := range []struct{ path, body string }{
			{"source.txt", "SOURCE important user data"},
			{"target.txt", "DEST existing user data"},
		} {
			got := e.do(t, http.MethodGet, e.davPath(want.path), nil, nil)
			if got.Code != http.StatusOK || got.Body.String() != want.body {
				t.Fatalf("%s: %s after a refused MOVE = %d %q", mode, want.path, got.Code, got.Body.String())
			}
		}
		if nodeID(t, e, "source.txt") != srcID || nodeID(t, e, "target.txt") != dstID {
			t.Fatalf("%s: an audited-refused MOVE changed a node's identity", mode)
		}
		if items := trashedNodes(t, e); len(items) != 0 {
			t.Fatalf("%s: an unauditable MOVE put %d item(s) in the trash: %v", mode, len(items), items)
		}
	})
}

// TestMoveOverwriteAcrossParents covers the shape where the stage and the destination are in
// DIFFERENT collections: the source is relocated into the destination's parent, then swapped, and a
// rollback has to put it back under its ORIGINAL parent, not merely under a different name.
func TestMoveOverwriteAcrossParents(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		for _, dir := range []string{"from", "to"} {
			if rec := e.do(t, "MKCOL", e.davPath(dir), nil, nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: mkcol %s: %d", mode, dir, rec.Code)
			}
		}
		if rec := e.do(t, http.MethodPut, e.davPath("from/replace.txt"),
			strings.NewReader("SOURCE bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed source: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("to/target.txt"),
			strings.NewReader("OLD TARGET bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed target: %d", mode, rec.Code)
		}
		srcID := nodeID(t, e, "from/replace.txt")
		oldID := nodeID(t, e, "to/target.txt")

		// A refused cross-parent overwrite (the trash holds 1 byte) changes nothing, in either place.
		e.h.WithTrashLimit(func(context.Context) int64 { return 1 })
		rec := e.do(t, "MOVE", e.davPath("from/replace.txt"), nil,
			map[string]string{"Destination": e.davPath("to/target.txt")})
		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("%s: a cross-parent overwrite with a full trash answered %d (want 507)",
				mode, rec.Code)
		}
		if got := e.do(t, http.MethodGet, e.davPath("from/replace.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "SOURCE bytes" {
			t.Fatalf("%s: the source after a refused cross-parent overwrite: %d %q",
				mode, got.Code, got.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("to/target.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "OLD TARGET bytes" {
			t.Fatalf("%s: the destination after a refused cross-parent overwrite: %d %q",
				mode, got.Code, got.Body.String())
		}
		if nodeID(t, e, "from/replace.txt") != srcID {
			t.Fatalf("%s: the refused move left the source under a different identity", mode)
		}

		// With the trash lifted it succeeds, and both sides are correct afterwards.
		e.h.WithTrashLimit(nil)
		rec = e.do(t, "MOVE", e.davPath("from/replace.txt"), nil,
			map[string]string{"Destination": e.davPath("to/target.txt")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: cross-parent MOVE overwrite = %d (%s)", mode, rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("to/target.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "SOURCE bytes" {
			t.Fatalf("%s: the destination after a cross-parent overwrite: %d %q",
				mode, got.Code, got.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("from/replace.txt"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s: the source is still readable after a MOVE: %d", mode, got.Code)
		}
		if now := nodeID(t, e, "to/target.txt"); now != srcID {
			t.Fatalf("%s: the destination holds %s, want the moved source %s", mode, now, srcID)
		}

		// The displaced destination is restorable with its own bytes under its new parent.
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.Restore(ctx, oldID)
			return rerr
		}); err != nil {
			t.Fatalf("%s: restore: %v", mode, err)
		}
		restored := nodeAt(t, e, oldID)
		if restored.ParentID == nil {
			t.Fatalf("%s: the restored destination has no parent", mode)
		}
		name := childNameIn(t, e, *restored.ParentID, oldID)
		if name == "" {
			t.Fatalf("%s: the restored destination is not visible under its parent", mode)
		}
		body, err := displacedBytes(t, e, path.Join("to", name))
		if err != nil {
			t.Fatalf("%s: the restored old target under its parent has no bytes: %v", mode, err)
		}
		if body != "OLD TARGET bytes" {
			t.Fatalf("%s: the restored old target under its parent holds %q", mode, body)
		}
	})
}

// TestMoveOverwriteKeepsTheDisplacedBytesReadable is P0-DAV-003 for files.
//
// The audit's scenario, minus the unsupported step: it restored the displaced node and then renamed
// it with a bare metadata transaction, which no user path does and which cannot work on a path-backed
// layout for ANY node (renaming metadata does not move bytes). What the product must guarantee is
// that the displaced destination's bytes survive the overwrite and are readable at the path its
// metadata names after a restore — the sequence the trash UI performs.
func TestMoveOverwriteKeepsTheDisplacedBytesReadable(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("source.txt"),
			strings.NewReader("CURRENT SOURCE"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed source: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("target.txt"),
			strings.NewReader("IRREPLACEABLE OLD TARGET"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed target: %d", mode, rec.Code)
		}
		srcID := nodeID(t, e, "source.txt")
		oldID := nodeID(t, e, "target.txt")

		rec := e.do(t, "MOVE", e.davPath("source.txt"), nil,
			map[string]string{"Destination": e.davPath("target.txt")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: MOVE overwrite = %d (%s)", mode, rec.Code, rec.Body.String())
		}

		// The source node is now the destination: same identity, same bytes.
		if now := nodeID(t, e, "target.txt"); now != srcID {
			t.Fatalf("%s: the destination holds node %s, want the moved source %s", mode, now, srcID)
		}
		if got := e.do(t, http.MethodGet, e.davPath("target.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "CURRENT SOURCE" {
			t.Fatalf("%s: the moved source's bytes at the destination: %d %q",
				mode, got.Code, got.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("source.txt"), nil, nil); got.Code != http.StatusNotFound {
			t.Fatalf("%s: the source name still resolves after a MOVE: %d", mode, got.Code)
		}

		// The displaced destination is a trash entry with its own identity.
		var trashed *store.Node
		for _, n := range trashedNodes(t, e) {
			if n.ID == oldID {
				copied := n
				trashed = &copied
			}
		}
		if trashed == nil {
			t.Fatalf("%s: the displaced destination (node %s) is not in the trash", mode, oldID)
		}

		// Restore it the way the trash does, then read it under the name its metadata now carries.
		// That path HAS to be the one its bytes are at; on the tree layout the old code let the
		// physical move overwrite them, so this read answered 500.
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.Restore(ctx, oldID)
			return rerr
		}); err != nil {
			t.Fatalf("%s: restore the displaced destination: %v", mode, err)
		}
		name := childNameOf(t, e, oldID)
		if name == "" {
			t.Fatalf("%s: the restored node is not visible in its directory", mode)
		}
		body, err := displacedBytes(t, e, name)
		if err != nil {
			t.Fatalf("%s: the restored old target (%q) has no bytes: %v. A trash entry whose bytes "+
				"are gone is not a recovery: it is a promise the deployment cannot keep.",
				mode, name, err)
		}
		if body != "IRREPLACEABLE OLD TARGET" {
			t.Fatalf("%s: the restored old target holds %q", mode, body)
		}
	})
}

// TestMoveOverwriteOfADirectoryKeepsTheDisplacedTreeReadable is the same property for a collection,
// where the physical operation is a whole-subtree rename.
func TestMoveOverwriteOfADirectoryKeepsTheDisplacedTreeReadable(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		for _, dir := range []string{"src", "dst"} {
			if rec := e.do(t, "MKCOL", e.davPath(dir), nil, nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: mkcol %s: %d", mode, dir, rec.Code)
			}
		}
		if rec := e.do(t, http.MethodPut, e.davPath("src/new.txt"),
			strings.NewReader("new bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("dst/old.txt"),
			strings.NewReader("old bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}
		oldID := nodeID(t, e, "dst")

		rec := e.do(t, "MOVE", e.davPath("src"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: directory MOVE overwrite = %d (%s)", mode, rec.Code, rec.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("dst/new.txt"), nil, nil); got.Code != http.StatusOK ||
			got.Body.String() != "new bytes" {
			t.Fatalf("%s: the replacement's child: %d %q", mode, got.Code, got.Body.String())
		}
		if got := e.do(t, http.MethodGet, e.davPath("dst/old.txt"), nil, nil); got.Code == http.StatusOK {
			t.Fatalf("%s: the replaced directory still serves its old child", mode)
		}

		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.Restore(ctx, oldID)
			return rerr
		}); err != nil {
			t.Fatalf("%s: restore: %v", mode, err)
		}
		name := childNameOf(t, e, oldID)
		if name == "" {
			t.Fatalf("%s: the restored directory is not visible in the root", mode)
		}
		body, err := displacedBytes(t, e, path.Join(name, "old.txt"))
		if err != nil {
			t.Fatalf("%s: the restored directory's child has no bytes: %v. The whole displaced "+
				"subtree must survive, not just its top node.", mode, err)
		}
		if body != "old bytes" {
			t.Fatalf("%s: the restored directory's child holds %q", mode, body)
		}
	})
}

// TestAFailedMoveOverwriteLeavesBothSidesIntact is the promise under every one of the three P0s:
// a MOVE that cannot be performed must not have changed anything.
//
// The trigger is the trash cap, the same one the third review used, because it fails INSIDE the
// metadata transaction that would otherwise commit the overwrite.
func TestAFailedMoveOverwriteLeavesBothSidesIntact(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("source.txt"),
			strings.NewReader("SOURCE important user data"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed source: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("target.txt"),
			strings.NewReader("DEST existing user data"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed target: %d", mode, rec.Code)
		}
		srcID := nodeID(t, e, "source.txt")
		dstID := nodeID(t, e, "target.txt")

		e.h.WithTrashLimit(func(context.Context) int64 { return 1 })

		rec := e.do(t, "MOVE", e.davPath("source.txt"), nil,
			map[string]string{"Destination": e.davPath("target.txt")})
		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("%s: MOVE with a full trash answered %d (%s), want 507",
				mode, rec.Code, rec.Body.String())
		}

		for _, want := range []struct{ path, body string }{
			{"source.txt", "SOURCE important user data"},
			{"target.txt", "DEST existing user data"},
		} {
			got := e.do(t, http.MethodGet, e.davPath(want.path), nil, nil)
			if got.Code != http.StatusOK || got.Body.String() != want.body {
				t.Fatalf("%s: %s after a refused overwrite = %d %q, want %q",
					mode, want.path, got.Code, got.Body.String(), want.body)
			}
		}
		if nodeID(t, e, "source.txt") != srcID || nodeID(t, e, "target.txt") != dstID {
			t.Fatalf("%s: a refused overwrite changed a node's identity", mode)
		}
		if items := trashedNodes(t, e); len(items) != 0 {
			t.Fatalf("%s: a refused overwrite put %d item(s) in the trash: %v", mode, len(items), items)
		}
	})
}

// displacedBytes reads a trashed node's content at the path its metadata names.
//
// The reserved names a swap leaves behind are no longer addressable over DAV — that is the point
// of refusing them — so "the displaced bytes survived" is checked where the bytes actually live
// rather than through the namespace. A user reaches them the way the product intends: a restore
// through the API, which renames the node out of the reserved name and moves the bytes with it
// (covered by TestRestoreRecoversTheNameAfterAMoveOverwrite in internal/server).
func displacedBytes(t *testing.T, e *testEnv, rel string) (string, error) {
	t.Helper()
	rc, err := e.h.content.Open(context.Background(),
		layout.Node{Owner: e.user.ID.String(), Path: rel})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, rerr := io.ReadAll(rc)
	return string(b), rerr
}

// A swap's staging name is not a resource.
//
// These names exist only between a replacement being staged and installed. They are filtered out
// of listings, and a client that guessed one could read content mid-copy or corrupt a swap in
// flight, so addressing them answers 404 — the same answer as a name that does not exist, which
// is deliberate: a 403 would confirm that an overwrite is happening at that path right now
// (2026-10-24 review, M4).
func TestReservedSwapNamesAreNotAddressable(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		for _, name := range []string{
			"0123456789abcdef.newcloud-staged-budget.txt",
			"0123456789abcdef.newcloud-replaced-budget.txt",
		} {
			if rec := e.do(t, http.MethodGet, e.davPath(name), nil, nil); rec.Code != http.StatusNotFound {
				t.Fatalf("%s: GET %q = %d, want 404: a swap's staging name must not be readable",
					mode, name, rec.Code)
			}
			if rec := e.do(t, http.MethodPut, e.davPath(name),
				strings.NewReader("injected"), nil); rec.Code != http.StatusNotFound {
				t.Fatalf("%s: PUT %q = %d, want 404: writing to a staging name would corrupt a "+
					"swap in flight", mode, name, rec.Code)
			}
			if rec := e.do(t, "PROPFIND", e.davPath(name),
				strings.NewReader(propfindAllProp), map[string]string{"Depth": "0"}); rec.Code == http.StatusMultiStatus {
				t.Fatalf("%s: PROPFIND %q answered 207; the refusal must not disclose whether the "+
					"name exists", mode, name)
			}
		}
	})
}

// A DELETE frees the path its content occupied.
//
// Trashing is a metadata operation, so on a path-backed layout the bytes used to stay exactly where
// the deleted file was. Two things followed: the name was not free until the retention sweep ran, so
// nothing could publish there without replacing; and the retained copy sat at a path derived from a
// name that no longer existed in the namespace. The delete now moves the content to a reserved
// sibling name first — and the restore route moves it back, which is why this is safe to do.
//
// The blob layout has nothing to move (its path is a metadata mapping over content-addressed bytes),
// which is asserted too, because "nothing happened" there is the correct answer, not a miss.
func TestADeleteFreesTheContentPath(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("gone.txt"),
			strings.NewReader("retained bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodDelete, e.davPath("gone.txt"), nil, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: delete: %d", mode, rec.Code)
		}

		_, err := displacedBytes(t, e, "gone.txt")
		switch mode {
		case layout.ModeTree:
			if err == nil {
				t.Fatal("tree: the deleted file's content is still at the path it occupied, so the " +
					"name cannot be reused without replacing and the retained copy is reachable at a " +
					"name that is no longer in the namespace")
			}
		default:
			if err == nil {
				t.Log("blob: content is content-addressed, so the path is only a mapping")
			}
		}
	})
}
