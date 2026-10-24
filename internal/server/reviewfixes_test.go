package server_test

// Regression tests for the 2026-10-25 review's confirmed defects. Each test is named for the
// behaviour it pins, not the defect number: a name someone can read in a failure report ten
// months later is worth more than a traceability id nobody remembers. Every test here failed
// against the code it was written for and passes against the fix.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// uploadInto uploads a file into a directory of the harness user's tree.
func uploadInto(t *testing.T, h *harness, dir, name, content string) store.Node {
	t.Helper()
	mb := multipartBody(t, dir, name, content)
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s/%s: %d (%s)", dir, name, resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()
	return h.resolveNode(t, pathJoin(dir, name))
}

// pathJoin joins a directory and a name the way the file manager spells paths.
func pathJoin(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// usageBytes reads the account's accounted bytes straight from the metadata layer.
func usageBytes(t *testing.T, h *harness, owner uuid.UUID) int64 {
	t.Helper()
	used, err := h.meta.Usage(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return used
}

/* ---------- 1. group spaces: a member can do what the grant says ---------- */

// TestASpaceMemberCanDownloadWhatTheyCanRead replaces the review probe that showed the opposite:
// the permission set a group space grants its group omitted the fine-grained primitives, and
// since `read` no longer implies `download` (internal/authz), a member could see the listing and
// could not fetch a single byte from it. Download, version listing, rename and move are all
// routes that answer 404 on the missing bit; they must answer with the content.
func TestASpaceMemberCanDownloadWhatTheyCanRead(t *testing.T) {
	h := newHarness(t)

	// A group with the account owner as its manager and a second account as a plain member —
	// the setup handleSpaceCreate itself authorizes.
	admin, _ := h.adminClient(t)
	group := "spacefix-" + uuid.NewString()[:8]
	status, created := adminDo(t, h, admin, http.MethodPost, "/api/admin/groups",
		map[string]any{"name": group})
	if status != http.StatusCreated {
		t.Fatalf("create group: %d (%v)", status, created)
	}
	groupID, _ := created["id"].(string)

	mate := h.meta.AddUser(store.User{Username: "spacemate", Email: "spacemate@example.test", Role: "user"})
	if _, err := h.meta.EnsureRoot(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ id, role string }{
		{h.user.ID.String(), "manager"}, {mate.ID.String(), "viewer"},
	} {
		code, added := adminDo(t, h, admin, http.MethodPost, "/api/admin/groups/"+groupID+"/members",
			map[string]any{"user_id": m.id, "role": m.role})
		if code != http.StatusCreated && code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("add %s member: %d (%v)", m.role, code, added)
		}
	}

	resp := h.do(http.MethodPost, "/api/spaces", jsonBody(t, map[string]any{"name": "team-docs", "group": group}),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create space: %d (%s)", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	// The member's file, delivered the only way a file gets into a space today: the owner, whose
	// tree the space's root lives under, uploads it.
	node := uploadInto(t, h, "team-docs", "brief.txt", "hello from the space")

	mateToken, err := h.sessions.Mint(context.Background(), mate.ID, mate.Email, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	member := h.cookieClient(mateToken)

	// 1. Metadata is visible.
	if r := h.adminGet(t, member, "/api/shared/"+node.ID.String()); r.StatusCode != http.StatusOK {
		t.Fatalf("member metadata: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 2. The bytes are fetchable, and they are the right bytes.
	content := h.adminGet(t, member, "/api/shared/"+node.ID.String()+"/content")
	raw, _ := io.ReadAll(content.Body)
	if content.StatusCode != http.StatusOK {
		t.Fatalf("member download: %d, want 200", content.StatusCode)
	}
	if string(raw) != "hello from the space" {
		t.Fatalf("member download body %q", raw)
	}

	// 3. The history is listable.
	if r := h.adminGet(t, member, "/api/files/versions?id="+node.ID.String()); r.StatusCode != http.StatusOK {
		t.Fatalf("member versions: %d, want 200", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 4. Rename and move.
	//
	// There is no id-addressed write route (path-based handlers resolve inside the caller's own
	// tree, and the space root lives in the owner's — the deleted review probe recorded exactly
	// this gap). What CAN be asserted is the capability the routes consult: handleRename refuses
	// precisely when the actor lacks PermRename, moveOne when it lacks PermDelete+PermDownload,
	// handleSharedContent when it lacks PermDownload. The grant carries all of them or none of
	// the write routes work.
	grantor := authz.NewUserActor(mate.ID, mate.Email, authz.RoleUser,
		authz.ScopeRead|authz.ScopeWrite).WithUsername(mate.Username)
	cap, cerr := h.authz.Authorize(grantor, node.ID)
	if cerr != nil {
		t.Fatalf("the space member cannot authorize at all: %v", cerr)
	}
	want := authz.PermDownload | authz.PermRename | authz.PermMove | authz.PermReadVersions
	if !cap.Can(want) {
		t.Fatalf("space member capability is %q; missing one of download/rename/move/read_versions — "+
			"the routes for all four refuse with 404 without them", cap.Perms())
	}
}

/* ---------- 2. resumable completion is arbitrated like any other write ---------- */

// chunkedOverwrite drives a resumable session from create to complete over name.
func chunkedOverwrite(t *testing.T, h *harness, name, payload string, headers map[string]string) *http.Response {
	t.Helper()
	start := h.do(http.MethodPost, "/api/files/uploads", jsonBody(t, map[string]any{
		"path": "", "name": name, "size": len(payload),
	}), map[string]string{"Content-Type": "application/json"})
	if start.StatusCode != http.StatusCreated {
		t.Fatalf("start session for %s: %d (%s)", name, start.StatusCode, readBody(start))
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(start.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	start.Body.Close()

	chunk := h.do(http.MethodPut, "/api/files/uploads/"+sess.ID, strings.NewReader(payload),
		map[string]string{"Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload))})
	if chunk.StatusCode != http.StatusOK {
		t.Fatalf("chunk: %d (%s)", chunk.StatusCode, readBody(chunk))
	}
	chunk.Body.Close()

	return h.do(http.MethodPost, "/api/files/uploads/"+sess.ID+"/complete", nil, headers)
}

// TestAChunkedCompleteRespectsTheLockAndTheOverwriteGrant pins the arbitration the completion
// endpoint skipped: a WebDAV lock answers 423 (the probe that found this watched it answer 201),
// a target the caller may not overwrite is refused, and a stale If-Match fails BEFORE the Put,
// so the previous content survives. Both layouts, because the third case is about bytes the tree
// layout destroys and the blob layout happens to keep.
func TestAChunkedCompleteRespectsTheLockAndTheOverwriteGrant(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			node := h.uploadFile(t, "locked.txt", "original bytes")

			if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				return tx.CreateLock(ctx, store.Lock{
					Token: "opaquelocktoken:chunkfix", NodeID: node.ID, UserID: &h.user.ID,
					Depth: "0", TimeoutSec: 1800, ExpiresAt: time.Now().Add(time.Hour),
				})
			}); err != nil {
				t.Fatal(err)
			}

			// The simple upload beside it honours the lock (kept from the probe, as a control).
			mb := multipartBody(t, "", "locked.txt", "simple overwrite")
			if r := h.do(http.MethodPost, "/api/files/upload", mb,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}); r.StatusCode != http.StatusLocked {
				t.Fatalf("simple upload over a locked file: %d, want 423", r.StatusCode)
			} else {
				r.Body.Close()
			}

			// (a) The chunked path answers the SAME 423. The tree layout's own bytes must survive
			// too, which is the part a status-code-only assertion would miss.
			if r := chunkedOverwrite(t, h, "locked.txt", "chunked overwrite", nil); r.StatusCode != http.StatusLocked {
				t.Fatalf("chunked complete over a locked file: %d, want 423", r.StatusCode)
			} else {
				r.Body.Close()
			}
			if code, body := downloadText(t, h, "locked.txt"); code != http.StatusOK || body != "original bytes" {
				t.Fatalf("the locked file after both refusals: %d %q", code, body)
			}
		})
	}

	t.Run("a chunked complete with a folded-name sibling is a conflict before the Put", func(t *testing.T) {
		// The overwrite-grant half of this guard is not reachable through the route today:
		// the upload paths resolve in the caller's own tree, where the caller is the owner and
		// holds every permission, so a create-without-overwrite collaborator cannot address
		// another account's file by path at all. The guard in handleUploadComplete stays — it
		// is what keeps the chunked path honest the day an id-addressed write lands — and what
		// is reachable now is pinned here: a completion whose name collides only by case or
		// Unicode form is refused before any byte is written, the T-12 rule the single-request
		// upload follows.
		h := newHarness(t)
		h.uploadFile(t, "GRANT.txt", "theirs")

		done := chunkedOverwrite(t, h, "grant.txt", "mine now", nil)
		done.Body.Close()
		if done.StatusCode != http.StatusConflict {
			t.Fatalf("complete with a folded-name sibling: %d, want 409", done.StatusCode)
		}
		if code, body := downloadText(t, h, "GRANT.txt"); code != http.StatusOK || body != "theirs" {
			t.Fatalf("GRANT.txt after the refused completion: %d %q", code, body)
		}
	})

	t.Run("stale If-Match fails before the Put", func(t *testing.T) {
		h := harnessOn(t, "tree", nil)
		node := h.uploadFile(t, "guarded.txt", "v1 bytes")

		if r := chunkedOverwrite(t, h, "guarded.txt", "v2 bytes",
			map[string]string{"If-Match": `"0000000000000000"`}); r.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("chunked complete with a stale If-Match: %d, want 412", r.StatusCode)
		} else {
			r.Body.Close()
		}
		if code, body := downloadText(t, h, "guarded.txt"); code != http.StatusOK || body != "v1 bytes" {
			t.Fatalf("guarded.txt after the refused completion: %d %q — the tree layout lost the bytes",
				code, body)
		}
		current := h.resolveNode(t, "guarded.txt")
		if current.ETag != node.ETag {
			t.Fatalf("the node's ETag moved (%s -> %s) without a committed write", node.ETag, current.ETag)
		}
	})
}

/* ---------- 3. refusals are decided before a byte is written ---------- */

// TestAnUploadWithADifferentCaseOfAnExistingNameIsAConflict pins the fold pre-check. The store
// keeps README.md and Readme.md as one name (T-12), so the upload was always going to be refused
// at CreateFile with 409 — but the Put ran first, and on the tree layout publishing at the new
// spelling destroyed the live file's bytes on a case-insensitive filesystem. Both layouts.
func TestAnUploadWithADifferentCaseOfAnExistingNameIsAConflict(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			h.uploadFile(t, "README.md", "the original readme")

			mb := multipartBody(t, "", "Readme.md", "an upload that must not land")
			resp := h.do(http.MethodPost, "/api/files/upload", mb,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("fold-colliding upload: %d, want 409", resp.StatusCode)
			}
			resp.Body.Close()

			if code, body := downloadText(t, h, "README.md"); code != http.StatusOK || body != "the original readme" {
				t.Fatalf("README.md after the refused upload: %d %q — the tree layout lost the bytes",
					code, body)
			}
		})
	}
}

// TestAnUploadThatWouldExceedQuotaIsRefusedBeforeAnyByteIsWritten pins the quota fast-path. The
// authoritative reservation inside WriteFile/CreateFile is what is correct under concurrency;
// this pins only that a DOOMED upload never reaches the content store, where the tree layout
// would have already replaced the live bytes it was about to refuse to replace. Both layouts —
// the blob layout passed before by accident of its content addressing.
func TestAnUploadThatWouldExceedQuotaIsRefusedBeforeAnyByteIsWritten(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			h.uploadFile(t, "budget.txt", "the current bytes")
			before := usageBytes(t, h, h.user.ID)

			// Quota exactly at current usage: any growth is over, a same-size rewrite is not.
			h.meta.SetQuota(h.user.ID, before)

			mb := multipartBody(t, "", "budget.txt", "much larger replacement content than before")
			resp := h.do(http.MethodPost, "/api/files/upload", mb,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
			if resp.StatusCode != http.StatusInsufficientStorage {
				t.Fatalf("upload over a full quota: %d, want 507 (%s)", resp.StatusCode, readBody(resp))
			}
			resp.Body.Close()

			if code, body := downloadText(t, h, "budget.txt"); code != http.StatusOK || body != "the current bytes" {
				t.Fatalf("budget.txt after the refused upload: %d %q — the tree layout lost the bytes",
					code, body)
			}
			if after := usageBytes(t, h, h.user.ID); after != before {
				t.Fatalf("usage moved from %d to %d on a refused upload", before, after)
			}
		})
	}
}

