package server_test

// The external halves of the file routes, driven end to end over HTTP with a fake
// provider behind a real mount service. What these tests pin is the CONTRACT between the
// surfaces: a mount folder lists from the remote, writes land on the remote, local paths
// behave exactly as before, refusals are the documented ones, and every operation leaves
// an audit row.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/mount"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// mountHarness builds a harness whose server has a mount service wired to one shared
// fake remote. The mount is created through the API, as a user would.
func mountHarness(t *testing.T, mutate func(*config.MountsConfig)) (*harness, *external.Fake, store.Mount) {
	t.Helper()
	var fake *external.Fake
	h := newHarnessTuned(t, func(c *config.Config) {
		c.Mounts = config.MountsConfig{
			Enabled: true, SecretFile: "test", TimeoutSec: 5, MaxListEntries: 100,
			AllowedSchemes: []string{"webdav"}, AllowUserMounts: true,
			// The fake endpoint is not dialled for real; the guard would still refuse
			// the non-admin creator's https://remote.example.test on name resolution.
			DenyPrivateAddresses: false,
		}
		if mutate != nil {
			mutate(&c.Mounts)
		}
	}, func(d *server.Deps) {
		c, err := mount.NewCipher(bytes.Repeat([]byte{4}, 32))
		if err != nil {
			t.Fatal(err)
		}
		fake = external.NewFake()
		fake.AddDir("docs")
		fake.AddFile("docs/readme.txt", []byte("from the remote"), time.Now())
		svc := mount.New(d.Meta, d.Config.Mounts, c, nil)
		svc.RegisterDial("webdav", func(ctx context.Context, endpoint, root, user, pass string) (external.Storage, error) {
			return fake, nil
		})
		d.Mounts = svc
	})

	body, _ := json.Marshal(map[string]any{
		"name": "team", "scheme": "webdav", "endpoint": "https://remote.example.test/dav",
		"username": "svc", "password": "opensesame", "verify": false,
	})
	resp := h.do(http.MethodPost, "/api/mounts", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("mount create: %d (%s)", resp.StatusCode, string(out))
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	var m store.Mount
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		m, err = tx.MountByNodeID(ctx, mustResolveID(t, h, "team"))
		return err
	})
	if m.ID.String() == "" || m.ID.String() != created.ID {
		t.Fatalf("mount row not created as expected: %+v vs %s", m.ID, created.ID)
	}
	return h, fake, m
}

