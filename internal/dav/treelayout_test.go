package dav

// Tree-layout coverage for COPY overwrite (PRG-001).
//
// The blob layout keeps its hierarchy in the database and its bytes under a content address, so a
// metadata rename swaps the destination without touching a filesystem. The tree layout mirrors the
// hierarchy ON DISK: renaming the metadata row does not move anything, and a filesystem rename
// cannot land on an existing non-empty directory.
//
// The first version of the NC-RR-005 staged swap renamed the metadata first and moved the bytes
// afterwards, which therefore failed in the only layout where the order matters: the physical
// destination path was still occupied by the old tree, the move was refused, and the compensation
// tried to rename the original back onto a name the staged node already held. An independent audit
// reproduced it end to end — COPY 500, old file 404, new file 500 — while every existing COPY test
// passed, because the only DAV harness was blob-backed.
//
// Every test here runs the same scenarios against BOTH layouts. A layout-specific guarantee that is
// only tested on the other layout is not a guarantee.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/tree"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// newTreeEnv wires the same handler over the REAL tree.Store, which is what production selects when
// storage.layout is "tree". Using the real implementation rather than a fake is the point: the bug
// was a disagreement between the metadata model and the filesystem, and a fake would have to model
// that disagreement itself to show it.
func newTreeEnv(t *testing.T) *testEnv {
	t.Helper()

	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Limits.MaxProfindChildren = 1000

	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}

	content, err := tree.New(t.TempDir())
	if err != nil {
		t.Fatalf("tree store: %v", err)
	}
	t.Cleanup(func() { _ = content.Close() })

	appPass := auth.NewAppPasswords(meta)
	secret, _, err := appPass.Issue(context.Background(), user.ID, "test", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	h := New(cfg, meta, content, authz.NewService(&authzAccessor{m: meta}), appPass)
	return &testEnv{h: h, meta: meta, user: user, secret: secret, extra: map[string]string{}}
}

// layouts runs a subtest against each content layout, so a scenario cannot silently cover one.
func layouts(t *testing.T, fn func(t *testing.T, e *testEnv, mode layout.Mode)) {
	t.Helper()
	for _, tc := range []struct {
		name string
		mode layout.Mode
		env  func(*testing.T) *testEnv
	}{
		{"blob", layout.ModeBlob, newEnv},
		{"tree", layout.ModeTree, newTreeEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn(t, tc.env(t), tc.mode)
		})
	}
}

// TestCopyOverwriteOfADirectoryWorksInEveryLayout is the audit's PRG-001 probe, run against both
// layouts and asserting the exact bytes.
func TestCopyOverwriteOfADirectoryWorksInEveryLayout(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		seedOverwriteTree(t, e)

		rec := e.do(t, "COPY", e.davPath("src"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: COPY over an existing directory = %d, want 204 (%s). A 500 here means "+
				"the swap tried to land the new tree on a path the old tree still occupied.",
				mode, rec.Code, rec.Body.String())
		}

		// The new content is live, at the real destination name, with the right bytes.
		got := e.do(t, http.MethodGet, e.davPath("dst/a.txt"), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: GET dst/a.txt after a successful overwrite = %d, want 200",
				mode, got.Code)
		}
		if body := got.Body.String(); len(body) != 1024 {
			t.Fatalf("%s: dst/a.txt = %d bytes, want the source content of 1024", mode, len(body))
		}

		// The old child is gone from the live tree.
		if old := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil); old.Code == http.StatusOK {
			t.Fatalf("%s: the previous destination's file survived the overwrite in the live tree",
				mode)
		}

		// And every child of the source came across, not just the first.
		for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
			if r := e.do(t, http.MethodGet, e.davPath("dst/"+n), nil, nil); r.Code != http.StatusOK {
				t.Fatalf("%s: dst/%s missing after the overwrite: %d", mode, n, r.Code)
			}
		}
	})
}

