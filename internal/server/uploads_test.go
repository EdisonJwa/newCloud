package server_test

// The resumable upload endpoints, exercised over HTTP.
//
// The unit tests in internal/upload cover the session state machine. These cover the part that
// only exists at the protocol boundary: Content-Range parsing, authorization on the target
// directory, the 409 that tells a client where to resume, and that a completed upload produces
// exactly the same node a single-request upload would.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

type uploadRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type uploadSession struct {
	ID        string        `json:"id"`
	Offset    int64         `json:"offset"`
	ChunkSize int64         `json:"chunk_size"`
	Ranges    []uploadRange `json:"ranges"`
}

func startUpload(t *testing.T, h *harness, dir, name string, size int64) uploadSession {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/files/uploads",
		jsonBody(t, map[string]any{"path": dir, "name": name, "size": size}),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.StatusCode, readBody(resp))
	}
	var out uploadSession
	if err := json.Unmarshal([]byte(readBody(resp)), &out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return out
}

func sendChunk(t *testing.T, h *harness, id string, start, end, total int64, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp := h.do(http.MethodPut, "/api/files/uploads/"+id, strings.NewReader(body), map[string]string{
		"Content-Type":  "application/octet-stream",
		"Content-Range": fmt.Sprintf("bytes %d-%d/%d", start, end, total),
	})
	raw := readBody(resp)
	var payload map[string]any
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &payload)
	}
	return resp, payload
}

// TestResumableUploadRoundTrip is the whole feature in one test: two chunks, then publish, then
// read the file back through the ordinary download endpoint.
func TestResumableUploadRoundTrip(t *testing.T) {
	h := newHarness(t)

	session := startUpload(t, h, "", "chunked.txt", 10)
	if session.Offset != 0 {
		t.Fatalf("a new session starts at 0, got %d", session.Offset)
	}

	resp, payload := sendChunk(t, h, session.ID, 0, 4, 10, "hello")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk 1: %d %v", resp.StatusCode, payload)
	}
	if payload["offset"] != float64(5) {
		t.Fatalf("offset after chunk 1 = %v, want 5", payload["offset"])
	}

	resp2, payload2 := sendChunk(t, h, session.ID, 5, 9, 10, "world")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("chunk 2: %d %v", resp2.StatusCode, payload2)
	}

	done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	defer done.Body.Close()
	if done.StatusCode != http.StatusCreated {
		t.Fatalf("complete: %d %s", done.StatusCode, readBody(done))
	}

	// The published file must be readable through the normal path, with the right bytes.
	get := h.do(http.MethodGet, "/api/files/download?path=chunked.txt", nil, nil)
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", get.StatusCode)
	}
	if body := readBody(get); body != "helloworld" {
		t.Fatalf("content = %q, want helloworld", body)
	}
}

