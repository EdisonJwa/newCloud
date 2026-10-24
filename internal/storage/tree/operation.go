package tree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/consistency"
	"github.com/EdisonJwa/newCloud/internal/storage/fsrename"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// stageDir is outside every user namespace. It is excluded from listings like
// tmpDir and cannot be claimed by an owner name (owners starting with "." are
// rejected).
const stageDir = ".newcloud-stage"

// StagePut writes bytes into an operation-private tree namespace. The live path
// is untouched until PublishNoReplace/PublishReplace succeeds.
func (s *Store) StagePut(ctx context.Context, opID uuid.UUID, rel string, r io.Reader, size int64) (layout.PutResult, error) {
	if err := validateOpID(opID); err != nil {
		return layout.PutResult{}, err
	}
	staged, err := s.stageRel(opID, rel)
	if err != nil {
		return layout.PutResult{}, err
	}
	if err := s.mkdirParent(staged); err != nil {
		return layout.PutResult{}, err
	}

	tmp := staged + ".tmp-" + randomSuffix()
	f, err := s.root.Create(tmp)
	if err != nil {
		return layout.PutResult{}, fmt.Errorf("tree: stage create: %w", err)
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hasher), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		f.Close()
		_ = s.root.Remove(tmp)
		return layout.PutResult{}, fmt.Errorf("tree: stage write: %w", err)
	}
	if size >= 0 && written != size {
		f.Close()
		_ = s.root.Remove(tmp)
		return layout.PutResult{}, fmt.Errorf("%w: declared %d bytes, received %d", layout.ErrSizeMismatch, size, written)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = s.root.Remove(tmp)
		return layout.PutResult{}, fmt.Errorf("tree: stage fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = s.root.Remove(tmp)
		return layout.PutResult{}, fmt.Errorf("tree: stage close: %w", err)
	}
	// Replacing a path inside this operation's own stage is safe: no external
	// reader can resolve it, and a retry must be able to overwrite its own
	// partial result.
	if err := s.root.Rename(tmp, staged); err != nil {
		_ = s.root.Remove(tmp)
		return layout.PutResult{}, fmt.Errorf("tree: stage publish: %w", err)
	}
	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))
	return layout.PutResult{Hash: sum, Size: written}, nil
}

// StageMkdir creates an operation-private directory.
func (s *Store) StageMkdir(ctx context.Context, opID uuid.UUID, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOpID(opID); err != nil {
		return err
	}
	staged, err := s.stageRel(opID, rel)
	if err != nil {
		return err
	}
	return s.root.MkdirAll(staged, 0o750)
}

// PublishNoReplace atomically moves one staged path to its final live path. It
// never replaces an existing destination; a conflict is reported as
// fsrename.ErrExist and the destination is unchanged.
func (s *Store) PublishNoReplace(ctx context.Context, opID uuid.UUID, stagedRel string, final layout.Node) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOpID(opID); err != nil {
		return err
	}
	staged, err := s.stageRel(opID, stagedRel)
	if err != nil {
		return err
	}
	finalRel, err := s.rel(final)
	if err != nil {
		return err
	}
	if err := s.mkdirParent(finalRel); err != nil {
		return err
	}
	if err := fsrename.RenameNoReplace(s.abs(staged), s.abs(finalRel)); err != nil {
		return fmt.Errorf("tree: publish no-replace: %w", err)
	}
	return nil
}

// PublishReplace moves the existing final path aside and installs the staged
// path under the same no-replace primitive. The old content is retained under
// the operation stage (not deleted), so the journal can recover or collect it.
// backup.Path is interpreted inside the operation stage, never as a live path.
func (s *Store) PublishReplace(ctx context.Context, opID uuid.UUID, stagedRel string, final, backup layout.Node) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOpID(opID); err != nil {
		return err
	}
	staged, err := s.stageRel(opID, stagedRel)
	if err != nil {
		return err
	}
	finalRel, err := s.rel(final)
	if err != nil {
		return err
	}
	backupName := backup.Path
	if backupName == "" {
		backupName = "backup"
	}
	backupRel, err := s.stageRel(opID, backupName)
	if err != nil {
		return err
	}
	if _, err := s.root.Stat(finalRel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return layout.ErrNotFound
		}
		return err
	}
	if err := s.mkdirParent(backupRel); err != nil {
		return err
	}
	if err := fsrename.RenameNoReplace(s.abs(finalRel), s.abs(backupRel)); err != nil {
		return fmt.Errorf("tree: move old content aside: %w", err)
	}
	if err := fsrename.RenameNoReplace(s.abs(staged), s.abs(finalRel)); err != nil {
		// Best-effort restore: the caller still sees the original error, and the
		// backup remains at a known operation path for recovery.
		restoreErr := fsrename.RenameNoReplace(s.abs(backupRel), s.abs(finalRel))
		if restoreErr != nil {
			return fmt.Errorf("tree: install staged content: %w (restore old content also failed: %v)", err, restoreErr)
		}
		return fmt.Errorf("tree: install staged content: %w", err)
	}
	return nil
}

// Discard removes one operation's private stage. It never touches a live path.
func (s *Store) Discard(ctx context.Context, opID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOpID(opID); err != nil {
		return err
	}
	root, err := s.stageRel(opID, "")
	if err != nil {
		return err
	}
	if err := s.root.RemoveAll(root); err != nil {
		return fmt.Errorf("tree: discard stage: %w", err)
	}
	return nil
}

// StagePath reports the operation-private path for diagnostics/tests. It is not
// a live address and must never be handed to an external request.
func (s *Store) StagePath(opID uuid.UUID, rel string) (string, error) {
	if err := validateOpID(opID); err != nil {
		return "", err
	}
	staged, err := s.stageRel(opID, rel)
	if err != nil {
		return "", err
	}
	return s.abs(staged), nil
}

func validateOpID(opID uuid.UUID) error {
	if opID == uuid.Nil {
		return fmt.Errorf("%w: empty operation id", layout.ErrInvalidNode)
	}
	return nil
}

func (s *Store) stageRel(opID uuid.UUID, rel string) (string, error) {
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("%w: absolute path", layout.ErrUnsafePath)
	}
	parts := []string{stageDir, opID.String()}
	if rel != "" {
		segs, err := layout.ValidatePath(rel)
		if err != nil {
			return "", err
		}
		parts = append(parts, segs...)
	}
	return path.Join(parts...), nil
}

func (s *Store) abs(rel string) string {
	return filepath.Join(s.dir, filepath.FromSlash(rel))
}

// Ensure the tree package still satisfies the existing read/write contract.
var _ layout.ContentStore = (*Store)(nil)
var _ consistency.StagingStore = (*Store)(nil)

// ProbeNoReplace verifies the primitive on this store's actual volume. Callers
// must refuse to enable the tree layout when this fails.
func (s *Store) ProbeNoReplace() error { return fsrename.Probe(s.dir) }
