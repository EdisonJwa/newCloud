package upload

// Parallel and out-of-order chunks.
//
// The sequential-only protocol treated the offset as a strict cursor: a chunk had to start exactly
// where the last one ended. That is simple, but it makes a single slow chunk block every chunk
// behind it and forces a client to discover the whole chain one round trip at a time. These tests
// pin the replacement contract:
//
//   - any disjoint range inside the declared size is accepted, whatever its position;
//   - the contiguous offset still advances only when the hole before it is filled;
//   - overlapping ranges are refused rather than raced;
//   - the acknowledged SET survives a restart, not just its contiguous prefix.
//
// File CONTENT is asserted, not only offsets: the failure mode of a sparse-range implementation is
// a file of the right length made of the wrong bytes, which an offset-only test would pass.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOutOfOrderChunksAssembleInPlace(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "out-of-order.bin", 12)
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately backwards: the last chunk first, then the middle, then the gap in front.
	if off, err := m.Append(ctx, owner, s.ID, 8, strings.NewReader("ijkl"), 4); err != nil || off != 0 {
		t.Fatalf("last chunk: offset=%d err=%v, want offset 0 (a hole is still open)", off, err)
	}
	offset, ranges := s.Progress()
	if offset != 0 || len(ranges) != 1 || ranges[0] != (Range{8, 12}) {
		t.Fatalf("after the last chunk: offset=%d ranges=%v", offset, ranges)
	}

	// A duplicate of an acknowledged range must be refused without writing its body. Sending
	// different bytes is the dangerous case: accepting them would make the file depend on timing.
	if _, err := m.Append(ctx, owner, s.ID, 10, strings.NewReader("ZZ"), 2); err == nil {
		t.Fatal("an overlapping chunk must be refused")
	} else {
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("overlap error = %v, want ConflictError", err)
		}
		if conflict.Offset != 0 || len(conflict.Ranges) != 1 || conflict.Ranges[0] != (Range{8, 12}) {
			t.Fatalf("conflict = offset %d ranges %v", conflict.Offset, conflict.Ranges)
		}
	}

	if off, err := m.Append(ctx, owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil || off != 0 {
		t.Fatalf("middle chunk: offset=%d err=%v", off, err)
	}
	if off, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil || off != 12 {
		t.Fatalf("front chunk: offset=%d err=%v, want 12 after the hole closes", off, err)
	}

	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if got := readStaged(t, f); got != "abcdefghijkl" {
		t.Fatalf("content = %q, want abcdefghijkl", got)
	}
}

func TestParallelDisjointChunksAreAllAccepted(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	const (
		total   = 64
		partLen = 8
		workers = total / partLen
	)
	s, err := m.Create(owner, "", "parallel.bin", total)
	if err != nil {
		t.Fatal(err)
	}

	// Every worker sends a distinct range at the same time. The session lock is held only long
	// enough to reserve the range; the copy itself runs without it, which is what makes this
	// parallel rather than eight requests queued behind one mutex.
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := bytes.Repeat([]byte{byte('a' + i)}, partLen)
			_, err := m.Append(ctx, owner, s.ID, int64(i*partLen), bytes.NewReader(body), partLen)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("parallel chunk: %v", err)
		}
	}

	offset, ranges := s.Progress()
	if offset != total {
		t.Fatalf("contiguous offset = %d, want %d", offset, total)
	}
	if len(ranges) != 1 || ranges[0] != (Range{0, total}) {
		t.Fatalf("ranges = %v, want one [0,%d) interval", ranges, total)
	}

	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < workers; i++ {
		want := bytes.Repeat([]byte{byte('a' + i)}, partLen)
		if got := body[i*partLen : (i+1)*partLen]; !bytes.Equal(got, want) {
			t.Fatalf("chunk %d = %q, want %q: two writers' bytes landed in each other's range", i, got, want)
		}
	}
}

func TestOverlappingChunkReportsTheAcknowledgedRanges(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "overlap.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatal(err)
	}

	_, err = m.Append(ctx, owner, s.ID, 2, strings.NewReader("efgh"), 4)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("partial overlap = %v, want ConflictError", err)
	}
	if conflict.Offset != 4 {
		t.Fatalf("conflict offset = %d, want 4", conflict.Offset)
	}
	if len(conflict.Ranges) != 1 || conflict.Ranges[0] != (Range{0, 4}) {
		t.Fatalf("conflict ranges = %v, want [0,4)", conflict.Ranges)
	}

	// The refused body must not have been written: the range it overlapped is still "abcd".
	if _, err := m.Append(ctx, owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatal(err)
	}
	f, _, err := m.Finish(owner, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readStaged(t, f); got != "abcdefgh" {
		t.Fatalf("content = %q, want abcdefgh", got)
	}
}

func TestOutOfOrderRangesSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()
	ctx := context.Background()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	s, err := first.Create(owner, "", "resume-ranges.bin", 12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(ctx, owner, s.ID, 8, strings.NewReader("ijkl"), 4); err != nil {
		t.Fatal(err)
	}
	release(first)

	second := restart(t, dir)
	got, err := second.Get(owner, s.ID)
	if err != nil {
		t.Fatalf("recover the session: %v", err)
	}
	offset, ranges := got.Progress()
	if offset != 4 {
		t.Fatalf("recovered contiguous offset = %d, want 4", offset)
	}
	if len(ranges) != 2 || ranges[0] != (Range{0, 4}) || ranges[1] != (Range{8, 12}) {
		t.Fatalf("recovered ranges = %v, want [0,4) and [8,12)", ranges)
	}

	// Only the missing middle is left, and the client can send it without re-sending either end.
	if _, err := second.Append(ctx, owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatal(err)
	}
	f, _, err := second.Finish(owner, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readStaged(t, f); got != "abcdefghijkl" {
		t.Fatalf("content after out-of-order resume = %q", got)
	}
}

func TestRecoveryTruncatesOnlyPastTheHighestAcknowledgedRange(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()
	ctx := context.Background()

	first, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	s, err := first.Create(owner, "", "crash-ranges.bin", 12)
	if err != nil {
		t.Fatal(err)
	}
	// A leading hole is intentional: the acknowledged bytes are [8,12), so recovery must keep that
	// range and must not truncate the file at the contiguous offset 0.
	if _, err := first.Append(ctx, owner, s.ID, 8, strings.NewReader("ijkl"), 4); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash window: bytes at the tail without a record update.
	f, err := os.OpenFile(s.StagingPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("XXXX"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	release(first)

	second := restart(t, dir)
	info, err := os.Stat(s.StagingPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 12 {
		t.Fatalf("staging file size after recovery = %d, want 12 (the acknowledged high-water mark)", info.Size())
	}
	got, err := second.Get(owner, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if offset, _ := got.Progress(); offset != 0 {
		t.Fatalf("contiguous offset after recovery = %d, want 0", offset)
	}
	if _, err := second.Append(ctx, owner, s.ID, 0, strings.NewReader("abcdefgh"), 8); err != nil {
		t.Fatal(err)
	}
	out, _, err := second.Finish(owner, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body := readStaged(t, out); body != "abcdefghijkl" {
		t.Fatalf("content = %q, want abcdefghijkl", body)
	}
}

// gateReader blocks inside Read until the test releases it, which is what lets a test observe a
// chunk that is genuinely in flight rather than one that has already been copied.
type gateReader struct {
	started chan struct{}
	release chan struct{}
	data    []byte
	once    sync.Once
	pos     int
}

func (r *gateReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestShutdownLetsAnInFlightChunkFinish is the graceful-stop half of the concurrency contract.
// Shutdown marks the session closing (so no new range can be reserved), then waits for the writer
// to finish and record its range before closing the handle. Closing it first would turn a deploy
// into a failed request and lose bytes the client had already sent.
func TestShutdownLetsAnInFlightChunkFinish(t *testing.T) {
	dir := t.TempDir()
	owner := uuid.New()
	ctx := context.Background()

	m, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Create(owner, "", "shutdown-in-flight.bin", 8)
	if err != nil {
		t.Fatal(err)
	}

	reader := &gateReader{started: make(chan struct{}), release: make(chan struct{}), data: []byte("abcd")}
	appendErr := make(chan error, 1)
	go func() {
		_, err := m.Append(ctx, owner, s.ID, 0, reader, 4)
		appendErr <- err
	}()
	<-reader.started

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- m.Shutdown() }()

	close(reader.release)
	if err := <-appendErr; err != nil {
		t.Fatalf("the in-flight chunk failed during shutdown: %v", err)
	}
	if err := <-shutdownErr; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	second, err := NewManager(dir, time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	recovered, err := second.Get(owner, s.ID)
	if err != nil {
		t.Fatalf("the session was lost across shutdown: %v", err)
	}
	if offset, _ := recovered.Progress(); offset != 4 {
		t.Fatalf("recovered offset = %d, want the in-flight chunk's 4", offset)
	}
	if _, err := second.Append(ctx, owner, s.ID, 4, strings.NewReader("efgh"), 4); err != nil {
		t.Fatal(err)
	}
	f, _, err := second.Finish(owner, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readStaged(t, f); got != "abcdefgh" {
		t.Fatalf("content after shutdown resume = %q", got)
	}
}

func TestZeroLengthChunkReservesNothing(t *testing.T) {
	m := newManager(t)
	owner := uuid.New()
	ctx := context.Background()

	s, err := m.Create(owner, "", "empty-chunk.bin", 8)
	if err != nil {
		t.Fatal(err)
	}
	// A zero-byte chunk at 4 must not look like an in-flight reservation to the real [0,4) chunk.
	if off, err := m.Append(ctx, owner, s.ID, 4, strings.NewReader(""), 0); err != nil || off != 0 {
		t.Fatalf("empty chunk: offset=%d err=%v", off, err)
	}
	if _, err := m.Append(ctx, owner, s.ID, 0, strings.NewReader("abcd"), 4); err != nil {
		t.Fatalf("the real chunk was refused by an empty reservation: %v", err)
	}
}