// TestOutOfOrderChunksAndStatusOverHTTP covers the protocol surface of the parallel feature: a
// chunk beyond a hole is accepted, the status endpoint reports both the contiguous prefix and the
// acknowledged ranges, and completion waits for the hole to close.
func TestOutOfOrderChunksAndStatusOverHTTP(t *testing.T) {
	h := newHarness(t)
	session := startUpload(t, h, "", "out-of-order.txt", 12)
	if len(session.Ranges) != 0 {
		t.Fatalf("a new session reports ranges %v, want none", session.Ranges)
	}

	resp, payload := sendChunk(t, h, session.ID, 8, 11, 12, "ijkl")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("out-of-order chunk: %d %v", resp.StatusCode, payload)
	}
	if payload["offset"] != float64(0) {
		t.Fatalf("offset after the out-of-order chunk = %v, want 0 (hole at 0)", payload["offset"])
	}
	ranges := payload["ranges"].([]any)
	if len(ranges) != 1 || ranges[0].(map[string]any)["start"] != float64(8) ||
		ranges[0].(map[string]any)["end"] != float64(12) {
		t.Fatalf("ranges after the out-of-order chunk = %v, want [{8,12}]", ranges)
	}

	// Status is the authoritative half of resume: localStorage cannot know this shape.
	status := h.do(http.MethodGet, "/api/files/uploads/"+session.ID, nil, nil)
	defer status.Body.Close()
	if status.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", status.StatusCode, readBody(status))
	}
	var reported uploadSession
	if err := json.Unmarshal([]byte(readBody(status)), &reported); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if reported.Offset != 0 || len(reported.Ranges) != 1 ||
		reported.Ranges[0] != (uploadRange{8, 12}) {
		t.Fatalf("status = offset %d ranges %v", reported.Offset, reported.Ranges)
	}

	// The file is still not publishable with a hole.
	done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	if done.StatusCode != http.StatusConflict {
		t.Fatalf("complete with a hole: %d, want 409", done.StatusCode)
	}
	done.Body.Close()

	if resp, payload := sendChunk(t, h, session.ID, 0, 3, 12, "abcd"); resp.StatusCode != http.StatusOK {
		t.Fatalf("front chunk: %d %v", resp.StatusCode, payload)
	} else {
		resp.Body.Close()
	}
	resp, payload = sendChunk(t, h, session.ID, 4, 7, 12, "efgh")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || payload["offset"] != float64(12) {
		t.Fatalf("middle chunk: %d %v, want offset 12", resp.StatusCode, payload)
	}

	done = h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	if done.StatusCode != http.StatusCreated {
		t.Fatalf("complete: %d %s", done.StatusCode, readBody(done))
	}
	done.Body.Close()
	get := h.do(http.MethodGet, "/api/files/download?path=out-of-order.txt", nil, nil)
	defer get.Body.Close()
	if body := readBody(get); body != "abcdefghijkl" {
		t.Fatalf("content = %q, want abcdefghijkl", body)
	}
}

// TestChunkWithWrongOffsetReturnsTheRealOffset is what makes resume possible: the client is
// told where to continue instead of having to restart.
func TestChunkWithWrongOffsetReturnsTheRealOffset(t *testing.T) {
	h := newHarness(t)
	session := startUpload(t, h, "", "resume.txt", 8)

	resp, _ := sendChunk(t, h, session.ID, 0, 3, 8, "abcd")
	resp.Body.Close()

	// A retried first chunk: the client is at 0, the server is at 4.
	conflict, payload := sendChunk(t, h, session.ID, 0, 3, 8, "abcd")
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", conflict.StatusCode)
	}
	if payload["offset"] != float64(4) {
		t.Fatalf("conflict offset = %v, want 4", payload["offset"])
	}

	// Resuming from the reported offset finishes the file correctly.
	resp2, _ := sendChunk(t, h, session.ID, 4, 7, 8, "efgh")
	resp2.Body.Close()
	done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	done.Body.Close()

	get := h.do(http.MethodGet, "/api/files/download?path=resume.txt", nil, nil)
	defer get.Body.Close()
	if body := readBody(get); body != "abcdefgh" {
		t.Fatalf("content = %q, want abcdefgh", body)
	}
}

// TestCompleteRejectsIncompleteUpload: publishing a truncated file under the real name would be
// silent data loss.
func TestCompleteRejectsIncompleteUpload(t *testing.T) {
	h := newHarness(t)
	session := startUpload(t, h, "", "short.txt", 8)

	resp, _ := sendChunk(t, h, session.ID, 0, 3, 8, "abcd")
	resp.Body.Close()

	done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	if done.StatusCode != http.StatusConflict {
		done.Body.Close()
		t.Fatalf("want 409 for an incomplete upload, got %d", done.StatusCode)
	}
	payload := decode(t, done)
	if payload["error"] != "offset_mismatch" {
		t.Fatalf("complete conflict code = %v, want offset_mismatch", payload["error"])
	}
	if payload["offset"] != float64(4) {
		t.Fatalf("complete conflict offset = %v, want 4", payload["offset"])
	}

	// Nothing may exist under the destination name.
	list := listRoot(t, h)
	if _, ok := findFile(list, "short.txt"); ok {
		t.Fatal("an incomplete upload must not appear in the listing")
	}
}

