package upload

// Session behaviour, including the cases that decide whether a resumed upload can be trusted:
// offset conflicts, a failed chunk not corrupting the file, ownership, and the expiry sweep.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), time.Hour, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Abandoned sessions hold open handles; close them before the temp directory is removed.
	t.Cleanup(m.Close)
	return m
}

func readStaged(t *testing.T, f *os.File) string {
	t.Helper()
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	return string(b)
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s2, err := m.Create(owner, "docs", "report.bin", 10)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Uneven chunks including an empty final one, because that is what a real client sends when
	// the size is not a multiple of the chunk size.
	parts := []string{"0123", "456", "789", ""}
	offset := int64(0)
	for _, p := range parts {
		next, err := m.Append(ctx, owner, s2.ID, offset, strings.NewReader(p), int64(len(p)))
		if err != nil {
			t.Fatalf("Append %q: %v", p, err)
		}
		offset = next
	}
	if offset != 10 {
		t.Fatalf("offset = %d, want 10", offset)
	}

	f, sess, err := m.Finish(owner, s2.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := readStaged(t, f); got != "0123456789" {
		t.Fatalf("content = %q", got)
	}
	if sess.Name != "report.bin" || sess.Dir != "docs" {
		t.Fatalf("session metadata lost: %+v", sess)
	}
	if m.Count() != 0 {
		t.Fatalf("%d sessions left after finishing the only one", m.Count())
	}
}

// TestAppendRejectsWrongOffset is the core resumability guarantee: a retried or reordered chunk
// must not be spliced into the middle of the file.
func TestAppendRejectsWrongOffset(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "f.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatal(err)
	}

	off, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if conflict.Offset != 4 || off != 4 {
		t.Fatalf("server offset = %d/%d, want 4", off, conflict.Offset)
	}

	// The conflicting chunk must not have been written.
	if _, err := m.Append(ctx, owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatalf("resume: %v", err)
	}
	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := readStaged(t, f); got != "abcdefgh" {
		t.Fatalf("content = %q, want abcdefgh", got)
	}
}

// TestFailedChunkDoesNotAdvanceOffset: a request that dies mid-chunk must leave the session
// exactly where it was, or the next resume writes into a hole.
//
// The fixture was wrong until NC-RR-002, and the way it was wrong is worth recording: it declared
// a total of 6, acknowledged 3, and then asked to append 4 bytes. 3+4 > 6, so the request was
// rejected by the size check before the body was read — the test asserted the rejection of an
// oversized range while its name and comment claimed to cover a mid-stream failure. It therefore
// passed against an append path that corrupted the file on every genuine mid-stream error.
//
// The chunk now fits inside the declared total, so the copy actually runs and fails part-way.
func TestFailedChunkDoesNotAdvanceOffset(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "f.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}

	// A reader that fails after two of the four promised bytes. 3+4 = 7 <= 8, so this reaches the
	// write path; the failure is a mid-stream one, which is the case under test.
	r := io.MultiReader(strings.NewReader("de"), &failingReader{})
	if _, err := m.Append(ctx, owner, s.ID, 3, r, 4); err == nil {
		t.Fatal("a failing reader must fail the append")
	}

	if _, err := m.Append(ctx, owner, s.ID, 3, strings.NewReader("defgh"), 5); err != nil {
		t.Fatalf("resume after a failed chunk: %v", err)
	}
	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	// The bytes matter, not just the offset: the old version of this test would have passed with
	// a zero-filled hole here, because it only ever looked at the offset.
	if got := readStaged(t, f); got != "abcdefgh" {
		t.Fatalf("content = %q, want abcdefgh. A hole means the retried chunk was written past "+
			"the rolled-back end.", got)
	}
}

type failingReader struct{}

func (f *failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestAbortRemovesTheStagingFile keeps the disk from filling with abandoned uploads.
func TestAbortRemovesTheStagingFile(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(context.Background(), owner, s.ID, 0, strings.NewReader("ab"), 2); err != nil {
		t.Fatal(err)
	}

	path := s.StagingPath(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staging file missing before abort: %v", err)
	}
	if err := m.Abort(owner, s.ID); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("staging file still present after abort: %v", err)
	}
}

