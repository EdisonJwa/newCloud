package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/auth"
	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// harness wires the whole HTTP surface over in-memory storage, so the tests
// exercise the same code path production does without needing a database, an IdP
// or a network.
type harness struct {
	t       *testing.T
	srv     *httptest.Server
	meta    *store.Memory
	user    store.User
	session string
	// mailer records verification codes so tests can complete the email flow without a
	// real transport. It never sends anything.
	mailer *recordingMailer
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Limits.MaxProfindChildren = 100
	cfg.Limits.MaxFileBytes = 1 << 20

	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}

	blobs, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content, err := blobstore.New(blobs, &adapter{r: meta.Resolver()})
	if err != nil {
		t.Fatal(err)
	}

	secret := bytes.Repeat([]byte("k"), 32)
	tickets, err := ticket.NewIssuer(secret)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessions(tickets, meta, time.Hour)

	session, err := sessions.Mint(context.Background(), user.ID, user.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}

	shares := share.New(meta, share.TestArgon())
	shares.SetTicketIssuer(tickets)
	mailer := &recordingMailer{}

	s, err := server.New(server.Deps{
		Config: cfg, Meta: meta, Content: content,
		Authz:   authz.NewService(&accessor{m: meta}),
		Tickets: tickets, Sessions: sessions,
		AppPass: auth.NewAppPasswords(meta),
		Shares:  shares,
		Mailer:  mailer,
		Log:     testLogger(t),
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	return &harness{t: t, srv: ts, meta: meta, user: user, session: session, mailer: mailer}
}

// do issues an authenticated request.
func (h *harness) do(method, path string, body io.Reader, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// decode reads a JSON body into a generic map.
func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func TestHealthEndpointsArePublic(t *testing.T) {
	h := newHarness(t)

	resp, err := http.Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", resp.StatusCode)
	}

	resp2, err := http.Get(h.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body := decode(t, resp2)
	if body["layout"] != "blob" {
		t.Fatalf("readyz should report the active layout, got %v", body["layout"])
	}
}

// TestAPIRequiresSession is the boundary that must never regress.
func TestAPIRequiresSession(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/files/?path=/"},
		{http.MethodGet, "/api/shares/"},
		{http.MethodGet, "/api/apppasswords/"},
		{http.MethodGet, "/api/auth/me"},
	} {
		req, _ := http.NewRequest(tc.method, h.srv.URL+tc.path, nil)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s: want 401 without a session, got %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestMeReturnsAccount(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/api/auth/me", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["username"] != "alice" {
		t.Fatalf("username: %v", body["username"])
	}
}

func TestMkdirListUploadDownload(t *testing.T) {
	h := newHarness(t)

	// Create a directory.
	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"docs"}`), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: want 201, got %d: %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	// Upload a file into it.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "docs")
	fw, _ := mw.CreateFormFile("file", "report.txt")
	_, _ = io.WriteString(fw, "quarterly numbers")
	_ = mw.Close()

	resp = h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: want 201, got %d: %s", resp.StatusCode, readBody(resp))
	}
	uploaded := decode(t, resp)
	if uploaded["name"] != "report.txt" {
		t.Fatalf("uploaded name: %v", uploaded["name"])
	}

	// List the directory.
	resp = h.do(http.MethodGet, "/api/files/?path=docs", nil, nil)
	list := decode(t, resp)
	children, _ := list["children"].([]any)
	if len(children) != 1 {
		t.Fatalf("want 1 child, got %d", len(children))
	}

	// Download it back.
	resp = h.do(http.MethodGet, "/api/files/download?path=docs/report.txt", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: want 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "quarterly numbers" {
		t.Fatalf("content: %q", got)
	}
	// A download must never be rendered as a document.
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download must be an attachment")
	}
}

func TestDeleteAndRestore(t *testing.T) {
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
	upload("a.txt", "hello")

	resp := h.do(http.MethodDelete, "/api/files/?path=a.txt", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do(http.MethodGet, "/api/files/download?path=a.txt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted file should be 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestShareRoundTrip is the headline user journey: create a link, then fetch the
// file through it without any session.
func TestShareRoundTrip(t *testing.T) {
	h := newHarness(t)

	// Upload a file.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "shared.txt")
	_, _ = io.WriteString(fw, "shared content")
	_ = mw.Close()
	resp := h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()})
	resp.Body.Close()

	// Create a share for it.
	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"shared.txt","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create share: want 201, got %d: %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)
	if token == "" {
		t.Fatal("share creation must return the token once")
	}

	// Open the share page anonymously.
	page, err := http.Get(h.srv.URL + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	if page.StatusCode != http.StatusOK {
		t.Fatalf("share page: want 200, got %d", page.StatusCode)
	}
	page.Body.Close()

	// Download through the share.
	dl, err := http.Get(h.srv.URL + "/s/" + token + "/download")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("share download: want 200, got %d", dl.StatusCode)
	}
	got, _ := io.ReadAll(dl.Body)
	if string(got) != "shared content" {
		t.Fatalf("shared content: %q", got)
	}
}

// TestShareDownloadLimit: "usable three times" must mean exactly three.
func TestShareDownloadLimit(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "limited.txt")
	_, _ = io.WriteString(fw, "x")
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"limited.txt","kind":"link","max_downloads":2}`),
		map[string]string{"Content-Type": "application/json"})
	created := decode(t, resp)
	token, _ := created["token"].(string)

	for i := 0; i < 2; i++ {
		r, err := http.Get(h.srv.URL + "/s/" + token + "/download")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("download %d: want 200, got %d", i+1, r.StatusCode)
		}
	}

	// The third must be refused, and the refusal must not look different from an
	// unknown token.
	r, err := http.Get(h.srv.URL + "/s/" + token + "/download")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("exhausted share: want 404, got %d", r.StatusCode)
	}
}

