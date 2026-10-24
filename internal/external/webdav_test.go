package external_test

// The WebDAV provider, exercised against this repository's own WebDAV handler.
//
// Serving a real RFC 4918 implementation on an httptest listener means every assertion
// runs against actual PROPFIND XML, actual status codes and actual href encoding — and
// the client being able to drive the server it lives next to is the cheapest
// interoperability proof available for both. The adapters below mirror the ones in
// internal/dav's tests for the same reason: the blob store speaks string owners and
// blob hashes, the metadata store speaks uuids and raw hashes.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/dav"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

type resolverAdapter struct{ r store.Resolver }

func (a *resolverAdapter) HashFor(ctx context.Context, owner, p string) (blob.Hash, bool, error) {
	h, ok, err := a.r.HashFor(ctx, owner, p)
	return blob.Hash(h), ok, err
}

func (a *resolverAdapter) Record(ctx context.Context, owner, p string, h blob.Hash, isDir bool) error {
	return a.r.Record(ctx, owner, p, [32]byte(h), isDir)
}

func (a *resolverAdapter) Forget(ctx context.Context, owner, p string) error {
	return a.r.Forget(ctx, owner, p)
}

func (a *resolverAdapter) IsDir(ctx context.Context, owner, p string) (bool, bool, error) {
	return a.r.IsDir(ctx, owner, p)
}

func (a *resolverAdapter) Children(ctx context.Context, owner, dir string, limit int) ([]layout.Node, error) {
	nodes, err := a.r.Children(ctx, owner, dir, limit)
	if err != nil {
		return nil, err
	}
	out := make([]layout.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, layout.Node{Owner: owner, Path: n.Path, IsDir: n.IsDir, Size: n.Size})
	}
	return out, nil
}

type authzAccessor struct{ m store.Store }

func (a *authzAccessor) LoadNode(id uuid.UUID) (authz.Node, error) {
	var n store.Node
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.Node(ctx, id)
		n = got
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authz.Node{}, authz.ErrNotFound
		}
		return authz.Node{}, err
	}
	return authz.Node{ID: n.ID, OwnerID: n.OwnerID, ParentID: n.ParentID}, nil
}

func (a *authzAccessor) OwnerGroupPerm(actorID, nodeID uuid.UUID) (authz.Perm, bool, error) {
	var (
		perms store.Perm
		ok    bool
	)
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		p, found, gerr := tx.EffectivePermsFor(ctx, actorID, nodeID)
		perms, ok = p, found
		return gerr
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return authz.Perm(perms), ok, nil
}

func (a *authzAccessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	return nil, nil
}

func webdavServer(t *testing.T) (*httptest.Server, string, string) {
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
	authorizer := authz.NewService(&authzAccessor{m: meta})
	appPass := auth.NewAppPasswords(meta)
	secret, _, err := appPass.Issue(context.Background(), user.ID, "mount-test", "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The listener is bound before the handler is built: MOVE validates its Destination
	// header against the configured public URL, so the config must name the real one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.PublicURL = "http://" + ln.Addr().String()
	h := dav.New(cfg, meta, content, authorizer, appPass)
	ts := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, user.Username, secret
}

func TestWebDAVProviderAgainstTheRealHandler(t *testing.T) {
	ts, username, secret := webdavServer(t)
	ctx := context.Background()

	storage, err := external.DialWebDAV(ctx, ts.URL+"/dav/"+username, "", username, secret)
	if err != nil {
		t.Fatal(err)
	}

	if err := storage.Ping(ctx); err != nil {
		t.Fatalf("ping against a live server with good credentials: %v", err)
	}

	// A directory, a file in it, a file with a name that needs escaping.
	if err := storage.Mkdir(ctx, "docs"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := storage.Create(ctx, "docs/hello.txt", strings.NewReader("hello from the provider")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := storage.Create(ctx, "docs/a b&c.txt", strings.NewReader("tricky name")); err != nil {
		t.Fatalf("create with a name that must be escaped: %v", err)
	}

	entries, err := storage.List(ctx, "docs")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	// Names must come back decoded, not percent-encoded.
	if entries[0].Name != "a b&c.txt" || entries[1].Name != "hello.txt" {
		t.Fatalf("entry names wrong: %+v", entries)
	}
	if entries[1].Size != int64(len("hello from the provider")) {
		t.Fatalf("size not reported: %+v", entries[1])
	}

	st, err := storage.Stat(ctx, "docs/hello.txt")
	if err != nil || st.IsDir {
		t.Fatalf("stat file: %+v %v", st, err)
	}
	st, err = storage.Stat(ctx, "docs")
	if err != nil || !st.IsDir {
		t.Fatalf("stat dir: %+v %v", st, err)
	}
	if _, err := storage.Stat(ctx, "nope.txt"); err == nil {
		t.Fatal("a missing file must stat as not found")
	}

	rc, err := storage.Open(ctx, "docs/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello from the provider" {
		t.Fatalf("read back %q", string(body))
	}

	if err := storage.Rename(ctx, "docs/hello.txt", "docs/renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := storage.Stat(ctx, "docs/hello.txt"); err == nil {
		t.Fatal("the old name still exists after MOVE")
	}

	if err := storage.Remove(ctx, "docs/a b&c.txt", true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	entries, err = storage.List(ctx, "docs")
	if err != nil || len(entries) != 1 || entries[0].Name != "renamed.txt" {
		t.Fatalf("after remove: %+v %v", entries, err)
	}
}

func TestWebDAVProviderMapsBadCredentials(t *testing.T) {
	ts, username, _ := webdavServer(t)
	storage, err := external.DialWebDAV(context.Background(), ts.URL+"/dav/"+username, "", username, "wrong-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Ping(context.Background()); err == nil {
		t.Fatal("bad credentials must surface as an error, not an empty tree")
	}
}

func TestWebDAVProviderRefusesUnreachable(t *testing.T) {
	storage, err := external.DialWebDAV(context.Background(), "http://127.0.0.1:1/nothing", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Ping(context.Background()); err == nil {
		t.Fatal("a dead endpoint must ping as an error")
	}
}