// TestChunkRequiresAValidContentRange: a lenient parser here would write bytes at an offset
// nobody asked for.
func TestChunkRequiresAValidContentRange(t *testing.T) {
	h := newHarness(t)
	session := startUpload(t, h, "", "range.txt", 4)

	cases := []string{"", "bytes 0-3", "bytes 3-0/4", "bytes 0-3/2", "0-3/4", "bytes x-3/4"}
	for _, header := range cases {
		resp := h.do(http.MethodPut, "/api/files/uploads/"+session.ID, strings.NewReader("abcd"),
			map[string]string{"Content-Type": "application/octet-stream", "Content-Range": header})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("Content-Range %q: got %d, want 400", header, resp.StatusCode)
		}
	}
}

// TestUploadSessionIsOwnerScoped: another account must not be able to touch a session by id.
func TestUploadSessionIsOwnerScoped(t *testing.T) {
	h := newHarness(t)
	session := startUpload(t, h, "", "mine.txt", 4)

	// The harness has one user, so the token is replaced by an unrelated session id: the
	// server must treat an unknown id exactly like somebody else's.
	other := "00000000000000000000000000000000"
	resp, _ := sendChunk(t, h, other, 0, 3, 4, "abcd")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: got %d, want 404", resp.StatusCode)
	}

	done := h.do(http.MethodPost, "/api/files/uploads/"+other+"/complete", nil, nil)
	done.Body.Close()
	if done.StatusCode != http.StatusNotFound {
		t.Fatalf("complete on an unknown session: got %d, want 404", done.StatusCode)
	}

	// The real session is untouched.
	resp2, _ := sendChunk(t, h, session.ID, 0, 3, 4, "abcd")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("the real session must still work, got %d", resp2.StatusCode)
	}
}

// TestUploadRejectsUnwritableTarget: the target directory is authorized at session creation, so
// a path the caller cannot see never becomes a staging file.
func TestUploadRejectsUnwritableTarget(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/files/uploads",
		jsonBody(t, map[string]any{"path": "does-not-exist", "name": "x.bin", "size": 4}),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for a target that does not exist, got %d", resp.StatusCode)
	}
}

// TestUploadRejectsOversizedFile: the cap is checked before a byte is accepted.
func TestUploadRejectsOversizedFile(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodPost, "/api/files/uploads",
		jsonBody(t, map[string]any{"path": "", "name": "huge.bin", "size": 1 << 50}),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", resp.StatusCode)
	}
}

// TestAbortRemovesTheSession covers the client-cancelled path.
func TestAbortRemovesTheSession(t *testing.T) {
	h := newHarness(t)
	session := startUpload(t, h, "", "cancel.txt", 4)
	if _, _ = sendChunk(t, h, session.ID, 0, 1, 4, "ab"); true {
		// first chunk accepted, then the client gives up
	}

	resp := h.do(http.MethodDelete, "/api/files/uploads/"+session.ID, nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort: %d", resp.StatusCode)
	}

	again, _ := sendChunk(t, h, session.ID, 2, 3, 4, "cd")
	defer again.Body.Close()
	if again.StatusCode != http.StatusNotFound {
		t.Fatalf("a chunk after abort: got %d, want 404", again.StatusCode)
	}
}

// TestResumableAndSingleUploadAgree: both paths must produce the same node for the same bytes.
func TestResumableAndSingleUploadAgree(t *testing.T) {
	h := newHarness(t)

	// Single request.
	uploadWithType(t, h, "single.txt", "text/plain", "same-bytes")

	// Chunked.
	session := startUpload(t, h, "", "chunked.txt", 10)
	resp, _ := sendChunk(t, h, session.ID, 0, 9, 10, "same-bytes")
	resp.Body.Close()
	done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	done.Body.Close()

	// Same content means the same content address, so the same ETag and the same size. That
	// equality is the evidence both paths went through the same publish step rather than each
	// having its own.
	type published struct {
		Size int64  `json:"size"`
		ETag string `json:"etag"`
	}
	list := map[string]published{}
	listResp := h.do(http.MethodGet, "/api/files/?path=", nil, nil)
	defer listResp.Body.Close()
	var payload struct {
		Children []struct {
			Name string `json:"name"`
			published
		} `json:"children"`
	}
	if err := json.Unmarshal([]byte(readBody(listResp)), &payload); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	for _, child := range payload.Children {
		list[child.Name] = child.published
	}

	single, ok := list["single.txt"]
	if !ok {
		t.Fatal("single-request upload missing")
	}
	chunked, ok := list["chunked.txt"]
	if !ok {
		t.Fatal("chunked upload missing")
	}
	if single.Size != chunked.Size {
		t.Fatalf("sizes differ: %d vs %d", single.Size, chunked.Size)
	}
	if single.ETag == "" || single.ETag != chunked.ETag {
		t.Fatalf("etags differ: %q vs %q", single.ETag, chunked.ETag)
	}
}

