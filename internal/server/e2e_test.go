package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/EdisonJwa/newCloud/internal/metrics"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/share"
	"github.com/EdisonJwa/newCloud/internal/storage/blobstore"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
	"github.com/EdisonJwa/newCloud/internal/upload"
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
	// sessions mints a session for a second account, which cross-account tests need:
	// acting as somebody else is the only way to test that they are *not* allowed to
	// do something.
	sessions *auth.Sessions
	// uploads is the resumable-upload manager the server uses. Retained so a test can start
	// a session as one account and then try to touch it as another: the Manager is
	// in-memory, so the store cannot fabricate a session for the HTTP-layer check.
	uploads *upload.Manager
	// cfg is the configuration the server was built with. Tests that exercise a
	// configurable feature mutate it before rebuilding the server.
	cfg *config.Config
	// authz is the same authorizer the server uses, so a test can ask the question
	// the handlers ask rather than approximating it.
	authz *authz.Service
	// metrics is the registry the server was given, or nil when the harness deliberately has
	// none. Most tests run without it: /metrics is a deployment-time surface, and the branch
	// where it is absent is itself worth testing.
	metrics *metrics.Registry
	// blobRoot is the local content-addressed root, read by tests that reason about what a backup
	// copy of the blob tree would contain.
	blobRoot string
}

