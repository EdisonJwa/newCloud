// Package blobstore adapts the content-addressed blob.Store to the unified
// layout.ContentStore interface.
//
// The adapter exists because the two layouts answer different questions well:
//
//   - Blob storage knows nothing about users or directories. Its unit is
//     "these bytes have this hash". That is what makes deduplication and cheap
//     versions possible.
//   - The application thinks in (owner, path) pairs.
//
// The bridge is the metadata layer: nodes.path identifies a file, and
// blobs.sha256 identifies its content. This package therefore cannot answer
// List or Walk on its own — those require the database, and the caller must
// fall back to a metadata query. Returning ErrWalkUnsupported is the honest
// answer rather than silently returning an empty result.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// Store implements layout.ContentStore on top of a blob.Store.
//
// It owns no state of its own. Everything that needs to be shared across
// processes — which path maps to which content address — is delegated to the
// NodeResolver, which in production queries PostgreSQL. That is deliberate: a
// per-process map here would work perfectly on one host and silently diverge the
// moment a second replica existed.
type Store struct {
	blobs   blob.Store
	resolve NodeResolver
	// spoolDir is where uploads are staged before being hashed and published.
	// Empty means the system temp directory, which is only appropriate for small
	// development setups: a container often has a read-only /tmp, and staging on a
	// different filesystem turns the publish into a full copy.
	spoolDir string
}

// NodeResolver looks up the content address for a node. It is the metadata
// layer's read path, narrowed to the single question the storage layer asks.
//
// Implementations must be safe to use from several processes at once. The
// in-memory implementation in internal/storage/resolve exists for tests and for
// wiring the blob layout before the PostgreSQL resolver lands; it is not a
// production resolver and the binary says so at startup.
type NodeResolver interface {
	// HashFor returns the content address of a file node.
	HashFor(ctx context.Context, owner, path string) (blob.Hash, bool, error)
	// Record associates a content address with a node path. It is called after
	// a successful write so the resolver and the bytes never disagree.
	Record(ctx context.Context, owner, path string, h blob.Hash, isDir bool) error
	// Forget removes a node from the index.
	Forget(ctx context.Context, owner, path string) error
	// Children returns the direct children of a directory, sorted by path.
	Children(ctx context.Context, owner, dir string, limit int) ([]layout.Node, error)
	// IsDir reports whether a node is a directory.
	IsDir(ctx context.Context, owner, path string) (bool, bool, error)
}

// New builds the adapter.
func New(blobs blob.Store, resolve NodeResolver) (*Store, error) {
	if blobs == nil {
		return nil, errors.New("blobstore: nil blob store")
	}
	if resolve == nil {
		return nil, errors.New("blobstore: nil node resolver")
	}
	return &Store{blobs: blobs, resolve: resolve}, nil
}

// WithSpoolDir sets the staging directory for uploads.
func (s *Store) WithSpoolDir(dir string) *Store {
	s.spoolDir = dir
	return s
}

func (s *Store) Mode() layout.Mode { return layout.ModeBlob }

func (s *Store) Capabilities() layout.Caps { return layout.CapsFor(layout.ModeBlob) }

func (s *Store) Put(ctx context.Context, n layout.Node, r io.Reader, size int64) (layout.PutResult, error) {
	if n.IsDir {
		return layout.PutResult{}, fmt.Errorf("%w: cannot Put a directory", layout.ErrInvalidNode)
	}
	if n.Owner == "" {
		return layout.PutResult{}, fmt.Errorf("%w: empty owner", layout.ErrUnsafePath)
	}
	if n.Path != "" {
		if _, err := layout.ValidatePath(n.Path); err != nil {
			return layout.PutResult{}, err
		}
	}

	// Hash exactly once, while spooling. Wrapping r in a TeeReader here as well
	// would hash the payload twice and every declared address would be wrong --
	// a failure mode worth naming because it is silent until the store's own
	// verification rejects the write.
	hasher := blob.NewHasher()
	res, err := s.putStreaming(ctx, n, r, size, hasher)
	return res, err
}

func (s *Store) putStreaming(ctx context.Context, n layout.Node, r io.Reader, size int64, hasher *blob.Hasher) (layout.PutResult, error) {
	// The blob store needs the hash before the bytes, so buffer to a temp file
	// only when the size is unknown or huge. For the common case we read once
	// into memory only if it is small, otherwise we spill to disk.
	//
	// Simplification for M0: spool to a temp file. This keeps memory flat for
	// a 100 GiB upload, which is the property that matters.
	spool, h, err := spoolToTemp(s.spoolDir, r, hasher)
	if err != nil {
		return layout.PutResult{}, err
	}
	defer spool.Close()

	if err := s.blobs.Put(ctx, h, spool, size); err != nil {
		return layout.PutResult{}, err
	}

	// Did this path already exist, and was it already this hash?
	prev, existed, err := s.resolve.HashFor(ctx, n.Owner, n.Path)
	if err != nil {
		return layout.PutResult{}, err
	}

	if err := s.resolve.Record(ctx, n.Owner, n.Path, h, false); err != nil {
		return layout.PutResult{}, err
	}

	out := layout.PutResult{
		Hash:             [32]byte(h),
		ReplacedExisting: existed,
	}
	if info, err := s.blobs.Stat(ctx, h); err == nil {
		out.Size = info.Size
	}
	// Deduplicated means: the bytes were already stored, i.e. a different node
	// referenced this content before this write.
	if existed && prev == h {
		out.Deduplicated = true
	} else if blobExists(ctx, s.blobs, h) && !existed {
		// The content may still be shared by another node; the reference count
		// lives in the database, so the honest answer is "not newly written".
		out.Deduplicated = true
	}
	return out, nil
}