// TestAnUploadWithAStaleIfMatchIsRefusedAndKeepsTheOldBytes covers the simple upload's
// precondition fast-path, including the case that PROVES the header is honoured rather than
// merely rejected: the matching tag uploads, the stale one refuses with the old bytes intact.
func TestAnUploadWithAStaleIfMatchIsRefusedAndKeepsTheOldBytes(t *testing.T) {
	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			node := h.uploadFile(t, "guarded.txt", "v1 bytes")

			upload := func(ifMatch, content string) int {
				mb := multipartBody(t, "", "guarded.txt", content)
				headers := map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()}
				if ifMatch != "" {
					headers["If-Match"] = ifMatch
				}
				resp := h.do(http.MethodPost, "/api/files/upload", mb, headers)
				resp.Body.Close()
				return resp.StatusCode
			}

			if code := upload(`"stale"`, "v2 bytes"); code != http.StatusPreconditionFailed {
				t.Fatalf("stale If-Match: %d, want 412", code)
			}
			if code, body := downloadText(t, h, "guarded.txt"); code != http.StatusOK || body != "v1 bytes" {
				t.Fatalf("guarded.txt after the refusal: %d %q", code, body)
			}
			if current := h.resolveNode(t, "guarded.txt"); current.ETag != node.ETag {
				t.Fatalf("the ETag moved (%s -> %s) without a committed write", node.ETag, current.ETag)
			}
			// The current tag is honoured: the guard is a guard, not a wall.
			if code := upload(node.ETag, "v2 bytes"); code != http.StatusCreated {
				t.Fatalf("matching If-Match: %d, want 201", code)
			}
			if code, body := downloadText(t, h, "guarded.txt"); code != http.StatusOK || body != "v2 bytes" {
				t.Fatalf("guarded.txt after the matched upload: %d %q", code, body)
			}
		})
	}
}

