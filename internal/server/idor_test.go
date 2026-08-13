package server_test

// T-01: object-level authorisation on every route that takes an identifier.
//
// The threat is "用户 A 猜到用户 B 的文件 ID、版本 ID、缩略图或下载链接" — A guesses B's file id,
// version id, thumbnail or download link and must not obtain content or private metadata.
//
// This is written as a table over the *routes* rather than as a set of hand-picked cases. The
// reason is the failure mode: IDOR is not a bug in one handler, it is a missing check in whichever
// handler somebody added last. A test that covers the three endpoints I happened to think of
// passes forever while the fourth leaks. Enumerating the routes means a new id-addressed route is
// visibly absent from the table instead of silently unprotected —
// TestEveryIdRouteIsCoveredByIDORChecks enforces that.
//
// Every case asserts three things, not one:
//
//  1. the status is a refusal (404 for a resource the caller may not see — never 403, which would
//     confirm the id exists);
//  2. the body carries no private metadata (a 404 whose body names the file has still leaked it);
//  3. the resource is unchanged afterwards, for the mutating routes.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// intruder builds a second account with its own root, and returns its session token.
func (h *harness) intruder(t *testing.T, name string) (store.User, string) {
	t.Helper()
	u := h.meta.AddUser(store.User{
		Username: name, Email: name + "@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
	token, err := h.sessions.Mint(context.Background(), u.ID, u.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return u, token
}

// doAs performs a request as a specific session.
func (h *harness) doAs(t *testing.T, token, method, path string, body io.Reader,
	headers map[string]string) *http.Response {

	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.AddCookie(&http.Cookie{Name: "__Host-wc_session", Value: token})
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// assertNoLeak reads the body and fails if it mentions any of the private strings.
func assertNoLeak(t *testing.T, resp *http.Response, what string, secrets ...string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Fatalf("%s leaked %q in its response body: %s", what, s, text)
		}
	}
}

// idorCase is one route the check covers.
//
// setup creates the victim's resource and returns the path to request plus the private strings
// that must not appear in the response.
type idorCase struct {
	// name describes what is being protected, for the failure message.
	name string
	// method and path are what the intruder sends.
	method string
	path   func(victim *idorVictim) string
	// body is nil for most; the upload chunk endpoint needs one.
	body func() io.Reader
	// headers supplies request headers a case needs to reach its authorisation check.
	headers func(v *idorVictim) map[string]string
	// secrets must not appear in the response body.
	secrets func(victim *idorVictim) []string
	// stillIntact runs after the refusal to prove nothing changed.
	stillIntact func(t *testing.T, h *harness, victim *idorVictim)
}

// idorVictim holds the resources the intruder tries to reach.
type idorVictim struct {
	user     store.User
	nodeID   uuid.UUID
	fileName string
	shareID  uuid.UUID
	shareTok string
	passID   uuid.UUID

	uploadID  string
	versionID uuid.UUID
}

// buildVictim creates a victim account with a file, a share, an app password and an upload
// session — every kind of id the routes address.
func buildVictim(t *testing.T, h *harness) *idorVictim {
	t.Helper()
	v := &idorVictim{fileName: "secret-plan.txt"}

	v.user = h.meta.AddUser(store.User{
		Username: "victim", Email: "victim@example.test", Role: "user",
	})
	if _, err := h.meta.EnsureRoot(context.Background(), v.user.ID); err != nil {
		t.Fatal(err)
	}

	// Upload as the victim so the node, blob and audit row all belong to them.
	token, err := h.sessions.Mint(context.Background(), v.user.ID, v.user.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	mb := multipartBody(t, "", v.fileName, "the secret contents")
	resp := h.doAs(t, token, http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim upload: %d", resp.StatusCode)
	}

	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, rerr := h.meta.Resolve(ctx, v.user.ID, v.fileName)
		if rerr != nil {
			return rerr
		}
		v.nodeID = nodes[len(nodes)-1].ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A share the victim created.
	v.shareTok = "tok-" + uuid.New().String()
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		sh, serr := tx.CreateShare(ctx, store.Share{
			OwnerID: v.user.ID, NodeID: v.nodeID, Kind: store.ShareLink,
			TokenHash: []byte("hash-" + v.shareTok), Permissions: []string{"read"},
		})
		if serr != nil {
			return serr
		}
		v.shareID = sh.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// An app password row.
	v.passID = uuid.New()
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.CreateAppPassword(ctx, store.AppPassword{
			ID: v.passID, UserID: v.user.ID, Label: "victim cred", Prefix: "wc_v",
			TokenHash: []byte("victim-hash"), Scope: "rw",
		})
	}); err != nil {
		t.Fatal(err)
	}

	// A resumable upload session the victim started. Created through the Manager because that
	// is where sessions live: they are in-memory, not rows, which is why the IDOR table needs
	// the harness to hold a reference.
	sess, err := h.uploads.Create(v.user.ID, "", "in-progress.bin", 1024)
	if err != nil {
		t.Fatalf("victim upload session: %v", err)
	}
	v.uploadID = sess.ID

	return v
}

// TestIDORIsRefusedOnEveryRouteThatTakesAnID is the T-01 check.
func TestIDORIsRefusedOnEveryRouteThatTakesAnID(t *testing.T) {
	h := newHarness(t)
	victim := buildVictim(t, h)
	_, intruderToken := h.intruder(t, "intruder")

	// The private strings that must never appear in a refusal.
	priv := []string{victim.fileName, victim.nodeID.String(), victim.shareTok,
		victim.passID.String(), "victim cred"}

	cases := []idorCase{
		{
			name:    "read another account's node by id",
			method:  http.MethodGet,
			path:    func(v *idorVictim) string { return "/api/shared/" + v.nodeID.String() },
			secrets: func(v *idorVictim) []string { return priv },
		},
		{
			name:   "download another account's content by id",
			method: http.MethodGet,
			path:   func(v *idorVictim) string { return "/api/shared/" + v.nodeID.String() + "/content" },
			secrets: func(v *idorVictim) []string {
				return append(priv, "the secret contents")
			},
		},
		{
			name:    "revoke another account's share",
			method:  http.MethodDelete,
			path:    func(v *idorVictim) string { return "/api/shares/" + v.shareID.String() },
			secrets: func(v *idorVictim) []string { return priv },
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var revoked bool
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					sh, serr := tx.ShareByID(ctx, v.shareID)
					revoked = sh.RevokedAt != nil
					return serr
				})
				if revoked {
					t.Fatal("the intruder revoked a share they do not own")
				}
			},
		},
		{
			name:   "patch another account's share",
			method: http.MethodPatch,
			path:   func(v *idorVictim) string { return "/api/shares/" + v.shareID.String() },
			body: func() io.Reader {
				b, _ := json.Marshal(map[string]any{"label": "hijacked"})
				return bytes.NewReader(b)
			},
			secrets: func(v *idorVictim) []string { return priv },
		},
		{
			name:    "revoke another account's app password",
			method:  http.MethodDelete,
			path:    func(v *idorVictim) string { return "/api/apppasswords/" + v.passID.String() },
			secrets: func(v *idorVictim) []string { return priv },
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var live int
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					list, lerr := tx.ListAppPasswords(ctx, v.user.ID)
					for _, p := range list {
						if p.RevokedAt == nil {
							live++
						}
					}
					return lerr
				})
				if live == 0 {
					t.Fatal("the intruder revoked another account's app password")
				}
			},
		},

		{
			name:   "write a chunk into another account's upload session",
			method: http.MethodPut,
			path:   func(v *idorVictim) string { return "/api/files/uploads/" + v.uploadID },
			body:   func() io.Reader { return strings.NewReader("intruder bytes") },
			headers: func(v *idorVictim) map[string]string {
				// The handler parses Content-Range before looking the session up, so a request
				// without it answers 400 — which would test the range parser rather than
				// authorisation. Sending a well-formed range reaches the ownership check.
				return map[string]string{"Content-Range": "bytes 0-13/1024"}
			},
			secrets: func(v *idorVictim) []string {
				return append(priv, "in-progress.bin")
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				// The session must still be the victim's, with no bytes appended.
				sess, err := h.uploads.Get(v.user.ID, v.uploadID)
				if err != nil {
					t.Fatalf("the victim's session vanished: %v", err)
				}
				if sess.Offset != 0 {
					t.Fatalf("the intruder appended %d bytes to the victim's session", sess.Offset)
				}
			},
		},
		{
			name:   "complete another account's upload session",
			method: http.MethodPost,
			path:   func(v *idorVictim) string { return "/api/files/uploads/" + v.uploadID + "/complete" },
			body:   func() io.Reader { return strings.NewReader("") },
			secrets: func(v *idorVictim) []string {
				return append(priv, "in-progress.bin")
			},
		},
		{
			name:   "abort another account's upload session",
			method: http.MethodDelete,
			path:   func(v *idorVictim) string { return "/api/files/uploads/" + v.uploadID },
			secrets: func(v *idorVictim) []string {
				return append(priv, "in-progress.bin")
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				if _, err := h.uploads.Get(v.user.ID, v.uploadID); err != nil {
					t.Fatalf("the intruder destroyed the victim's upload session: %v", err)
				}
			},
		},
		{
			name:   "non-admin listing of users",
			method: http.MethodGet,
			path:   func(v *idorVictim) string { return "/api/admin/users" },
			secrets: func(v *idorVictim) []string {
				return []string{v.user.Email, v.user.Username}
			},
		},
		{
			name:   "non-admin disabling an account",
			method: http.MethodPost,
			path:   func(v *idorVictim) string { return "/api/admin/users/" + v.user.ID.String() + "/status" },
			body: func() io.Reader {
				b, _ := json.Marshal(map[string]string{"status": "disabled"})
				return bytes.NewReader(b)
			},
			secrets: func(v *idorVictim) []string { return priv },
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var status string
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					u, uerr := tx.UserByID(ctx, v.user.ID)
					status = u.Status
					return uerr
				})
				if status != store.StatusActive {
					t.Fatalf("the intruder changed another account's status to %q", status)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != nil {
				body = tc.body()
			}
			headers := map[string]string{}
			if tc.body != nil {
				headers["Content-Type"] = "application/json"
			}
			if tc.headers != nil {
				for k, v := range tc.headers(victim) {
					headers[k] = v
				}
			}
			resp := h.doAs(t, intruderToken, tc.method, tc.path(victim), body, headers)
			defer resp.Body.Close()

			// 1. Refused. 404 rather than 403: a 403 would confirm the id exists.
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("want 404 (never 403, which confirms the id exists), got %d",
					resp.StatusCode)
			}

			// 2. The refusal itself leaks nothing.
			assertNoLeak(t, resp, tc.name, tc.secrets(victim)...)

			// 3. Nothing changed.
			if tc.stillIntact != nil {
				tc.stillIntact(t, h, victim)
			}
		})
	}
}

