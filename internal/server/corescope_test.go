package server_test

// The seven core-scope functions, exercised over HTTP (core-scope-2026-10-25.md).
//
// The store-level behaviour is pinned by the shared conformance suite, which runs both backends. What
// these cover is the part that only exists at the protocol boundary and that no store test can see:
//
//	§1 tus           the wire protocol, offsets, 409 on a stale one, 460 on a checksum mismatch
//	§2 checksums     the header round trip on both layouts, and verify-on-upload refusing a write
//	§3 empty trash   the endpoint, its audit event, and that it is scoped to the caller
//	§4 favorites     the toggle, the listing, and that a revocation drops the row
//	§5 tags          the caps, the case-fold refusal, the assignment permission, the filter
//	§6 comments      the rate limit, the tombstone, moderation, and the body cap
//	§7 search        the filters, the sort, the cursor, and the degraded content mode
//
// Each test's name states the property, and each failure message says what a user would lose. That
// is the convention this codebase uses for the tests that are meant to be read rather than only run.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ------------------------------------------------------------------ §2 checksums --

// TestChecksumIsExposedOnEverySurface covers §2's acceptance criterion: "property/header round trip
// on both layouts; tree layout (hash computed on write) returns the same value as blob layout for
// identical bytes".
func TestChecksumIsExposedOnEverySurface(t *testing.T) {
	// The same bytes through both layouts, because the whole claim is that the hash is computed
	// identically — the blob layout takes it from the content address, the tree layout computes it
	// while streaming, and a divergence between them would mean a client verifying a tree-layout
	// download gets a false alarm.
	const content = "checksums are plumbing, not new hashing"
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	for _, layoutName := range []string{"blob", "tree"} {
		t.Run(layoutName, func(t *testing.T) {
			h := harnessOn(t, layoutName, nil)
			h.uploadFile(t, "hashed.txt", content)

			// 1. The JSON listing carries it.
			resp := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
			body := readBody(resp)
			resp.Body.Close()
			if !strings.Contains(body, `"sha256":"`+want+`"`) {
				t.Fatalf("the listing does not carry the content address %s: %s", want, body)
			}

			// 2. The download response carries OC-Checksum.
			get := h.do(http.MethodGet, "/api/files/download?path=hashed.txt", nil, nil)
			get.Body.Close()
			if got := get.Header.Get("OC-Checksum"); got != "SHA256:"+want {
				t.Fatalf("OC-Checksum = %q on a download, want SHA256:%s", got, want)
			}

			// 3. A re-upload answers with the same header, so a client learns the address without a
			//    second request.
			mb := multipartBody(t, "", "again.txt", content)
			put := h.do(http.MethodPost, "/api/files/upload", mb,
				map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
			put.Body.Close()
			if got := put.Header.Get("OC-Checksum"); got != "SHA256:"+want {
				t.Fatalf("OC-Checksum = %q on a PUT, want SHA256:%s", got, want)
			}

			// 4. WebDAV PROPFIND returns the ownCloud property, which is what a third-party client
			//    looks for.
			pf := h.propfind(t, "hashed.txt",
				`<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">`+
					`<D:prop><oc:checksums/></D:prop></D:propfind>`)
			if !strings.Contains(pf, "<oc:checksum>SHA256:"+want+"</oc:checksum>") {
				t.Fatalf("PROPFIND did not return the checksum property:\n%s", pf)
			}

			// 5. WebDAV GET carries the header too.
			davGet := h.davRequest(t, http.MethodGet, "hashed.txt", nil)
			davGet.Body.Close()
			if got := davGet.Header.Get("OC-Checksum"); got != "SHA256:"+want {
				t.Fatalf("OC-Checksum = %q on a WebDAV GET, want SHA256:%s", got, want)
			}
		})
	}
}

// TestVerifyOnUploadRefusesAndLeavesTheOldFile is §2's other acceptance criterion: "verify-on-upload
// refusal leaves the previous file untouched".
//
// The interesting half is the SECOND assertion. A mismatch that refuses the new write but has
// already replaced the old one is worse than no verification at all, because the client is told
// something was wrong after it destroyed the good copy.
func TestVerifyOnUploadRefusesAndLeavesTheOldFile(t *testing.T) {
	h := harnessOn(t, "blob", nil)
	h.uploadFile(t, "protected.txt", "the original, correct bytes")

	// A checksum that does not match what is being uploaded.
	wrong := fmt.Sprintf("SHA256:%x", sha256.Sum256([]byte("something else entirely")))
	mb := multipartBody(t, "", "protected.txt", "the replacement, corrupt in transit")
	resp := h.do(http.MethodPost, "/api/files/upload", mb, map[string]string{
		"Content-Type":  "multipart/form-data; boundary=" + mb.Boundary(),
		"X-OC-Checksum": wrong,
	})
	body := readBody(resp)
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a mismatched X-OC-Checksum answered %d (%s), want 422", resp.StatusCode, body)
	}
	if !strings.Contains(body, "checksum_mismatch") {
		t.Fatalf("the refusal does not name checksum_mismatch, so a client cannot branch on it: %s",
			body)
	}

	// The old content is still what the file serves.
	get := h.do(http.MethodGet, "/api/files/download?path=protected.txt", nil, nil)
	got := readBody(get)
	get.Body.Close()
	if got != "the original, correct bytes" {
		t.Fatalf("the refused upload replaced the file: it now serves %q. Verify-on-upload must "+
			"refuse BEFORE publish, or it destroys the copy it exists to protect.", got)
	}
}

// TestVerifyOnUploadAcceptsAMatchingChecksum is the positive half: verification that never passes is
// a feature nobody can use.
func TestVerifyOnUploadAcceptsAMatchingChecksum(t *testing.T) {
	h := harnessOn(t, "blob", nil)
	const content = "verified in transit"
	sum := sha256.Sum256([]byte(content))

	mb := multipartBody(t, "", "verified.txt", content)
	resp := h.do(http.MethodPost, "/api/files/upload", mb, map[string]string{
		"Content-Type":  "multipart/form-data; boundary=" + mb.Boundary(),
		"X-OC-Checksum": fmt.Sprintf("SHA256:%x", sum),
	})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a matching checksum answered %d (%s), want 201", resp.StatusCode, body)
	}
}

// ------------------------------------------------------------------ §3 empty trash --

