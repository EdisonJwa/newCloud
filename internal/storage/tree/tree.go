// Package tree implements the "classic filesystem tree" storage layout.
//
// What the user sees is what is on disk:
//
//	<root>/<owner>/<path/to/file.txt>
//
// This is the layout an administrator can inspect with ls, back up with rsync,
// and reason about without a database. It is also the layout where a single
// path bug becomes remote file disclosure, so containment is not left to
// string validation alone: every operation goes through os.Root, which resolves
// paths with openat(2) and refuses to traverse out of the root even when a
// symlink points there.
//
// Defence in depth, in order:
//
//  1. layout.Join rejects traversal, separators inside a segment, NUL, control
//     characters, Windows device names and over-long components.
//  2. os.Root confines resolution to the root at the syscall level, so a bug in
//     layer 1 still cannot read /etc/passwd.
//  3. Writes go to a temp file and are published with rename(2), so a reader
//     never observes a half-written file.
package tree

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// tmpDir is the per-store staging directory. It lives at the top level of the
// root and is excluded from listings; owner names starting with "." are
// rejected so a user can never create or collide with it.
const tmpDir = ".newcloud-tmp"

// Store is a tree-layout ContentStore.
type Store struct {
	root *os.Root
	dir  string
	// now is injectable so tests can assert on mtime handling.
	now func() time.Time
}

// New opens (creating if needed) a tree-layout store rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("tree: create root: %w", err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("tree: open root: %w", err)
	}
	if err := r.MkdirAll(tmpDir, 0o750); err != nil {
		r.Close()
		return nil, fmt.Errorf("tree: create temp area: %w", err)
	}
	return &Store{root: r, dir: dir, now: time.Now}, nil
}

// Close releases the root handle.
func (s *Store) Close() error { return s.root.Close() }

func (s *Store) Mode() layout.Mode { return layout.ModeTree }

func (s *Store) Capabilities() layout.Caps { return layout.CapsFor(layout.ModeTree) }

// rel converts a Node into a path relative to the store root.
//
// The owner becomes the first path component. It is validated as strictly as any
// other segment: a username of ".." would otherwise place every file of that
// account outside the store.
func (s *Store) rel(n layout.Node) (string, error) {
	if n.Owner == "" {
		return "", fmt.Errorf("%w: empty owner", layout.ErrUnsafePath)
	}
	if strings.HasPrefix(n.Owner, ".") {
		return "", fmt.Errorf("%w: owner must not start with a dot", layout.ErrUnsafePath)
	}
	segs := []string{n.Owner}

	if n.Path != "" {
		if path.IsAbs(n.Path) || strings.HasPrefix(n.Path, "/") {
			return "", fmt.Errorf("%w: absolute path", layout.ErrUnsafePath)
		}
		for _, seg := range strings.Split(n.Path, "/") {
			if err := layout.ValidateSegment(seg); err != nil {
				return "", err
			}
			segs = append(segs, seg)
		}
	}
	return filepath.Join(segs...), nil
}

func (s *Store) Put(ctx context.Context, n layout.Node, r io.Reader, size int64) (layout.PutResult, error) {
	if n.IsDir {
		return layout.PutResult{}, fmt.Errorf("%w: cannot Put a directory", layout.ErrInvalidNode)
	}
	rel, err := s.rel(n)
	if err != nil {
		return layout.PutResult{}, err
	}
	if err := s.mkdirParent(rel); err != nil {
		return layout.PutResult{}, err
	}

	existed := false
	if _, err := s.root.Stat(rel); err == nil {
		existed = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return layout.PutResult{}, fmt.Errorf("tree: stat target: %w", err)
	}

	// Stage in the temp area so an interrupted upload leaves the previous
	// content intact instead of a truncated file.
	tmpName := filepath.Join(tmpDir, fmt.Sprintf("put-%d-%s", time.Now().UnixNano(), randomSuffix()))
	f, err := s.root.Create(tmpName)
	if err != nil {
		return layout.PutResult{}, fmt.Errorf("tree: create temp: %w", err)
	}
	cleanup := func() {
		f.Close()
		s.root.Remove(tmpName)
	}

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hasher), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		cleanup()
		return layout.PutResult{}, fmt.Errorf("tree: write: %w", err)
	}
	if size >= 0 && written != size {
		cleanup()
		return layout.PutResult{}, fmt.Errorf("%w: declared %d bytes, received %d", layout.ErrSizeMismatch, size, written)
	}

	if err := f.Sync(); err != nil {
		cleanup()
		return layout.PutResult{}, fmt.Errorf("tree: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		s.root.Remove(tmpName)
		return layout.PutResult{}, fmt.Errorf("tree: close: %w", err)
	}

	if err := s.root.Rename(tmpName, rel); err != nil {
		s.root.Remove(tmpName)
		return layout.PutResult{}, fmt.Errorf("tree: publish: %w", err)
	}

	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))
	return layout.PutResult{
		Hash: sum, Size: written,
		Deduplicated:     false, // tree layout never deduplicates
		ReplacedExisting: existed,
	}, nil
}

