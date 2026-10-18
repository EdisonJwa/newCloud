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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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
)

// ConflictError reports that the chunk did not start where the server expected. Offset is
// authoritative: the client resumes from it rather than guessing.
type ConflictError struct {
	Offset int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("upload: offset conflict, server is at %d", e.Offset)
}

// Session is one in-progress upload.
type Session struct {
	ID      string
	Owner   uuid.UUID
	Dir     string
	Name    string
	Total   int64
	Offset  int64
	Created time.Time

	mu      sync.Mutex
	file    *os.File
	touched time.Time
	closed  bool
}

// record is the on-disk form of a session (WC-GAP-002).
//
// It is deliberately explicit JSON rather than a gob or a binary struct: a resumed upload is
// diagnosed by an operator reading this file, and a format that needs the running binary to explain
// itself is the wrong choice for the one piece of state that exists to survive the binary going away.
//
// `Offset` names the number of bytes ALREADY written to the staging file and fsynced. Every field is
// written on every advance, which is a few hundred bytes per chunk: negligible next to the chunk
// that just crossed the network, and it keeps the recovery path to a single read.
type record struct {
	ID      string    `json:"id"`
	Owner   string    `json:"owner"`
	Dir     string    `json:"dir"`
	Name    string    `json:"name"`
	Total   int64     `json:"total"`
	Offset  int64     `json:"offset"`
	Created time.Time `json:"created"`
	Touched time.Time `json:"touched"`
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

	// Rule 3: the offset must be a promise about bytes on disk.
	info, err := os.Stat(dataPath)
	if err != nil {
		_ = os.Remove(recPath)
		return false
	}
	offset := r.Offset
	if info.Size() < offset {
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}
	if info.Size() > offset {
		// The bytes ran ahead of the record: a crash between the write and the record update. The
		// record is the acknowledged offset, so the extra bytes are discarded. Truncating here is
		// what keeps "resume at the acknowledged offset" true rather than "resume at whatever
		// landed".
		if terr := os.Truncate(dataPath, offset); terr != nil {
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
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		_ = file.Close()
		_ = os.Remove(recPath)
		_ = os.Remove(dataPath)
		return false
	}

	s := &Session{
		ID: id, Owner: owner, Dir: r.Dir, Name: r.Name, Total: r.Total,
		Offset: offset, Created: r.Created, touched: r.Touched, file: file,
	}
	m.sessions[id] = s
	m.byOwner[owner]++
	return true
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
	s := &Session{
		ID: id, Owner: owner, Dir: dir, Name: name, Total: total,
		Created: now, touched: now, file: file,
	}
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
	if ok && m.stale(s) {
		m.forgetLocked(s)
		victim = s
		ok = false
	}
	m.mu.Unlock()

	if victim != nil {
		// Dropping the file needs the session lock, which must not be taken while the manager
		// lock is held.
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

// Append writes one chunk.
//
// start must equal the session's current offset. Accepting an arbitrary offset would let a
// retried request interleave with a fresh one and produce bytes that hash to nothing; the
// client is told the real offset instead and resumes from there.
func (m *Manager) Append(ctx context.Context, owner uuid.UUID, id string, start int64, r io.Reader, size int64) (int64, error) {
	s, err := m.Get(owner, id)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, ErrNotFound
	}
	if start != s.Offset {
		return s.Offset, &ConflictError{Offset: s.Offset}
	}
	if size < 0 {
		return s.Offset, errors.New("upload: chunk size is required")
	}
	if s.Total > 0 && s.Offset+size > s.Total {
		return s.Offset, fmt.Errorf("upload: chunk would exceed the declared size %d", s.Total)
	}

	written, err := io.Copy(s.file, &ctxReader{ctx: ctx, r: io.LimitReader(r, size)})
	if err != nil || written != size {
		// A partially written chunk must not advance the offset, or the file gains a hole.
		// Truncating back keeps the session usable after a failed request.
		if terr := s.file.Truncate(s.Offset); terr != nil {
			s.closed = true
			return 0, fmt.Errorf("upload: write failed and the staging file could not be repaired: %w", err)
		}
		if err != nil {
			return s.Offset, fmt.Errorf("upload: write chunk: %w", err)
		}
		return s.Offset, fmt.Errorf("upload: short read: got %d of %d bytes", written, size)
	}

	// Ordering is the whole durability argument (WC-GAP-002): fsync the bytes, THEN record the
	// offset. A crash between the two leaves bytes on disk that the record does not mention, and
	// recovery truncates them (see recoverOne), so the client re-sends a chunk it had not been
	// told about. Doing it the other way round would record an offset for bytes that might not
	// have landed, and the resumed file would contain a gap that hashes to something wrong.
	newOffset := s.Offset + written
	if err := s.file.Sync(); err != nil {
		// The bytes are written but not durable. Rolling the offset back is the safe move: the
		// client retries the chunk, which is cheap, whereas pretending it is durable is not.
		if terr := s.file.Truncate(s.Offset); terr != nil {
			s.closed = true
		}
		return s.Offset, fmt.Errorf("upload: sync chunk: %w", err)
	}
	prev := s.Offset
	s.Offset = newOffset
	s.touched = m.now()
	if err := m.persist(s); err != nil {
		// The offset could not be recorded. Report the chunk as unacknowledged and roll back, so
		// the in-memory state never claims more than the disk will resume to.
		s.Offset = prev
		return s.Offset, fmt.Errorf("upload: record offset: %w", err)
	}
	return s.Offset, nil
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
		Total: s.Total, Offset: s.Offset, Created: s.Created, Touched: s.touched,
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
	if s.closed {
		s.mu.Unlock()
		m.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	if s.Total > 0 && s.Offset != s.Total {
		offset := s.Offset
		s.mu.Unlock()
		m.mu.Unlock()
		return nil, nil, &ConflictError{Offset: offset}
	}
	m.forgetLocked(s)
	m.mu.Unlock()

	if err := s.file.Sync(); err != nil {
		_ = s.file.Close()
		s.mu.Unlock()
		m.discard(s)
		return nil, nil, fmt.Errorf("upload: sync: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		_ = s.file.Close()
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
		if m.stale(s) {
			victims = append(victims, s)
		}
	}
	for _, s := range victims {
		m.forgetLocked(s)
	}
	m.mu.Unlock()

	for _, s := range victims {
		s.mu.Lock()
		s.closed = true
		_ = s.file.Close()
		s.mu.Unlock()
		m.discard(s)
	}
	return len(victims)
}

// Close aborts every live session and removes its staging file. It exists for shutdown and
// for tests that abandon sessions; the per-request paths handle themselves.
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
		s.closed = true
		_ = s.file.Close()
		s.mu.Unlock()
		m.discard(s)
	}
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

// stale reports whether a session has been idle past the TTL. The caller must hold the manager
// lock, so this takes the session lock in the one direction that is safe.
func (m *Manager) stale(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.now().Sub(s.touched) > m.ttl
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
