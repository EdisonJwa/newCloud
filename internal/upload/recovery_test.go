package upload

// Regression tests for the two upload defects an independent re-review reproduced on 2026-10-23
// (NC-RR-001 and NC-RR-002).
//
// Both are about the difference between "the server says it is at offset N" and "the bytes on disk
// are exactly the N bytes the client sent". The existing suite tested the offset and not the bytes,
// which is why it was green while a retried chunk after a mid-stream failure wrote a zero-filled
// hole into the staging file.
//
// Every test here asserts the FILE CONTENT, not an offset, a status or a mock invocation. A test
// that checks the offset would have passed against the bug.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// resettingReader fails after serving a prefix, which models a connection reset part-way through a
// chunk. The failure lands DEEP inside the copy, after real bytes have already been written, which
// is the case the old fixture accidentally excluded: it promised 4 bytes for a 6-byte file at
// offset 3, so the request was rejected on the size check BEFORE the body was read and the
// interesting branch was never entered.
type resettingReader struct {
	prefix string
}

func (f *resettingReader) Read(p []byte) (int, error) {
	if f.prefix == "" {
		return 0, errors.New("connection reset")
	}
	n := copy(p, f.prefix)
	f.prefix = f.prefix[n:]
	return n, nil
}

// TestARetriedChunkAfterAMidStreamFailureLeavesNoHole is NC-RR-002, the reproduced defect.
//
// Sequence, exactly as the review executed it:
//
//  1. declare 8 bytes, succeed with "abc"  -> offset 3
//  2. send "de" then have the connection reset, still within the declared total
//  3. retry from the server's offset 3 with "fghij"
//  4. the staged bytes must be "abcfghij"
//
// The bug produced "abc\x00\x00fghij": the write truncated the file to 3 but left the handle at 5,
// so the retry was written past the repaired end and the kernel zero-filled the gap.
func TestARetriedChunkAfterAMidStreamFailureLeavesNoHole(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "hole.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if off, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abc"), 3); err != nil || off != 3 {
		t.Fatalf("first chunk: offset=%d err=%v", off, err)
	}

	// A chunk WITHIN the declared total (3 + 4 = 7 <= 8) whose body dies after two bytes. The
	// size check passes, so the copy runs and the failure path is the one under test.
	body := io.MultiReader(strings.NewReader("de"), &resettingReader{})
	if _, err := m.Append(ctx, owner, s.ID, 3, body, 4); err == nil {
		t.Fatal("a reader that fails mid-chunk must fail the append")
	}

	// The session must still be usable and must still be at 3.
	got, err := m.Get(owner, s.ID)
	if err != nil {
		t.Fatalf("the session was discarded instead of rolled back: %v", err)
	}
	if got.Offset != 3 {
		t.Fatalf("offset after the failure = %d, want 3", got.Offset)
	}

	if off, err := m.Append(ctx, owner, s.ID, 3, strings.NewReader("fghij"), 5); err != nil || off != 8 {
		t.Fatalf("retry: offset=%d err=%v", off, err)
	}

	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if body := readStaged(t, f); body != "abcfghij" {
		t.Fatalf("staged bytes = %q, want %q. A zero-filled gap means the retried chunk was "+
			"written past the rolled-back end: truncating a file does not move its handle.",
			body, "abcfghij")
	}
}

// TestAnAppendRepositionsTheHandleToTheAcknowledgedOffset covers the other half of NC-RR-002.
//
// The review's fix guidance is "always position the file at the authoritative offset before each
// append", and that is what makes the rollback paths safe even if one of them is ever missed. A
// Go file's length and its I/O cursor are independent: after a failure that truncated the file the
// cursor may still point past the new end, so the NEXT append must seek rather than assume the
// handle is where the offset says.
//
// The test forces exactly that state without depending on a platform-specific fault: it seeks the
// handle past the acknowledged offset, then appends. Without the pre-write seek the kernel would
// zero-fill the gap and the file would be longer than the client's own size.
func TestAnAppendRepositionsTheHandleToTheAcknowledgedOffset(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "reposition.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatal(err)
	}

	// Simulate a predecessor that left the cursor past the acknowledged end - the state a failed
	// chunk produces when its truncate succeeds but its seek does not.
	s.mu.Lock()
	if _, err := s.file.Seek(6, io.SeekStart); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()

	if _, err := m.Append(ctx, owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatalf("append after a stale cursor: %v", err)
	}
	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if body := readStaged(t, f); body != "abcdefgh" {
		t.Fatalf("staged bytes = %q, want %q. A zero-filled gap means the append trusted the "+
			"handle's position instead of seeking to the acknowledged offset.", body, "abcdefgh")
	}
}