func TestSharePasswordGate(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "secret.txt")
	_, _ = io.WriteString(fw, "classified")
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"secret.txt","kind":"link","password":"hunter2"}`),
		map[string]string{"Content-Type": "application/json"})
	created := decode(t, resp)
	token, _ := created["token"].(string)

	// Page says a password is required.
	page, _ := http.Get(h.srv.URL + "/s/" + token)
	info := decode(t, page)
	if info["requires_password"] != true {
		t.Fatal("share page should ask for a password")
	}

	// Download without verifying must fail.
	r, _ := http.Get(h.srv.URL + "/s/" + token + "/download")
	r.Body.Close()
	if r.StatusCode == http.StatusOK {
		t.Fatal("download must be refused before the password is verified")
	}

	// Wrong password.
	vr, _ := http.Post(h.srv.URL+"/s/"+token+"/verify", "application/json",
		strings.NewReader(`{"password":"wrong"}`))
	vr.Body.Close()
	if vr.StatusCode == http.StatusOK {
		t.Fatal("wrong password must not verify")
	}
}

// TestShareTokenIsNotExposedByList: listing shares must not re-expose tokens.
func TestShareTokenIsNotExposedByList(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "f.txt")
	_, _ = io.WriteString(fw, "x")
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"f.txt","kind":"link"}`),
		map[string]string{"Content-Type": "application/json"})
	created := decode(t, resp)
	token, _ := created["token"].(string)

	listResp := h.do(http.MethodGet, "/api/shares/", nil, nil)
	raw := readBody(listResp)

	if strings.Contains(raw, token) {
		t.Fatal("listing shares must not leak the plaintext token")
	}
}

