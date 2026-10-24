// Package lease implements the runtime singleton ownership from ADR-0013 §6.6.
//
// PostgreSQL session advisory locks are released when the connection dies, so a
// process that only acquires at startup can lose ownership while it is still
// writing. This package keeps the lock on a dedicated connection, exposes the
// epoch a publish transaction must check, and closes Lost when the connection is
// no longer usable. The caller must then fail closed.
package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrHeld means another process owns the singleton lease.
var ErrHeld = errors.New("lease: already held by another process")

// Lease is one process's ownership record.
type Lease struct {
	Epoch   uint64
	OwnerID uuid.UUID
	// Lost is closed when ownership can no longer be proven. A process must stop
	// accepting writes when it fires, not merely log it.
	Lost <-chan struct{}
	// Close releases the lease; it is safe to call more than once.
	Close func()
}

// FailStop runs stop exactly once when the lease is lost or ctx is cancelled.
// The returned function stops the watcher without invoking stop. A server wires
// stop to readiness=false, listener drain and root-context cancellation; the
// helper exists so that "lease lost" cannot be reduced to a log line.
func FailStop(ctx context.Context, l Lease, stop func()) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-l.Lost:
			once.Do(stop)
		case <-ctx.Done():
		case <-done:
		}
	}()
	return func() {
		once.Do(func() { close(done) })
	}
}

// MemoryLease is the deterministic test implementation.
type MemoryLease struct {
	mu    sync.Mutex
	held  bool
	epoch uint64
	gen   uint64
	lost  chan struct{}
}

// NewMemory returns an unheld test lease.
func NewMemory() *MemoryLease { return &MemoryLease{lost: make(chan struct{})} }

// Acquire takes the memory lease and increments the epoch.
func (m *MemoryLease) Acquire(owner uuid.UUID) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held {
		return Lease{}, ErrHeld
	}
	m.held = true
	m.epoch++
	m.gen++
	// A new ownership period gets a fresh loss channel; a previous Lease value
	// keeps its already-closed channel and cannot mistake it for the new owner.
	if m.lost == nil || isClosed(m.lost) {
		m.lost = make(chan struct{})
	}
	gen, lost := m.gen, m.lost
	var once sync.Once
	return Lease{
		Epoch: m.epoch, OwnerID: owner, Lost: lost,
		Close: func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.held && m.gen == gen {
				m.held = false
				once.Do(func() { close(lost) })
			}
		},
	}, nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Lose simulates a runtime lease loss in tests.
func (m *MemoryLease) Lose() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held = false
	if m.lost != nil && !isClosed(m.lost) {
		close(m.lost)
	}
}

// PGLease owns a PostgreSQL session advisory lock on a dedicated connection.
type PGLease struct {
	pool         *pgxpool.Pool
	key          int64
	conn         *pgxpool.Conn
	epoch        atomic.Uint64
	lost         chan struct{}
	once         sync.Once
	closed       atomic.Bool
	backendPID   atomic.Uint32
	monitorEvery time.Duration
}

// NewPG prepares a lease on pool. key must be stable for one deployment.
func NewPG(pool *pgxpool.Pool, key int64) *PGLease {
	return &PGLease{pool: pool, key: key, lost: make(chan struct{}), monitorEvery: 5 * time.Second}
}

// WithMonitorIntervalForTest shortens the heartbeat interval. Production uses
// five seconds; a test that must observe an actual connection loss uses a
// shorter interval instead of sleeping for the production one.
func (l *PGLease) WithMonitorIntervalForTest(d time.Duration) *PGLease {
	l.monitorEvery = d
	return l
}

// BackendPID is the PostgreSQL backend holding the advisory lock. It exists for
// the lease-loss test, which terminates that backend.
func (l *PGLease) BackendPID() uint32 { return l.backendPID.Load() }

// Acquire takes the advisory lock, increments the epoch row and starts the
// monitor. A second process gets ErrHeld.
func (l *PGLease) Acquire(ctx context.Context, owner uuid.UUID) (Lease, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("lease: acquire connection: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&got); err != nil {
		conn.Release()
		return Lease{}, fmt.Errorf("lease: try lock: %w", err)
	}
	if !got {
		conn.Release()
		return Lease{}, ErrHeld
	}
	var epoch int64
	if err := conn.QueryRow(ctx, `
		UPDATE consistency_lease
		SET epoch = epoch + 1, owner_id = $1, expires_at = now() + interval '30 seconds', updated_at = now()
		WHERE id = 1
		RETURNING epoch`, owner).Scan(&epoch); err != nil {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, l.key)
		conn.Release()
		return Lease{}, fmt.Errorf("lease: increment epoch: %w", err)
	}
	l.conn = conn
	l.backendPID.Store(conn.Conn().PgConn().PID())
	l.epoch.Store(uint64(epoch))
	go l.monitor()

	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			l.closed.Store(true)
			l.once.Do(func() { close(l.lost) })
			if l.conn != nil {
				_, _ = l.conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, l.key)
				l.conn.Release()
			}
		})
	}
	return Lease{Epoch: uint64(epoch), OwnerID: owner, Lost: l.lost, Close: closeFn}, nil
}

func (l *PGLease) monitor() {
	t := time.NewTicker(l.monitorEvery)
	defer t.Stop()
	for range t.C {
		if l.closed.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var one int
		err := l.conn.QueryRow(ctx, `SELECT 1`).Scan(&one)
		cancel()
		if err != nil {
			l.closed.Store(true)
			l.once.Do(func() { close(l.lost) })
			return
		}
	}
}