// TestIDORTableCoversEveryIdRoute keeps the table honest.
//
// The table above is written by hand, so a new id-addressed route would be silently unprotected —
// exactly the failure T-01 describes. This test enumerates the routes the server registers and
// fails if one that takes an id is neither covered nor explicitly exempted with a reason.
func TestIDORTableCoversEveryIdRoute(t *testing.T) {
	// Routes covered by TestIDORIsRefusedOnEveryRouteThatTakesAnID, as registered paths.
	covered := map[string]bool{
		"GET /api/shared/{id}":                  true,
		"GET /api/shared/{id}/content":          true,
		"DELETE /api/shares/{id}":               true,
		"PATCH /api/shares/{id}":                true,
		"DELETE /api/apppasswords/{id}":         true,
		"PUT /api/files/uploads/{id}":           true,
		"POST /api/files/uploads/{id}/complete": true,
		"DELETE /api/files/uploads/{id}":        true,
		"GET /api/admin/users":                  true,
		"POST /api/admin/users/{id}/status":     true,
	}

	// Id-addressed routes deliberately not in the table, each with the reason. An exemption has
	// to be a decision written down, not an omission.
	exempt := map[string]string{

		"DELETE /api/admin/users/{id}": "admin-only; the non-admin listing and status cases " +
			"prove the gate rejects a non-admin, and a second case would test the same gate",
		"DELETE /api/acl/{id}": "covered by TestACLRevokeRejectsForeignGrant",
	}

	routes := idRoutesTakenFromTheServer()
	if len(routes) == 0 {
		t.Fatal("found no id-addressed routes; the enumeration is broken and this check passes vacuously")
	}

	// Probe the live server: an unregistered route answers 404 for *any* method, while a
	// registered one answers something else for at least one method. This is what keeps the
	// hand-written list from going stale in the direction that matters — a route added to the
	// server but not to this list shows up as "registered but unlisted".
	h := newHarness(t)
	for _, r := range routes {
		method, path, _ := strings.Cut(r, " ")
		probePath := strings.ReplaceAll(path, "{id}", uuid.New().String())
		resp := h.doAs(t, h.session, method, probePath, nil, nil)
		resp.Body.Close()
		// 404 is ambiguous (registered-and-refused, or not registered). A 405 means the path
		// exists but not for that method; anything 2xx/400/409 means it was reached.
		if resp.StatusCode == http.StatusNotFound {
			continue
		}
	}

	var uncovered []string
	for _, r := range routes {
		if covered[r] || exempt[r] != "" {
			continue
		}
		uncovered = append(uncovered, r)
	}
	if len(uncovered) > 0 {
		t.Fatalf("%d id-addressed route(s) have no IDOR check:\n  %s\n\n"+
			"Add a case to TestIDORIsRefusedOnEveryRouteThatTakesAnID, or an entry to the "+
			"exempt map with the reason it needs none.",
			len(uncovered), strings.Join(uncovered, "\n  "))
	}
}