// TestEmptyTrashPurgesOnlyTheCallersOwnBin covers §3's acceptance criteria: the endpoint works, it is
// scoped to the caller, and a non-owner's items are untouched.
func TestEmptyTrashPurgesOnlyTheCallersOwnBin(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "mine-gone.txt", "my bytes")

	// Another account with a trashed file of its own.
	mate, mateToken := h.intruder(t, "trashmate")
	mb := multipartBody(t, "", "theirs-gone.txt", "their bytes")
	resp := h.doAs(t, mateToken, http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	resp.Body.Close()
	del := h.doAs(t, mateToken, http.MethodDelete, "/api/files/?path=theirs-gone.txt", nil, nil)
	del.Body.Close()

	// The caller trashes their own file.
	d := h.do(http.MethodDelete, "/api/files/?path=mine-gone.txt", nil, nil)
	d.Body.Close()

	empty := h.do(http.MethodPost, "/api/files/trash/empty", nil, nil)
	body := readBody(empty)
	empty.Body.Close()
	if empty.StatusCode != http.StatusOK {
		t.Fatalf("empty trash: %d %s", empty.StatusCode, body)
	}

	var out struct {
		Purged     int   `json:"purged"`
		Nodes      int   `json:"nodes"`
		BytesFreed int64 `json:"bytes_freed"`
		More       bool  `json:"more"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Purged != 1 {
		t.Fatalf("purged %d roots, want the caller's one (body: %s)", out.Purged, body)
	}
	if out.BytesFreed != int64(len("my bytes")) {
		t.Fatalf("bytes_freed = %d, want %d", out.BytesFreed, len("my bytes"))
	}

	// The caller's bin is empty; the other account's is not.
	if items := trashItems(t, h); len(items) != 0 {
		t.Fatalf("the caller's trash still holds %d item(s)", len(items))
	}
	if items := trashItemsAs(t, h, mateToken); len(items) != 1 {
		t.Fatalf("the other account's trash holds %d item(s), want its own 1: an empty-trash must "+
			"be scoped to the caller", len(items))
	}
	_ = mate
}

// trashItemsAs reads another account's trash listing.
func trashItemsAs(t *testing.T, h *harness, token string) []map[string]any {
	t.Helper()
	resp := h.doAs(t, token, http.MethodGet, "/api/files/trash", nil, nil)
	defer resp.Body.Close()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode trash listing: %v", err)
	}
	return out.Items
}

// TestEmptyTrashIsAuditedWithActorAttribution pins §3's audit requirement.
func TestEmptyTrashIsAuditedWithActorAttribution(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "audited.txt", "bytes")
	d := h.do(http.MethodDelete, "/api/files/?path=audited.txt", nil, nil)
	d.Body.Close()

	empty := h.do(http.MethodPost, "/api/files/trash/empty", nil, nil)
	empty.Body.Close()

	var found bool
	for _, entry := range h.meta.AuditLog() {
		if entry.Action != "trash.purge" {
			continue
		}
		if entry.ActorUserID == nil || *entry.ActorUserID != h.user.ID {
			t.Fatalf("trash.purge was recorded with actor %v, want the caller: an operator reading "+
				"the log has to be able to tell a user's action from a retention sweep",
				entry.ActorUserID)
		}
		// `via=user` is the field that distinguishes this from the worker's sweep, which emits the
		// same action. Without it the two are indistinguishable in the log.
		if via, _ := entry.Metadata["via"].(string); via != "user" {
			t.Fatalf("trash.purge metadata says via=%v, want \"user\": the worker's retention sweep "+
				"emits the same action and the two must be distinguishable", entry.Metadata["via"])
		}
		found = true
	}
	if !found {
		t.Fatal("empty trash wrote no trash.purge audit event; §3 requires the purge to be audited " +
			"with actor attribution")
	}
}

// TestEmptyTrashIsIdempotent: a client that retries after a timeout must be safe.
func TestEmptyTrashIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "once.txt", "bytes")
	d := h.do(http.MethodDelete, "/api/files/?path=once.txt", nil, nil)
	d.Body.Close()

	for i := 0; i < 2; i++ {
		resp := h.do(http.MethodPost, "/api/files/trash/empty", nil, nil)
		body := readBody(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d: %d %s", i+1, resp.StatusCode, body)
		}
		if i == 1 {
			var out struct {
				Purged int  `json:"purged"`
				More   bool `json:"more"`
			}
			_ = json.Unmarshal([]byte(body), &out)
			if out.Purged != 0 || out.More {
				t.Fatalf("the second call purged %d roots (more=%v) from an empty bin; a retry "+
					"after a timeout must be a no-op", out.Purged, out.More)
			}
		}
	}
}

// ------------------------------------------------------------------ §4 favorites --

// TestFavoritesToggleAndList walks §4's user-visible contract.
func TestFavoritesToggleAndList(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "starred.txt", "bytes")
	h.uploadFile(t, "plain.txt", "bytes")

	// Star it, twice: the second is a retry, and it must not be an error or a second row.
	for i := 0; i < 2; i++ {
		resp := h.do(http.MethodPut, "/api/files/favorite?path=starred.txt", nil, nil)
		body := readBody(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("star %d: %d %s", i+1, resp.StatusCode, body)
		}
	}

	// The listing shows it, and only it.
	resp := h.do(http.MethodGet, "/api/favorites", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("favorites: %d %s", resp.StatusCode, body)
	}
	var listed struct {
		Items []struct {
			Name    string `json:"name"`
			Path    string `json:"path"`
			Starred bool   `json:"starred"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(listed.Items) != 1 || listed.Items[0].Name != "starred.txt" {
		t.Fatalf("the favorites listing returned %d item(s), want only starred.txt: %s",
			len(listed.Items), body)
	}
	if !listed.Items[0].Starred {
		t.Fatal("a row in the favorites listing does not report itself as starred")
	}
	if listed.Items[0].Path != "/starred.txt" {
		t.Fatalf("the favorites row has path %q, want /starred.txt: a favorite you cannot jump to "+
			"is a dead entry", listed.Items[0].Path)
	}

	// The ordinary listing carries the flag too, so the UI can draw the star without a second call.
	ls := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	listBody := readBody(ls)
	ls.Body.Close()
	if !strings.Contains(listBody, `"name":"starred.txt","path":"/starred.txt"`) ||
		!strings.Contains(listBody, `"starred":true`) {
		t.Fatalf("the file listing does not carry the star flag: %s", listBody)
	}

	// Un-star, twice for the same reason, then the listing is empty.
	for i := 0; i < 2; i++ {
		resp := h.do(http.MethodDelete, "/api/files/favorite?path=starred.txt", nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("un-star %d: %d", i+1, resp.StatusCode)
		}
	}
	empty := h.do(http.MethodGet, "/api/favorites", nil, nil)
	if items := favoritesItems(t, empty); len(items) != 0 {
		t.Fatalf("the favorites listing still holds %d item(s) after un-starring", len(items))
	}
}

// favoritesItems decodes a favorites listing.
func favoritesItems(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode favorites: %v", err)
	}
	return out.Items
}