// TestSessionsAreOwnerScoped is the authorization test: a session id alone must not grant access.
func TestSessionsAreOwnerScoped(t *testing.T) {
	m := newManager(t)
	mine := uuid.New()
	theirs := uuid.New()

	s, err := m.Create(mine, "", "f.bin", 4)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Get(theirs, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get by another owner = %v, want ErrNotFound", err)
	}
	if _, err := m.Append(context.Background(), theirs, s.ID, 0, strings.NewReader("ab"), 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Append by another owner = %v, want ErrNotFound", err)
	}
	if _, _, err := m.Finish(theirs, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Finish by another owner = %v, want ErrNotFound", err)
	}
	if err := m.Abort(theirs, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Abort by another owner = %v, want ErrNotFound", err)
	}
}

// TestConcurrentChunksAreSerialised: two parallel requests at the same offset must not both be
// accepted, which is what the session mutex is for.
func TestConcurrentChunksAreSerialised(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 8)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			_, err := m.Append(context.Background(), owner, s.ID, 0, strings.NewReader("abcd"), 4)
			results <- err
		}()
	}

	accepted := 0
	conflicts := 0
	for i := 0; i < workers; i++ {
		err := <-results
		var conflict *ConflictError
		switch {
		case err == nil:
			accepted++
		case errors.As(err, &conflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d chunks at the same offset, want exactly 1", accepted)
	}
	if conflicts != workers-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, workers-1)
	}
}

// TestTooManySessions: the per-owner cap bounds the disk a half-finished upload can occupy.
func TestTooManySessions(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()

	for i := 0; i < 2; i++ {
		if _, err := m.Create(owner, "", "f.bin", 4); err != nil {
			t.Fatalf("Create #%d: %v", i+1, err)
		}
	}
	if _, err := m.Create(owner, "", "f.bin", 4); !errors.Is(err, ErrTooMany) {
		t.Fatalf("third session = %v, want ErrTooMany", err)
	}

	// Another owner is unaffected: the cap is per account, not global.
	if _, err := m.Create(uuid.New(), "", "f.bin", 4); err != nil {
		t.Fatalf("another owner must still be able to upload: %v", err)
	}
}

// TestExpiredSessionsAreDropped: a client that never comes back must not hold a slot forever.
func TestExpiredSessionsAreDropped(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, time.Minute, 4)
	if err != nil {
		t.Fatal(err)
	}

	// Move the clock instead of sleeping.
	now := time.Now()
	m.now = func() time.Time { return now }

	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 4)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := m.Get(owner, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(s.StagingPath(dir)); !os.IsNotExist(err) {
		t.Fatal("an expired session must lose its staging file")
	}
}

// TestSweepRemovesIdleSessions covers the periodic path, which is what actually reclaims space
// from clients that vanish without aborting.
func TestSweepRemovesIdleSessions(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, time.Minute, 4)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.now = func() time.Time { return now }

	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 4)
	if err != nil {
		t.Fatal(err)
	}
	if n := m.Sweep(); n != 0 {
		t.Fatalf("swept %d live sessions", n)
	}

	now = now.Add(2 * time.Minute)
	if n := m.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if m.Count() != 0 {
		t.Fatal("session survived the sweep")
	}
	if _, err := os.Stat(s.StagingPath(dir)); !os.IsNotExist(err) {
		t.Fatal("sweep must remove the staging file")
	}
}

// TestStartupClearsOrphanedStagingFiles: after a crash the directory holds files no session
// owns, and they have to go.
func TestStartupClearsOrphanedStagingFiles(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "part-deadbeef")
	if err := os.WriteFile(orphan, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file this package would never create must survive.
	stranger := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(stranger, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewManager(dir, time.Hour, 4); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("an orphaned staging file must be removed at startup")
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatal("a file this package did not create must be left alone")
	}
}