// TestCopyOverwriteOfAFileWorksInEveryLayout covers the single-file destination, which is what most
// COPY requests actually target.
func TestCopyOverwriteOfAFileWorksInEveryLayout(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("src.bin"),
			strings.NewReader("the replacement bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed src: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("dst.bin"),
			strings.NewReader("the original bytes"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed dst: %d", mode, rec.Code)
		}

		rec := e.do(t, "COPY", e.davPath("src.bin"), nil,
			map[string]string{"Destination": e.davPath("dst.bin")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: file COPY overwrite = %d, want 204 (%s)",
				mode, rec.Code, rec.Body.String())
		}

		got := e.do(t, http.MethodGet, e.davPath("dst.bin"), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: GET dst.bin = %d, want 200", mode, got.Code)
		}
		if body := got.Body.String(); body != "the replacement bytes" {
			t.Fatalf("%s: dst.bin = %q, want the replacement", mode, body)
		}

		// The source is untouched by an overwrite of somebody else's path.
		src := e.do(t, http.MethodGet, e.davPath("src.bin"), nil, nil)
		if src.Code != http.StatusOK || src.Body.String() != "the replacement bytes" {
			t.Fatalf("%s: the source changed during a COPY: %d %q", mode, src.Code, src.Body.String())
		}
	})
}

// TestAFailedCopyOverwriteKeepsTheOriginalInEveryLayout is the NC-RR-005 property re-checked per
// layout. It passed on blob and was never run on tree, which is how the regression shipped.
func TestAFailedCopyOverwriteKeepsTheOriginalInEveryLayout(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		seedOverwriteTree(t, e)
		e.meta.SetQuota(e.user.ID, 4600)

		rec := e.do(t, "COPY", e.davPath("src"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		if okStatus(rec.Code) {
			t.Skip("the copy fit the quota, so the failure this test induces did not happen")
		}

		got := e.do(t, http.MethodGet, e.davPath("dst/keep.txt"), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: GET dst/keep.txt after a FAILED overwrite = %d, want 200. The "+
				"original was lost by an operation that reported failure.", mode, got.Code)
		}
		if body := got.Body.String(); body != "the original bytes" {
			t.Fatalf("%s: the destination's bytes changed during a failed overwrite: %q", mode, body)
		}
	})
}

// TheReplacedDestinationIsRecoverableInEveryLayout is the "old version recoverable" half of the
// audit's acceptance criteria.
//
// A replacement that destroys the previous bytes is not a safe overwrite even if the new content
// landed correctly. The displaced original must be restorable from the trash, with its own bytes and
// its own node identity (so its versions and ACLs still apply).
func TestTheReplacedDestinationIsRecoverableInEveryLayout(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("src.txt"),
			strings.NewReader("the replacement"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed src: %d", mode, rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("dst.txt"),
			strings.NewReader("the precious original"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("%s: seed dst: %d", mode, rec.Code)
		}

		// Remember the node id so the restore can be checked against the SAME node, not merely
		// against something with the right name.
		var originalID [16]byte
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			n, err := e.meta.Resolve(ctx, e.user.ID, "dst.txt")
			if err != nil {
				return err
			}
			originalID = n[len(n)-1].ID
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}

		if rec := e.do(t, "COPY", e.davPath("src.txt"), nil,
			map[string]string{"Destination": e.davPath("dst.txt")}); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: COPY overwrite = %d (%s)", mode, rec.Code, rec.Body.String())
		}

		// The displaced node is in the trash and is the same node.
		var trashed []store.Node
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			items, derr := tx.Trashed(ctx, e.user.ID, 100)
			trashed = items
			return derr
		}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		var found bool
		for i := range trashed {
			if trashed[i].ID == originalID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: the replaced destination (node %s) is not in the trash, so the previous "+
				"version is not recoverable", mode, originalID)
		}

		// Restore it and read it back. The name it returns under differs by layout: the tree layout
		// keeps the reserved backup name because that is the path its bytes live at, while the blob
		// layout can return the original name since its bytes are content-addressed. What must hold
		// in both is that reading it yields the PREVIOUS bytes.
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, rerr := tx.Restore(ctx, originalID)
			return rerr
		}); err != nil {
			t.Fatalf("%s: restore: %v", mode, err)
		}

		root, err := e.meta.Resolve(context.Background(), e.user.ID, "")
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		var restoredName string
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			kids, kerr := tx.Children(ctx, root[len(root)-1].ID, 1000)
			for _, k := range kids {
				if k.ID == originalID {
					restoredName = k.Name
				}
			}
			return kerr
		}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if restoredName == "" {
			t.Fatalf("%s: the restored node is not visible in the tree root", mode)
		}

		got := e.do(t, http.MethodGet, e.davPath(restoredName), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: GET the restored original (%q) = %d, want 200. The trash row exists but "+
				"its bytes were not preserved, so the previous version is only nominally "+
				"recoverable.", mode, restoredName, got.Code)
		}
		if body := got.Body.String(); body != "the precious original" {
			t.Fatalf("%s: the restored original holds %q, want the previous version's bytes",
				mode, body)
		}
	})
}