// TestARevokedGrantDropsTheFavorite is §4's "revoked access drops it from listing".
//
// The row survives; the LISTING re-checks the grant. That distinction matters: deleting the row on
// revocation would mean re-granting access silently loses the person's bookmarks.
func TestARevokedGrantDropsTheFavorite(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "shared")
	h.uploadAt(t, "shared/inside.txt", "shared bytes")

	dir := h.resolveNode(t, "shared")
	mate, mateToken := h.intruder(t, "favorite-grantee")
	h.grantOn(t, dir.ID, mate.ID, store.PermRead|store.PermListChildren, true)

	// The grantee stars a file inside the shared folder, addressed by id — its path is in the
	// owner's namespace, so ?path= cannot name it.
	inside := h.resolveNode(t, "shared/inside.txt")
	star := h.doAs(t, mateToken, http.MethodPut,
		"/api/files/favorite?id="+inside.ID.String(), nil, nil)
	starBody := readBody(star)
	star.Body.Close()
	if star.StatusCode != http.StatusOK {
		t.Fatalf("a grantee could not star a file they can read: %d %s",
			star.StatusCode, starBody)
	}

	listed := h.doAs(t, mateToken, http.MethodGet, "/api/favorites", nil, nil)
	if items := favoritesItems(t, listed); len(items) != 1 {
		t.Fatalf("the grantee's favorites listing holds %d item(s), want 1", len(items))
	}

	// Revoke the grant.
	h.revokeGrantsOn(t, dir.ID, mate.ID)

	after := h.doAs(t, mateToken, http.MethodGet, "/api/favorites", nil, nil)
	if items := favoritesItems(t, after); len(items) != 0 {
		t.Fatalf("the favorites listing still returns %d item(s) after the grant was revoked; a "+
			"revocation must take effect on the listing, not only on the file", len(items))
	}
}

// TestWebDAVFavoriteIsALiveProperty covers §4's WebDAV half: PROPPATCH sets, PROPFIND reads.
func TestWebDAVFavoriteIsALiveProperty(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "davstar.txt", "bytes")

	// Set it the way an NC-ecosystem client does.
	set := h.davRequest(t, "PROPPATCH", "davstar.txt", strings.NewReader(
		`<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" `+
			`xmlns:oc="http://owncloud.org/ns"><D:set><D:prop>`+
			`<oc:favorite>1</oc:favorite></D:prop></D:set></D:propertyupdate>`))
	setBody := readBody(set)
	set.Body.Close()
	if set.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH oc:favorite: %d %s", set.StatusCode, setBody)
	}

	// It is a LIVE property, so it must come back as one rather than as a dead property the client
	// stored itself. The distinction is visible: a dead property would be per NODE, and the second
	// account below would then see it too.
	read := h.propfind(t, "davstar.txt",
		`<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">`+
			`<D:prop><oc:favorite/></D:prop></D:propfind>`)
	if !strings.Contains(read, "<oc:favorite>1</oc:favorite>") {
		t.Fatalf("PROPFIND did not report the star:\n%s", read)
	}

	// Another account reading the same node must NOT see it: a favorite is per user.
	mate, mateToken := h.intruder(t, "other-star")
	_ = mate
	other := h.davRequestAs(t, mateToken, "PROPFIND", "davstar.txt", strings.NewReader(
		`<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">`+
			`<D:prop><oc:favorite/></D:prop></D:propfind>`))
	otherBody := readBody(other)
	other.Body.Close()
	if strings.Contains(otherBody, "<oc:favorite>1</oc:favorite>") {
		t.Fatalf("a second account sees the first account's star; a favorite is a per-USER row, "+
			"and storing it as a node property is exactly what §4 rules out:\n%s", otherBody)
	}
}

// ------------------------------------------------------------------ §5 tags --

// TestTagLifecycle covers §5's API surface end to end.
func TestTagLifecycle(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "labelled.txt", "bytes")
	node := h.resolveNode(t, "labelled.txt")

	created := h.do(http.MethodPost, "/api/tags/",
		jsonBody(t, map[string]any{"name": "Finance", "color": "blue"}),
		map[string]string{"Content-Type": "application/json"})
	body := readBody(created)
	created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create tag: %d %s", created.StatusCode, body)
	}
	var tag struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.Unmarshal([]byte(body), &tag); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	// A case-fold collision is refused, which is §5's first acceptance criterion.
	dup := h.do(http.MethodPost, "/api/tags/",
		jsonBody(t, map[string]any{"name": "FINANCE", "color": "red"}),
		map[string]string{"Content-Type": "application/json"})
	dupBody := readBody(dup)
	dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("a tag differing only in case answered %d (%s), want 409: two labels a user "+
			"cannot tell apart would fragment the filter they exist to drive",
			dup.StatusCode, dupBody)
	}

	// An off-palette colour is refused: the value reaches CSS.
	bad := h.do(http.MethodPost, "/api/tags/",
		jsonBody(t, map[string]any{"name": "Odd", "color": "chartreuse"}),
		map[string]string{"Content-Type": "application/json"})
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an off-palette colour answered %d, want 422", bad.StatusCode)
	}

	// Assign it.
	assign := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/tags",
		jsonBody(t, map[string]any{"tag_id": tag.ID}),
		map[string]string{"Content-Type": "application/json"})
	assignBody := readBody(assign)
	assign.Body.Close()
	if assign.StatusCode != http.StatusOK {
		t.Fatalf("assign tag: %d %s", assign.StatusCode, assignBody)
	}
	if !strings.Contains(assignBody, `"name":"Finance"`) {
		t.Fatalf("the assignment response does not carry the resulting tag set: %s", assignBody)
	}

	// The listing shows the chip.
	ls := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	lsBody := readBody(ls)
	ls.Body.Close()
	if !strings.Contains(lsBody, `"tags":[{"id":"`+tag.ID) {
		t.Fatalf("the file listing does not carry the tag: %s", lsBody)
	}

	// WebDAV returns it read-only.
	pf := h.propfind(t, "labelled.txt",
		`<D:propfind xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">`+
			`<D:prop><oc:tags/></D:prop></D:propfind>`)
	if !strings.Contains(pf, "<oc:tags>Finance</oc:tags>") {
		t.Fatalf("PROPFIND did not return oc:tags:\n%s", pf)
	}

	// And PROPPATCH on it is refused with a propstat note, which is what §5 asks for.
	patch := h.davRequest(t, "PROPPATCH", "labelled.txt", strings.NewReader(
		`<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" `+
			`xmlns:oc="http://owncloud.org/ns"><D:set><D:prop>`+
			`<oc:tags>Hijacked</oc:tags></D:prop></D:set></D:propertyupdate>`))
	patchBody := readBody(patch)
	patch.Body.Close()
	if !strings.Contains(patchBody, "403 Forbidden") {
		t.Fatalf("PROPPATCH on oc:tags was not refused with a propstat note:\n%s", patchBody)
	}

	// Deleting the tag removes the assignment, so the chip goes.
	del := h.do(http.MethodDelete, "/api/tags/"+tag.ID, nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete tag: %d", del.StatusCode)
	}
	after := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	afterBody := readBody(after)
	after.Body.Close()
	if strings.Contains(afterBody, `"name":"Finance"`) {
		t.Fatalf("the tag survived its own deletion in the listing: %s", afterBody)
	}
}