func (s *Store) Open(ctx context.Context, n layout.Node) (io.ReadSeekCloser, error) {
	h, ok, err := s.resolve.HashFor(ctx, n.Owner, n.Path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, layout.ErrNotFound
	}
	return s.blobs.Open(ctx, h)
}

func (s *Store) Stat(ctx context.Context, n layout.Node) (layout.Node, error) {
	isDir, ok, err := s.resolve.IsDir(ctx, n.Owner, n.Path)
	if err != nil {
		return layout.Node{}, err
	}
	if !ok {
		return layout.Node{}, layout.ErrNotFound
	}

	out := n
	out.IsDir = isDir
	if isDir {
		out.Size = 0
		return out, nil
	}
	h, ok, err := s.resolve.HashFor(ctx, n.Owner, n.Path)
	if err != nil {
		return layout.Node{}, err
	}
	if !ok {
		return layout.Node{}, layout.ErrNotFound
	}
	info, err := s.blobs.Stat(ctx, h)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			// Metadata says the file exists but the bytes are gone. Surfacing
			// this as not-found is a lie that hides corruption; make it loud.
			return layout.Node{}, fmt.Errorf("blobstore: metadata references missing content %s for %s/%s: %w",
				h, n.Owner, n.Path, layout.ErrNotFound)
		}
		return layout.Node{}, err
	}
	out.Size = info.Size
	return out, nil
}

// Mkdir creates a directory marker.
//
// In blob layout a directory is purely a metadata concept: there is nothing to
// create on disk. The resolver records it so listings work.
func (s *Store) Mkdir(ctx context.Context, n layout.Node) error {
	if !n.IsDir {
		return fmt.Errorf("%w: Mkdir requires a directory node", layout.ErrInvalidNode)
	}
	return s.resolve.Record(ctx, n.Owner, n.Path, blob.Hash{}, true)
}

// Move updates the metadata mapping. The bytes do not move at all, which is why
// renaming a directory containing a million files is O(1) in this layout.
func (s *Store) Move(ctx context.Context, src, dst layout.Node) error {
	if src.Owner != dst.Owner {
		return fmt.Errorf("%w: cross-owner move must be copy+delete", layout.ErrInvalidNode)
	}
	h, ok, err := s.resolve.HashFor(ctx, src.Owner, src.Path)
	if err != nil {
		return err
	}
	isDir := false
	if !ok {
		isDir, ok, err = s.resolve.IsDir(ctx, src.Owner, src.Path)
		if err != nil {
			return err
		}
		if !ok {
			return layout.ErrNotFound
		}
		if err := s.resolve.Record(ctx, dst.Owner, dst.Path, blob.Hash{}, true); err != nil {
			return err
		}
	} else {
		if err := s.resolve.Record(ctx, dst.Owner, dst.Path, h, false); err != nil {
			return err
		}
	}
	// The bytes do not move; only the path mapping does. Remove the source
	// mapping here rather than leaving it to the caller: an adapter that leaves
	// a dangling entry would make a moved file readable at both paths.
	_ = isDir
	if err := s.resolve.Forget(ctx, src.Owner, src.Path); err != nil {
		return err
	}
	return nil
}

// Delete removes the metadata mapping. It deliberately does not delete bytes:
// reference counting lives in the database, because the same content may be
// referenced by other users, versions or the trash.
func (s *Store) Delete(ctx context.Context, n layout.Node) error {
	if _, ok, err := s.resolve.IsDir(ctx, n.Owner, n.Path); err != nil {
		return err
	} else if !ok {
		return layout.ErrNotFound
	}
	return s.resolve.Forget(ctx, n.Owner, n.Path)
}

// List delegates to the metadata layer. Blob storage has no notion of
// directories, so there is nothing to enumerate on disk.
func (s *Store) List(ctx context.Context, dir layout.Node, limit int) ([]layout.Node, error) {
	return s.resolve.Children(ctx, dir.Owner, dir.Path, limit)
}

// Walk is unsupported: enumerating a subtree requires the directory tree, which
// lives in PostgreSQL for this layout.
func (s *Store) Walk(ctx context.Context, dir layout.Node, fn func(layout.Node) error) error {
	return fmt.Errorf("%w: query the node tree in PostgreSQL", layout.ErrWalkUnsupported)
}

func blobExists(ctx context.Context, store blob.Store, h blob.Hash) bool {
	_, err := store.Stat(ctx, h)
	return err == nil
}

var _ layout.ContentStore = (*Store)(nil)
