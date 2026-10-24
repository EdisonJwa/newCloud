package upload

// WC-GAP-002: a resumable upload must survive a process restart.
//
// The property is narrow and testable: after a NEW Manager is constructed over the same staging
// directory, a client that resumes at the last acknowledged offset must (a) be told that offset
// rather than an error, (b) not have to re-send the bytes it already sent, and (c) end up with a
// file whose bytes are exactly what it sent in order.
//
// Every test here builds a second Manager instead of simulating a restart, because that is
// literally what the process does on boot: NewManager over the existing directory. A test that
// poked at internal fields would pass even if NewManager still deleted the staging files.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// restart returns a fresh Manager over the same directory, as a new process would build.
//
// The previous Manager is NOT closed. That is deliberate: Close() aborts every session and returns
// its file handle, which is graceful shutdown, and this is testing a crash. What it does mean is
// that the first Manager's open file handle outlives the test, so on Windows the temp directory
// cannot be unlinked while it is held. The handles are released by the garbage collector, which is
// not deterministic, so the caller closes the first Manager explicitly once it has finished
// simulating the crash ??see crashAfter below.
func restart(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// release closes a Manager's open handles WITHOUT discarding its state on disk.
//
// Close() is graceful shutdown: it deletes the staging files. A crash leaves them. To let a test
// simulate a crash and still finish on Windows, the open handles are closed directly and the files
// left in place ??the same disk state a SIGKILL produces.
func release(m *Manager) {
	m.mu.Lock()
	live := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.mu.Lock()
		_ = s.file.Close()
		s.mu.Unlock()
	}
}

// TestASessionResumesAfterARestart is the gap itself.
//
// Two chunks of four bytes into an eight-byte upload, then a new Manager. The client asks to
// continue from offset 8 and must be accepted ??before this work the session was gone and the
// answer was ErrNotFound, which the HTTP layer reports as 404 and the client reads as "start over".
func TestASessionResumesAfterARestart(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "docs", "resume.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID

	if off, err := first.Append(context.Background(), owner, id, 0, bytes.NewReader([]byte("abcd")), 4); err != nil || off != 4 {
		t.Fatalf("first chunk: offset=%d err=%v", off, err)
	}
	// Deliberately no Close(): a restart is a crash, and Close would discard the sessions. This is
	// the difference between testing restart recovery and testing graceful shutdown.
	release(first)
	second := restart(t, dir)

	if got := second.Recovered(); got != 1 {
		t.Fatalf("recovered %d sessions after restart, want 1", got)
	}

	got, err := second.Get(owner, id)
	if err != nil {
		t.Fatalf("the session did not survive the restart: %v", err)
	}
	if got.Offset != 4 {
		t.Fatalf("resumed at offset %d, want 4", got.Offset)
	}

	// Resume exactly where the server said, and finish.
	if off, err := second.Append(context.Background(), owner, id, 4, bytes.NewReader([]byte("efgh")), 4); err != nil || off != 8 {
		t.Fatalf("resume chunk: offset=%d err=%v", off, err)
	}
	f, _, err := second.Finish(owner, id)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "abcdefgh" {
		t.Fatalf("the resumed file is %q, want %q ??the bytes were reordered or lost", body, "abcdefgh")
	}
}