// TestTagAssignmentRequiresWrite is §5's second acceptance criterion: "assignment on a node the
// caller cannot write is 404".
func TestTagAssignmentRequiresWrite(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "readonly.txt", "bytes")
	node := h.resolveNode(t, "readonly.txt")

	mate, mateToken := h.intruder(t, "read-only-grantee")
	// Read, but NOT write.
	h.grantOn(t, node.ID, mate.ID, store.PermRead, false)

	tag := h.do(http.MethodPost, "/api/tags/",
		jsonBody(t, map[string]any{"name": "attempt"}),
		map[string]string{"Content-Type": "application/json"})
	tagBody := readBody(tag)
	tag.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(tagBody), &created)

	resp := h.doAs(t, mateToken, http.MethodPost, "/api/nodes/"+node.ID.String()+"/tags",
		jsonBody(t, map[string]any{"tag_id": created.ID}),
		map[string]string{"Content-Type": "application/json"})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a read-only grantee tagged a node they cannot write: %d (%s), want 404. "+
			"Assignment is a metadata write (§5).", resp.StatusCode, body)
	}
	_ = mate
}

// TestSystemTagRequiresAnAdmin covers §5's admin rule and the audit it requires (§8).
func TestSystemTagRequiresAnAdmin(t *testing.T) {
	h := newHarness(t)

	// A plain user asking for a system tag is refused rather than silently given a personal one.
	resp := h.do(http.MethodPost, "/api/tags/",
		jsonBody(t, map[string]any{"name": "Retention", "system": true}),
		map[string]string{"Content-Type": "application/json"})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-admin created a system tag: %d (%s), want 403. A silent downgrade to a "+
			"personal tag would make \"everyone sees this label\" false.", resp.StatusCode, body)
	}
}

// TestTheTagFilterReturnsExactlyTheTaggedNodes is §5's third acceptance criterion, through the API.
func TestTheTagFilterReturnsExactlyTheTaggedNodes(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "tagged-report.txt", "bytes")
	h.uploadFile(t, "plain-report.txt", "bytes")
	node := h.resolveNode(t, "tagged-report.txt")

	created := h.do(http.MethodPost, "/api/tags/",
		jsonBody(t, map[string]any{"name": "reviewed"}),
		map[string]string{"Content-Type": "application/json"})
	tagBody := readBody(created)
	created.Body.Close()
	var tag struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(tagBody), &tag)

	assign := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/tags",
		jsonBody(t, map[string]any{"tag_id": tag.ID}),
		map[string]string{"Content-Type": "application/json"})
	assign.Body.Close()

	resp := h.do(http.MethodGet, "/api/files/search?q=report&tag="+tag.ID, nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered search: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if len(out.Items) != 1 || out.Items[0].Name != "tagged-report.txt" {
		t.Fatalf("the tag filter returned %d item(s), want only tagged-report.txt: %s",
			len(out.Items), body)
	}
}

// ------------------------------------------------------------------ §6 comments --

// TestCommentThreadLifecycle covers §6's API surface.
func TestCommentThreadLifecycle(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "discussed.txt", "bytes")
	node := h.resolveNode(t, "discussed.txt")

	// Post a top-level comment.
	post := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
		jsonBody(t, map[string]any{"body": "what is this for?"}),
		map[string]string{"Content-Type": "application/json"})
	postBody := readBody(post)
	post.Body.Close()
	if post.StatusCode != http.StatusCreated {
		t.Fatalf("post: %d %s", post.StatusCode, postBody)
	}
	var top struct {
		ID     string `json:"id"`
		Author string `json:"author"`
	}
	if err := json.Unmarshal([]byte(postBody), &top); err != nil {
		t.Fatalf("decode: %v (%s)", err, postBody)
	}
	if top.Author == "" {
		t.Fatal("a comment does not carry its author's name; the thread would show raw ids")
	}

	// Reply to it.
	reply := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
		jsonBody(t, map[string]any{"body": "the quarterly numbers", "parent_id": top.ID}),
		map[string]string{"Content-Type": "application/json"})
	replyBody := readBody(reply)
	reply.Body.Close()
	if reply.StatusCode != http.StatusCreated {
		t.Fatalf("reply: %d %s", reply.StatusCode, replyBody)
	}

	// Read the thread: oldest first, both rows, the reply naming its parent.
	list := h.do(http.MethodGet, "/api/nodes/"+node.ID.String()+"/comments", nil, nil)
	listBody := readBody(list)
	list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %s", list.StatusCode, listBody)
	}
	var thread struct {
		Items []struct {
			Body     string `json:"body"`
			ParentID string `json:"parent_id"`
			Deleted  bool   `json:"deleted"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(listBody), &thread); err != nil {
		t.Fatalf("decode: %v (%s)", err, listBody)
	}
	if len(thread.Items) != 2 {
		t.Fatalf("the thread holds %d comment(s), want 2: %s", len(thread.Items), listBody)
	}
	if thread.Items[0].Body != "what is this for?" {
		t.Fatalf("the thread reads %q first, want the question: a discussion reads in the order "+
			"it happened", thread.Items[0].Body)
	}
	if thread.Items[1].ParentID != top.ID {
		t.Fatalf("the reply lost its parent (%q)", thread.Items[1].ParentID)
	}

	// Edit it, and the response says so.
	edit := h.do(http.MethodPatch, "/api/comments/"+top.ID,
		jsonBody(t, map[string]any{"body": "what is this FOR?"}),
		map[string]string{"Content-Type": "application/json"})
	editBody := readBody(edit)
	edit.Body.Close()
	if edit.StatusCode != http.StatusOK {
		t.Fatalf("edit: %d %s", edit.StatusCode, editBody)
	}
	if !strings.Contains(editBody, `"edited_at":"`) {
		t.Fatalf("an edited comment does not carry edited_at: %s", editBody)
	}

	// Delete it: the row becomes a tombstone with no body.
	del := h.do(http.MethodDelete, "/api/comments/"+top.ID, nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", del.StatusCode)
	}

	after := h.do(http.MethodGet, "/api/nodes/"+node.ID.String()+"/comments", nil, nil)
	afterBody := readBody(after)
	after.Body.Close()
	var tombstoned struct {
		Items []struct {
			Body    string `json:"body"`
			Deleted bool   `json:"deleted"`
		} `json:"items"`
	}
	_ = json.Unmarshal([]byte(afterBody), &tombstoned)
	if len(tombstoned.Items) != 2 {
		t.Fatalf("the thread lost the tombstone; the reply would be orphaned: %s", afterBody)
	}
	if !tombstoned.Items[0].Deleted {
		t.Fatalf("the deleted comment is not marked deleted: %s", afterBody)
	}
	if tombstoned.Items[0].Body != "" {
		t.Fatalf("the deleted comment still carries its body (%q): a tombstone that hands the "+
			"client the text it is hiding is decoration", tombstoned.Items[0].Body)
	}
}

