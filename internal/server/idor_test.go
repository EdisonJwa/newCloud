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

	// The social substrate's own ids (core-scope §4, §5, §6): a comment the victim wrote and a tag
	// the victim defined. Both are id-addressed routes, so both are enumeration surfaces.
	commentID   uuid.UUID
	commentBody string
	tagID       uuid.UUID

	// A tus session the victim started (core-scope §1), for the same reason as uploadID: the
	// manager is in-memory, so the harness has to hold the reference.
	tusID string

	// A notification the victim received (M4): the read route is id-addressed, so dismissing
	// somebody else's bell — or even learning whose bell rang — is an enumeration surface.
	notificationID    uuid.UUID
	notificationLabel string
}

// jsonReader encodes a body for a case that needs one.
func jsonReader(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
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

	// A tus session (core-scope §1), created through the same manager. It is a different session
	// from uploadID because the two protocols have their own verbs, and the table probes both.
	tus, terr := h.uploads.Create(v.user.ID, "", "tus-in-progress.bin", 4096)
	if terr != nil {
		t.Fatalf("victim tus session: %v", terr)
	}
	v.tusID = tus.ID

	// A comment the victim wrote on their own file, and a personal tag they defined. Both are
	// addressed by id like everything else, so the table above must prove the intruder cannot reach
	// them either.
	v.commentBody = "victim's private note"
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		made, cerr := tx.CreateComment(ctx, store.Comment{
			NodeID: v.nodeID, AuthorID: v.user.ID, Body: v.commentBody,
		}, store.CommentLimit{})
		if cerr != nil {
			return cerr
		}
		v.commentID = made.ID
		tag, terr := tx.CreateTag(ctx, store.Tag{OwnerID: &v.user.ID, Name: "victim-label"})
		if terr != nil {
			return terr
		}
		v.tagID = tag.ID

		// A notification only the victim is entitled to see or dismiss (M4).
		v.notificationLabel = "victim's confidential folder"
		if nerr := tx.Notify(ctx, store.NewNotification{
			UserID: v.user.ID, Kind: store.NotifyShareReceived,
			DedupeKey: "idor:" + uuid.NewString(),
			Meta:      map[string]any{"label": v.notificationLabel},
		}); nerr != nil {
			return nerr
		}
		items, _, _, lerr := tx.Notifications(ctx, v.user.ID, "", 10)
		if lerr != nil {
			return lerr
		}
		if len(items) > 0 {
			v.notificationID = items[0].ID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

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
			name:   "read another account's upload session status",
			method: http.MethodGet,
			path:   func(v *idorVictim) string { return "/api/files/uploads/" + v.uploadID },
			secrets: func(v *idorVictim) []string {
				return append(priv, "in-progress.bin")
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
			name:   "non-admin adding a group member",
			method: http.MethodPost,
			// A group id that need not exist: requireAdmin runs before the lookup, so the refusal
			// proves the gate rather than the 404 an unknown id would give anyway.
			path: func(v *idorVictim) string {
				return "/api/admin/groups/" + uuid.New().String() + "/members"
			},
			body: func() io.Reader {
				b, _ := json.Marshal(map[string]string{"username": "intruder"})
				return bytes.NewReader(b)
			},
			secrets: func(v *idorVictim) []string { return priv },
		},
		{
			name:   "non-admin changing a quota",
			method: http.MethodPost,
			path:   func(v *idorVictim) string { return "/api/admin/users/" + v.user.ID.String() + "/quota" },
			body: func() io.Reader {
				b, _ := json.Marshal(map[string]int64{"bytes": 1})
				return bytes.NewReader(b)
			},
			secrets: func(v *idorVictim) []string { return priv },
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var quota int64
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					u, uerr := tx.UserByID(ctx, v.user.ID)
					quota = u.QuotaBytes
					return uerr
				})
				if quota == 1 {
					t.Fatal("the intruder changed another account's quota to 1 byte")
				}
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

		// The social substrate (core-scope §4, §5, §6). Every route below takes a node or comment
		// id, so every one of them is an enumeration surface if the grant check is misplaced.
		{
			name:    "star another account's file",
			method:  http.MethodPut,
			path:    func(v *idorVictim) string { return "/api/files/favorite?id=" + v.nodeID.String() },
			secrets: func(v *idorVictim) []string { return priv },
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				// The star is the intruder's own row, so the harm is not the victim's data but the
				// confirmation that the id exists. Assert both: no row was created, and nothing about
				// the victim's own state changed.
				var starred bool
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					list, lerr := tx.FavoritesAfter(ctx, v.user.ID, "", 50)
					for _, item := range list {
						if item.Node.ID == v.nodeID {
							starred = true
						}
					}
					return lerr
				})
				if starred {
					t.Fatal("the intruder's star landed on the victim's favorite listing")
				}
			},
		},
		{
			name:    "tag another account's file",
			method:  http.MethodPost,
			path:    func(v *idorVictim) string { return "/api/nodes/" + v.nodeID.String() + "/tags" },
			body:    func() io.Reader { return jsonReader(map[string]string{"tag_id": uuid.New().String()}) },
			secrets: func(v *idorVictim) []string { return priv },
		},
		{
			// M4: the inbox's read route. A stranger must not dismiss — or confirm the
			// existence of — anybody else's notification; the refusal is the same 404 for a
			// foreign id and an absent one, and the victim's bell must still be ringing.
			name:   "dismiss another account's notification",
			method: http.MethodPost,
			path:   func(v *idorVictim) string { return "/api/notifications/" + v.notificationID.String() + "/read" },
			secrets: func(v *idorVictim) []string {
				return append(priv, v.notificationLabel)
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					n, err := tx.UnreadNotificationCount(ctx, v.user.ID)
					if err != nil {
						return err
					}
					if n != 1 {
						t.Fatalf("victim has %d unread notifications after the intrusion, want the original 1", n)
					}
					return nil
				})
			},
		},
		{
			name:    "untag another account's file",
			method:  http.MethodDelete,
			path:    func(v *idorVictim) string { return "/api/nodes/" + v.nodeID.String() + "/tags" },
			body:    func() io.Reader { return jsonReader(map[string]string{"tag_id": uuid.New().String()}) },
			secrets: func(v *idorVictim) []string { return priv },
		},
		{
			name:    "read another account's discussion thread",
			method:  http.MethodGet,
			path:    func(v *idorVictim) string { return "/api/nodes/" + v.nodeID.String() + "/comments" },
			secrets: func(v *idorVictim) []string { return append(priv, "the secret contents") },
		},
		{
			name:   "post into another account's discussion thread",
			method: http.MethodPost,
			path:   func(v *idorVictim) string { return "/api/nodes/" + v.nodeID.String() + "/comments" },
			body:   func() io.Reader { return jsonReader(map[string]string{"body": "intruder was here"}) },
			secrets: func(v *idorVictim) []string {
				return append(priv, "intruder was here")
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var thread []store.Comment
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					got, cerr := tx.CommentsAfter(ctx, v.nodeID, 0, 50)
					thread = got
					return cerr
				})
				// The victim's own comment is expected; what must not be there is the intruder's.
				for _, c := range thread {
					if strings.Contains(c.Body, "intruder was here") {
						t.Fatal("the intruder posted into the victim's discussion thread")
					}
				}
				if len(thread) != 1 {
					t.Fatalf("the thread holds %d comment(s), want only the victim's own", len(thread))
				}
			},
		},
		{
			name:   "edit another account's comment",
			method: http.MethodPatch,
			path:   func(v *idorVictim) string { return "/api/comments/" + v.commentID.String() },
			body:   func() io.Reader { return jsonReader(map[string]string{"body": "rewritten"}) },
			secrets: func(v *idorVictim) []string {
				return append(priv, v.commentBody)
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var got store.Comment
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					c, cerr := tx.CommentByID(ctx, v.commentID)
					got = c
					return cerr
				})
				if got.Body != v.commentBody {
					t.Fatalf("the intruder rewrote the victim's comment: %q", got.Body)
				}
			},
		},
		{
			name:   "delete another account's comment",
			method: http.MethodDelete,
			path:   func(v *idorVictim) string { return "/api/comments/" + v.commentID.String() },
			secrets: func(v *idorVictim) []string {
				return append(priv, v.commentBody)
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var got store.Comment
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					c, cerr := tx.CommentByID(ctx, v.commentID)
					got = c
					return cerr
				})
				if got.Deleted() {
					t.Fatal("the intruder deleted the victim's comment")
				}
			},
		},
		{
			name:    "rename another account's tag",
			method:  http.MethodPatch,
			path:    func(v *idorVictim) string { return "/api/tags/" + v.tagID.String() },
			body:    func() io.Reader { return jsonReader(map[string]string{"name": "hijacked", "color": "red"}) },
			secrets: func(v *idorVictim) []string { return append(priv, "victim-label") },
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				var got store.Tag
				_ = h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					tag, terr := tx.TagByID(ctx, v.tagID)
					got = tag
					return terr
				})
				if got.Name == "hijacked" {
					t.Fatal("the intruder renamed another account's tag")
				}
			},
		},
		{
			name:   "delete another account's tag",
			method: http.MethodDelete,
			path:   func(v *idorVictim) string { return "/api/tags/" + v.tagID.String() },
			secrets: func(v *idorVictim) []string {
				return append(priv, "victim-label")
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
					_, terr := tx.TagByID(ctx, v.tagID)
					return terr
				}); err != nil {
					t.Fatalf("the intruder deleted another account's tag: %v", err)
				}
			},
		},

		// The tus endpoints (core-scope §1). They address a session by id, and a session is a
		// half-written file: reaching somebody else's is both a denial of service and a way to
		// publish bytes into their namespace. Every verb is in the table because the manager's
		// ownership check is per call, and one verb that forgot it would be enough.
		{
			name:   "read another account's tus session",
			method: http.MethodHead,
			path:   func(v *idorVictim) string { return "/api/files/tus/" + v.tusID },
			headers: func(v *idorVictim) map[string]string {
				return map[string]string{"Tus-Resumable": "1.0.0"}
			},
			secrets: func(v *idorVictim) []string {
				return append(priv, "tus-in-progress.bin")
			},
		},
		{
			name:   "append to another account's tus session",
			method: http.MethodPatch,
			path:   func(v *idorVictim) string { return "/api/files/tus/" + v.tusID },
			body:   func() io.Reader { return strings.NewReader("intruder bytes") },
			headers: func(v *idorVictim) map[string]string {
				return map[string]string{
					"Tus-Resumable": "1.0.0",
					"Upload-Offset": "0",
					"Content-Type":  "application/offset+octet-stream",
				}
			},
			secrets: func(v *idorVictim) []string {
				return append(priv, "tus-in-progress.bin")
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				sess, err := h.uploads.Get(v.user.ID, v.tusID)
				if err != nil {
					t.Fatalf("the victim's tus session vanished: %v", err)
				}
				if sess.Offset != 0 {
					t.Fatalf("the intruder appended %d bytes to the victim's tus session", sess.Offset)
				}
			},
		},
		{
			name:   "terminate another account's tus session",
			method: http.MethodDelete,
			path:   func(v *idorVictim) string { return "/api/files/tus/" + v.tusID },
			headers: func(v *idorVictim) map[string]string {
				return map[string]string{"Tus-Resumable": "1.0.0"}
			},
			secrets: func(v *idorVictim) []string {
				return append(priv, "tus-in-progress.bin")
			},
			stillIntact: func(t *testing.T, h *harness, v *idorVictim) {
				if _, err := h.uploads.Get(v.user.ID, v.tusID); err != nil {
					t.Fatalf("the intruder destroyed the victim's tus session: %v", err)
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
		"GET /api/files/uploads/{id}":           true,
		"PUT /api/files/uploads/{id}":           true,
		"POST /api/files/uploads/{id}/complete": true,
		"DELETE /api/files/uploads/{id}":        true,
		"GET /api/admin/users":                  true,
		"POST /api/admin/users/{id}/status":     true,
		"POST /api/admin/groups/{id}/members":   true,
		"POST /api/admin/users/{id}/quota":      true,
		// The social substrate (core-scope §4, §5, §6). Every one of these is id-addressed, so
		// every one gets a case above rather than an exemption.
		"POST /api/nodes/{id}/tags":     true,
		"DELETE /api/nodes/{id}/tags":   true,
		"GET /api/nodes/{id}/comments":  true,
		"POST /api/nodes/{id}/comments": true,
		"PATCH /api/comments/{id}":      true,
		"DELETE /api/comments/{id}":     true,
		"PATCH /api/tags/{id}":          true,
		"DELETE /api/tags/{id}":         true,
		// tus (core-scope §1). The session manager enforces ownership per verb, so each one is a
		// separate case rather than an exemption: one verb that forgot the check would be enough.
		"HEAD /api/files/tus/{id}":   true,
		"PATCH /api/files/tus/{id}":  true,
		"DELETE /api/files/tus/{id}": true,
		// M4: dismissing somebody else's notification must be the same 404 as an absent id.
		"POST /api/notifications/{id}/read": true,
	}

	// Id-addressed routes deliberately not in the table, each with the reason. An exemption has
	// to be a decision written down, not an omission.
	exempt := map[string]string{

		"DELETE /api/admin/users/{id}": "admin-only; the non-admin listing and status cases " +
			"prove the gate rejects a non-admin, and a second case would test the same gate",
		"DELETE /api/acl/{id}": "covered by TestACLRevokeRejectsForeignGrant",
		"GET /api/nodes/{id}": "covered by TestANodeBelongingToSomeoneElseLooksAbsent in " +
			"nodes_test.go, which checks the same thing for both node routes: a caller with no grant " +
			"gets the same 404 as for an id that does not exist",
		"GET /api/nodes/{id}/children": "covered by the same test as GET /api/nodes/{id}; the " +
			"listing goes through the identical Authorize check before it reads a child",
		"DELETE /api/admin/groups/{id}": "covered by TestAdminCanDeleteAGroup in spacesadmin_test.go, " +
			"which checks the admin gate, the 404 for an absent group and the refusal for a non-admin",
		"DELETE /api/admin/groups/{id}/members/{userID}": "admin-only; the member-add case above " +
			"proves the gate rejects a non-admin, and the removal runs behind the same check",
		"DELETE /api/mounts/{id}": "the handler passes the actor to mount.Unmount, which answers " +
			"ErrNotOwner as 404 for another account's mount; the harness leaves the mount service " +
			"nil, so a cross-account probe here would test the 503 mounts-disabled path instead",
	}

	h := newHarness(t)
	routes := idRoutesTakenFromTheServer(t, h)
	if len(routes) < 8 {
		t.Fatalf("found only %d id-addressed routes; the parse is probably broken and this check "+
			"would report full coverage of almost nothing", len(routes))
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
// idRoutesTakenFromTheServer asks the router which routes it actually has.
//
// It used to return a hand-written slice, under a name that claimed otherwise. That made the whole
// IDOR guard circular: a fixed list compared against a fixed coverage map, so a route added to the
// server was invisible to the check that exists to find it. The test's own comment claimed a live
// probe kept the list from going stale — and that probe computed a status code and discarded it, so
// nothing kept anything.
//
// chi.Walk reports full patterns including group prefixes, which is why this is not a source parse:
// the first attempt at one produced "/{id}" for every grouped route and could not tell
// /api/shares/{id} from /api/apppasswords/{id}.
func idRoutesTakenFromTheServer(t *testing.T, h *harness) []string {
	t.Helper()
	if h.srvServer == nil {
		t.Fatal("the harness does not expose the server; the route table cannot be enumerated")
	}
	var out []string
	for _, r := range h.srvServer.Routes() {
		if strings.Contains(r, "{id}") {
			out = append(out, r)
		}
	}
	return out
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
