// Package upload manages resumable upload sessions.
//
// Why this exists: the single-request upload works, but a 20 GiB file over a link that drops at
// 19 GiB starts again from zero. Chunked, resumable uploads turn that failure into "retry the
// last chunk". For a product that advertises large self-hosted storage, that is the difference
// between usable and not.
//
// Sessions are DURABLE (WC-GAP-002). Each session writes a small metadata record beside its staging
// file, so a restart resumes from the last acknowledged offset instead of discarding the transfer.
// The record is written after the bytes it describes and fsynced, which is the ordering that makes
// the offset trustworthy: an offset can never name bytes that are not on disk, so a crash costs at
// most the last chunk rather than producing a file with a hole.
//
// This replaced an in-memory map. The earlier comment here argued that persisting sessions "would
// need a table, a cleanup job and a migration" and would be worse than an honest in-memory scope.
// The cleanup job and the TTL already existed; what was missing was the record itself, and the cost
// of not having it is that a deploy, a crash or an OOM killed every transfer in flight.
package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound is returned for an unknown, expired, finished or foreign session. All four
	// look alike on purpose: answering differently for "exists but not yours" would confirm
	// that somebody else's session id is real.
	ErrNotFound = errors.New("upload: not found")

	// ErrTooMany is returned when the per-owner cap on concurrent sessions is reached. It
	// bounds the disk one account can occupy with half-finished uploads.
	ErrTooMany = errors.New("upload: too many concurrent sessions")
	// ErrPendingFull is returned when the declared size of an account's unfinished uploads would
	// exceed limits.max_pending_upload_bytes. Distinct from ErrTooMany: one is "too many sessions",
	// the other "too much outstanding data", and a client should react to them differently.
	ErrPendingFull = errors.New("upload: pending uploads exceed the account limit")

	// ErrChecksumMismatch is returned when a chunk does not match the client's declared digest
	// (tus's checksum extension, core-scope §1).
	//
	// The chunk's bytes are written but NOT acknowledged: the range is removed from the session
	// again, so the offset does not advance and a retry sends the same range. That ordering is the
	// whole requirement — an acknowledged range that failed verification would tell the client the
	// corrupt chunk is already there.
	ErrChecksumMismatch = errors.New("upload: chunk checksum does not match")
)

// ConflictError reports that the chunk overlaps bytes the server already holds. Offset is the
// contiguous prefix the client can rely on, and Ranges is the full set of acknowledged half-open
// intervals, which is what a parallel client needs to know which chunks are still missing.
type ConflictError struct {
	Offset int64
	Ranges []Range
	// Incomplete is set by Finish when the upload still has a hole, as opposed to Append
	// refusing a chunk that overlaps bytes already present. Both are 409; the client's next move
	// is the same (send what the ranges say is missing), but the message can be honest.
	Incomplete bool
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("upload: offset conflict, server is at %d", e.Offset)
}

// Range is one acknowledged half-open byte interval, [Start, End), in a session's staging file.
// The protocol's Content-Range end is inclusive; this representation is exclusive so that a range
// can be expressed as Start+length without the off-by-one that makes holes easy to create.
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Session is one in-progress upload.
//
// Offset is the contiguous acknowledged prefix ([0, Offset) has every byte). Out-of-order chunks
// are tracked separately in received; the two are always kept consistent by addRangeLocked.
type Session struct {
	ID      string
	Owner   uuid.UUID
	Dir     string
	Name    string
	Total   int64
	Offset  int64
	Created time.Time

	mu sync.Mutex
	// cond is signalled whenever an in-flight chunk finishes. Teardown marks closing, then waits
	// for writers to drain before closing the handle, so a concurrent WriteAt cannot race Close.
	cond *sync.Cond
	file *os.File
	// received is the sorted, merged set of acknowledged ranges.
	received []Range
	// inflight reserves ranges currently being written. A reservation is what makes two parallel
	// requests for overlapping bytes conflict instead of racing; it is not persisted.
	inflight []Range
	// writers counts Append calls that have reserved a range and are still copying/syncing.
	writers int
	// closing stops NEW reservations while already-running writes are allowed to finish. It is set
	// by Finish/Abort/Shutdown and by a broken-handle path.
	closing bool
	// broken records a local staging-file failure that makes the session unusable. Such a session
	// is reaped rather than offered for resume, because its bytes are of unknown provenance.
	broken bool
	// lengthSet records that a deferred length has been declared (tus creation-defer-length).
	// Without it a client could re-declare the total after every chunk, making the per-file limit a
	// suggestion rather than a bound.
	lengthSet bool
	// partial marks a tus concatenation part (core-scope §1).
	//
	// A partial is COMPLETE when it reaches its declared length, but complete is not the same as
	// finished: it is one piece of a file whose final request has not arrived, so it must stay
	// resumable and publishable by the join. Without this flag the PATCH that completed the part
	// would publish it as a file of its own — a stray artefact per parallel chunk.
	partial bool
	touched time.Time
	closed  bool
}

// Progress returns the contiguous acknowledged offset and a copy of the acknowledged ranges.
func (s *Session) Progress() (int64, []Range) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Offset, cloneRanges(s.received)
}