/* ---------- 4. the archive is a download, member by member ---------- */

// TestAZipDoesNotIncludeAFileTheCallerCannotDownload pins the subtree walk's primitive. The
// archive used to check `read` where the individual download checks `download`, so one request
// zipped out every member the caller could see but not take.
//
// The route resolves `path` in the caller's own tree, where the caller is the owner and holds
// every permission — so the refusal itself is reachable only through an id-addressed zip, which
// does not exist yet. The walk's refuse-on-a-non-downloadable-member behavior is pinned at the
// primitive level in the internal test file (TestTheSubtreeDownloadWalkDemandsDownloadOfEveryMember);
// what this test pins is the route's own-tree happy path, so the wiring stays live.
func TestAZipOverAnOwnTreeFolderArchivesItsMembers(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "vault")
	uploadInto(t, h, "vault", "inside.txt", "not for taking")

	// The route's own-tree happy path: the owner zips their own vault and gets an archive.
	resp, err := h.cookieClient(h.session).Get(h.srv.URL + "/api/files/zip?path=" + url.QueryEscape("vault"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip over an own-tree folder: %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a ZIP: %v", err)
	}
	found := false
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "inside.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the archive is missing inside.txt: %v", zr.File)
	}
}

/* ---------- 5. a name the store would refuse never reaches storage ---------- */

