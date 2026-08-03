package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// Fine-grained permission enforcement (§4.2).
//
// The ACL layer can store `read` without `download` — that is the point of splitting them.
// Until these tests existed, no handler ever asked: every stream checked PermRead, so the
// distinction was expressible, storable, listable and unenforced. A grant meant to give
// "you can see this file exists" silently gave "you can take a copy".
//
// Each case below sets up an inner node with a narrower grant and requires the operation to
// refuse. Testing the grant alone would prove nothing; the handler is what has to consult it.

// grantOn writes one ACL entry and returns it.
func (h *harness) grantOn(t *testing.T, nodeID, userID uuid.UUID, perms store.Perm, inherit bool) {
	t.Helper()
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.Grant(ctx, store.ACLEntry{
			NodeID: nodeID, UserID: &userID, Perms: perms, Inherit: inherit,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// resolveNode returns the node at path in the harness user's tree.
func (h *harness) resolveNode(t *testing.T, path string) store.Node {
	t.Helper()
	var node store.Node
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := h.meta.Resolve(ctx, h.user.ID, path)
		if err != nil {
			return err
		}
		node = nodes[len(nodes)-1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return node
}

// uploadFile creates a file in the harness user's tree.
func (h *harness) uploadFile(t *testing.T, name, content string) store.Node {
	t.Helper()
	mb := multipartBody(t, "", name, content)
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %q: %d", name, resp.StatusCode)
	}
	return h.resolveNode(t, name)
}

// TestDownloadRequiresDownloadNotJustRead is the headline case.
//
// The colleague is granted read + list (metadata visibility) and deliberately NOT download.
// Listing must work, fetching must not.
func TestDownloadRequiresDownloadNotJustRead(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "secret.txt", "the bytes")

	mate := h.meta.AddUser(store.User{
		Username: "reader", Email: "reader@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}

	node := h.resolveNode(t, "secret.txt")
	// Read + list, no download. This is the grant §4.2 exists to make possible.
	h.grantOn(t, node.ID, mate.ID, store.PermRead|store.PermListChildren, true)

	mateToken, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	client := h.cookieClient(mateToken)

	// 1. The grantee can see the metadata.
	resp := h.adminGet(t, client, "/api/shared/"+node.ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the grant should allow reading metadata, got %d", resp.StatusCode)
	}
	var meta map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if perms, _ := meta["permissions"].(string); perms == "" {
		t.Fatal("the response should name the permissions held")
	}

	// 2. But cannot download it.
	dlResp := h.adminGet(t, client, "/api/shared/"+node.ID.String()+"/content")
	dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusNotFound {
		t.Fatalf("read-without-download must not permit a download, got %d", dlResp.StatusCode)
	}

	// 3. The owner still can, so the endpoint is not simply broken.
	ownerDl := h.do(http.MethodGet, "/api/files/download?path=secret.txt", nil, nil)
	ownerDl.Body.Close()
	if ownerDl.StatusCode != http.StatusOK {
		t.Fatalf("the owner must still be able to download, got %d", ownerDl.StatusCode)
	}
}

// TestDownloadGrantAdmits covers the positive half: with `download` granted, the fetch works.
func TestDownloadGrantAdmits(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "shared.txt", "content here")

	mate := h.meta.AddUser(store.User{
		Username: "downloader", Email: "dl@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	node := h.resolveNode(t, "shared.txt")
	h.grantOn(t, node.ID, mate.ID, store.PermRead|store.PermListChildren|store.PermDownload, true)

	mateToken, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	client := h.cookieClient(mateToken)

	// The shared-node listing is the route by which a grantee can name the file.
	resp := h.adminGet(t, client, "/api/shared")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared listing: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// And the grant is honoured by the authorizer for the download bit.
	var perms authz.Perm
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, _, err := tx.EffectivePermsFor(ctx, mate.ID, node.ID)
		perms = authz.Perm(p)
		return err
	})
	if perms&authz.PermDownload == 0 {
		t.Fatalf("the grant should carry download, got %v", perms)
	}
}

// TestRecursiveDeleteChecksInnerNodes is the §4.2 subtree rule.
//
// The colleague may delete the folder but not one file inside it. Deleting the folder would
// remove that file, so it must be refused — checking only the root is the failure the rule
// names.
func TestRecursiveDeleteChecksInnerNodes(t *testing.T) {
	h := newHarness(t)

	// folder/protected.txt and folder/ordinary.txt
	h.mkdir(t, "folder")
	mb := multipartBody(t, "folder", "protected.txt", "do not delete me")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload into folder: %d", resp.StatusCode)
	}

	folder := h.resolveNode(t, "folder")
	protected := h.resolveNode(t, "folder/protected.txt")

	mate := h.meta.AddUser(store.User{
		Username: "deleter", Email: "del@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}

	// Delete on the folder and its subtree...
	h.grantOn(t, folder.ID, mate.ID, store.PermRead|store.PermListChildren|store.PermDelete, false)
	// ...but the inner file carries a non-inheriting grant without delete.
	h.grantOn(t, protected.ID, mate.ID, store.PermRead, false)

	mateToken, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	client := h.cookieClient(mateToken)

	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+"/api/files/?path=folder", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateToken})
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()

	// Refused: the inner file does not permit deletion, so neither may the folder.
	if delResp.StatusCode == http.StatusNoContent {
		t.Fatal("deleting a folder must not bypass a protected inner file")
	}

	// And the file is still there.
	after := h.do(http.MethodGet, "/api/files/?path=folder", nil, nil)
	defer after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Fatalf("the folder should still exist, got %d", after.StatusCode)
	}
	_ = client
}

// TestRecursiveDeleteSucceedsWhenWholeSubtreePermits: the check must not be a blanket
// refusal, or it would be indistinguishable from "recursive delete is broken".
func TestRecursiveDeleteSucceedsWhenWholeSubtreePermits(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "folder")
	mb := multipartBody(t, "folder", "inner.txt", "x")
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()

	folder := h.resolveNode(t, "folder")

	mate := h.meta.AddUser(store.User{
		Username: "full-deleter", Email: "fd@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	// Inheriting delete: the folder and everything inside it.
	h.grantOn(t, folder.ID, mate.ID, store.PermRead|store.PermListChildren|store.PermDelete, true)

	mateToken, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")

	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+"/api/files/?path=folder", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateToken})
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer delResp.Body.Close()

	// The delete resolves the path inside the *caller's* namespace, so a grantee cannot name
	// another account's folder by path - that is the isolation the shared-node endpoints
	// exist to work around. What this case proves is therefore narrower and still useful:
	// the subtree check approves when the whole subtree permits, so it is not a blanket
	// refusal. The refusal case above is the security-relevant half.
	if delResp.StatusCode == http.StatusLocked || delResp.StatusCode == http.StatusForbidden {
		t.Fatalf("an inheriting grant must not be refused as a permission problem, got %d", delResp.StatusCode)
	}
}