// record is the on-disk form of a session (WC-GAP-002).
//
// It is deliberately explicit JSON rather than a gob or a binary struct: a resumed upload is
// diagnosed by an operator reading this file, and a format that needs the running binary to explain
// itself is the wrong choice for the one piece of state that exists to survive the binary going away.
//
// `Offset` names the contiguous prefix that is ALREADY written to the staging file and fsynced.
// `Ranges` names every acknowledged interval, including ranges beyond a hole. A record written by
// the sequential-only version has no `ranges` field at all (nil after unmarshal), which recovery
// treats as the single range [0, Offset) — that is what keeps an in-flight upgrade resumable.
//
// Every field is written on every advance, which is a few hundred bytes per chunk: negligible next
// to the chunk that just crossed the network, and it keeps the recovery path to a single read.
type record struct {
	ID      string    `json:"id"`
	Owner   string    `json:"owner"`
	Dir     string    `json:"dir"`
	Name    string    `json:"name"`
	Total   int64     `json:"total"`
	Offset  int64     `json:"offset"`
	Ranges  []Range   `json:"ranges"`
	Created time.Time `json:"created"`
	Touched time.Time `json:"touched"`
	// LengthSet records a declared deferred length, so a restart does not let a client re-declare
	// the total after bytes have landed (tus creation-defer-length, core-scope §1).
	LengthSet bool `json:"length_set,omitempty"`
	// Partial marks a tus concatenation part, so a restart does not turn one into a published file.
	Partial bool `json:"partial,omitempty"`
}

// SetPartial marks a session as a tus concatenation part (core-scope §1).
//
// It is set right after creation and before any byte is written, and it is persisted so a restart
// does not lose the distinction: a partial that came back as an ordinary session would be published
// as its own file the moment it filled.
func (m *Manager) SetPartial(owner uuid.UUID, id string) error {
	s, err := m.Get(owner, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing {
		return ErrNotFound
	}
	s.partial = true
	return m.persist(s)
}

// IsPartial reports whether a session is a concatenation part.
func (s *Session) IsPartial() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.partial
}

// Manager owns the sessions and their staging files.
//
// Lock ordering, which matters: the manager lock is always taken before a session lock and is
// never held while a session lock is held. Append takes only the session lock; Finish and
// Abort take the manager lock, release it, then the session lock. Getting this backwards is a
// deadlock between a completing upload and an aborting one.
type Manager struct {
	dir      string
	ttl      time.Duration
	perOwner int
	// maxPendingBytes caps the DECLARED size of one account's unfinished uploads (§4.3). Zero means
	// unlimited. The sum is computed from the live sessions rather than kept as a counter: sessions
	// are bounded by perOwner, so the sum is cheap, and a counter is one more thing that can drift
	// out of step with the sessions it describes.
	maxPendingBytes int64
	now             func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
	byOwner  map[uuid.UUID]int
	// recovered is how many sessions startup brought back from disk (WC-GAP-002). Reported so an
	// operator can tell a restart that resumed work from one that silently dropped it.
	recovered int
}

// Recovered reports how many sessions were resumed from disk at startup.
func (m *Manager) Recovered() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recovered
}

// NewManager prepares the staging directory and RECOVERS whatever a previous process left.
//
// The recovery rules, in order, because each one is a way the on-disk state can be inconsistent:
//
//  1. A metadata record whose staging file is missing is discarded. This happens if the staging
//     directory was cleaned but the records were not, and a session that cannot produce bytes is
//     worse than no session: the client would resume at an offset that does not exist.
//  2. A staging file with no record is deleted. It was written by a process that died before
//     recording its first chunk, so no client has ever been told an offset for it.
//  3. A record whose offset is larger than the staging file is discarded, for the same reason as
//     (1): the offset is a promise about bytes.
//  4. A record older than the TTL is discarded rather than loaded, so a long outage does not
//     resurrect a thousand dead uploads and hold the pending-bytes cap against live accounts.
//  5. A recovery-temp file (`part-*.tmp`) is deleted: it is the half-written record of a crash
//     during the write below, and the previous record is still intact.
func NewManager(dir string, ttl time.Duration, perOwner int) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("upload: staging directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: create staging directory: %w", err)
	}

	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if perOwner <= 0 {
		perOwner = 4
	}
	m := &Manager{
		dir:      dir,
		ttl:      ttl,
		perOwner: perOwner,
		now:      time.Now,
		sessions: map[string]*Session{},
		byOwner:  map[uuid.UUID]int{},
	}
	if err := m.recover(); err != nil {
		return nil, err
	}
	return m, nil
}

// recover rebuilds the session table from the staging directory.
func (m *Manager) recover() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("upload: read staging directory: %w", err)
	}

	present := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			present[entry.Name()] = true
		}
	}

	var recovered int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		// Only ever touch files this package creates. A shared or misconfigured staging directory
		// must not lose somebody else's data to a startup sweep.
		switch {
		case strings.HasPrefix(name, "part-") && strings.HasSuffix(name, ".json"):
			id := strings.TrimSuffix(strings.TrimPrefix(name, "part-"), ".json")
			if m.recoverOne(id, present) {
				recovered++
			}
		case strings.HasSuffix(name, ".tmp"):
			// A record write interrupted by a crash. The previous record is authoritative.
			_ = os.Remove(filepath.Join(m.dir, name))
		case strings.HasPrefix(name, "part-"):
			// A staging file with no record: written before its first chunk was recorded.
			id := strings.TrimPrefix(name, "part-")
			if !present["part-"+id+".json"] {
				_ = os.Remove(filepath.Join(m.dir, name))
			}
		}
	}

	// A record naming a staging file that recovery refused to load (expired, or short) has already
	// been removed along with its bytes, so nothing further is needed here.
	if recovered > 0 {
		// Volume is not logged from this package (it has no logger by design); the caller reports
		// the count so an operator can see that a restart resumed work rather than losing it.
		m.recovered = recovered
	}
	return nil
}