// TestAResumedSessionAcceptsAnOutOfOrderChunk: after recovery a client may keep filling gaps in
// any order. The recovered contiguous offset stays 2 (the file is not publishable yet), but the
// chunk at [4,6) is acknowledged and must survive the next restart.
func TestAResumedSessionAcceptsAnOutOfOrderChunk(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "gap.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(context.Background(), owner, sess.ID, 0, bytes.NewReader([]byte("ab")), 2); err != nil {
		t.Fatal(err)
	}

	release(first)
	second := restart(t, dir)
	off, err := second.Append(context.Background(), owner, sess.ID, 4, bytes.NewReader([]byte("ef")), 2)
	if err != nil {
		t.Fatalf("a disjoint out-of-order chunk after recovery: %v", err)
	}
	if off != 2 {
		t.Fatalf("contiguous offset after the out-of-order chunk = %d, want 2", off)
	}
	if _, _, err := second.Finish(owner, sess.ID); err == nil {
		t.Fatal("Finish must refuse an upload with a hole")
	}
	if _, err := second.Append(context.Background(), owner, sess.ID, 2, bytes.NewReader([]byte("cd")), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Append(context.Background(), owner, sess.ID, 6, bytes.NewReader([]byte("gh")), 2); err != nil {
		t.Fatal(err)
	}
	f, _, err := second.Finish(owner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	body, _ := io.ReadAll(f)
	if string(body) != "abcdefgh" {
		t.Fatalf("content after out-of-order resume = %q, want abcdefgh", body)
	}
}

// TestRecoveryTruncatesBytesTheRecordDoesNotAcknowledge.
//
// The crash window this closes: bytes are written and fsynced, then the process dies before the
// record is updated. Recovery must resume at the RECORDED offset, not at the file's size ??otherwise
// the client's retry of that chunk would append it after the bytes already there and the file would
// contain the chunk twice.
func TestRecoveryTruncatesBytesTheRecordDoesNotAcknowledge(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "crash.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(context.Background(), owner, sess.ID, 0, bytes.NewReader([]byte("abcd")), 4); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash: append bytes to the staging file WITHOUT updating the record, which is
	// exactly the state a process killed between fsync and rename leaves behind.
	dataPath := filepath.Join(dir, "part-"+sess.ID)
	f, err := os.OpenFile(dataPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("XXXX"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	release(first)
	second := restart(t, dir)
	got, err := second.Get(owner, sess.ID)
	if err != nil {
		t.Fatalf("the session should still be recoverable: %v", err)
	}
	if got.Offset != 4 {
		t.Fatalf("recovered at offset %d, want the recorded 4", got.Offset)
	}

	// The unacknowledged bytes must be gone, so re-sending from 4 produces the right file.
	if _, err := second.Append(context.Background(), owner, sess.ID, 4, bytes.NewReader([]byte("efgh")), 4); err != nil {
		t.Fatal(err)
	}
	out, _, err := second.Finish(owner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	body, _ := io.ReadAll(out)
	if string(body) != "abcdefgh" {
		t.Fatalf("the file is %q; the unacknowledged tail was not truncated", body)
	}
}

// TestAnExpiredSessionIsNotResurrected: recovery honours the TTL.
//
// A deployment that was down for a week must not come back holding a thousand sessions and counting
// their declared sizes against live accounts' pending-upload caps.
func TestAnExpiredSessionIsNotResurrected(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "old.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if (sess == nil) != false {
		t.Fatal("create returned nil session")
	}

	// Rewrite the record's timestamp into the past. Doing it through the file rather than by
	// waiting is the only way to test the boundary without a clock abstraction.
	recPath := filepath.Join(dir, "part-"+sess.ID+".json")
	raw, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	r.Touched = time.Now().Add(-48 * time.Hour)
	r.Created = r.Touched
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	release(first)
	second := restart(t, dir)
	if got := second.Recovered(); got != 0 {
		t.Fatalf("recovered %d expired sessions, want 0", got)
	}
	if _, err := second.Get(owner, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session was resurrected: %v", err)
	}
	// And its bytes are gone, not orphaned.
	if _, err := os.Stat(filepath.Join(dir, "part-"+sess.ID)); !os.IsNotExist(err) {
		t.Fatal("an expired session left its staging file behind")
	}
}

// TestARecordWithoutBytesIsDiscarded: the other half of the pair.
func TestARecordWithoutBytesIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "gone.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the bytes, keeping the record. The handle must be closed first: on Windows an open
	// handle blocks the unlink, and the point of the test is the resulting on-disk state.
	release(first)
	if err := os.Remove(filepath.Join(dir, "part-"+sess.ID)); err != nil {
		t.Fatal(err)
	}

	second := restart(t, dir)
	if got := second.Recovered(); got != 0 {
		t.Fatalf("recovered %d sessions with no bytes, want 0", got)
	}
	if _, err := second.Get(owner, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a record without bytes produced a session: %v", err)
	}
	// The dangling record must not be left to confuse the next restart.
	if _, err := os.Stat(filepath.Join(dir, "part-"+sess.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("a record with no staging file was left behind")
	}
}

// TestAnAbortedSessionIsNotRecovered: teardown removes the record, not just the bytes.
func TestAnAbortedSessionIsNotRecovered(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "abort.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Abort(owner, sess.ID); err != nil {
		t.Fatal(err)
	}

	release(first)
	second := restart(t, dir)
	if got := second.Recovered(); got != 0 {
		t.Fatalf("an aborted session was recovered (%d)", got)
	}
}

// TestAFinishedSessionIsNotRecovered: publishing removes the record too.
func TestAFinishedSessionIsNotRecovered(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "done.bin", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(context.Background(), owner, sess.ID, 0, bytes.NewReader([]byte("done")), 4); err != nil {
		t.Fatal(err)
	}
	f, _, err := first.Finish(owner, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	// The caller publishes the bytes; the staging file is then removed by the HTTP layer, which
	// knows the post-publish path. The RECORD must already be gone by now, so a restart cannot
	// resurrect a session whose content is committed.
	if _, err := os.Stat(filepath.Join(dir, "part-"+sess.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("Finish left the session record behind; a restart would resurrect a published upload")
	}

	release(first)
	second := restart(t, dir)
	if got := second.Recovered(); got != 0 {
		t.Fatalf("a finished session was recovered (%d)", got)
	}
}

// TestThePendingBytesCapCountsRecoveredSessions.
//
// Recovery must restore the accounting, not just the sessions: otherwise a restart would hand every
// account a fresh allowance of staging space, and the §4.3 cap would be resettable by crashing.
func TestThePendingBytesCapCountsRecoveredSessions(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	first.SetMaxPendingBytes(100)
	if _, err := first.Create(owner, "", "one.bin", 60); err != nil {
		t.Fatal(err)
	}

	release(first)
	second := restart(t, dir)
	second.SetMaxPendingBytes(100)

	// 60 is already outstanding, so another 60 exceeds 100 and must be refused.
	if _, err := second.Create(owner, "", "two.bin", 60); !errors.Is(err, ErrPendingFull) {
		t.Fatalf("want ErrPendingFull after recovery (60 outstanding + 60 > 100), got %v", err)
	}
	// A 40-byte session still fits, which proves the cap is not simply refusing everything.
	if _, err := second.Create(owner, "", "three.bin", 40); err != nil {
		t.Fatalf("a session within the cap was refused: %v", err)
	}
}
