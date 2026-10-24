package dav

// The external WebDAV branch, driven over HTTP against a fake provider behind a real
// mount service. What is pinned: a mount folder PROPFINDs from the remote, GET streams
// remote bytes, PUT/MKCOL/DELETE/MOVE land on the remote, LOCK/COPY below a mount are
// refused with the documented statuses, and the local surface is untouched.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/mount"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func externalHarness(t *testing.T) (*httptest.Server, string, string, *external.Fake) {
	t.Helper()
	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}
	blobs, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content, err := blobstore.New(blobs, &resolverAdapter{r: meta.Resolver()})
	if err != nil {
		t.Fatal(err)
	}
	appPass := auth.NewAppPasswords(meta)
	secret, _, err := appPass.Issue(context.Background(), user.ID, "mount-test", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}

	fake := external.NewFake()
	fake.AddDir("docs")
	fake.AddFile("docs/readme.txt", []byte("remote bytes"), time.Now())

	cfg := config.MountsConfig{
		Enabled: true, SecretFile: "test", TimeoutSec: 5, MaxListEntries: 100,
		AllowedSchemes: []string{"webdav"}, AllowUserMounts: true,
	}
	cipher, err := mount.NewCipher(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := mount.New(meta, cfg, cipher, nil)
	svc.RegisterDial("webdav", func(ctx context.Context, endpoint, root, u, p string) (external.Storage, error) {
		return fake, nil
	})
	if _, err := svc.Create(context.Background(), user, mount.CreateInput{
		Name: "team", Scheme: "webdav", Endpoint: "https://remote.example.test/dav", Verify: false,
	}); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	full := config.Default()
	full.Server.PublicURL = "http://" + ln.Addr().String()
	h := New(full, meta, content, authz.NewService(&authzAccessor{m: meta}), appPass).WithMounts(svc)
	ts := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, "alice", secret, fake
}

func davDo(t *testing.T, ts *httptest.Server, user, secret, method, urlPath string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+urlPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, secret)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestDAVBrowsesAndEditsAMountInPlace(t *testing.T) {
	ts, user, secret, fake := externalHarness(t)

	// PROPFIND the mount root: the remote's docs/ folder is right there.
	resp := davDo(t, ts, user, secret, "PROPFIND", "/dav/alice/team/", nil, map[string]string{"Depth": "1"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("propfind mount root: %d %s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "docs") || !strings.Contains(string(body), "<D:collection/>") {
		t.Fatalf("mount root listing missing the remote directory: %s", string(body))
	}

	// PROPFIND below it, then GET the remote file.
	resp = davDo(t, ts, user, secret, "PROPFIND", "/dav/alice/team/docs/readme.txt", nil, map[string]string{"Depth": "0"})
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("propfind remote file: %d", resp.StatusCode)
	}
	resp = davDo(t, ts, user, secret, http.MethodGet, "/dav/alice/team/docs/readme.txt", nil, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "remote bytes" {
		t.Fatalf("get remote file: %d %q", resp.StatusCode, string(body))
	}

	// PUT lands on the remote.
	resp = davDo(t, ts, user, secret, http.MethodPut, "/dav/alice/team/docs/written.txt",
		strings.NewReader("dav wrote this"), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	rc, err := fake.Open(context.Background(), "docs/written.txt")
	if err != nil {
		t.Fatalf("the PUT never reached the remote: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "dav wrote this" {
		t.Fatalf("remote bytes wrong: %q", string(got))
	}

	// MKCOL, MOVE within the mount, DELETE.
	resp = davDo(t, ts, user, secret, "MKCOL", "/dav/alice/team/newdir", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkcol: %d", resp.StatusCode)
	}
	resp = davDo(t, ts, user, secret, "MOVE", "/dav/alice/team/docs/written.txt", nil,
		map[string]string{"Destination": ts.URL + "/dav/alice/team/newdir/written.txt"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("move within the mount: %d", resp.StatusCode)
	}
	resp = davDo(t, ts, user, secret, http.MethodDelete, "/dav/alice/team/newdir/written.txt", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := fake.Stat(context.Background(), "newdir/written.txt"); err == nil {
		t.Fatal("the remote delete did not happen")
	}
}

func TestDAVRefusesTheDocumentedOperationsBelowAMount(t *testing.T) {
	ts, user, secret, _ := externalHarness(t)

	// LOCK below a mount: refused, not faked.
	resp := davDo(t, ts, user, secret, "LOCK", "/dav/alice/team/docs/readme.txt",
		strings.NewReader(`<?xml version="1.0"?><D:lockinfo><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`),
		nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lock below a mount: %d, want 403", resp.StatusCode)
	}

	// COPY out of a mount: a copy wearing COPY's name is still a copy.
	resp = davDo(t, ts, user, secret, "COPY", "/dav/alice/team/docs/readme.txt", nil,
		map[string]string{"Destination": ts.URL + "/dav/alice/local-copy.txt"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("copy out of a mount: %d, want 403", resp.StatusCode)
	}

	// MOVE across the boundary: refused as cross-storage.
	resp = davDo(t, ts, user, secret, "MOVE", "/dav/alice/team/docs/readme.txt", nil,
		map[string]string{"Destination": ts.URL + "/dav/alice/local-copy.txt"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cross-storage move: %d, want 409", resp.StatusCode)
	}

	// DELETE of the mount point itself: not a file operation.
	resp = davDo(t, ts, user, secret, http.MethodDelete, "/dav/alice/team", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("deleting the mount point: %d, want 403", resp.StatusCode)
	}

	// The local surface is untouched: a plain folder still answers PROPFIND normally.
	resp = davDo(t, ts, user, secret, "PROPFIND", "/dav/alice/", nil, map[string]string{"Depth": "1"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(string(body), "team") {
		t.Fatalf("local root listing changed: %d %s", resp.StatusCode, string(body))
	}
}

func TestDAVAnswers502WhenTheRemoteIsDead(t *testing.T) {
	ts, user, secret, fake := externalHarness(t)
	fake.Err = external.ErrUnavailable

	resp := davDo(t, ts, user, secret, "PROPFIND", "/dav/alice/team/", nil, map[string]string{"Depth": "1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("dead remote propfind: %d, want 502", resp.StatusCode)
	}
}