// rootNode returns the harness account's root directory.
func (h *harness) rootNode(t *testing.T) store.Node {
	t.Helper()
	var root store.Node
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.EnsureRoot(ctx, h.user.ID)
		root = got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

// blobRootOf returns the local blob root the server writes to.
func (h *harness) blobRootOf(t *testing.T) string {
	t.Helper()
	if h.blobRoot == "" {
		t.Fatal("the harness has no blob root; this test needs the local backend")
	}
	return h.blobRoot
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessTuned(t, nil, nil)
}

// newHarnessWith mutates the configuration before the server is built.
func newHarnessWith(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	return newHarnessTuned(t, mutate, nil)
}

// newHarnessTuned is the general form: it can change the config and the deps the server is
// built with. Reaching the deps matters for surfaces that are optional, because "optional" and
// "unreachable" look identical from a test that cannot turn them on.
// newHarnessWith builds the harness with a config override applied *before* the server
// is constructed.
//
// The order matters: a test that mutated the returned cfg would be mutating a copy the
// server already captured, so the feature under test would silently stay disabled and
// the test would prove nothing (or, worse, pass for the wrong reason).
func newHarnessTuned(t *testing.T, mutate func(*config.Config), tune func(*server.Deps)) *harness {
	t.Helper()

	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Limits.MaxProfindChildren = 100
	cfg.Limits.MaxFileBytes = 1 << 20
	if mutate != nil {
		mutate(&cfg)
	}

	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "user"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}

	// The blob root is retained: a backup copies this directory, so a test about backup ordering
	// needs to see exactly what the copy would contain.
	blobRoot := t.TempDir()
	blobs, err := local.New(blobRoot)
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

	// Resumable uploads stage in the test's own directory so a failing test cannot leave
	// files behind and two tests cannot see each other's sessions.
	uploads, err := upload.NewManager(t.TempDir(), time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(uploads.Close)

	authorizer := authz.NewService(&accessor{m: meta})
	deps := server.Deps{
		Config: cfg, Meta: meta, Content: content,
		Authz:   authorizer,
		Tickets: tickets, Sessions: sessions,
		AppPass: auth.NewAppPasswords(meta),
		Shares:  shares,
		Mailer:  mailer,
		Uploads: uploads,
		Log:     testLogger(t),
		Version: "test",
	}
	// The hook runs after every field is set, so a test can add an optional dependency
	// (metrics, say) without this constructor having to know about it.
	if tune != nil {
		tune(&deps)
	}

	s, err := server.New(deps)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	h := &harness{t: t, srv: ts, meta: meta, user: user, session: session,
		mailer: mailer, sessions: sessions, cfg: &cfg, authz: authorizer, uploads: uploads}
	h.metrics = deps.Metrics
	h.blobRoot = blobRoot
	return h
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

	// The payload is exactly what the bundled frontend sends, hide_contents included.
	//
	// That field is redundant — the service forces it on for request shares — but sending
	// it is what the client does, and the API rejects unknown fields. Omitting it from the
	// request struct therefore broke "create a drop box" from the UI with a 400 while this
	// test passed, because the test only ever checked the response.
	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"inbox","kind":"request","hide_contents":true}`),
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
	// Resolve real grants. This used to be a stub returning "no grant", which meant
	// the whole HTTP surface was tested with ACLs disabled: a handler that forgot to
	// honour a grant, or a bug in the grant-to-authorizer wiring, could not fail a
	// test. The fake accessor exists to avoid a database, not to avoid the model.
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
func (a *accessor) ShareForActor(actorID, nodeID uuid.UUID) (*authz.ShareGrant, error) {
	return nil, nil
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestReadinessReflectsDependencyHealth is what makes /readyz worth having.
//
// A probe that returns 200 unconditionally tells an orchestrator nothing, and the one
// failure it must catch — "the process is up but cannot reach its data" — is exactly
// the one a static body hides.
func TestReadinessReflectsDependencyHealth(t *testing.T) {
	h := newHarness(t)

	resp, err := http.Get(h.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a healthy instance must report ready, got %d (%v)", resp.StatusCode, body)
	}
	checks, _ := body["checks"].(map[string]any)
	if checks == nil {
		t.Fatal("readyz must report which dependencies it checked")
	}
	if checks["metadata"] != "ok" {
		t.Fatalf("metadata check should pass, got %v", checks["metadata"])
	}

	// Break the metadata store under the running server and confirm the probe
	// notices. This is the failure the probe exists for.
	h.meta.BreakForTest()
	defer h.meta.UnbreakForTest()

	resp2, err := http.Get(h.srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable dependency must fail the probe, got %d", resp2.StatusCode)
	}
}

// mkdir creates a directory in the harness user's tree, failing the test on refusal.
func (h *harness) mkdir(t *testing.T, path string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	resp := h.do(http.MethodPost, "/api/files/mkdir", bytes.NewReader(body), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir %q: want 201, got %d", path, resp.StatusCode)
	}
}

// TestShareRangeRequestsDoNotConsumeDownloads is requirement line 217:
//
//	分享链接的"最大下载次数"不能简单地按 HTTP 请求数计费，因为一次媒体播放或断点续传
//	可能产生多个 Range 请求。
//
// A media player seeking through a shared video issues one Range request per seek, and a
// resumed download is another request for the same bytes. Charging each one consumed a
// limited share almost immediately — "usable 3 times" became "usable for one playback".
//
// This is the behaviour the download-session table exists for, and no server-level test
// covered it: the existing limit test only issues plain GETs, where request count and
// download count happen to coincide.
func TestShareRangeRequestsDoNotConsumeDownloads(t *testing.T) {
	h := newHarness(t)

	// Big enough to have interesting ranges.
	content := strings.Repeat("abcdefgh", 4096) // 32 KiB
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "video.bin")
	_, _ = io.WriteString(fw, content)
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	// Exactly one download allowed: any request that is charged a second time fails.
	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"video.bin","kind":"link","max_downloads":1}`),
		map[string]string{"Content-Type": "application/json"})
	created := decode(t, resp)
	token, _ := created["token"].(string)
	url := h.srv.URL + "/s/" + token + "/download"

	// The first request pays for the download. A Range on the very first request is also
	// a first download — the session does not exist yet, so it must be charged, and that
	// is what makes the line below meaningful rather than trivially true.
	first, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first download: want 200, got %d", first.StatusCode)
	}

	// Every subsequent seek and resume rides the session and must not cost anything.
	ranges := []string{"bytes=0-1023", "bytes=8192-16383", "bytes=16384-", "bytes=0-"}
	for _, rng := range ranges {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", rng)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusPartialContent && r.StatusCode != http.StatusOK {
			t.Fatalf("range %q: want 206 or 200, got %d. A Range request consumed the "+
				"share's single download, which is the over-counting the download-session "+
				"mechanism exists to prevent — a video seek would exhaust a limited share.", rng, r.StatusCode)
		}
	}

	// And the limit is still real: a genuine second full download is refused.
	second, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("a second FULL download of an exhausted share: want 404, got %d. Range "+
			"requests must be free, but a fresh download must still be charged.", second.StatusCode)
	}
}