// recoverOne loads a single session record, returning false if it could not be resumed.
//
// Every false return removes both the record and its staging file: leaving a record that no session
// backs would make a later Get answer with an offset the server cannot honour.
func (m *Manager) recoverOne(id string, present map[string]bool) bool {
	recPath := filepath.Join(m.dir, "part-"+id+".json")
	dataPath := filepath.Join(m.dir, "part-"+id)

	if !present["part-"+id] {
		// Rule 1: no bytes, so no session.
		_ = os.Remove(recPath)
		return false
	}

	raw, err := os.ReadFile(recPath)
	if err != nil {
		_ = os.Remove(recPath)
		return false
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil || r.ID != id || r.Owner == "" {
		// A corrupt record is not repairable from the inside: the offset it carried is exactly the
		// information that was lost. Discard the pair rather than resume at a guessed offset.
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}
	owner, err := uuid.Parse(r.Owner)
	if err != nil {
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}

	// Rule 4: do not resurrect a session the TTL sweep would have taken.
	if m.now().Sub(r.Touched) > m.ttl {
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}

	// Rule 3: every acknowledged range must be a promise about bytes on disk.
	info, err := os.Stat(dataPath)
	if err != nil {
		_ = os.Remove(recPath)
		return false
	}
	received, offset, err := recoveryRanges(r)
	if err != nil {
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}
	highWater := int64(0)
	if len(received) > 0 {
		highWater = received[len(received)-1].End
	}
	if info.Size() < highWater {
		// The record names bytes the file does not contain: there is no honest offset to resume to.
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}
	if info.Size() > highWater {
		// Bytes past the highest acknowledged range were written by a chunk whose record update did
		// not land. Truncate them, because no client was ever told they were accepted. A hole
		// BETWEEN acknowledged ranges is not truncatable and is left alone: it is missing according
		// to `received`, and the next retry overwrites it.
		if terr := os.Truncate(dataPath, highWater); terr != nil {
			_ = os.Remove(recPath)
			_ = os.Remove(dataPath)
			return false
		}
	}

	// Rule 5 for this session: it may also have left a temp record.
	_ = os.Remove(recPath + ".tmp")

	file, err := os.OpenFile(dataPath, os.O_RDWR, 0o600)
	if err != nil {
		_ = os.Remove(recPath)
		return false
	}

	s := newSession(id, owner, r.Dir, r.Name, r.Total, r.Created, r.Touched, file)
	s.Offset = offset
	s.received = received
	s.lengthSet = r.LengthSet
	s.partial = r.Partial
	m.sessions[id] = s
	m.byOwner[owner]++
	return true
}

// recoveryRanges turns a record into the acknowledged ranges it promises.
//
// A record from the sequential-only format has Ranges == nil (the field is absent). Its promise is
// exactly [0, Offset), which is why an upgrade in the middle of an upload resumes rather than
// throwing the transfer away. A record from this version always has a non-nil Ranges slice, even
// when it is empty, so "no bytes yet" and "an old record with offset 0" remain distinguishable.
func recoveryRanges(r record) ([]Range, int64, error) {
	if r.Total < 0 {
		return nil, 0, errors.New("upload: record has a negative total")
	}
	if r.Ranges == nil {
		if r.Offset < 0 || r.Offset > r.Total {
			return nil, 0, errors.New("upload: legacy record has an invalid offset")
		}
		if r.Offset == 0 {
			return []Range{}, 0, nil
		}
		return []Range{{Start: 0, End: r.Offset}}, r.Offset, nil
	}

	received, err := validateRanges(r.Ranges, r.Total)
	if err != nil {
		return nil, 0, err
	}
	offset := contiguousOffset(received)
	if offset != r.Offset {
		// Both halves of the record are written under the same session lock, so a disagreement
		// cannot be a normal crash window; it is corruption, and guessing which half is right is
		// how a resumed upload gets bytes nobody sent.
		return nil, 0, errors.New("upload: record offset disagrees with its ranges")
	}
	return received, offset, nil
}

// validateRanges checks a persisted range list and returns it with adjacent ranges merged.
//
// Overlaps are rejected rather than merged: two acknowledged intervals that overlap could describe
// different bytes for the same offset, and there is no way after the fact to decide which write was
// the client's. The writer in this package never produces an overlap; a record that has one is
// corrupt.
func validateRanges(rs []Range, total int64) ([]Range, error) {
	out := make([]Range, 0, len(rs))
	for _, r := range rs {
		if r.Start < 0 || r.End <= r.Start {
			return nil, fmt.Errorf("upload: record has an invalid range [%d,%d)", r.Start, r.End)
		}
		if r.End > total {
			return nil, fmt.Errorf("upload: record range [%d,%d) exceeds total %d", r.Start, r.End, total)
		}
		if len(out) > 0 {
			last := out[len(out)-1]
			if r.Start < last.End {
				return nil, fmt.Errorf("upload: record ranges overlap at %d", r.Start)
			}
			if r.Start == last.End {
				out[len(out)-1].End = r.End
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// Create starts a session for one file.
func (m *Manager) Create(owner uuid.UUID, dir, name string, total int64) (*Session, error) {
	if total < 0 {
		return nil, errors.New("upload: total size must not be negative")
	}
	name = path.Base(name)
	if name == "" || name == "." || name == "/" {
		return nil, errors.New("upload: invalid file name")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.byOwner[owner] >= m.perOwner {
		return nil, ErrTooMany
	}
	if m.maxPendingBytes > 0 {
		var pending int64
		for _, s := range m.sessions {
			if s.Owner == owner {
				pending += s.Total
			}
		}
		if pending+total > m.maxPendingBytes {
			return nil, ErrPendingFull
		}
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(filepath.Join(m.dir, "part-"+id), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("upload: create staging file: %w", err)
	}

	now := m.now()
	s := newSession(id, owner, dir, name, total, now, now, file)
	// Record the session before returning it (WC-GAP-002). A session a client has been told about
	// but the disk has not recorded would vanish on restart, which is the defect this exists to
	// fix. Failing here rather than returning an unrecorded session keeps the promise exact: if
	// Create succeeds, the session survives a restart.
	if err := m.persist(s); err != nil {
		_ = file.Close()
		m.discard(s)
		return nil, err
	}
	m.sessions[id] = s
	m.byOwner[owner]++
	return s, nil
}

// Get returns a live session owned by owner.
func (m *Manager) Get(owner uuid.UUID, id string) (*Session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok && s.Owner != owner {
		ok = false
	}
	var victim *Session
	if ok && m.expireLocked(s) {
		m.forgetLocked(s)
		victim = s
		ok = false
	}
	if ok {
		// A session being torn down (Finish, Abort, Shutdown, or a broken handle) must not be
		// handed out: Append would reject it anyway, and a status read would report a progress
		// snapshot that is about to be deleted.
		s.mu.Lock()
		if s.closed || s.closing {
			ok = false
		}
		s.mu.Unlock()
	}
	m.mu.Unlock()

	if victim != nil {
		// Dropping the file needs the session lock, which must not be taken while the manager
		// lock is held. expireLocked set closing, so no new writer can reserve a range.
		victim.mu.Lock()
		victim.closed = true
		_ = victim.file.Close()
		victim.mu.Unlock()
		m.discard(victim)
	}
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// Append writes one chunk at the range the client declared.
//
// Unlike the sequential-only version, start does NOT have to equal Offset: a client may send
// chunks in any order, and may send several at once. Two rules keep that safe:
//
//   - A range that overlaps an acknowledged range or another in-flight range is refused with a
//     ConflictError carrying the acknowledged set. Without that reservation two concurrent
//     writers could put different bytes at the same offset and the winner would be whichever
//     kernel write landed last.
//   - The file is written with WriteAt, not with a shared seek-then-copy, so disjoint chunks do
//     not influence each other's position and can be in flight at the same time.
//
// The returned offset remains the contiguous prefix: it is what a sequential client resumes from,
// and what Finish checks. A parallel client reads Ranges (or GET status) to learn the holes.
func (m *Manager) Append(ctx context.Context, owner uuid.UUID, id string, start int64, r io.Reader, size int64) (int64, error) {
	s, err := m.Get(owner, id)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	if s.closed || s.closing {
		s.mu.Unlock()
		return 0, ErrNotFound
	}
	if size < 0 {
		s.mu.Unlock()
		return s.Offset, errors.New("upload: chunk size is required")
	}
	if start < 0 || start > s.Total {
		s.mu.Unlock()
		return s.Offset, fmt.Errorf("upload: chunk starts outside the declared size %d", s.Total)
	}
	if size > s.Total-start {
		s.mu.Unlock()
		return s.Offset, fmt.Errorf("upload: chunk would exceed the declared size %d", s.Total)
	}

	// A zero-length chunk is a no-op. It exists in the protocol for the case where a client's
	// final slice is empty (size is an exact multiple of the chunk size); it must not reserve or
	// acknowledge a range.
	if size == 0 {
		offset := s.Offset
		s.mu.Unlock()
		return offset, nil
	}

	span := Range{Start: start, End: start + size}
	if overlapsAny(s.received, span) || overlapsAny(s.inflight, span) {
		offset, ranges := s.Offset, cloneRanges(s.received)
		s.mu.Unlock()
		return offset, &ConflictError{Offset: offset, Ranges: ranges}
	}
	s.inflight = append(s.inflight, span)
	s.writers++
	s.mu.Unlock()

	written, copyErr := s.writeChunk(ctx, span, r)
	var syncErr error
	if copyErr == nil && written == size {
		// fsync before acknowledging, per range. A concurrent chunk at another offset may have
		// written bytes too; fsync flushes the whole file, which is a superset of this chunk and
		// never makes an unacknowledged range durable enough to be trusted — the record is what
		// decides that.
		syncErr = s.file.Sync()
	}

	s.mu.Lock()
	s.removeInflightLocked(span)
	s.writers--
	if s.writers == 0 {
		s.cond.Broadcast()
	}

	localErr := copyErr
	if localErr == nil {
		localErr = syncErr
	}
	if localErr != nil && errors.Is(localErr, os.ErrClosed) {
		// The staging handle is gone. Every future write to this session would fail the same way,
		// and a resume would splice bytes of unknown provenance into the user's file, so the
		// honest answer is to invalidate it rather than leave it looking resumable.
		s.broken = true
		s.closing = true
		offset := s.Offset
		s.mu.Unlock()
		m.reapBroken(s)
		return offset, fmt.Errorf("upload: staging file is closed: %w", localErr)
	}
	if copyErr != nil {
		offset := s.Offset
		s.mu.Unlock()
		return offset, fmt.Errorf("upload: write chunk: %w", copyErr)
	}
	if written != size {
		offset := s.Offset
		s.mu.Unlock()
		return offset, fmt.Errorf("upload: short read: got %d of %d bytes", written, size)
	}
	if syncErr != nil {
		offset := s.Offset
		s.mu.Unlock()
		return offset, fmt.Errorf("upload: sync chunk: %w", syncErr)
	}

	// Ordering is the durability argument: fsync the bytes, THEN record the acknowledged ranges.
	// A crash between the two leaves bytes the record does not mention; recovery truncates the
	// unacknowledged tail and the client re-sends just that range.
	//
	// A persist failure removes the range from memory again, so the server never advertises an
	// acknowledgement its own restart would not honour. The bytes stay in the file but remain
	// unacknowledged; a retry overwrites them, exactly as it would after a crash.
	before := cloneRanges(s.received)
	prevOffset, prevTouched := s.Offset, s.touched
	s.addRangeLocked(span)
	s.touched = m.now()
	if err := m.persist(s); err != nil {
		s.received = before
		s.Offset = prevOffset
		s.touched = prevTouched
		offset := s.Offset
		s.mu.Unlock()
		return offset, fmt.Errorf("upload: record offset: %w", err)
	}
	offset := s.Offset
	s.mu.Unlock()
	return offset, nil
}

// writeChunk copies exactly span's length to span.Start using WriteAt.
//
// The handle's shared file position is deliberately not used. A seek-then-copy would be correct
// only while one request owns the session; with parallel chunks two writers would race for the
// same cursor. WriteAt is the primitive the OS already provides for "these bytes at this offset".
func (s *Session) writeChunk(ctx context.Context, span Range, r io.Reader) (int64, error) {
	src := io.LimitReader(&ctxReader{ctx: ctx, r: r}, span.End-span.Start)
	dst := &chunkWriter{file: s.file, off: span.Start}
	return io.Copy(dst, src)
}

type chunkWriter struct {
	file *os.File
	off  int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n, err := w.file.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}

// addRangeLocked inserts a non-overlapping range and recomputes the contiguous prefix.
func (s *Session) addRangeLocked(span Range) {
	s.received = mergeRange(s.received, span)
	s.Offset = contiguousOffset(s.received)
}

func (s *Session) removeInflightLocked(span Range) {
	for i, r := range s.inflight {
		if r == span {
			s.inflight = append(s.inflight[:i], s.inflight[i+1:]...)
			return
		}
	}
}

// reapBroken discards a session whose staging file failed locally.
//
// `closing` was already set by Append, so no new range can be reserved. The wait is for writers
// that were already in flight: they must finish before the handle is closed and the files
// unlinked, or a WriteAt could land in a file that has been deleted (on Unix) or fail with an
// unrelated error (Windows) while the teardown reports success.
func (m *Manager) reapBroken(s *Session) {
	m.mu.Lock()
	if _, live := m.sessions[s.ID]; live {
		m.forgetLocked(s)
	}
	m.mu.Unlock()

	s.mu.Lock()
	s.closing = true
	for s.writers > 0 {
		s.cond.Wait()
	}
	s.closed = true
	_ = s.file.Close()
	s.mu.Unlock()
	m.discard(s)
}

// persist writes the session's metadata record atomically.
//
// Written to a temp file and renamed into place, for the same reason the blob store publishes with
// rename: a half-written record read during recovery would describe an offset that was never
// acknowledged, and there is no way to tell a truncated JSON document from a corrupt one after the
// fact. rename(2) is atomic within a directory, so a reader sees either the old record or the new.
//
// The directory is NOT fsynced. That is a deliberate limit: it means a power loss can lose the last
// record update while keeping the bytes, which recovery handles by truncating to the previous
// offset. Losing the whole staging directory is a different failure and is not survivable anyway.
func (m *Manager) persist(s *Session) error {
	rec := record{
		ID: s.ID, Owner: s.Owner.String(), Dir: s.Dir, Name: s.Name,
		Total: s.Total, Offset: s.Offset, Ranges: cloneRanges(s.received),
		Created: s.Created, Touched: s.touched, LengthSet: s.lengthSet, Partial: s.partial,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("upload: encode session record: %w", err)
	}

	final := filepath.Join(m.dir, "part-"+s.ID+".json")
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("upload: create session record: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("upload: write session record: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("upload: sync session record: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upload: close session record: %w", err)
	}
	// On Windows os.Rename replaces an existing file; on Unix it does too. Both give the reader
	// either the previous record or this one, never a mixture.
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upload: publish session record: %w", err)
	}
	return nil
}

// removeRecord drops a session's metadata record.
func (m *Manager) removeRecord(id string) {
	_ = os.Remove(filepath.Join(m.dir, "part-"+id+".json"))
}

// discard removes a session's staging file AND its record.
//
// One helper rather than an os.Remove at each call site, because the record is the thing that makes
// a session reappear after a restart: a path that forgot it would leave a client able to resume an
// upload whose bytes are gone, which is precisely the failure the recovery rules in NewManager
// would have to clean up. Every teardown path funnels through here so that cannot happen.
func (m *Manager) discard(s *Session) {
	_ = os.Remove(s.stagingPath(m.dir))
	m.removeRecord(s.ID)
}

// Finish closes the session and hands back the staged file for publishing.
//
// The caller owns the returned file and must close it. The session is gone either way: once
// the bytes are complete there is nothing left to resume, and keeping it would let a client
// publish the same staging file twice.
func (m *Manager) Finish(owner uuid.UUID, id string) (*os.File, *Session, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok && s.Owner != owner {
		ok = false
	}
	if !ok {
		m.mu.Unlock()
		return nil, nil, ErrNotFound
	}

	// Peek at the offset while still holding the manager lock: taking the session lock here is
	// the permitted direction, and peeking first means an incomplete upload never leaves the
	// session map at all.
	s.mu.Lock()
	if s.closed || s.closing {
		s.mu.Unlock()
		m.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	if s.Total > 0 && s.Offset != s.Total {
		offset, ranges := s.Offset, cloneRanges(s.received)
		s.mu.Unlock()
		m.mu.Unlock()
		return nil, nil, &ConflictError{Offset: offset, Ranges: ranges, Incomplete: true}
	}
	m.forgetLocked(s)
	// Stop new reservations before the manager lock is released. An already-running writer may
	// still be finishing; it is allowed to complete and record its range, so the wait below is
	// outside the lock (the session lock must be available for that writer to signal it).
	s.closing = true
	s.mu.Unlock()
	m.mu.Unlock()

	// The session has been removed from the registry, so no new Append can find it. Wait for any
	// writer that got its reservation before closing.
	s.mu.Lock()
	for s.writers > 0 {
		s.cond.Wait()
	}
	if s.closed {
		s.mu.Unlock()
		m.discard(s)
		return nil, nil, ErrNotFound
	}
	if err := s.file.Sync(); err != nil {
		_ = s.file.Close()
		s.closed = true
		s.mu.Unlock()
		m.discard(s)
		return nil, nil, fmt.Errorf("upload: sync: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		_ = s.file.Close()
		s.closed = true
		s.mu.Unlock()
		m.discard(s)
		return nil, nil, fmt.Errorf("upload: rewind: %w", err)
	}
	s.closed = true
	s.mu.Unlock()
	// The record goes now, not when the caller removes the staging file (WC-GAP-002).
	//
	// The two have different lifetimes on purpose: the bytes must survive until the caller has
	// published them, but the SESSION is over the moment it is finished. Leaving the record would
	// let a restart resurrect a session whose content is already committed, and the client would
	// be invited to resume an upload that no longer exists.
	m.removeRecord(s.ID)
	return s.file, s, nil
}

// Abort discards a session and its staging file.
func (m *Manager) Abort(owner uuid.UUID, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok && s.Owner != owner {
		ok = false
	}
	if ok {
		m.forgetLocked(s)
	}
	m.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	s.mu.Lock()
	s.closing = true
	for s.writers > 0 {
		s.cond.Wait()
	}
	s.closed = true
	_ = s.file.Close()
	s.mu.Unlock()

	m.discard(s)
	return nil
}

// Sweep aborts sessions idle past the TTL and returns how many went.
func (m *Manager) Sweep() int {
	m.mu.Lock()
	victims := make([]*Session, 0, 4)
	for _, s := range m.sessions {
		if m.expireLocked(s) {
			victims = append(victims, s)
		}
	}
	for _, s := range victims {
		m.forgetLocked(s)
	}
	m.mu.Unlock()

	for _, s := range victims {
		s.mu.Lock()
		s.closing = true
		for s.writers > 0 {
			s.cond.Wait()
		}
		s.closed = true
		_ = s.file.Close()
		s.mu.Unlock()
		m.discard(s)
	}
	return len(victims)
}

// Close aborts every live session and removes its staging file. It exists for tests that abandon
// sessions and for an operator-initiated purge; the per-request paths handle themselves.
//
// It is NOT what a graceful shutdown should call (NC-RR-001). Deletion is a deliberate act, and an
// ordinary stop-and-start is not one: see Shutdown.
func (m *Manager) Close() {
	m.mu.Lock()
	live := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.sessions = map[string]*Session{}
	m.byOwner = map[uuid.UUID]int{}
	m.mu.Unlock()

	for _, s := range live {
		s.mu.Lock()
		s.closing = true
		for s.writers > 0 {
			s.cond.Wait()
		}
		s.closed = true
		_ = s.file.Close()
		s.mu.Unlock()
		m.discard(s)
	}
}

// Shutdown stops serving uploads WITHOUT destroying the work in flight (NC-RR-001).
//
// Why this is separate from Close, and why Close was the defect: main registered
// `defer uploads.Close()`, so every ordinary restart, deploy or upgrade aborted every live session
// and deleted its staging bytes and record. A SIGKILL crash recovered; a clean `systemctl restart`
// did not. The documentation claimed sessions survive restarts unconditionally, and that claim was
// false for the lifecycle people actually use.
//
// What a shutdown must do is make the CURRENT state durable and then release the handles:
//
//  1. fsync each staging file, so the bytes the client was told about are on disk and not merely
//     in the page cache of a process that is about to exit;
//  2. persist each record with a fresh touch time, so a session is not resurrected-then-expired by
//     the TTL sweep immediately after an upgrade;
//  3. close the file handle, because the next process needs to open it and Windows will refuse
//     while this one still holds it;
//  4. leave the bytes and the record exactly where recovery looks for them.
//
// Deletion stays available through Abort (an explicit client action) and Sweep (expiry). Nothing in
// the normal lifecycle should reach for it, which is why the destructive variant is now named for
// what it does rather than for when it happens to be called.
//
// Errors are returned in aggregate rather than at the first failure: one session that could not be
// synced must not prevent the other eleven from being written out, and the caller wants to log
// which ones are at risk.
func (m *Manager) Shutdown() error {
	m.mu.Lock()
	live := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.sessions = map[string]*Session{}
	m.byOwner = map[uuid.UUID]int{}
	m.mu.Unlock()

	var errs []error
	for _, s := range live {
		s.mu.Lock()
		s.closing = true
		// Let a chunk that already reserved a range finish and record it. Closing the handle first
		// would turn a graceful shutdown into a failed request whose bytes are then lost from the
		// record even though they are on disk.
		for s.writers > 0 {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			continue
		}
		s.touched = m.now()
		// Record first, then close: the record must never describe bytes that are not durable,
		// and the bytes here are already written. Syncing updates nothing observable, so the
		// ordering that matters is that both happen before the handle is released.
		if err := s.file.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("upload: sync %s: %w", s.ID, err))
		}
		if err := m.persist(s); err != nil {
			errs = append(errs, fmt.Errorf("upload: record %s: %w", s.ID, err))
		}
		s.closed = true
		if err := s.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("upload: close %s: %w", s.ID, err))
		}
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Dir reports the staging directory, so the caller can compute a session's file path for
// cleanup after publishing.
func (m *Manager) Dir() string { return m.dir }

// Count reports the live session count, for tests and diagnostics.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// expireLocked marks an idle session closing, returning true if it should be swept.
//
// The caller must hold the manager lock, so taking the session lock here is the permitted
// direction. A session with a writer in flight is never expired: the TTL measures idle time, and a
// chunk that is still arriving has not been idle. Setting closing while the session lock is held is
// what closes the race between observing "idle" and a new Append reserving a range.
func (m *Manager) expireLocked(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closing || s.writers > 0 {
		return false
	}
	if m.now().Sub(s.touched) <= m.ttl {
		return false
	}
	s.closing = true
	return true
}

func (m *Manager) forgetLocked(s *Session) {
	if _, ok := m.sessions[s.ID]; !ok {
		return
	}
	delete(m.sessions, s.ID)
	if m.byOwner[s.Owner] > 0 {
		m.byOwner[s.Owner]--
	}
	if m.byOwner[s.Owner] == 0 {
		delete(m.byOwner, s.Owner)
	}
}

func (s *Session) stagingPath(dir string) string { return filepath.Join(dir, "part-"+s.ID) }

// StagingPath returns where the bytes for this session live, for cleanup by the caller.
func (s *Session) StagingPath(dir string) string { return s.stagingPath(dir) }

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("upload: session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func newSession(id string, owner uuid.UUID, dir, name string, total int64, created, touched time.Time, file *os.File) *Session {
	s := &Session{
		ID: id, Owner: owner, Dir: dir, Name: name, Total: total,
		Created: created, touched: touched, file: file,
		received: make([]Range, 0),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// cloneRanges copies a range list, returning a non-nil empty slice for no ranges. The non-nil
// distinction is load-bearing in the on-disk record: nil means "written by the sequential-only
// version, use Offset", while [] means "this version, and nothing has been acknowledged yet".
func cloneRanges(rs []Range) []Range {
	out := make([]Range, len(rs))
	copy(out, rs)
	return out
}

// overlapsAny reports whether want intersects any of rs. Intervals are half-open, so a chunk that
// begins exactly where another ends does not overlap it.
func overlapsAny(rs []Range, want Range) bool {
	for _, r := range rs {
		if want.Start < r.End && r.Start < want.End {
			return true
		}
	}
	return false
}

// mergeRange inserts span into a sorted range list and merges adjacency. Overlaps are not expected
// here (Append rejects them); merging is for the adjacent case so the persisted set stays canonical.
func mergeRange(rs []Range, span Range) []Range {
	out := make([]Range, 0, len(rs)+1)
	out = append(out, rs...)
	out = append(out, span)
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })

	merged := out[:0]
	for _, r := range out {
		if len(merged) == 0 || r.Start > merged[len(merged)-1].End {
			merged = append(merged, r)
			continue
		}
		if r.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = r.End
		}
	}
	return merged
}

// contiguousOffset returns the length of the acknowledged prefix starting at zero.
func contiguousOffset(rs []Range) int64 {
	var off int64
	for _, r := range rs {
		if r.Start > off {
			break
		}
		if r.End > off {
			off = r.End
		}
	}
	return off
}

// ctxReader stops a chunk write when the request is cancelled.

// ctxReader stops a chunk write when the request is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// SetMaxPendingBytes caps the declared size of one account's unfinished uploads.
//
// Declared, not written: the point is to stop a client from reserving unbounded staging space, and a
// session that never receives its bytes is already reclaimed by the TTL sweep.
func (m *Manager) SetMaxPendingBytes(n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxPendingBytes = n
}

// SetLength records the size of a session whose length was deferred (tus creation-defer-length,
// core-scope §1).
//
// The size arrives with the LAST chunk instead of at creation, so it is bounded here rather than by
// the caller: a deferred session that could name any total afterwards would make the per-file limit
// advisory. It may only be set ONCE and only before any byte is written — a session whose declared
// length changed after bytes landed would have ranges that no longer mean anything.
func (m *Manager) SetLength(owner uuid.UUID, id string, total int64) error {
	if total < 0 {
		return errors.New("upload: total size must not be negative")
	}
	s, err := m.Get(owner, id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.closing {
		return ErrNotFound
	}
	if s.Offset > 0 || len(s.received) > 0 {
		// Bytes have been written against the cap. Shrinking the total below them would strand
		// acknowledged ranges outside the file, and growing it would make the deferred declaration a
		// suggestion.
		return fmt.Errorf("upload: the length can only be set before any byte is written")
	}
	if s.lengthSet {
		return fmt.Errorf("upload: the length is already set")
	}
	s.Total = total
	s.lengthSet = true
	if err := m.persist(s); err != nil {
		return fmt.Errorf("upload: record the declared length: %w", err)
	}
	return nil
}

// AppendWithChecksum writes one chunk and verifies a client-supplied digest before acknowledging it.
//
// The verification is the tus checksum extension (core-scope §1). The ordering it must implement:
//
//	write the bytes → compute the digest → compare → ACKNOWLEDGE ONLY IF IT MATCHES
//
// A mismatch leaves the range unacknowledged, which is what makes the client's retry correct: the
// server's reported offset is still the one before the chunk, so the retry sends the same range
// rather than skipping a chunk that was never accepted.
//
// have is whether a digest was supplied at all: verification is opt-in, and a client that sent no
// checksum is not a client whose chunk failed verification.
func (m *Manager) AppendWithChecksum(ctx context.Context, owner uuid.UUID, id string, start int64,
	r io.Reader, size int64, want [32]byte, have bool) (int64, error) {

	if !have {
		return m.Append(ctx, owner, id, start, r, size)
	}

	s, err := m.Get(owner, id)
	if err != nil {
		return 0, err
	}

	// The same admission checks Append makes, in the same order, so the two cannot disagree about
	// which ranges are acceptable.
	s.mu.Lock()
	if s.closed || s.closing {
		s.mu.Unlock()
		return 0, ErrNotFound
	}
	if size < 0 {
		s.mu.Unlock()
		return s.Offset, errors.New("upload: chunk size is required")
	}
	if start < 0 || start > s.Total {
		s.mu.Unlock()
		return s.Offset, fmt.Errorf("upload: chunk starts outside the declared size %d", s.Total)
	}
	if size > s.Total-start {
		s.mu.Unlock()
		return s.Offset, fmt.Errorf("upload: chunk would exceed the declared size %d", s.Total)
	}
	if size == 0 {
		offset := s.Offset
		s.mu.Unlock()
		return offset, nil
	}

	span := Range{Start: start, End: start + size}
	if overlapsAny(s.received, span) || overlapsAny(s.inflight, span) {
		offset, ranges := s.Offset, cloneRanges(s.received)
		s.mu.Unlock()
		return offset, &ConflictError{Offset: offset, Ranges: ranges}
	}
	s.inflight = append(s.inflight, span)
	s.writers++
	s.mu.Unlock()

	hasher := sha256.New()
	written, copyErr := s.writeChunk(ctx, span, io.TeeReader(r, hasher))

	var syncErr error
	if copyErr == nil && written == size {
		syncErr = s.file.Sync()
	}

	s.mu.Lock()
	s.removeInflightLocked(span)
	s.writers--
	if s.writers == 0 {
		s.cond.Broadcast()
	}

	localErr := copyErr
	if localErr == nil {
		localErr = syncErr
	}
	if localErr != nil {
		if errors.Is(localErr, os.ErrClosed) {
			s.broken = true
			s.closing = true
		}
		offset := s.Offset
		s.mu.Unlock()
		if s.broken {
			m.reapBroken(s)
		}
		return offset, fmt.Errorf("upload: write chunk: %w", localErr)
	}
	if written != size {
		offset := s.Offset
		s.mu.Unlock()
		return offset, fmt.Errorf("upload: short read: got %d of %d bytes", written, size)
	}

	// The comparison, BEFORE the range is acknowledged. The bytes are in the file either way; what
	// the mismatch withholds is the ACKNOWLEDGEMENT, and a retry overwrites them.
	var got [32]byte
	copy(got[:], hasher.Sum(nil))
	if got != want {
		offset := s.Offset
		s.mu.Unlock()
		return offset, ErrChecksumMismatch
	}

	before := cloneRanges(s.received)
	prevOffset, prevTouched := s.Offset, s.touched
	s.addRangeLocked(span)
	s.touched = m.now()
	if err := m.persist(s); err != nil {
		s.received = before
		s.Offset = prevOffset
		s.touched = prevTouched
		offset := s.Offset
		s.mu.Unlock()
		return offset, fmt.Errorf("upload: record offset: %w", err)
	}
	offset := s.Offset
	s.mu.Unlock()
	return offset, nil
}

// Concatenate copies the bytes of finished partial sessions into one target session.
//
// This is the tus concatenation extension (core-scope §1): parallelism comes from several partial
// uploads joined at the end, which replaces the out-of-order chunking the bespoke protocol offered.
// The parts are read from their staging files and appended to the target's, so the target ends up
// with the same bytes a sequential upload of the whole file would have produced — which is what
// makes the existing publish path and its ETag invariant apply unchanged.
func (m *Manager) Concatenate(owner uuid.UUID, targetID string, partIDs []string) error {
	target, err := m.Get(owner, targetID)
	if err != nil {
		return err
	}

	// The parts are opened for reading and the target for writing, in that order, so a failure to
	// read a part leaves the target untouched rather than half-written.
	parts := make([]*os.File, 0, len(partIDs))
	defer func() {
		for _, f := range parts {
			_ = f.Close()
		}
	}()

	var total int64
	for _, id := range partIDs {
		sess, gerr := m.Get(owner, id)
		if gerr != nil {
			return gerr
		}
		sess.mu.Lock()
		// A partial that is not complete would concatenate a hole into the middle of the file, which
		// is worse than refusing: the result would look like a valid upload of the wrong bytes.
		offset, _ := sess.Offset, sess.received
		complete := sess.Total == 0 || offset == sess.Total
		path := sess.stagingPath(m.dir)
		sess.mu.Unlock()
		if !complete {
			return fmt.Errorf("upload: partial %s is not complete", id)
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return fmt.Errorf("upload: open partial %s: %w", id, oerr)
		}
		parts = append(parts, f)
		total += offset
	}

	target.mu.Lock()
	defer target.mu.Unlock()
	if target.closed || target.closing {
		return ErrNotFound
	}
	if target.Total != total {
		return fmt.Errorf("upload: the joined parts hold %d bytes but the target declared %d",
			total, target.Total)
	}

	for i, f := range parts {
		written, cerr := io.Copy(&chunkWriter{file: target.file, off: target.Offset},
			&ctxReader{ctx: context.Background(), r: f})
		if cerr != nil {
			return fmt.Errorf("upload: join part %d: %w", i, cerr)
		}
		target.addRangeLocked(Range{Start: target.Offset, End: target.Offset + written})
	}
	if err := target.file.Sync(); err != nil {
		return fmt.Errorf("upload: sync the joined upload: %w", err)
	}
	if err := m.persist(target); err != nil {
		return fmt.Errorf("upload: record the joined upload: %w", err)
	}
	return nil
}