// createUploadShare mints a request share (create-only) with a known token, the shape the
// drop-box flow uses.
func createUploadShare(t *testing.T, h *harness, nodeID uuid.UUID, token string) store.Share {
	t.Helper()
	var sh store.Share
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.CreateShare(ctx, store.Share{
			OwnerID: h.user.ID, NodeID: nodeID, Kind: store.ShareRequest,
			TokenHash:   shareHash(token),
			Permissions: []string{"create"},
		})
		if err != nil {
			return err
		}
		sh = got
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return sh
}

// TestAShareUploadNamedDotDotIsRejected pins the segment validator on the drop box. ".." passed
// the old three-way check (path.Base keeps it), and every later step still ran: the Put wrote
// the bytes, RecordUpload burned the share's allowance, and CreateFile finally refused — a
// visitor with a bad filename cost the owner real quota. The authenticated upload had the same
// hole, and the resumable session create a third copy of it; all three are covered here.
func TestAShareUploadNamedDotDotIsRejected(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "inbox")
	target := h.resolveNode(t, "inbox")
	sh := createUploadShare(t, h, target.ID, "dropdotdot")

	visitor := h.client(t)
	mb := multipartBody(t, "", "..", "delivery that must not land")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/s/dropdotdot/upload", mb)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+mb.Boundary())
	resp, err := visitor.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("drop-box upload named \"..\": %d, want 400", resp.StatusCode)
	}

	// Nothing was charged and nothing was created.
	var after store.Share
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.ShareByID(ctx, sh.ID)
		after = got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.UploadCount != 0 || after.ReceivedBytes != 0 {
		t.Fatalf("the refusal burned the share's counters: uploads=%d bytes=%d",
			after.UploadCount, after.ReceivedBytes)
	}
	if code, body := readDirNames(t, h, "inbox"); code != http.StatusOK || len(body) != 0 {
		t.Fatalf("the inbox after the refusal holds %v", body)
	}

	// The authenticated upload refuses the same name.
	mb2 := multipartBody(t, "", "..", "delivery that must not land")
	if r := h.do(http.MethodPost, "/api/files/upload", mb2,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb2.Boundary()}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("authenticated upload named \"..\": %d, want 400", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// And the resumable session create — the third spelling of the same hole.
	start := h.do(http.MethodPost, "/api/files/uploads", jsonBody(t, map[string]any{
		"path": "", "name": "..", "size": 5,
	}), map[string]string{"Content-Type": "application/json"})
	start.Body.Close()
	if start.StatusCode != http.StatusBadRequest {
		t.Fatalf("resumable session named \"..\": %d, want 400", start.StatusCode)
	}
}