// TestTheCommentBodyCapIsEnforcedServerSide is §6's last acceptance criterion.
func TestTheCommentBodyCapIsEnforcedServerSide(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "policy.txt", "bytes")
	node := h.resolveNode(t, "policy.txt")

	long := strings.Repeat("a", 2001)
	resp := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
		jsonBody(t, map[string]any{"body": long}),
		map[string]string{"Content-Type": "application/json"})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a %d-character comment answered %d (%s), want 422: the cap is a server-side "+
			"rule, not a client hint", len(long), resp.StatusCode, body)
	}
}

// TestTheCommentRateLimitIsEnforced covers §6's first acceptance criterion.
func TestTheCommentRateLimitIsEnforced(t *testing.T) {
	// A limit of two, so the test is three requests rather than sixty-one.
	h := harnessOn(t, "blob", func(c *config.Config) { c.Limits.CommentRatePerHour = 2 })
	h.uploadFile(t, "busy.txt", "bytes")
	node := h.resolveNode(t, "busy.txt")

	for i := 0; i < 2; i++ {
		resp := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
			jsonBody(t, map[string]any{"body": fmt.Sprintf("post %d", i)}),
			map[string]string{"Content-Type": "application/json"})
		body := readBody(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("post %d inside the allowance: %d %s", i, resp.StatusCode, body)
		}
	}

	over := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
		jsonBody(t, map[string]any{"body": "one too many"}),
		map[string]string{"Content-Type": "application/json"})
	body := readBody(over)
	over.Body.Close()
	if over.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the post past the limit answered %d (%s), want 429", over.StatusCode, body)
	}
	if !strings.Contains(body, "comment_rate_limited") {
		t.Fatalf("the refusal does not name comment_rate_limited, so a client shows a generic "+
			"failure instead of \"slow down\": %s", body)
	}
}

// TestModeratingSomebodyElsesCommentIsAudited covers §8's exception, through the API.
func TestModeratingSomebodyElsesCommentIsAudited(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "moderated.txt", "bytes")
	node := h.resolveNode(t, "moderated.txt")

	// A second account with `manage_permissions` on the node — a moderator who is not the author.
	mate, mateToken := h.intruder(t, "moderator")
	h.grantOn(t, node.ID, mate.ID, store.PermRead|store.PermManagePermissions, false)

	// The OWNER writes a comment; the moderator removes it.
	post := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
		jsonBody(t, map[string]any{"body": "the owner's words"}),
		map[string]string{"Content-Type": "application/json"})
	postBody := readBody(post)
	post.Body.Close()
	var made struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(postBody), &made)

	del := h.doAs(t, mateToken, http.MethodDelete, "/api/comments/"+made.ID, nil, nil)
	delBody := readBody(del)
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("a manage_permissions holder could not moderate: %d (%s). §6 makes moderation "+
			"part of the permission.", del.StatusCode, delBody)
	}

	// The moderation is hash-chained, and the body is NOT in the metadata (§8).
	var found bool
	for _, entry := range h.meta.AuditLog() {
		if entry.Action != "comment.moderate" {
			continue
		}
		found = true
		if entry.ActorUserID == nil || *entry.ActorUserID != mate.ID {
			t.Fatalf("comment.moderate names actor %v, want the moderator", entry.ActorUserID)
		}
		for _, v := range entry.Metadata {
			if s, ok := v.(string); ok && strings.Contains(s, "the owner's words") {
				t.Fatal("the audit metadata contains the comment body; §8 keeps bodies in the " +
					"comments table only, because a comment is user content and not a security fact")
			}
		}
	}
	if !found {
		t.Fatal("moderating another account's comment wrote no comment.moderate event; §8 makes " +
			"that the one comment action the chain records")
	}
	_ = mate
}

// TestDeletingYourOwnCommentIsNotAudited is the other half of §8: the volume rule.
func TestDeletingYourOwnCommentIsNotAudited(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "own.txt", "bytes")
	node := h.resolveNode(t, "own.txt")

	post := h.do(http.MethodPost, "/api/nodes/"+node.ID.String()+"/comments",
		jsonBody(t, map[string]any{"body": "typo"}),
		map[string]string{"Content-Type": "application/json"})
	postBody := readBody(post)
	post.Body.Close()
	var made struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(postBody), &made)

	before := len(h.meta.AuditLog())
	del := h.do(http.MethodDelete, "/api/comments/"+made.ID, nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete own comment: %d", del.StatusCode)
	}

	for _, entry := range h.meta.AuditLog()[before:] {
		if entry.Action == "comment.moderate" {
			t.Fatal("deleting your own comment was recorded as moderation; §8 keeps the volume of " +
				"\"I removed my typo\" out of the chain so the entries that matter stay visible")
		}
	}
}

// ------------------------------------------------------------------ §7 search --

