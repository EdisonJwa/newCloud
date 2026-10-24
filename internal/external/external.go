// Package external is the provider seam for mounted storage (ADR-0014).
//
// Everything above this package speaks nodes and permissions; everything below it speaks
// remotes. The interface is deliberately shaped like a filesystem a request can serve from
// in one round trip, not like a sync source: there is no "walk", no recursive copy, no
// batch — the callers compose those from List and Open, exactly as a local request composes
// them from the metadata store and the content store.
//
// Paths handed to a Storage are clean, slash-separated, and relative to the mount's root:
// "" is the root, "docs" a child, "docs/a.txt" a grandchild. Names are the remote's own;
// this package does not fold case or normalize Unicode — the remote is the authority on its
// own names, and a mount that silently renamed remote files would be a mount that lies
// about what it did.
package external

import (
	"context"
	"errors"
	"io"
	"time"
)

// Errors every provider maps its remote's failures onto. The HTTP layer translates these
// once, the same way it translates store errors — a provider surfacing a raw socket error
// as 500 is a provider making an outage undiagnosable.
var (
	// ErrNotFound: the remote has nothing at this path.
	ErrNotFound = errors.New("external: not found")
	// ErrNotADirectory: a listing or child-create targeted a file.
	ErrNotADirectory = errors.New("external: not a directory")
	// ErrExists: a create found the name already taken.
	ErrExists = errors.New("external: already exists")
	// ErrPermission: the remote refused the credentials for this operation.
	ErrPermission = errors.New("external: permission denied")
	// ErrTooLarge: the entry count or byte count exceeded the configured mount limit.
	ErrTooLarge = errors.New("external: too large")
	// ErrUnavailable: the remote could not be reached, or answered in a way no client
	// can use. This is the 502: a dead remote must not look like an empty folder.
	ErrUnavailable = errors.New("external: unavailable")
	// ErrUnsupported: the scheme names a provider that is not compiled in (smb today,
	// ADR-0014) or an operation the protocol cannot express.
	ErrUnsupported = errors.New("external: unsupported")
)

// Entry is one remote file or directory, as a listing or a Stat answers it.
type Entry struct {
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// Storage is one mount's live view of a remote tree.
//
// Implementations are not safe for concurrent use by the same instance; the caller dials
// per request (or pools) and closes what it opens. The context bounds every round trip —
// a mount whose remote stalls must surface ErrUnavailable at the deadline, not hold the
// request goroutine.
type Storage interface {
	// List returns the direct children of dir, sorted by name. An empty result for an
	// existing empty directory is correct; ErrNotADirectory says the path is a file.
	List(ctx context.Context, dir string) ([]Entry, error)
	// Stat answers one entry. The root ("") always exists and is always a directory.
	Stat(ctx context.Context, path string) (Entry, error)
	// Open returns a reader over a file's bytes. The caller must Close it.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
	// Create writes a file, replacing any file already at path. It returns the byte
	// count, which is the only size the remote is guaranteed to report honestly.
	Create(ctx context.Context, path string, r io.Reader) (int64, error)
	// Mkdir creates one directory. The remote's own rules about existing parents apply.
	Mkdir(ctx context.Context, path string) error
	// Remove deletes a file, or a directory when recursive and the protocol allows it.
	Remove(ctx context.Context, path string, recursive bool) error
	// Rename moves a path to a new path within the same remote.
	Rename(ctx context.Context, oldPath, newPath string) error
	// Ping checks the remote is reachable and the credentials open it. It is what mount
	// creation and the health field run; it is never on a request's critical path.
	Ping(ctx context.Context) error
}

// DialFunc builds a Storage for one mount. The mount service owns credential decryption
// and the scheme allowlist; a DialFunc receives plaintext and decides nothing about policy.
type DialFunc func(ctx context.Context, endpoint, rootPath, username, password string) (Storage, error)
