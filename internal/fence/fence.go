// Package fence provides the hierarchical read/write intents from ADR-0013
// §6.3.
//
// The fence is keyed by stable identity, not by a path string: a node keeps its
// ID across a rename, and a name is a separate resource from the node currently
// occupying it. Callers pass the whole chain explicitly:
//
//	// A reader of folder/file.txt:
//	Acquire(ctx, Shared(Node(folder)), Shared(Node(child)))
//
//	// A directory MOVE:
//	Acquire(ctx, Shared(Node(parent)), Exclusive(Node(folder)),
//	        Exclusive(Node(folderNameKey)), Exclusive(Node(destNameKey)))
//
// Acquiring the ancestor shared on every child operation is what makes an
// exclusive directory intent conflict with a child write; the fence deliberately
// does not walk the metadata tree itself, because a caller must resolve and then
// revalidate the chain (a rename can change it).
package fence

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// Kind distinguishes a node identity from a parent/name entry.
type Kind uint8

const (
	// Node identifies a node by its immutable ID.
	Node Kind = iota + 1
	// Name identifies one (parent ID, name) entry. Creating, deleting or
	// renaming a name takes this intent; replacing content does not.
	Name
)

// Mode is the intent's strength.
type Mode uint8

const (
	// Shared permits other readers and conflicts with an exclusive holder.
	Shared Mode = iota + 1
	// Exclusive conflicts with every other holder.
	Exclusive
)

// Key is the stable identity of a fenced resource. Mode is deliberately not part
// of the key: Read and Write on the same Node must contend on the same lock.
type Key struct {
	Owner  uuid.UUID
	Kind   Kind
	NodeID uuid.UUID
	Parent uuid.UUID
	Name   string
}

// NodeKey returns the identity key for a node.
func NodeKey(owner, nodeID uuid.UUID) Key {
	return Key{Owner: owner, Kind: Node, NodeID: nodeID}
}

// NameKey returns the identity key for a parent/name entry.
func NameKey(owner, parent uuid.UUID, name string) Key {
	return Key{Owner: owner, Kind: Name, Parent: parent, Name: name}
}

// Intent is one key and the strength requested for it.
type Intent struct {
	Key  Key
	Mode Mode
}

// Read and Write build intents succinctly.
func Read(k Key) Intent  { return Intent{Key: k, Mode: Shared} }
func Write(k Key) Intent { return Intent{Key: k, Mode: Exclusive} }

// Release releases everything acquired by one Acquire call.
type Release func()

// ErrDuplicateIntent means the same key was requested twice in one acquisition.
// That is a no-upgrade rule, not a supported way to strengthen a lock: taking
// Shared then Exclusive would deadlock against another reader or silently change
// the caller's assumption.
var ErrDuplicateIntent = errors.New("fence: duplicate key in one acquisition")

// ErrInvalidIntent means a zero/unknown key or mode was passed.
var ErrInvalidIntent = errors.New("fence: invalid intent")

type keyState struct {
	readers int
	writer  bool
}

// Locker is an in-process hierarchical fence.
//
// It is intentionally not a distributed lock: ADR-0013 A1 restricts v1 to one
// active process, and Phase 1 enforces that with a PostgreSQL lease, a Tree OS
// lock and an epoch. A later PostgreSQL backend can implement the same interface
// with the same key ordering.
type Locker struct {
	mu      sync.Mutex
	held    map[Key]*keyState
	changed chan struct{}
}

// New returns an empty fence.
func New() *Locker {
	return &Locker{held: make(map[Key]*keyState), changed: make(chan struct{})}
}

// Acquire takes every intent atomically, or none. Keys are sorted internally so
// two callers presenting the same set in different orders cannot deadlock.
//
// The variadic slice may alias caller memory. Acquire copies it before sorting
// and the Release closure captures only that private copy; a caller is therefore
// free to reuse its slice immediately after Acquire returns.
func (l *Locker) Acquire(ctx context.Context, intents ...Intent) (Release, error) {
	if len(intents) == 0 {
		return func() {}, nil
	}
	owned := append([]Intent(nil), intents...)
	if err := validate(owned); err != nil {
		return nil, err
	}
	sort.Slice(owned, func(i, j int) bool { return less(owned[i].Key, owned[j].Key) })

	for {
		l.mu.Lock()
		if l.canAcquire(owned) {
			for _, in := range owned {
				st := l.state(in.Key)
				if in.Mode == Exclusive {
					st.writer = true
				} else {
					st.readers++
				}
			}
			l.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() { l.release(owned) })
			}, nil
		}
		ch := l.changed
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

func validate(intents []Intent) error {
	seen := make(map[Key]struct{}, len(intents))
	for _, in := range intents {
		if in.Mode != Shared && in.Mode != Exclusive {
			return fmt.Errorf("%w: mode %d", ErrInvalidIntent, in.Mode)
		}
		if in.Key.Kind != Node && in.Key.Kind != Name {
			return fmt.Errorf("%w: kind %d", ErrInvalidIntent, in.Key.Kind)
		}
		if in.Key.Kind == Node && in.Key.NodeID == uuid.Nil {
			return fmt.Errorf("%w: empty node id", ErrInvalidIntent)
		}
		if in.Key.Kind == Name && (in.Key.Parent == uuid.Nil || in.Key.Name == "") {
			return fmt.Errorf("%w: empty name key", ErrInvalidIntent)
		}
		if _, dup := seen[in.Key]; dup {
			return fmt.Errorf("%w: %+v", ErrDuplicateIntent, in.Key)
		}
		seen[in.Key] = struct{}{}
	}
	return nil
}

func (l *Locker) state(k Key) *keyState {
	st := l.held[k]
	if st == nil {
		st = &keyState{}
		l.held[k] = st
	}
	return st
}

func (l *Locker) canAcquire(intents []Intent) bool {
	for _, in := range intents {
		st := l.held[in.Key]
		if st == nil {
			continue
		}
		if st.writer {
			return false
		}
		if in.Mode == Exclusive && st.readers > 0 {
			return false
		}
	}
	return true
}

func (l *Locker) release(intents []Intent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, in := range intents {
		st := l.held[in.Key]
		if st == nil {
			continue
		}
		if in.Mode == Exclusive {
			st.writer = false
		} else if st.readers > 0 {
			st.readers--
		}
		if st.readers == 0 && !st.writer {
			delete(l.held, in.Key)
		}
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

// Held reports how many keys currently have a holder. It exists for tests and
// diagnostics; it is not a synchronization primitive.
func (l *Locker) Held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

func less(a, b Key) bool {
	if a.Owner != b.Owner {
		return a.Owner.String() < b.Owner.String()
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.NodeID != b.NodeID {
		return a.NodeID.String() < b.NodeID.String()
	}
	if a.Parent != b.Parent {
		return a.Parent.String() < b.Parent.String()
	}
	return a.Name < b.Name
}