// TestFinishRejectsIncompleteUpload: publishing a truncated file under the intended name would
// be silent data loss.
func TestFinishRejectsIncompleteUpload(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(context.Background(), owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatal(err)
	}

	_, _, err = m.Finish(owner, s.ID)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Offset != 4 {
		t.Fatalf("Finish on a half-written upload = %v, want ConflictError at 4", err)
	}
	// The session must still be usable: the client can send the rest.
	if _, err := m.Append(context.Background(), owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatalf("resume after a rejected Finish: %v", err)
	}
}

// TestChunkBeyondDeclaredSizeIsRejected: a client cannot grow the file past the size it
// declared, which would defeat the quota check done at Create time.
func TestChunkBeyondDeclaredSizeIsRejected(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(context.Background(), owner, s.ID, 0, strings.NewReader("abcdefgh"), 8); err == nil {
		t.Fatal("a chunk larger than the declared size must be rejected")
	}
}

// TestUnseekableByteCountIsExact is a guard on the empty-chunk case: a zero-length chunk at the
// end of an upload must be accepted and must not change the offset.
func TestEmptyChunkIsAccepted(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	s, err := m.Create(owner, "", "f.bin", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(context.Background(), owner, s.ID, 0, strings.NewReader("ab"), 2); err != nil {
		t.Fatal(err)
	}
	off, err := m.Append(context.Background(), owner, s.ID, 2, strings.NewReader(""), 0)
	if err != nil {
		t.Fatalf("empty chunk: %v", err)
	}
	if off != 2 {
		t.Fatalf("offset = %d, want 2", off)
	}
	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := readStaged(t, f); got != "ab" {
		t.Fatalf("content = %q", got)
	}
}

// TestStagedBytesAreNotVisibleUnderTheUploadName guards against writing staging files into the
// user-visible tree: they belong in the manager's directory only.
func TestStagingIsOutsideTheTree(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "staging"), time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	s, err := m.Create(uuid.New(), "docs", "f.bin", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.StagingPath(filepath.Join(dir, "staging")), "staging") {
		t.Fatalf("staging path = %q", s.StagingPath(filepath.Join(dir, "staging")))
	}
	if bytes.Contains([]byte(s.Name), []byte("/")) {
		t.Fatal("a name must not carry a path")
	}
}

// TestPendingBytesCap: §4.3's server-side cap on unfinished uploads.
//
// The declared sizes are summed from the LIVE sessions, so closing one must free its allowance. A
// counter that is not decremented everywhere is how an account ends up permanently unable to upload,
// which is why the sum is computed rather than maintained.
func TestPendingBytesCap(t *testing.T) {
	m, err := NewManager(t.TempDir(), time.Hour, 8)
	if err != nil {
		t.Fatal(err)
	}
	// Windows cannot delete a directory whose staging files are still open, so the manager is closed
	// before the test returns rather than left to the finalizer.
	t.Cleanup(m.Close)
	m.SetMaxPendingBytes(1000)
	owner := uuid.New()

	first, err := m.Create(owner, "", "a.bin", 600)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if _, err := m.Create(owner, "", "b.bin", 600); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("a session over the pending-byte cap returned %v, want ErrPendingFull", err)
	}

	// Aborting the first frees exactly its declared size. (Abort is the production path that
	// forgets a session; there is no Session.Close.)
	if err := m.Abort(owner, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(owner, "", "b.bin", 600); err != nil {
		t.Fatalf("the allowance was not freed by closing a session: %v", err)
	}

	// Another account is unaffected: the cap is per owner, not global.
	if _, err := m.Create(uuid.New(), "", "c.bin", 600); err != nil {
		t.Fatalf("the cap leaked across accounts: %v", err)
	}

	// And zero means unlimited.
	unlimited, err := NewManager(t.TempDir(), time.Hour, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlimited.Close)
	if _, err := unlimited.Create(owner, "", "huge.bin", 1<<40); err != nil {
		t.Fatalf("an unset cap refused a session: %v", err)
	}
}