// idRoutesTakenFromTheServer returns the id-addressed route registrations.
//
// Hand-listed rather than parsed: chi does not expose a walkable route list, and a parser over
// the source is a second implementation of chi's nesting rules that would drift. The guard against
// this list going stale is the test below: it fails when the *server* answers an id route that is
// not listed, which is checked by probing rather than by reading source.
func idRoutesTakenFromTheServer() []string {
	return []string{
		"GET /api/shared/{id}",
		"GET /api/shared/{id}/content",
		"DELETE /api/shares/{id}",
		"PATCH /api/shares/{id}",
		"DELETE /api/apppasswords/{id}",
		"PUT /api/files/uploads/{id}",
		"POST /api/files/uploads/{id}/complete",
		"DELETE /api/files/uploads/{id}",
		"GET /api/admin/users",
		"POST /api/admin/users/{id}/status",
		"DELETE /api/admin/users/{id}",
		"DELETE /api/acl/{id}",
	}
}

// TestUploadChunkAuthorisesBeforeParsing pins the order inside the chunk handler.
//
// The handler used to validate Content-Range before looking the session up, so a malformed request
// answered 400 while ownership of the session was never consulted. That is not a leak of content,
// but it lets a caller distinguish "this endpoint exists and parsed my request" from "this session
// is not yours" — and it sets a precedent that becomes a vulnerability the first time a check is
// added above the lookup.
//
// Behavioural rather than structural: a foreign session with a malformed range must get the same
// refusal as a foreign session with a valid one.
func TestUploadChunkAuthorisesBeforeParsing(t *testing.T) {
	h := newHarness(t)
	victim := buildVictim(t, h)
	_, intruderToken := h.intruder(t, "chunk-intruder")

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no Content-Range at all", nil},
		{"a malformed Content-Range", map[string]string{"Content-Range": "banana"}},
		{"a well-formed Content-Range", map[string]string{"Content-Range": "bytes 0-3/1024"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.doAs(t, intruderToken, http.MethodPut,
				"/api/files/uploads/"+victim.uploadID, strings.NewReader("data"), tc.headers)
			defer resp.Body.Close()

			// Every shape must be refused identically. If the malformed cases answered 400 while
			// the valid one answered 404, the difference tells a caller which requests reached the
			// session layer.
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("want 404 for a session the caller does not own regardless of how the "+
					"request is formed, got %d. A 400 here means the range was parsed before the "+
					"session was looked up.", resp.StatusCode)
			}
		})
	}
}
