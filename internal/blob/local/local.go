// Package local implements blob.Store on a locally attached filesystem.
//
// Layout: <root>/<ab>/<cd>/<sha256>, i.e. two levels of two hex characters.
// With 40 TiB of content and 65536 shard directories that is roughly 180k files
// per leaf, which ext4/XFS handle comfortably; one flat directory would not.
package local

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/EdisonJwa/newCloud/internal/blob"
)

// Store is a blob.Store backed by a local directory.
type Store struct {
	root string
	tmp  string
}

// New creates the shard root and the temp directory.
//
// The temp directory lives *inside* root on purpose: publishing a blob is a
// rename(2), and rename across filesystems degrades to a full copy. At 40 TiB
// that is not a slowdown, it is an outage. CheckSameFilesystem enforces it at
// startup.
func New(root string) (*Store, error) {
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return nil, fmt.Errorf("blob/local: create root: %w", err)
	}
	return &Store{root: root, tmp: tmp}, nil
}

// CheckSameFilesystem verifies that a blob can be published with a single
// rename(2) from the temp area into the content area.
//
// It performs a real rename rather than comparing stat(2) device numbers: the
// property we need is "rename does not degrade to a copy", and on some setups
// (bind mounts, overlayfs, Windows volumes) the device comparison disagrees with
// what rename actually does. Probing once at startup is cheaper than discovering
// it while writing a 100 GiB file.
func (s *Store) CheckSameFilesystem() error {
	f, err := os.CreateTemp(s.tmp, "rename-probe-*")
	if err != nil {
		return fmt.Errorf("blob/local: create probe: %w", err)
	}
	src := f.Name()
	f.Close()

	dst := filepath.Join(s.root, "rename-probe-"+filepath.Base(src))
	err = os.Rename(src, dst)
	if err != nil {
		os.Remove(src)
		return fmt.Errorf("blob/local: %s and %s do not support a direct rename (%w); "+
			"publishing a blob would degrade from rename(2) to a full copy", s.tmp, s.root, err)
	}
	os.Remove(dst)
	return nil
}

// path maps a content address to its sharded location.
func (s *Store) path(h blob.Hash) string {
	hex := h.String()
	return filepath.Join(s.root, hex[0:2], hex[2:4], hex)
}

func (s *Store) Put(ctx context.Context, h blob.Hash, r io.Reader, size int64) error {
	dst := s.path(h)
	if _, err := os.Stat(dst); err == nil {
		return nil // idempotent: the content address is already present
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob/local: stat %s: %w", h, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("blob/local: mkdir shard: %w", err)
	}

	f, err := os.CreateTemp(s.tmp, "put-*")
	if err != nil {
		return fmt.Errorf("blob/local: create temp: %w", err)
	}
	tmpName := f.Name()
	defer func() {
		if f != nil {
			f.Close()
		}
		os.Remove(tmpName) // no-op once the rename succeeded
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hasher), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		return fmt.Errorf("blob/local: write: %w", err)
	}
	if size >= 0 && written != size {
		return fmt.Errorf("blob/local: size mismatch: declared %d, got %d", size, written)
	}
	var got blob.Hash
	copy(got[:], hasher.Sum(nil))
	if got != h {
		return fmt.Errorf("%w: declared %s, got %s", blob.ErrHashMismatch, h, got)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("blob/local: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("blob/local: close: %w", err)
	}
	f = nil

	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("blob/local: publish: %w", err)
	}
	syncDir(filepath.Dir(dst))
	return nil
}

// Open returns a seekable reader, which is what HTTP Range handling needs.
func (s *Store) Open(ctx context.Context, h blob.Hash) (io.ReadSeekCloser, error) {
	f, err := os.Open(s.path(h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, blob.ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

func (s *Store) Delete(ctx context.Context, h blob.Hash) error {
	err := os.Remove(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return blob.ErrNotFound
	}
	return err
}

func (s *Store) Stat(ctx context.Context, h blob.Hash) (blob.Info, error) {
	fi, err := os.Stat(s.path(h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return blob.Info{}, blob.ErrNotFound
		}
		return blob.Info{}, err
	}
	return blob.Info{Hash: h, Size: fi.Size()}, nil
}

// ctxReader aborts a copy when the request is cancelled so a dropped upload
// does not keep writing to disk.
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

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

var _ blob.Store = (*Store)(nil)