func (s *Store) Open(ctx context.Context, n layout.Node) (io.ReadSeekCloser, error) {
	rel, err := s.rel(n)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(rel)
	if err != nil {
		if isNotFound(err) {
			return nil, layout.ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

func (s *Store) Stat(ctx context.Context, n layout.Node) (layout.Node, error) {
	rel, err := s.rel(n)
	if err != nil {
		return layout.Node{}, err
	}
	fi, err := s.root.Stat(rel)
	if err != nil {
		if isNotFound(err) {
			return layout.Node{}, layout.ErrNotFound
		}
		return layout.Node{}, err
	}
	out := n
	out.IsDir = fi.IsDir()
	out.Size = fi.Size()
	out.ModTime = fi.ModTime()
	if fi.IsDir() {
		out.Size = 0
	}
	return out, nil
}

func (s *Store) Mkdir(ctx context.Context, n layout.Node) error {
	rel, err := s.rel(n)
	if err != nil {
		return err
	}
	return s.root.MkdirAll(rel, 0o750)
}

// Move performs a rename. In tree layout this is proportional to the number of
// descendants when moving a directory, because the filesystem must relink every
// entry; the caller should expect a long-running operation for large subtrees.
func (s *Store) Move(ctx context.Context, src, dst layout.Node) error {
	if src.Owner != dst.Owner {
		return fmt.Errorf("%w: cross-owner move must be copy+delete", layout.ErrInvalidNode)
	}
	from, err := s.rel(src)
	if err != nil {
		return err
	}
	to, err := s.rel(dst)
	if err != nil {
		return err
	}
	// Refuse to move a directory into its own subtree: the filesystem would
	// return EINVAL, but the error is clearer here and the check is cheap.
	if src.IsDir && strings.HasPrefix(to+string(filepath.Separator), from+string(filepath.Separator)) {
		return fmt.Errorf("%w: cannot move a directory into itself", layout.ErrInvalidNode)
	}
	if err := s.mkdirParent(to); err != nil {
		return err
	}
	if err := s.root.Rename(from, to); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return layout.ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, n layout.Node) error {
	rel, err := s.rel(n)
	if err != nil {
		return err
	}
	// A user must never be able to delete their whole account by deleting "/".
	if n.Path == "" {
		return fmt.Errorf("%w: refusing to delete a store root", layout.ErrInvalidNode)
	}
	// RemoveAll rather than Remove: it is the only form that works for both
	// files and non-empty directories, and the metadata layer decides what is
	// visible, not the filesystem.
	if err := s.root.RemoveAll(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return layout.ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Store) List(ctx context.Context, dir layout.Node, limit int) ([]layout.Node, error) {
	rel, err := s.rel(dir)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(rel)
	if err != nil {
		if isNotFound(err) {
			return nil, layout.ErrNotFound
		}
		return nil, err
	}
	defer f.Close()

	entries, err := f.ReadDir(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		// fs.ReadDir returns io.EOF only when limit is exhausted in some
		// implementations; a partial read is still useful.
		if len(entries) == 0 {
			return nil, err
		}
	}

	out := make([]layout.Node, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			// The entry vanished between listing and stat; skip it rather
			// than failing the whole directory.
			continue
		}
		child := layout.Node{
			Owner:   dir.Owner,
			Path:    path.Join(dir.Path, e.Name()),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if child.IsDir {
			child.Size = 0
		}
		out = append(out, child)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Walk streams every descendant. Tree layout can do this natively because the
// filesystem already holds the hierarchy; blob layout cannot without the
// database.
func (s *Store) Walk(ctx context.Context, dir layout.Node, fn func(layout.Node) error) error {
	rel, err := s.rel(dir)
	if err != nil {
		return err
	}
	return fs.WalkDir(s.root.FS(), filepath.ToSlash(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == filepath.ToSlash(rel) {
			return nil // do not yield the starting directory itself
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		info, ierr := d.Info()
		if ierr != nil {
			return nil // raced with a delete; skip
		}
		n := layout.Node{
			Owner:   dir.Owner,
			Path:    strings.TrimPrefix(p, strings.TrimSuffix(filepath.ToSlash(rel), "/")+"/"),
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if n.IsDir {
			n.Size = 0
		}
		return fn(n)
	})
}

// Usage sums the size of every file under a subtree.
//
// It is deliberately separate from the interface: the blob layout answers this
// from the database instead, so callers must not assume a walk is cheap.
func (s *Store) Usage(ctx context.Context, dir layout.Node) (int64, error) {
	var total int64
	err := s.Walk(ctx, dir, func(n layout.Node) error {
		if !n.IsDir {
			total += n.Size
		}
		return nil
	})
	return total, err
}

func (s *Store) mkdirParent(rel string) error {
	parent := filepath.Dir(rel)
	if parent == "." {
		return nil
	}
	return s.root.MkdirAll(parent, 0o750)
}

// ctxReader aborts a copy when the request is cancelled so a dropped upload does
// not keep writing to disk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}

func randomSuffix() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		// Uniqueness only needs to survive a single process for a few seconds
		// (the temp file is renamed immediately), so a timestamp is an
		// acceptable fallback rather than failing the upload.
		return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	}
	return fmt.Sprintf("%x", b)
}

var _ layout.ContentStore = (*Store)(nil)

// isNotFound treats a non-directory path component as "not found", not as a
// server error. Linux returns ENOTDIR for stat("file/child"); mapping only ENOENT
// made the same request 404 on Windows and 500 on Linux.
func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
