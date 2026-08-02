// Package upload manages resumable upload sessions.
//
// Why this exists: the single-request upload works, but a 20 GiB file over a link that drops at
// 19 GiB starts again from zero. Chunked, resumable uploads turn that failure into "retry the
// last chunk". For a product that advertises large self-hosted storage, that is the difference
// between usable and not.
//
// Sessions are held in memory and the bytes in one staging file per session. The deliberate
// boundary: an upload survives a dropped connection, not a server restart. Persisting sessions
// would need a table, a cleanup job and a migration; the dominant failure is the client going
// away, and a half-implemented durable version (orphaned rows, stale staging files) would be
// worse than this honest scope. Startup removes staging files that no session owns.
package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	now      func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
	byOwner  map[uuid.UUID]int
}

// NewManager prepares the staging directory and removes anything a previous process left.
// Orphaned staging files are the one piece of state a crash can leave, so startup is the right
// place to clear them.
func NewManager(dir string, ttl time.Duration, perOwner int) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("upload: staging directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: create staging directory: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("upload: read staging directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Only remove files this package could have created. Anything else in the directory
		// belongs to somebody else, and deleting it would be a serious bug.
		if strings.HasPrefix(entry.Name(), "part-") {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}

	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if perOwner <= 0 {
		perOwner = 4
	}
	return &Manager{
		dir:      dir,
		ttl:      ttl,
		perOwner: perOwner,
		now:      time.Now,
		sessions: map[string]*Session{},
		byOwner:  map[uuid.UUID]int{},
	}, nil
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
		_ = os.Remove(victim.stagingPath(m.dir))
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

	s.Offset += written
	s.touched = m.now()
	return s.Offset, nil
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
		_ = os.Remove(s.stagingPath(m.dir))
		return nil, nil, fmt.Errorf("upload: sync: %w", err)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		_ = s.file.Close()
		s.mu.Unlock()
		_ = os.Remove(s.stagingPath(m.dir))
		return nil, nil, fmt.Errorf("upload: rewind: %w", err)
	}
	s.closed = true
	s.mu.Unlock()
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

	_ = os.Remove(s.stagingPath(m.dir))
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
		_ = os.Remove(s.stagingPath(m.dir))
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
		_ = os.Remove(s.stagingPath(m.dir))
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