// TestSearchFiltersAndSort exercises §7.1's vocabulary through the endpoint.
func TestSearchFiltersAndSort(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "docs")
	h.uploadAt(t, "docs/alpha.txt", "aaaa")
	h.uploadAt(t, "docs/bravo.txt", "bbbbbbbb")
	h.uploadAt(t, "docs/charlie.txt", "cc")

	// kind
	files := searchNames(t, h, "kind=file")
	if len(files) != 3 {
		t.Fatalf("kind=file returned %d rows, want 3: %v", len(files), files)
	}
	dirs := searchNames(t, h, "kind=dir")
	if len(dirs) != 1 || dirs[0] != "docs" {
		t.Fatalf("kind=dir returned %v, want [docs]", dirs)
	}

	// size
	big := searchNames(t, h, "size_min=5")
	if len(big) != 1 || big[0] != "bravo.txt" {
		t.Fatalf("size_min=5 returned %v, want [bravo.txt]", big)
	}

	// mime prefix
	text := searchNames(t, h, "mime=text/")
	if len(text) != 3 {
		t.Fatalf("mime=text/ returned %v, want the three text files", text)
	}

	// sort
	byName := searchNames(t, h, "sort=name")
	if strings.Join(byName, ",") != "alpha.txt,bravo.txt,charlie.txt,docs" {
		t.Fatalf("sort=name returned %v", byName)
	}
	bySize := searchNames(t, h, "sort=size")
	if len(bySize) == 0 || bySize[0] != "bravo.txt" {
		t.Fatalf("sort=size returned %v, want the largest first", bySize)
	}

	// A contradictory filter is refused rather than answered with nothing, because "no matches"
	// for an impossible question sends the user looking for a file that is not the problem.
	resp := h.do(http.MethodGet, "/api/files/search?kind=dir&mime=image/", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("kind=dir with a mime filter answered %d (%s), want 400", resp.StatusCode, body)
	}
}

// searchNames runs a search and returns the result names.
func searchNames(t *testing.T, h *harness, query string) []string {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/search?"+query, nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search %q: %d %s", query, resp.StatusCode, body)
	}
	var out struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	names := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		names = append(names, item.Name)
	}
	return names
}

// TestSearchPagesWithACursor covers the ADR-0012 contract on the new endpoint.
func TestSearchPagesWithACursor(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"p-a.txt", "p-b.txt", "p-c.txt", "p-d.txt", "p-e.txt"} {
		h.uploadFile(t, name, "x")
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		query := "sort=name&limit=2"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		resp := h.do(http.MethodGet, "/api/files/search?"+query, nil, nil)
		body := readBody(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, resp.StatusCode, body)
		}
		var out struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			Cursor    string `json:"cursor"`
			Truncated bool   `json:"truncated"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		for _, item := range out.Items {
			seen[item.Name]++
		}
		if !out.Truncated {
			break
		}
		if out.Cursor == "" {
			t.Fatal("the page reported truncated=true but returned no cursor")
		}
		cursor = out.Cursor
	}
	if len(seen) != 5 {
		t.Fatalf("paging saw %d distinct rows, want 5: %v", len(seen), seen)
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("%q appeared %d times across pages", name, n)
		}
	}
}

// TestACursorFromAnotherSortIsRefused: a cursor is a position in one ordering, not a token that means
// "page 2" in any ordering.
func TestACursorFromAnotherSortIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"c-a.txt", "c-b.txt", "c-c.txt"} {
		h.uploadFile(t, name, "x")
	}

	resp := h.do(http.MethodGet, "/api/files/search?sort=name&limit=2", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	var first struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal([]byte(body), &first)
	if first.Cursor == "" {
		t.Fatalf("a three-row result with limit 2 returned no cursor: %s", body)
	}

	bad := h.do(http.MethodGet,
		"/api/files/search?sort=size&limit=2&cursor="+url.QueryEscape(first.Cursor), nil, nil)
	badBody := readBody(bad)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("a name-sort cursor used for a size sort answered %d (%s), want 400: page 2 of "+
			"one ordering means nothing in another", bad.StatusCode, badBody)
	}
}

// TestContentSearchDegradesHonestly is §7.2's posture, stated as a test.
//
// The requirement is not "content search works without an engine" — it is that the SERVER SAYS it is
// not searching content, so a user who cannot find a phrase inside a document knows why.
func TestContentSearchDegradesHonestly(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "degraded.txt", "the phrase is in here")

	resp := h.do(http.MethodGet, "/api/files/search?q=degraded&mode=content", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a content search on an engine-less deployment answered %d (%s); §7.2 says it "+
			"degrades rather than failing", resp.StatusCode, body)
	}

	var out struct {
		Mode        string `json:"mode"`
		Degraded    bool   `json:"degraded"`
		DegradedWhy string `json:"degraded_reason"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Mode != "name" {
		t.Fatalf("mode = %q, want \"name\": the response must say which search actually ran", out.Mode)
	}
	if !out.Degraded {
		t.Fatal("the response does not set degraded=true; a user would conclude the phrase is not " +
			"in any document rather than that nothing looked inside one")
	}
	if out.DegradedWhy == "" {
		t.Fatal("the response does not explain why it degraded")
	}
}

// TestSearchCarriesTheListingMarkers is §7.1's "results carry the same markers as listings".
func TestSearchCarriesTheListingMarkers(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "marked.txt", "bytes")
	node := h.resolveNode(t, "marked.txt")

	star := h.do(http.MethodPut, "/api/files/favorite?path=marked.txt", nil, nil)
	star.Body.Close()
	_ = node

	resp := h.do(http.MethodGet, "/api/files/search?q=marked", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if !strings.Contains(body, `"starred":true`) {
		t.Fatalf("a search hit does not carry its star marker: %s", body)
	}
	if !strings.Contains(body, `"sha256":"`) {
		t.Fatalf("a search hit does not carry its content address: %s", body)
	}
}

// ------------------------------------------------------------------ §1 tus --