// TestRangeReuseIsPerVisitor is the other half of the previous test, and the reason the
// session is keyed by visitor grant rather than by share alone.
//
// Making Range requests free must not make an exhausted share downloadable by everyone:
// a continuation rides a session that a *specific* visitor paid for. A different visitor
// has no session, so the same Range request is a first download and must be charged —
// and refused once the cap is spent.
//
// Without this, the fix for the previous test would be indistinguishable from "ignore the
// download limit when the request has a Range header", which is much worse than the bug.
func TestRangeReuseIsPerVisitor(t *testing.T) {
	h := newHarness(t)

	content := strings.Repeat("0123456789", 4096) // 40 KiB
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "shared.bin")
	_, _ = io.WriteString(fw, content)
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"shared.bin","kind":"link","max_downloads":1}`),
		map[string]string{"Content-Type": "application/json"})
	created := decode(t, resp)
	token, _ := created["token"].(string)
	url := h.srv.URL + "/s/" + token + "/download"

	// Visitor A pays for the download with a plain GET.
	first, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first download: want 200, got %d", first.StatusCode)
	}

	// Visitor B is a different client: different User-Agent, which is part of the
	// anonymous grant. Their Range request is a FIRST download, not a continuation.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("User-Agent", "a-different-visitor/1.0")
	other, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	other.Body.Close()

	if other.StatusCode == http.StatusPartialContent || other.StatusCode == http.StatusOK {
		t.Fatalf("a different visitor downloaded from an exhausted share with range status %d. "+
			"Range requests are free only for the visitor whose session paid for them; otherwise "+
			"the download limit can be bypassed by adding a Range header.", other.StatusCode)
	}
}

// TestRequestShareEnforcesTotalSpace is SHARE-05's 总空间 limit.
//
// The requirement asks for five limits on an upload link: file count, per-file size, total
// space, rate, and retention. The first two existed; the total did not, so a drop box with
// max_uploads=100 and max_bytes_per_file=10 MiB accepted a gigabyte — the count and the
// per-file cap were both satisfied while the number the owner actually cares about was
// unbounded.
//
// This test fills a small total cap one file at a time and requires the delivery that would
// cross it to be refused.
func TestRequestShareEnforcesTotalSpace(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"inbox"}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	// A 100-byte total, in a link that allows plenty of files.
	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"inbox","kind":"request","hide_contents":true,`+
			`"max_uploads":50,"max_total_bytes":100}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create request share: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)

	deliver := func(name string, size int) int {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = io.WriteString(fw, strings.Repeat("x", size))
		_ = mw.Close()
		up, err := http.Post(h.srv.URL+"/s/"+token+"/upload", mw.FormDataContentType(), body)
		if err != nil {
			t.Fatal(err)
		}
		defer up.Body.Close()
		return up.StatusCode
	}

	// 60 bytes fits under the 100-byte cap.
	if code := deliver("first.bin", 60); code != http.StatusCreated {
		t.Fatalf("first delivery (60 of 100 bytes): want 201, got %d", code)
	}

	// Another 60 crosses it. The count is still well under max_uploads, so only the byte
	// total can refuse this — which is exactly the limit that did not exist.
	if code := deliver("second.bin", 60); code == http.StatusCreated {
		t.Fatal("a delivery that exceeds the link's total-space cap was accepted. With " +
			"max_uploads=50 a drop box would accept 50 files of any permitted size; the " +
			"total is the only limit that bounds the space an anonymous visitor can occupy.")
	}

	// And 40 more still fits: the cap is a sum, not a per-request threshold.
	if code := deliver("third.bin", 40); code != http.StatusCreated {
		t.Fatalf("a delivery that exactly reaches the cap: want 201, got %d", code)
	}
}

// TestRequestShareStampsRetention is SHARE-05's 保留期限 limit.
//
// retention_seconds was stored, returned by the API, and enforced nowhere: a column with no
// behaviour, which reads as a working feature from every angle an operator can see. This is the
// same defect class as a background job that is declared and never invoked.
//
// The stamp has to be taken at delivery time rather than derived from the share later, because the
// policy in force when the bytes arrived is the one the owner agreed to.
func TestRequestShareStampsRetention(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"inbox"}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"inbox","kind":"request","hide_contents":true,`+
			`"retention_seconds":3600}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create request share: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "delivered.bin")
	_, _ = io.WriteString(fw, "bytes")
	_ = mw.Close()
	up, err := http.Post(h.srv.URL+"/s/"+token+"/upload", mw.FormDataContentType(), body)
	if err != nil {
		t.Fatal(err)
	}
	up.Body.Close()
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("delivery: want 201, got %d", up.StatusCode)
	}

	// The delivered node must carry the retention. Reading it back through the API is the
	// only check that proves the value reached the row rather than living in the response.
	listing := h.do(http.MethodGet, "/api/files/?path=inbox", nil, nil)
	raw := decode(t, listing)
	files, _ := raw["children"].([]any)
	if len(files) != 1 {
		t.Fatalf("want 1 delivered file, got %d", len(files))
	}
	entry, _ := files[0].(map[string]any)
	if entry["retain_until"] == nil {
		t.Fatal("the delivered file has no retain_until, so the link's retention_seconds did " +
			"nothing. A limit that is stored and never enforced is worse than an absent one: the " +
			"API reports it as set.")
	}
}

// TestRetentionSweepTrashesExpiredDeliveries covers the sweep, not just the stamp.
//
// The stamp is only half the feature: without something that acts on it, retain_until is a
// timestamp nobody reads. The sweep moves a lapsed delivery into the TRASH rather than deleting it,
// because a retention setting is a policy about how long deliveries are kept — not an instruction
// to destroy them irrecoverably. A misconfigured retention_seconds must still be recoverable.
func TestRetentionSweepTrashesExpiredDeliveries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"inbox"}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	// A delivery that is already due: a one-second retention, then a sweep with the clock
	// effectively in the future is not required — the store compares against now(), and the
	// test creates the node with a past deadline directly so it does not have to sleep.
	// Resolve the directory through the same path the API used, so this fails loudly if the
	// name is wrong rather than silently creating nodes somewhere unexpected.
	rootNodes, err := h.meta.Resolve(ctx, h.user.ID, "inbox")
	if err != nil {
		t.Fatalf("resolve inbox: %v", err)
	}
	inbox := rootNodes[len(rootNodes)-1]

	past := time.Now().Add(-time.Hour)
	var expiredID, liveID uuid.UUID
	err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		e, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: h.user.ID, ParentID: inbox.ID, Name: "expired.bin",
			Size: 1, Mime: "application/octet-stream", ETag: `"e1"`,
			BlobID: h.meta.BlobIDFor(ctx, [32]byte{1}, 1), BlobHash: [32]byte{1},
			RetainUntil: &past,
		})
		if cerr != nil {
			return cerr
		}
		expiredID = e.ID

		future := time.Now().Add(24 * time.Hour)
		l, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: h.user.ID, ParentID: inbox.ID, Name: "kept.bin",
			Size: 1, Mime: "application/octet-stream", ETag: `"e2"`,
			BlobID: h.meta.BlobIDFor(ctx, [32]byte{2}, 1), BlobHash: [32]byte{2},
			RetainUntil: &future,
		})
		if cerr != nil {
			return cerr
		}
		liveID = l.ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var moved int
	err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, eerr := tx.ExpireRetained(ctx)
		moved = n
		return eerr
	})
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if moved != 1 {
		t.Fatalf("swept %d nodes, want 1 (the one past its retain_until)", moved)
	}

	// The expired one is in the trash, not gone: recoverable by its owner.
	var expired, kept store.Node
	err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var e1, e2 error
		// Node() deliberately hides trashed rows, so the swept node has to be read through
		// NodeIncludingTrashed — which is also the function the restore flow uses, so this
		// asserts the recovery path exists rather than merely that a flag flipped.
		expired, e1 = tx.NodeIncludingTrashed(ctx, expiredID)
		kept, e2 = tx.NodeIncludingTrashed(ctx, liveID)
		return errors.Join(e1, e2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.TrashedAt == nil {
		t.Fatal("a delivery past its retain_until was not moved to the trash, so the retention " +
			"policy is stored and never applied")
	}
	if kept.TrashedAt != nil {
		t.Fatal("a delivery still inside its retention window was trashed")
	}
}

// TestRequestShareEnforcesUploadRate is SHARE-05's 速率 limit, the fifth and last.
//
// A rate is different in kind from the other four limits. Those are totals, and a column with a
// guard in the same UPDATE keeps them exact. A rate is a rolling window: no column can hold "how
// many arrived in the last N minutes", because the answer changes as time passes with no write
// happening. So the events are recorded and counted.
//
// This test also covers the distinction the implementation has to get right: hitting the rate is
// NOT the same as exhausting the allowance. A visitor over the rate must be told to slow down
// (429), not that the link is used up (404) — the allowance is intact, only the window is full.
func TestRequestShareEnforcesUploadRate(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"inbox"}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	// Two uploads per hour, with no count cap at all: only the rate can refuse the third.
	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"inbox","kind":"request","hide_contents":true,`+
			`"max_upload_rate":2,"upload_rate_window_seconds":3600}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create request share: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)

	deliver := func(name string) (int, http.Header) {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = io.WriteString(fw, "x")
		_ = mw.Close()
		up, err := http.Post(h.srv.URL+"/s/"+token+"/upload", mw.FormDataContentType(), body)
		if err != nil {
			t.Fatal(err)
		}
		defer up.Body.Close()
		_, _ = io.Copy(io.Discard, up.Body)
		return up.StatusCode, up.Header
	}

	if code, _ := deliver("one.bin"); code != http.StatusCreated {
		t.Fatalf("first delivery: want 201, got %d", code)
	}
	if code, _ := deliver("two.bin"); code != http.StatusCreated {
		t.Fatalf("second delivery: want 201, got %d", code)
	}

	code, hdr := deliver("three.bin")
	if code == http.StatusCreated {
		t.Fatal("a third delivery was accepted through a link limited to two per hour. Unlike " +
			"the totals, this limit cannot be a column: the count has to be measured over a " +
			"rolling window, which means recording the events.")
	}
	// 429, not 404: the link is alive and the allowance is intact. Telling a visitor their link
	// is gone when they merely need to wait is a worse answer than telling them to wait.
	if code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited delivery: want 429, got %d. Reporting the rate as 404 would tell "+
			"a legitimate visitor their link is dead.", code)
	}
	if hdr.Get("Retry-After") == "" {
		t.Fatal("a 429 without Retry-After gives the client nothing to back off with, so it " +
			"retries immediately into the same limit")
	}
}

// TestUploadRateWindowSlides is what separates a rate limit from a total.
//
// A limiter that never forgets is not a rate: it is the same as max_uploads with extra steps, and
// it would refuse a visitor forever after one burst. The window has to actually advance, so this
// test moves the store's clock rather than sleeping.
func TestUploadRateWindowSlides(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	resp := h.do(http.MethodPost, "/api/files/mkdir",
		strings.NewReader(`{"path":"inbox"}`), map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	resp = h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"inbox","kind":"request","hide_contents":true,`+
			`"max_upload_rate":1,"upload_rate_window_seconds":60}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create request share: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)
	shareID := created["share"].(map[string]any)["id"].(string)

	deliver := func() int {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		fw, _ := mw.CreateFormFile("file", "x.bin")
		_, _ = io.WriteString(fw, "x")
		_ = mw.Close()
		up, err := http.Post(h.srv.URL+"/s/"+token+"/upload", mw.FormDataContentType(), body)
		if err != nil {
			t.Fatal(err)
		}
		defer up.Body.Close()
		_, _ = io.Copy(io.Discard, up.Body)
		return up.StatusCode
	}

	if code := deliver(); code != http.StatusCreated {
		t.Fatalf("first delivery: want 201, got %d", code)
	}
	if code := deliver(); code != http.StatusTooManyRequests {
		t.Fatalf("second delivery inside a one-per-minute window: want 429, got %d", code)
	}

	// Advance the store's clock past the window. Without this, "the limit is a rolling
	// window" is a claim nothing checks, and a limiter that never forgets would pass the
	// first test in this pair.
	_ = shareID
	h.meta.AdvanceForTest(2 * time.Minute)

	if code := deliver(); code != http.StatusCreated {
		t.Fatalf("a delivery after the window elapsed: want 201, got %d. A limiter that never "+
			"forgets is not a rate — it is a permanent ban after one burst.", code)
	}
	_ = ctx
}

// TestEmailShareDownloadRecordsAccess is the wiring test for MarkRecipientAccess.
//
// The method was implemented on both backends and had no production caller: first_access_at and
// last_access_at were never written, so the columns that answer "did the recipient ever open this"
// stayed NULL forever. The conformance suite covers the method; only an end-to-end test covers the
// fact that anything calls it — the same distinction as a background job that is declared, logged
// and never invoked.
func TestEmailShareDownloadRecordsAccess(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	content := []byte("recipient-only content")
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "invite.txt")
	_, _ = io.WriteString(fw, string(content))
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"invite.txt","kind":"email","recipients":["guest@example.test"]}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create email share: %d %s", resp.StatusCode, readBody(resp))
	}
	created := decode(t, resp)
	token, _ := created["token"].(string)
	shareID, _ := created["share"].(map[string]any)["id"].(string)

	// Before any access, the recorded column must be empty — otherwise this test could pass
	// against a handler that marks access on the page load rather than on the download.
	readAccess := func() (bool, bool) {
		var first, last bool
		_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			recs, err := tx.RecipientsFor(ctx, uuid.MustParse(shareID))
			if err != nil {
				return err
			}
			for _, r := range recs {
				if r.Email == "guest@example.test" {
					first = r.FirstAccessAt != nil
					last = r.LastAccessAt != nil
				}
			}
			return nil
		})
		return first, last
	}

	if first, last := readAccess(); first || last {
		t.Fatal("access is recorded before any download happened")
	}

	// Verify as the invited address: request a code, then exchange it. That is the two-step flow
	// the recipient cookie comes from — a single POST to /verify is the PASSWORD endpoint, which
	// is why the first version of this test got a 404 rather than a cookie.
	req := h.do(http.MethodPost, "/s/"+token+"/recipient",
		strings.NewReader(`{"email":"guest@example.test"}`),
		map[string]string{"Content-Type": "application/json"})
	req.Body.Close()
	if req.StatusCode != http.StatusAccepted {
		t.Fatalf("recipient request: want 202, got %d", req.StatusCode)
	}
	code := h.mailer.codeFor("guest@example.test")
	if code == "" {
		t.Fatal("no verification code was generated")
	}

	vr := h.do(http.MethodPost, "/s/"+token+"/recipient/verify",
		strings.NewReader(`{"email":"guest@example.test","code":"`+code+`"}`),
		map[string]string{"Content-Type": "application/json"})
	vr.Body.Close()
	if vr.StatusCode != http.StatusOK {
		t.Fatalf("recipient verify: want 200, got %d", vr.StatusCode)
	}

	// The cookie from the verify response is the grant; carry it to the download explicitly, since
	// http.Get uses a bare client with no jar.
	dlReq, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range vr.Cookies() {
		dlReq.AddCookie(ck)
	}
	dl, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		t.Fatal(err)
	}
	dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download: want 200, got %d", dl.StatusCode)
	}

	first, last := readAccess()
	if !first || !last {
		t.Fatal("a completed download by a verified recipient recorded no access. " +
			"first_access_at and last_access_at exist to answer \"did the recipient ever open " +
			"this\", and nothing writes them.")
	}
}

// TestSharePasswordBruteForceIsThrottled is SHARE-02's 尝试上限 for passwords.
//
// The requirement asks for a rate limit, a validity period, an attempt cap and replay protection on
// both password and OTP verification. OTP had all four; password verification had NONE — a wrong
// password was audited, so an owner could see a burst afterwards, but nothing slowed it down. A
// share password is usually short and human-chosen, so online guessing was the realistic attack.
func TestSharePasswordBruteForceIsThrottled(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "guarded.txt")
	_, _ = io.WriteString(fw, "secret")
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"guarded.txt","kind":"link","password":"correct-horse"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create share: %d %s", resp.StatusCode, readBody(resp))
	}
	token, _ := decode(t, resp)["token"].(string)

	attempt := func(password string) int {
		r, err := http.Post(h.srv.URL+"/s/"+token+"/verify", "application/json",
			strings.NewReader(`{"password":"`+password+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		return r.StatusCode
	}

	// Guesses up to the limit are refused as wrong passwords — a normal 403, not a 429.
	for i := 0; i < 5; i++ {
		if code := attempt("guess-" + string(rune('a'+i))); code == http.StatusTooManyRequests {
			t.Fatalf("guess %d was throttled before the limit was reached", i+1)
		}
	}

	// The next one must be throttled. Without this, a short human-chosen password could be guessed
	// at whatever rate the network allows.
	if code := attempt("another-guess"); code != http.StatusTooManyRequests {
		t.Fatalf("guess 6 on a 5-attempt link: want 429, got %d. Password verification had no "+
			"attempt cap, so a share password could be brute forced at network speed.", code)
	}

	// And the CORRECT password is also refused while the throttle is in force — a limit that let
	// the right guess through would still leak whether a guess was right.
	if code := attempt("correct-horse"); code != http.StatusTooManyRequests {
		t.Fatalf("the correct password during a throttle: want 429, got %d. A cap that admits the "+
			"right guess still tells an attacker when they found it.", code)
	}
}