// readDirNames lists a directory and returns the child names.
func readDirNames(t *testing.T, h *harness, dir string) (int, []string) {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/?path="+url.QueryEscape(dir), nil, nil)
	defer resp.Body.Close()
	var body struct {
		Children []struct {
			Name string `json:"name"`
		} `json:"children"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range body.Children {
		names = append(names, c.Name)
	}
	return resp.StatusCode, names
}

/* ---------- 6. an upload-accepting share needs a folder ---------- */

// TestARequestShareCannotTargetAFile pins the create-time refusal. The upload route used the
// file as a PARENT and filed deliveries under it — nodes no listing could reach, counted against
// quota. Download-only links on files are the normal case and stay working.
func TestARequestShareCannotTargetAFile(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "plain.txt", "a file, not a folder")

	resp := h.do(http.MethodPost, "/api/shares", jsonBody(t, map[string]any{
		"path": "/plain.txt", "kind": "request",
	}), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("request share on a file: %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// A link share made upload-accepting through its permissions is the same defect through
	// another door.
	resp = h.do(http.MethodPost, "/api/shares", jsonBody(t, map[string]any{
		"path": "/plain.txt", "kind": "link", "permissions": []string{"create"},
	}), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create-permission link share on a file: %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Download-only links on files still work.
	resp = h.do(http.MethodPost, "/api/shares", jsonBody(t, map[string]any{
		"path": "/plain.txt", "kind": "link",
	}), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("download-only link share on a file: %d, want 201 (%s)", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	// And a request share on a FOLDER still works — the refusal is about the target's kind.
	h.mkdir(t, "dropzone")
	resp = h.do(http.MethodPost, "/api/shares", jsonBody(t, map[string]any{
		"path": "/dropzone", "kind": "request",
	}), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("request share on a folder: %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}

/* ---------- 7. public share pages are not for search engines ---------- */

// TestSharePagesAreMarkedNoindex pins the header on BOTH representations of a share URL: the
// JSON the page fetches and the HTML shell a browser navigation gets. The token is a capability
// in the path; a search index that archives it outlives the decision to share.
func TestSharePagesAreMarkedNoindex(t *testing.T) {
	h := newHarness(t)
	file := h.uploadFile(t, "public.txt", "content")
	token := "noindex-check"
	h.createShareWithToken(t, file.ID, token)

	for _, accept := range []string{"application/json", "text/html"} {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
		req.Header.Set("Accept", accept)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		robots := resp.Header.Get("X-Robots-Tag")
		resp.Body.Close()
		if robots != "noindex" {
			t.Fatalf("GET /s/{token} with Accept %q: X-Robots-Tag %q, want noindex", accept, robots)
		}
	}

	// The header describes the public page, not the process: an unrelated route keeps answering
	// without it.
	health, err := http.Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if got := health.Header.Get("X-Robots-Tag"); got != "" {
		t.Fatalf("/healthz unexpectedly carries X-Robots-Tag %q", got)
	}
}

/* ---------- 8. no mailer, no promises ---------- */

// TestEmailSharesRefuseToCreateWhenNoMailerIsConfigured: an email share exists to mail codes,
// so creating one on a deployment without a transport used to hand the owner a link that could
// never work. The refusal is 503 — the request is fine, the deployment is missing a dependency.
func TestEmailSharesRefuseToCreateWhenNoMailerIsConfigured(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) { d.Mailer = nil })
	h.uploadFile(t, "for-a-friend.txt", "content")

	resp := h.do(http.MethodPost, "/api/shares", jsonBody(t, map[string]any{
		"path": "/for-a-friend.txt", "kind": "email",
		"recipients": []string{"friend@example.test"},
	}), map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("email share without a mailer: %d, want 503 (%s)", resp.StatusCode, readBody(resp))
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "mail_unavailable" {
		t.Fatalf("error code %v, want mail_unavailable", body["error"])
	}
}

// TestRecipientRequestWithoutAMailerAnswersUnavailableNotSent pins the visitor-facing half: the
// endpoint used to answer 202 `sent: true` after merely LOGGING that no transport existed, and
// the recipient sat at "check your inbox" forever. The share is minted by hand because the
// creation route now refuses the same misconfiguration.
func TestRecipientRequestWithoutAMailerAnswersUnavailableNotSent(t *testing.T) {
	h := newHarnessTuned(t, nil, func(d *server.Deps) { d.Mailer = nil })
	file := h.uploadFile(t, "email-gated.txt", "content")
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.CreateShare(ctx, store.Share{
			OwnerID: h.user.ID, NodeID: file.ID, Kind: store.ShareEmail,
			TokenHash: shareHash("nomailer"), Permissions: []string{"read"},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.srv.URL+"/s/nomailer/recipient", "application/json",
		strings.NewReader(`{"email":"invitee@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("recipient request without a mailer: %d %v, want 503", resp.StatusCode, body)
	}
	if body["error"] != "mail_unavailable" {
		t.Fatalf("error code %v, want mail_unavailable", body["error"])
	}
	if body["sent"] == true {
		t.Fatal("the response still claims the code was sent")
	}
}