// TestCommitRejectsADeletedTarget is §6.1's 提交阶段重新验证 … 及目标冲突.
//
// An upload session can run for hours. The handler's comment says the precondition is re-evaluated
// at commit "not when the session was created … the target may have been replaced meanwhile", and
// writableParent runs at commit for the same reason. The case that can actually happen is the
// target going away: the uploader trashes the destination folder in another tab, and the upload
// finishes afterwards.
//
// Publishing into a trashed folder would produce a file inside a deleted subtree — invisible in
// every listing, occupying quota, and reachable only by a restore nobody knows they need.
func TestCommitRejectsADeletedTarget(t *testing.T) {
	h := newHarness(t)

	h.mkdir(t, "landing")
	session := startUpload(t, h, "landing", "late.bin", 5)

	chunk, _ := sendChunk(t, h, session.ID, 0, 4, 5, "hello")
	if chunk.StatusCode != http.StatusOK {
		t.Fatalf("chunk: %d %s", chunk.StatusCode, readBody(chunk))
	}
	chunk.Body.Close()

	// The destination disappears between the chunk and the commit.
	del := h.do(http.MethodDelete, "/api/files/?path=landing", nil, nil)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete landing: %d", del.StatusCode)
	}

	done := h.do(http.MethodPost, "/api/files/uploads/"+session.ID+"/complete", nil, nil)
	status := done.StatusCode
	payload := readBody(done)
	done.Body.Close()

	if status == http.StatusCreated {
		t.Fatalf("committing into a trashed folder succeeded (%d %s). The destination is checked "+
			"at commit precisely because an upload can outlive it; publishing here creates a file "+
			"inside a deleted subtree that no listing shows.", status, payload)
	}

	// And nothing was published: the file must not exist anywhere reachable.
	after := h.do(http.MethodGet, "/api/files/download?path=late.bin", nil, nil)
	after.Body.Close()
	if after.StatusCode == http.StatusOK {
		t.Fatal("the refused upload still published a file at the root")
	}
}

// TestChunkBeyondTheConfiguredSizeIsRefused enforces limits.upload_chunk_bytes.
//
// The key was validated and then never read: the handler compared the chunk only against
// max_file_bytes, so a client could send the whole file as one chunk and the setting meant nothing.
// A chunk limit that is not enforced does not bound the memory a single request can make the server
// stage, which is the only reason to have one.
func TestChunkBeyondTheConfiguredSizeIsRefused(t *testing.T) {
	const chunk = 4
	h := newHarnessWith(t, func(c *config.Config) {
		c.Limits.UploadChunkBytes = chunk
	})

	session := startUpload(t, h, "", "bounded.bin", 16)

	// A chunk exactly at the limit is fine: the setting bounds each request, it does not shrink
	// the permitted size.
	ok, payload := sendChunk(t, h, session.ID, 0, 3, 16, "abcd")
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("a chunk at the limit was refused: %d %v", ok.StatusCode, payload)
	}

	over, overPayload := sendChunk(t, h, session.ID, 4, 9, 16, "abcdef")
	defer over.Body.Close()
	if over.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a chunk over the limit returned %d, want 413: %v", over.StatusCode, overPayload)
	}
	if code := overPayload["error"]; code != "chunk_too_large" {
		// The code is what tells a client to retry with smaller ranges instead of giving up.
		t.Fatalf("error code = %v, want chunk_too_large", code)
	}

	// And the session must still work afterwards: a refused chunk is not a broken session.
	next, nextPayload := sendChunk(t, h, session.ID, 4, 7, 16, "efgh")
	defer next.Body.Close()
	if next.StatusCode != http.StatusOK {
		t.Fatalf("the session was unusable after a refused chunk: %d %v", next.StatusCode, nextPayload)
	}
}