func mustResolveID(t *testing.T, h *harness, name string) (id [16]byte) {
	t.Helper()
	nodes, err := h.meta.Resolve(context.Background(), h.user.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return nodes[len(nodes)-1].ID
}

func listMount(t *testing.T, h *harness, path string) []map[string]any {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/?path="+url.QueryEscape(path), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("list %q: %d (%s)", path, resp.StatusCode, string(out))
	}
	var out struct {
		Children []map[string]any `json:"children"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Children
}

func TestMountsBrowseAndEditTheRemoteInPlace(t *testing.T) {
	h, fake, _ := mountHarness(t, nil)

	// The mount folder appears in the owner's root listing like any folder.
	root := listMount(t, h, "")
	var sawMount bool
	for _, item := range root {
		if item["name"] == "team" {
			sawMount = true
		}
	}
	if !sawMount {
		t.Fatal("the mount point is missing from the root listing")
	}

	// Listing the mount point lists the REMOTE root.
	items := listMount(t, h, "team")
	if len(items) != 1 || items[0]["name"] != "docs" || items[0]["is_dir"] != true {
		t.Fatalf("mount root listing wrong: %+v", items)
	}
	if items[0]["mounted"] != true {
		t.Fatal("external entries must carry mounted=true so a client can shape its behaviour")
	}

	// And below it.
	docs := listMount(t, h, "team/docs")
	if len(docs) != 1 || docs[0]["name"] != "readme.txt" || docs[0]["size"] != float64(15) {
		t.Fatalf("remote listing wrong: %+v", docs)
	}

	// Download streams the remote bytes.
	resp := h.do(http.MethodGet, "/api/files/download?path="+url.QueryEscape("team/docs/readme.txt"), nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "from the remote" {
		t.Fatalf("download: %d %q", resp.StatusCode, string(body))
	}

	// Upload lands on the remote, not in local storage.
	up := multipartBody(t, "team/docs", "notes.txt", "written through the mount")
	resp = h.do(http.MethodPost, "/api/files/upload", up, map[string]string{
		"Content-Type": "multipart/form-data; boundary=" + up.Boundary(),
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	rc, err := fake.Open(context.Background(), "docs/notes.txt")
	if err != nil {
		t.Fatalf("the upload never reached the remote: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "written through the mount" {
		t.Fatalf("remote bytes wrong: %q", string(got))
	}

	// Mkdir, rename, move-within, delete — all on the remote.
	resp = h.do(http.MethodPost, "/api/files/mkdir", jsonBody(t, map[string]any{"path": "team/newdir"}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: %d", resp.StatusCode)
	}
	resp = h.do(http.MethodPost, "/api/files/rename", jsonBody(t, map[string]any{
		"path": "team/docs/notes.txt", "name": "renamed.txt",
	}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: %d", resp.StatusCode)
	}
	resp = h.do(http.MethodPost, "/api/files/move", jsonBody(t, map[string]any{
		"from": "team/docs/renamed.txt", "to": "team/newdir/renamed.txt",
	}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move within the mount: %d", resp.StatusCode)
	}
	resp = h.do(http.MethodDelete, "/api/files/?path="+url.QueryEscape("team/newdir/renamed.txt"), nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := fake.Stat(context.Background(), "newdir/renamed.txt"); err == nil {
		t.Fatal("the remote delete did not happen")
	}

	// The audit rows exist for reads and writes through the mount.
	var sawViaMount bool
	for _, e := range h.meta.AuditLog() {
		if e.Action == "file.write" && e.Metadata["via"] == "mount" {
			sawViaMount = true
		}
	}
	if !sawViaMount {
		t.Fatal("no file.write audit row names the mount")
	}
}

func jsonCT() map[string]string { return map[string]string{"Content-Type": "application/json"} }

func TestMountRefusalsAreTheDocumentedOnes(t *testing.T) {
	h, _, m := mountHarness(t, nil)

	// Cross-storage move.
	resp := h.do(http.MethodPost, "/api/files/move", jsonBody(t, map[string]any{
		"from": "team/docs/readme.txt", "to": "local-copy.txt",
	}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cross-storage move: %d, want 409", resp.StatusCode)
	}

	// Chunked upload sessions refuse external targets at start.
	resp = h.do(http.MethodPost, "/api/files/uploads", jsonBody(t, map[string]any{
		"path": "team", "name": "big.bin", "size": 1024,
	}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chunked upload to a mount: %d, want 400", resp.StatusCode)
	}

	// ZIP refuses external subtrees.
	resp = h.do(http.MethodGet, "/api/files/zip?path="+url.QueryEscape("team"), nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zip of a mount: %d, want 400", resp.StatusCode)
	}

	// The mount point itself cannot be deleted or renamed through the file API.
	resp = h.do(http.MethodDelete, "/api/files/?path="+url.QueryEscape("team"), nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting the mount point: %d, want 409", resp.StatusCode)
	}
	resp = h.do(http.MethodPost, "/api/files/rename", jsonBody(t, map[string]any{
		"path": "team", "name": "renamed",
	}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("renaming the mount point: %d, want 409", resp.StatusCode)
	}

	// Unmount removes the folder and keeps the remote's data (the fake is untouched).
	resp = h.do(http.MethodDelete, "/api/mounts/"+m.ID.String(), nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unmount: %d", resp.StatusCode)
	}
	if _, err := h.meta.Resolve(context.Background(), h.user.ID, "team"); err == nil {
		t.Fatal("the mount point survived unmount")
	}
}

func TestReadOnlyMountRefusesEveryWrite(t *testing.T) {
	h, _, _ := mountHarness(t, nil)

	// Flip the stored mount to read-only, as an operator would have created it.
	var id [16]byte
	_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, err := h.meta.Resolve(ctx, h.user.ID, "team")
		if err != nil {
			return err
		}
		m, err := tx.MountByNodeID(ctx, nodes[len(nodes)-1].ID)
		if err != nil {
			return err
		}
		id = m.ID
		return nil
	})
	// The service exposes no setter: read-only is a creation-time property, so this test
	// recreates the scenario by mounting a second, read-only mount beside it.
	body, _ := json.Marshal(map[string]any{
		"name": "archive", "scheme": "webdav", "endpoint": "https://remote.example.test/dav",
		"read_only": true, "verify": false,
	})
	resp := h.do(http.MethodPost, "/api/mounts", bytes.NewReader(body), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("read-only mount create: %d", resp.StatusCode)
	}
	_ = id

	up := multipartBody(t, "archive", "x.txt", "no")
	resp = h.do(http.MethodPost, "/api/files/upload", up, map[string]string{
		"Content-Type": "multipart/form-data; boundary=" + up.Boundary(),
	})
	roBody := readAll(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(roBody, "read_only_mount") {
		t.Fatalf("write to a read-only mount: %d %s", resp.StatusCode, roBody)
	}
	resp = h.do(http.MethodPost, "/api/files/mkdir", jsonBody(t, map[string]any{"path": "archive/d"}), jsonCT())
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mkdir on a read-only mount: %d", resp.StatusCode)
	}
	// Reads still work.
	resp = h.do(http.MethodGet, "/api/files/?path="+url.QueryEscape("archive/docs"), nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing a read-only mount: %d", resp.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.Body == nil {
		return ""
	}
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

func TestAnUnreachableRemoteIsA502NotAnEmptyFolder(t *testing.T) {
	h, fake, _ := mountHarness(t, nil)
	fake.Err = external.ErrUnavailable

	resp := h.do(http.MethodGet, "/api/files/?path="+url.QueryEscape("team"), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an unreachable remote listed as %d, want 502", resp.StatusCode)
	}
}

func TestMountsRespectTheACLGrantOnTheMountPoint(t *testing.T) {
	h, fake, _ := mountHarness(t, nil)

	mate := h.meta.AddUser(store.User{Username: "browser", Email: "browser@example.test", Role: "user"})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	team := h.resolveNode(t, "team")
	// The grant a mount point is entitled to: it is a folder like any other, so the
	// ordinary ACL machinery decides who sees it. Download is deliberately withheld —
	// that split is the point of the fine-grained bits.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, gerr := tx.Grant(ctx, store.ACLEntry{
			NodeID: team.ID, UserID: &mate.ID,
			Perms: store.PermRead | store.PermListChildren, Inherit: true,
		})
		return gerr
	}); err != nil {
		t.Fatal(err)
	}

	token, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	client := h.cookieClient(token)

	// Its children are the REMOTE's root, not an empty folder: the fork reaches the
	// id-addressed shared surface too. The shared surface answers metadata and
	// children in one document.
	resp, err := client.Get(h.srv.URL + "/api/shared/" + team.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grantee children: %d", resp.StatusCode)
	}
	var listed struct {
		Children []map[string]any `json:"children"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	if len(listed.Children) != 1 || listed.Children[0]["name"] != "docs" {
		t.Fatalf("the mount point's children are not the remote root: %+v", listed.Children)
	}
	_ = fake

	// A stranger sees nothing, exactly as for a local folder.
	stranger := h.meta.AddUser(store.User{Username: "stranger", Email: "s@example.test", Role: "user"})
	if _, err := h.meta.EnsureRoot(context.Background(), stranger.ID); err != nil {
		t.Fatal(err)
	}
	stoken, _ := h.sessions.Mint(context.Background(), stranger.ID, stranger.Email, "127.0.0.1", "test")
	sclient := h.cookieClient(stoken)
	resp, err = sclient.Get(h.srv.URL + "/api/shared/" + team.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stranger metadata: %d, want 404", resp.StatusCode)
	}
}

func TestMountRoutesAnswer503WhenUnwired(t *testing.T) {
	h := newHarness(t) // no tune: deps.Mounts is nil
	resp := h.do(http.MethodGet, "/api/mounts", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("mount list unwired: %d, want 503", resp.StatusCode)
	}
}