func TestRequestShareIsUploadOnly(t *testing.T) {
	h := newHarness(t)

	// Create a drop-box directory.
	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"inbox"}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"inbox","kind":"request"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create request share: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)
	sh, _ := created["share"].(map[string]any)
	if sh["hide_contents"] != true {
		t.Fatal("a request share must hide its contents")
	}

	// The page must not list files.
	page, _ := http.Get(h.srv.URL + "/s/" + token)
	info := decode(t, page)
	if info["hide_contents"] != true {
		t.Fatal("request share page must not reveal contents")
	}
	if _, hasChildren := info["children"]; hasChildren {
		t.Fatal("request share page must not include a file list")
	}

	// Upload into it anonymously.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "delivered.pdf")
	_, _ = io.WriteString(fw, "pdf bytes")
	_ = mw.Close()

	up, err := http.Post(h.srv.URL+"/s/"+token+"/upload", mw.FormDataContentType(), body)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Body.Close()
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("anonymous upload: want 201, got %d: %s", up.StatusCode, readBody(up))
	}
}

func TestUnknownShareTokenIsNotFound(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"bogus", strings.Repeat("A", 22)} {
		r, err := http.Get(h.srv.URL + "/s/" + tok)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("%q: want 404, got %d", tok, r.StatusCode)
		}
	}
}

func TestAppPasswordLifecycle(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/apppasswords/",
		strings.NewReader(`{"label":"laptop","scope":"rw"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create credential: want 201, got %d: %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	secret, _ := created["secret"].(string)
	if secret == "" {
		t.Fatal("the secret must be returned once")
	}
	if !strings.HasPrefix(secret, "wc_") {
		t.Fatalf("secret should carry the recognisable prefix: %q", secret)
	}

	// It must not be listed afterwards.
	listResp := h.do(http.MethodGet, "/api/apppasswords/", nil, nil)
	raw := readBody(listResp)
	if strings.Contains(raw, secret) {
		t.Fatal("the secret must not be recoverable from the list")
	}

	// Revoking it stops WebDAV authentication.
	id, _ := created["id"].(string)
	rev := h.do(http.MethodDelete, "/api/apppasswords/"+id, nil, nil)
	if rev.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: want 204, got %d", rev.StatusCode)
	}
	rev.Body.Close()
}

// TestWebDAVRequiresAppPassword checks the two credential paths stay separate: a
// session cookie is not accepted on /dav.
func TestWebDAVRequiresAppPassword(t *testing.T) {
	h := newHarness(t)

	req, _ := http.NewRequest("PROPFIND", h.srv.URL+"/dav/alice/", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: h.session})
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session cookie must not authenticate WebDAV, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatal("WebDAV must advertise Basic auth")
	}
}

func TestUnknownJSONFieldRejected(t *testing.T) {
	h := newHarness(t)
	// A typo in a permission field must be an error, not a silently weaker share.
	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"x","kind":"link","permisions":["read"]}`),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown field, got %d", resp.StatusCode)
	}
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// ---- adapters ----

type adapter struct{ r store.Resolver }

func (a *adapter) HashFor(ctx context.Context, owner, p string) (blob.Hash, bool, error) {
	h, ok, err := a.r.HashFor(ctx, owner, p)
	return blob.Hash(h), ok, err
}
func (a *adapter) Record(ctx context.Context, owner, p string, h blob.Hash, isDir bool) error {
	return a.r.Record(ctx, owner, p, [32]byte(h), isDir)
}
func (a *adapter) Forget(ctx context.Context, owner, p string) error {
	return a.r.Forget(ctx, owner, p)
}
func (a *adapter) IsDir(ctx context.Context, owner, p string) (bool, bool, error) {
	return a.r.IsDir(ctx, owner, p)
}
func (a *adapter) Children(ctx context.Context, owner, dir string, limit int) ([]layout.Node, error) {
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

type accessor struct{ m *store.Memory }

func (a *accessor) LoadNode(id uuid.UUID) (authz.Node, error) {
	var n store.Node
	err := a.m.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, gerr := tx.Node(ctx, id)
		n = got
		return gerr
	})
	if err != nil {
		return authz.Node{}, authz.ErrNotFound
	}
	return authz.Node{ID: n.ID, OwnerID: n.OwnerID, ParentID: n.ParentID}, nil
}
func (a *accessor) OwnerGroupPerm(actorID, nodeID uuid.UUID) (authz.Perm, bool, error) {
	return 0, false, nil
}
func (a *accessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	return nil, nil
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