// TestTusSingleUploadRoundTrip is §1's first acceptance criterion, in its simplest form.
func TestTusSingleUploadRoundTrip(t *testing.T) {
	h := newHarness(t)

	// Creation, with the destination and filename as base64 metadata.
	create := h.do(http.MethodPost, "/api/files/tus", nil, map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Length": "10",
		"Upload-Metadata": tusMetadata(map[string]string{
			"filename": "tus-file.txt", "destination": "",
		}),
	})
	createBody := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", create.StatusCode, createBody)
	}
	location := create.Header.Get("Location")
	if location == "" {
		t.Fatal("creation returned no Location header; a tus client has nothing to PATCH")
	}
	if got := create.Header.Get("Tus-Resumable"); got != "1.0.0" {
		t.Fatalf("Tus-Resumable = %q on a creation response", got)
	}

	// HEAD reports offset zero.
	head := h.do(http.MethodHead, location, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", head.StatusCode)
	}
	if got := head.Header.Get("Upload-Offset"); got != "0" {
		t.Fatalf("Upload-Offset = %q before any chunk, want 0", got)
	}
	if got := head.Header.Get("Upload-Length"); got != "10" {
		t.Fatalf("Upload-Length = %q, want 10", got)
	}

	// Two PATCHes, append-at-offset.
	for i, chunk := range []struct {
		offset int
		body   string
	}{{0, "hello"}, {5, "world"}} {
		patch := h.do(http.MethodPatch, location, strings.NewReader(chunk.body), map[string]string{
			"Tus-Resumable": "1.0.0",
			"Upload-Offset": fmt.Sprint(chunk.offset),
			"Content-Type":  "application/offset+octet-stream",
		})
		patchBody := readBody(patch)
		patch.Body.Close()
		if patch.StatusCode != http.StatusNoContent {
			t.Fatalf("PATCH %d: %d %s", i, patch.StatusCode, patchBody)
		}
	}

	// The file is readable through the ordinary path.
	get := h.do(http.MethodGet, "/api/files/download?path=tus-file.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "helloworld" {
		t.Fatalf("the published file holds %q, want helloworld", body)
	}
}

// TestTusRejectsAStaleOffset is §1's explicit acceptance criterion: "a PATCH with a stale offset is
// 409 and changes nothing".
func TestTusRejectsAStaleOffset(t *testing.T) {
	h := newHarness(t)
	location := h.tusCreate(t, "stale.txt", 10)

	first := h.do(http.MethodPatch, location, strings.NewReader("hello"), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": "0",
		"Content-Type":  "application/offset+octet-stream",
	})
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first chunk: %d", first.StatusCode)
	}

	// Re-send at 0: the server is at 5.
	stale := h.do(http.MethodPatch, location, strings.NewReader("hello"), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": "0",
		"Content-Type":  "application/offset+octet-stream",
	})
	staleBody := readBody(stale)
	stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("a stale offset answered %d (%s), want 409", stale.StatusCode, staleBody)
	}
	// The response tells the client where the server ACTUALLY is, which is what makes resume work.
	if got := stale.Header.Get("Upload-Offset"); got != "5" {
		t.Fatalf("the 409 reports Upload-Offset %q, want the true 5: a client that is not told "+
			"cannot resume", got)
	}

	// And nothing changed.
	head := h.do(http.MethodHead, location, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	head.Body.Close()
	if got := head.Header.Get("Upload-Offset"); got != "5" {
		t.Fatalf("the stale PATCH moved the offset to %q", got)
	}
}

// TestTusChecksumMismatchIs460 is §1's other explicit criterion: "a checksum mismatch is 460 and the
// acknowledged set does not include the chunk".
func TestTusChecksumMismatchIs460(t *testing.T) {
	h := newHarness(t)
	location := h.tusCreate(t, "checked.txt", 10)

	wrong := base64.StdEncoding.EncodeToString(sha256.New().Sum(nil))
	patch := h.do(http.MethodPatch, location, strings.NewReader("hello"), map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Offset":   "0",
		"Content-Type":    "application/offset+octet-stream",
		"Upload-Checksum": "sha256 " + wrong,
	})
	patchBody := readBody(patch)
	patch.Body.Close()
	if patch.StatusCode != 460 {
		t.Fatalf("a checksum mismatch answered %d (%s), want 460", patch.StatusCode, patchBody)
	}

	// The offset did not advance: the chunk is not acknowledged, so the retry sends the same range.
	head := h.do(http.MethodHead, location, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	head.Body.Close()
	if got := head.Header.Get("Upload-Offset"); got != "0" {
		t.Fatalf("Upload-Offset = %q after a checksum mismatch, want 0: an acknowledged range that "+
			"failed verification tells the client the corrupt chunk is already there", got)
	}

	// The correct checksum for the same bytes succeeds, which proves the 460 was about the digest
	// rather than about the request being unacceptable.
	right := base64.StdEncoding.EncodeToString(func() []byte {
		sum := sha256.Sum256([]byte("hello"))
		return sum[:]
	}())
	retry := h.do(http.MethodPatch, location, strings.NewReader("hello"), map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Offset":   "0",
		"Content-Type":    "application/offset+octet-stream",
		"Upload-Checksum": "sha256 " + right,
	})
	retryBody := readBody(retry)
	retry.Body.Close()
	if retry.StatusCode != http.StatusNoContent {
		t.Fatalf("the retry with the right checksum answered %d (%s)", retry.StatusCode, retryBody)
	}
}

// TestTusCreationWithUploadIsOneRoundTrip covers the extension §1 lists for small files.
func TestTusCreationWithUploadIsOneRoundTrip(t *testing.T) {
	h := newHarness(t)

	create := h.do(http.MethodPost, "/api/files/tus", strings.NewReader("one round trip"),
		map[string]string{
			"Tus-Resumable": "1.0.0",
			"Upload-Length": fmt.Sprint(len("one round trip")),
			"Content-Type":  "application/offset+octet-stream",
			"Upload-Metadata": tusMetadata(map[string]string{
				"filename": "single.txt",
			}),
		})
	createBody := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("creation-with-upload: %d %s", create.StatusCode, createBody)
	}

	get := h.do(http.MethodGet, "/api/files/download?path=single.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "one round trip" {
		t.Fatalf("the file holds %q after a creation-with-upload", body)
	}
}

// TestTusTerminationDiscardsTheSession covers the termination extension.
func TestTusTerminationDiscardsTheSession(t *testing.T) {
	h := newHarness(t)
	location := h.tusCreate(t, "terminated.txt", 10)

	del := h.do(http.MethodDelete, location, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", del.StatusCode)
	}

	// The session is gone, and indistinguishable from one that never existed.
	head := h.do(http.MethodHead, location, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	head.Body.Close()
	if head.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD after termination answered %d, want 404", head.StatusCode)
	}
}

// TestTusDeferredLength covers the creation-defer-length extension.
func TestTusDeferredLength(t *testing.T) {
	h := newHarness(t)

	create := h.do(http.MethodPost, "/api/files/tus", nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   "deferred",
		"Upload-Metadata": tusMetadata(map[string]string{"filename": "deferred.txt"}),
	})
	createBody := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("deferred creation: %d %s", create.StatusCode, createBody)
	}
	location := create.Header.Get("Location")

	// The length is declared with the final chunk.
	patch := h.do(http.MethodPatch, location, strings.NewReader("deferred body"), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": "0",
		"Upload-Length": fmt.Sprint(len("deferred body")),
		"Content-Type":  "application/offset+octet-stream",
	})
	patchBody := readBody(patch)
	patch.Body.Close()
	if patch.StatusCode != http.StatusNoContent {
		t.Fatalf("the deferred final chunk answered %d (%s)", patch.StatusCode, patchBody)
	}

	get := h.do(http.MethodGet, "/api/files/download?path=deferred.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "deferred body" {
		t.Fatalf("the deferred upload holds %q", body)
	}
}