// TestNoOverwriteArtifactsRemainInEveryLayout proves the reserved sibling names never become visible
// in the user's namespace, in either layout.
func TestNoOverwriteArtifactsRemainInEveryLayout(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		seedOverwriteTree(t, e)

		// One failed overwrite and one successful one: both paths must clean up after themselves.
		e.meta.SetQuota(e.user.ID, 4600)
		_ = e.do(t, "COPY", e.davPath("src"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		e.meta.SetQuota(e.user.ID, 0)
		rec := e.do(t, "COPY", e.davPath("src"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: the retried overwrite failed: %d (%s)", mode, rec.Code, rec.Body.String())
		}

		var names []string
		if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			nodes, err := e.meta.Resolve(ctx, e.user.ID, "")
			if err != nil {
				return err
			}
			kids, kerr := tx.Children(ctx, nodes[len(nodes)-1].ID, 1000)
			for _, k := range kids {
				names = append(names, k.Name)
			}
			return kerr
		}); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		for _, n := range names {
			if strings.Contains(n, ".newcloud-staged-") || strings.Contains(n, ".newcloud-replaced-") {
				t.Fatalf("%s: the intermediate %q is visible in the user's namespace", mode, n)
			}
		}
	})
}

// TestCopyOverwriteDeepTreeWorksInEveryLayout exercises a nested structure, where a partial
// filesystem move is most likely to leave a half-populated tree.
func TestCopyOverwriteDeepTreeWorksInEveryLayout(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		for _, dir := range []string{"src", "src/inner", "dst"} {
			if rec := e.do(t, "MKCOL", e.davPath(dir), nil, nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: mkcol %s: %d", mode, dir, rec.Code)
			}
		}
		for _, f := range []struct{ path, body string }{
			{"src/inner/deep.txt", "deep bytes"},
			{"dst/old.txt", "old"},
		} {
			if rec := e.do(t, http.MethodPut, e.davPath(f.path),
				strings.NewReader(f.body), nil); rec.Code != http.StatusCreated {
				t.Fatalf("%s: seed %s: %d", mode, f.path, rec.Code)
			}
		}

		rec := e.do(t, "COPY", e.davPath("src"), nil,
			map[string]string{"Destination": e.davPath("dst")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: nested COPY overwrite = %d (%s)", mode, rec.Code, rec.Body.String())
		}

		got := e.do(t, http.MethodGet, e.davPath("dst/inner/deep.txt"), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: the nested child did not survive the swap: %d", mode, got.Code)
		}
		if body := got.Body.String(); body != "deep bytes" {
			t.Fatalf("%s: dst/inner/deep.txt = %q", mode, body)
		}
	})
}