// TestShutdownPreservesUnfinishedUploads is NC-RR-001, the reproduced defect.
//
// A graceful stop is not an abort. Before this, `defer uploads.Close()` removed the staging bytes
// AND the record of every live session, so a deploy destroyed every transfer in flight ??while a
// SIGKILL recovered, because a crash leaves the directory untouched. The property pinned here is
// the one the documentation promises: stop, start, resume at the same offset with the same bytes.
func TestShutdownPreservesUnfinishedUploads(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()
	ctx := context.Background()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "docs", "large.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(ctx, owner, sess.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatal(err)
	}

	// A graceful stop, which is what a deploy or `systemctl restart` performs.
	if err := first.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Both halves of the state must survive: the bytes and the record.
	if _, err := os.Stat(sess.StagingPath(dir)); err != nil {
		t.Fatalf("graceful shutdown deleted the staging file: %v. A restart would lose 4 of 8 "+
			"bytes that the client was told were accepted.", err)
	}
	if _, err := os.Stat(dir + string(os.PathSeparator) + "part-" + sess.ID + ".json"); err != nil {
		t.Fatalf("graceful shutdown deleted the session record: %v", err)
	}

	// And a fresh process over the same directory resumes it.
	second, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if got := second.Recovered(); got != 1 {
		t.Fatalf("recovered %d sessions after a graceful restart, want 1", got)
	}
	recovered, err := second.Get(owner, sess.ID)
	if err != nil {
		t.Fatalf("the session did not survive a graceful restart: %v", err)
	}
	if recovered.Offset != 4 {
		t.Fatalf("resumed at offset %d, want the acknowledged 4", recovered.Offset)
	}

	if _, err := second.Append(ctx, owner, sess.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatalf("resume after a graceful restart: %v", err)
	}
	f, _, err := second.Finish(owner, sess.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if body := readStaged(t, f); body != "abcdefgh" {
		t.Fatalf("content after a graceful restart = %q, want %q", body, "abcdefgh")
	}
}

// TestShutdownThenRestartSurvivesRepeatedCycles pins the deploy case rather than the single
// restart: an upgrade that stops and starts twice must not erode the transfer.
func TestShutdownThenRestartSurvivesRepeatedCycles(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()
	ctx := context.Background()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := first.Create(owner, "", "cycles.bin", 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(ctx, owner, sess.ID, 0, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(); err != nil {
		t.Fatal(err)
	}

	for i, chunk := range []string{"def", "ghi"} {
		m, err := NewManager(dir, time.Hour, 4)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		off := int64((i + 1) * 3)
		if _, err := m.Append(ctx, owner, sess.ID, off, strings.NewReader(chunk), 3); err != nil {
			t.Fatalf("cycle %d append at %d: %v", i, off, err)
		}
		if i == 0 {
			if err := m.Shutdown(); err != nil {
				t.Fatalf("cycle %d shutdown: %v", i, err)
			}
			continue
		}
		f, _, err := m.Finish(owner, sess.ID)
		if err != nil {
			t.Fatalf("cycle %d finish: %v", i, err)
		}
		if body := readStaged(t, f); body != "abcdefghi" {
			t.Fatalf("content after two graceful restarts = %q, want %q", body, "abcdefghi")
		}
	}
}

// TestCloseIsStillDestructive guards the distinction the fix introduced rather than removing the
// capability: an explicit abort-all must still delete, so an operator or a test that asks for
// teardown does not silently leave gigabytes of staging space behind.
func TestCloseIsStillDestructive(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.Create(uuid.New(), "", "abandon.bin", 4)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()

	if _, err := os.Stat(sess.StagingPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("Close must remove the staging file; Stat err = %v", err)
	}
	if _, err := os.Stat(dir + string(os.PathSeparator) + "part-" + sess.ID + ".json"); !os.IsNotExist(err) {
		t.Fatalf("Close must remove the session record; Stat err = %v", err)
	}
}

// TestAnUnrepairableRollbackInvalidatesTheSession covers the branch the fix added.
//
// When the staging file cannot be restored to the acknowledged offset, the honest answer is that
// the session is gone, not that it is resumable: the bytes on disk are of unknown provenance, and
// resuming would splice them into the user's file. The rolled-back-but-usable case is covered
// above; this is its counterpart, and it must not be silently resumable.
func TestAnUnrepairableRollbackInvalidatesTheSession(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "unrepairable.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}

	// Close the underlying handle so the rollback's truncate/seek must fail, then attempt a chunk
	// that itself fails. The repair cannot succeed, so the session must be discarded.
	s.mu.Lock()
	_ = s.file.Close()
	s.mu.Unlock()

	body := io.MultiReader(strings.NewReader("de"), &resettingReader{})
	if _, err := m.Append(ctx, owner, s.ID, 3, body, 4); err == nil {
		t.Fatal("an append on a broken handle must fail")
	}

	if _, err := m.Get(owner, s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session whose rollback failed must be discarded, not left resumable; Get = %v",
			err)
	}
	// And nothing is left on disk for a restart to resurrect.
	if _, err := os.Stat(s.StagingPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("the staging file of an invalidated session must be removed; Stat err = %v", err)
	}
}

// TestARolledBackSessionKeepsTheRecordHonest: the record on disk must agree with the in-memory
// offset after a failed chunk, or a restart would resurrect a longer upload than the client sent.
func TestARolledBackSessionKeepsTheRecordHonest(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "honest.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("ab"), 2); err != nil {
		t.Fatal(err)
	}
	body := io.MultiReader(strings.NewReader("cd"), &resettingReader{})
	if _, err := m.Append(ctx, owner, s.ID, 2, body, 4); err == nil {
		t.Fatal("a mid-chunk failure must fail the append")
	}

	raw, err := os.ReadFile(m.Dir() + string(os.PathSeparator) + "part-" + s.ID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"offset":2`)) {
		t.Fatalf("the record does not say offset 2 after a failed chunk: %s", raw)
	}
}