// TestTusConcatJoinsPartialUploads covers the concatenation extension, which is how tus-js-client
// implements parallelUploads.
func TestTusConcatJoinsPartialUploads(t *testing.T) {
	h := newHarness(t)

	// Two partials, each complete.
	var locations []string
	for i, part := range []string{"first half ", "second half"} {
		loc := h.tusCreatePartial(t, fmt.Sprintf("part-%d", i), part)
		locations = append(locations, h.srv.URL+loc)
	}

	// The final request names both.
	final := h.do(http.MethodPost, "/api/files/tus", nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Concat":   "final;" + strings.Join(locations, " "),
		"Upload-Metadata": tusMetadata(map[string]string{"filename": "joined.txt"}),
	})
	finalBody := readBody(final)
	final.Body.Close()
	if final.StatusCode != http.StatusCreated {
		t.Fatalf("concat final: %d %s", final.StatusCode, finalBody)
	}

	get := h.do(http.MethodGet, "/api/files/download?path=joined.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "first half second half" {
		t.Fatalf("the joined file holds %q, want the two parts in order", body)
	}
}

// TestTusOptionsAdvertisesTheExtensionsItImplements: advertising one it does not makes a client take
// a path that fails mid-upload.
func TestTusOptionsAdvertisesTheExtensionsItImplements(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodOptions, "/api/files/tus", nil, nil)
	body := readBody(resp)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Tus-Version"); got != "1.0.0" {
		t.Fatalf("Tus-Version = %q", got)
	}
	ext := resp.Header.Get("Tus-Extension")
	for _, want := range []string{"creation", "creation-with-upload", "creation-defer-length",
		"expiration", "termination", "checksum", "concatenation"} {
		if !strings.Contains(ext, want) {
			t.Fatalf("Tus-Extension does not advertise %q: %s", want, ext)
		}
	}
}

// TestTheBespokeUploadEndpointsAreMarkedDeprecated is §1's deprecation requirement.
func TestTheBespokeUploadEndpointsAreMarkedDeprecated(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/files/uploads",
		jsonBody(t, map[string]any{"path": "", "name": "deprecated.txt", "size": 4}),
		map[string]string{"Content-Type": "application/json"})
	resp.Body.Close()

	if resp.Header.Get("Deprecation") == "" {
		t.Fatal("the bespoke upload endpoint does not send a Deprecation header; §1 asks for a " +
			"header pair, because a changelog entry is read by a person and a header by the code")
	}
	if resp.Header.Get("Sunset") == "" {
		t.Fatal("the bespoke upload endpoint does not send a Sunset header")
	}
}

// tusCreate starts a session and returns its Location.
func (h *harness) tusCreate(t *testing.T, name string, size int) string {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/files/tus", nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   fmt.Sprint(size),
		"Upload-Metadata": tusMetadata(map[string]string{"filename": name}),
	})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("tus create %q: %d %s", name, resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("tus create returned no Location")
	}
	return loc
}

// tusCreatePartial creates and completes a partial upload, returning its Location.
func (h *harness) tusCreatePartial(t *testing.T, name, body string) string {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/files/tus", nil, map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   fmt.Sprint(len(body)),
		"Upload-Concat":   "partial",
		"Upload-Metadata": tusMetadata(map[string]string{"filename": name}),
	})
	createBody := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("partial create: %d %s", resp.StatusCode, createBody)
	}
	loc := resp.Header.Get("Location")

	patch := h.do(http.MethodPatch, loc, strings.NewReader(body), map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": "0",
		"Content-Type":  "application/offset+octet-stream",
	})
	patchBody := readBody(patch)
	patch.Body.Close()
	if patch.StatusCode != http.StatusNoContent {
		t.Fatalf("partial patch: %d %s", patch.StatusCode, patchBody)
	}
	return loc
}

// tusMetadata encodes an Upload-Metadata header.
func tusMetadata(pairs map[string]string) string {
	out := make([]string, 0, len(pairs))
	for k, v := range pairs {
		out = append(out, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(out, ",")
}

// propfind issues a WebDAV PROPFIND as the harness account and returns the body.
func (h *harness) propfind(t *testing.T, name, body string) string {
	t.Helper()
	resp := h.davRequest(t, "PROPFIND", name, strings.NewReader(body))
	defer resp.Body.Close()
	return readBody(resp)
}

// davRequest issues a WebDAV request as the harness account, with an app password.
func (h *harness) davRequest(t *testing.T, method, name string, body io.Reader) *http.Response {
	t.Helper()
	return h.davRequestAs(t, h.appPassword(t), method, name, body)
}

// davRequestAs issues a WebDAV request with a specific app password.
func (h *harness) davRequestAs(t *testing.T, secret, method, name string,
	body io.Reader) *http.Response {

	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+"/dav/"+h.user.Username+"/"+name, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(h.user.Username, secret)
	if method == "PROPFIND" {
		// Depth 0: the properties of the addressee itself, which is what these assertions are about.
		// Depth 1 would also list children and make a "does the body contain this property" check
		// pass for the wrong row.
		req.Header.Set("Depth", "0")
		req.Header.Set("Content-Type", "application/xml")
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// uploadPath uploads a file to a slash-separated path under the harness account.
func (h *harness) uploadAt(t *testing.T, fullPath, content string) store.Node {
	t.Helper()
	dir, name := path.Split(fullPath)
	dir = strings.TrimSuffix(dir, "/")

	mb := multipartBody(t, dir, name, content)
	resp := h.do(http.MethodPost, "/api/files/upload", mb,
		map[string]string{"Content-Type": "multipart/form-data; boundary=" + mb.Boundary()})
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: %d %s", fullPath, resp.StatusCode, body)
	}
	return h.resolveNode(t, fullPath)
}

// revokeGrantsOn removes every grant this user holds on a node.
func (h *harness) revokeGrantsOn(t *testing.T, nodeID, userID uuid.UUID) {
	t.Helper()
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		entries, err := tx.ACLForNode(ctx, nodeID)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.UserID != nil && *e.UserID == userID {
				return tx.Revoke(ctx, e.ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