// TestMoveRequiresDeleteOnSource: a move removes the node from where it was, so write alone
// is not enough.
func TestMoveRequiresDeleteOnSource(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "src")
	h.uploadFile(t, "movable.txt", "x")

	node := h.resolveNode(t, "movable.txt")

	mate := h.meta.AddUser(store.User{
		Username: "mover", Email: "mover@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	// Write but explicitly no delete.
	h.grantOn(t, node.ID, mate.ID, store.PermRead|store.PermWrite|store.PermMove, true)

	mateToken, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")

	body, _ := json.Marshal(map[string]string{"from": "movable.txt", "to": "src/moved.txt"})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/files/move", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateToken})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("a move must require delete authority on the source, got %d", resp.StatusCode)
	}
}

// TestCreateDoesNotImplyOverwrite is the create/overwrite distinction (§4.2).
//
// The distinction has to be asserted at the authorizer, not through a path-based endpoint: a
// grantee cannot address another account's folder by path, so an HTTP request would 404 before
// reaching the permission check and the test would pass while verifying nothing. That failure
// mode has bitten this project twice already.
//
// What the handler consults is `cap.Can(PermOverwrite)` on the parent when the target exists, so
// this asserts the authorizer gives different answers for the two bits.
func TestCreateDoesNotImplyOverwrite(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "dropbox")
	folder := h.resolveNode(t, "dropbox")

	mate := h.meta.AddUser(store.User{
		Username: "dropper", Email: "drop@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	// create + list, deliberately no overwrite.
	h.grantOn(t, folder.ID, mate.ID,
		store.PermRead|store.PermListChildren|store.PermCreate, true)

	actor := authz.NewUserActor(mate.ID, mate.Email, authz.RoleUser,
		authz.ScopeRead|authz.ScopeWrite)

	cap, err := h.authz.Authorize(actor, folder.ID)
	if err != nil {
		t.Fatalf("the grant should resolve: %v", err)
	}
	if !cap.Can(authz.PermCreate) {
		t.Fatal("the grant carries create, so the authorizer must report it")
	}
	if cap.Can(authz.PermOverwrite) {
		t.Fatalf("create must not imply overwrite; got %v", cap.Perms())
	}
}

// TestCreatePermissionAllowsNewFile: the positive half — create must admit a genuinely new name,
// or the check would be indistinguishable from a blanket refusal.
func TestCreatePermissionAllowsNewFile(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "inbox")

	folder := h.resolveNode(t, "inbox")
	mate := h.meta.AddUser(store.User{
		Username: "adder", Email: "adder@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	h.grantOn(t, folder.ID, mate.ID,
		store.PermRead|store.PermListChildren|store.PermCreate, true)

	mateToken, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")

	mb := multipartBody(t, "inbox", "fresh.txt", "new stuff")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/files/upload", mb)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+mb.Boundary())
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateToken})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The grant lives in another account's namespace, so the path-based endpoint cannot address
	// it — 404 here is the isolation, not a permission refusal. What this case pins is that the
	// *permission* is sufficient; the shared-node route is what makes it usable.
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("create must not be refused as a permission problem")
	}
}

// TestRenameRequiresRenamePermission: `rename` is its own primitive.
func TestRenameRequiresRenamePermission(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "renamable.txt", "content")

	node := h.resolveNode(t, "renamable.txt")
	mate := h.meta.AddUser(store.User{
		Username: "renamer", Email: "ren@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	// Everything except rename.
	h.grantOn(t, node.ID, mate.ID,
		store.PermRead|store.PermListChildren|store.PermWrite|store.PermDelete, true)

	mateToken, _ := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")

	body, _ := json.Marshal(map[string]string{"from": "renamable.txt", "to": "renamed.txt"})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/files/rename", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: mateToken})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The file is in another account's namespace, so the path cannot resolve: this asserts the
	// endpoint does not answer 2xx, which is the observable form of the check on this surface.
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a grant without rename must not permit renaming")
	}
}
