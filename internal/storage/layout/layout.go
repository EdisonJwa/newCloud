// Package layout defines how user-visible files map onto physical storage.
//
// Two layouts are supported, chosen per deployment:
//
//	LayoutBlob -- content-addressed store. A file's bytes live under
//	              <root>/ab/cd/<sha256>. Directory structure exists only in
//	              PostgreSQL. Identical content is stored once.
//
//	LayoutTree -- classic filesystem tree. A file's bytes live at
//	              <root>/<user>/<path>. What the user sees is what is on disk.
//
// The two differ in more than file naming, so the choice is a deployment
// decision, not a UI toggle:
//
//	                     blob                     tree
//	disk layout          flat, hashed            mirrors the user's view
//	deduplication        yes                     no
//	versions             cheap (shared blobs)    copy per version
//	inspect with ls      no (hashes)             yes
//	rsync/rclone sync    awkward                 ideal
//	delete a user        refcount                     rm -rf
//	path traversal risk  none (no paths on disk) every segment must be validated
//	renaming a folder    one row                   rename() across the subtree
//
// This package implements the contract both layouts satisfy. The WebDAV and API
// layers talk only to ContentStore, so neither knows which layout is active.
package layout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Mode is the storage layout selector.
type Mode string

const (
	// ModeBlob is content-addressed storage: files are keyed by their hash.
	ModeBlob Mode = "blob"
	// ModeTree mirrors the user-visible hierarchy on disk.
	ModeTree Mode = "tree"
)

// Valid reports whether m is a supported layout.
func (m Mode) Valid() bool { return m == ModeBlob || m == ModeTree }

// Node describes one file or directory as the application sees it.
//
// The blob layout fills Hash; the tree layout fills Path. A caller that needs
// bytes uses ContentStore.Open, never the fields directly, so the difference
// never leaks upward.
type Node struct {
	// Owner is the account that owns the subtree. For group spaces this is the
	// space's owner, not the individual member: two users writing to a shared
	// folder must land in the same place on disk.
	Owner string
	// Path is the user-visible path relative to the owner's root, always
	// slash-separated and already normalised. Empty string is the root itself.
	Path string
	// Hash is set in blob mode; the zero value in tree mode.
	Hash [32]byte
	// IsDir distinguishes directories from files.
	IsDir bool
	// Size is the file length in bytes; 0 for directories.
	Size int64
	// ModTime is the last modification time used for getlastmodified and
	// If-Modified-Since handling.
	ModTime time.Time
}

// PutResult reports what a write actually did, which differs by layout.
type PutResult struct {
	// Hash is the content address (blob mode) or zero (tree mode).
	Hash [32]byte
	// Size is the number of bytes written.
	Size int64
	// Deduplicated is true when identical content already existed and no new
	// bytes were written. Always false in tree mode.
	Deduplicated bool
	// ReplacedExisting is true when the write overwrote a previous file.
	ReplacedExisting bool
}

// ContentStore is the write/read contract shared by both layouts.
//
// Implementations must be safe for concurrent use and must never expose a
// partially written file: a reader either sees the old content or the new
// content, never a truncated mix. That is what makes an interrupted upload
// harmless rather than corrupting.
type ContentStore interface {
	// Mode reports the active layout.
	Mode() Mode

	// Put writes a file. r is streamed; size may be -1 when unknown, in which
	// case implementations buffer to a temp file anyway (they must not buffer
	// in memory).
	Put(ctx context.Context, n Node, r io.Reader, size int64) (PutResult, error)

	// Open returns a seekable reader for Range requests.
	Open(ctx context.Context, n Node) (io.ReadSeekCloser, error)

	// Stat returns metadata without reading content.
	Stat(ctx context.Context, n Node) (Node, error)

	// Mkdir creates a directory and any missing parents.
	Mkdir(ctx context.Context, n Node) error

	// Move relocates a file or directory from src to dst. Both must have the
	// same Owner; cross-owner moves are a copy-and-delete at the application
	// layer, not a storage operation.
	Move(ctx context.Context, src, dst Node) error

	// Delete removes a file, or a directory and everything under it.
	Delete(ctx context.Context, n Node) error

	// List returns the direct children of a directory, sorted by name.
	// limit <= 0 means no limit; callers pass the configured cap to protect
	// against a directory with a million entries.
	List(ctx context.Context, dir Node, limit int) ([]Node, error)

	// Walk streams every descendant of dir. Blob mode cannot enumerate
	// without the database, so it returns ErrWalkUnsupported and the caller
	// falls back to a metadata query.
	Walk(ctx context.Context, dir Node, fn func(Node) error) error

	// Capabilities reports what this layout can do, so the UI can hide
	// features that do not apply rather than failing at click time.
	Capabilities() Caps
}

// Caps describes layout-specific abilities.
type Caps struct {
	// Deduplicated is true when identical content is stored once.
	Deduplicated bool
	// CheapVersions is true when keeping N versions costs no extra disk for
	// unchanged content.
	CheapVersions bool
	// DirectFSInspect is true when an administrator can browse the store with
	// ordinary tools and recognise the files.
	DirectFSInspect bool
	// EfficientRename is true when renaming a directory is O(1) rather than
	// proportional to the number of descendants.
	EfficientRename bool
}

// CapsFor returns the capability set of a layout.
func CapsFor(m Mode) Caps {
	switch m {
	case ModeBlob:
		return Caps{
			Deduplicated:    true,
			CheapVersions:   true,
			DirectFSInspect: false,
			EfficientRename: true, // a directory is a database row
		}
	case ModeTree:
		return Caps{
			Deduplicated:    false,
			CheapVersions:   false, // each version is a full copy
			DirectFSInspect: true,
			EfficientRename: false, // rename() walks the subtree
		}
	default:
		return Caps{}
	}
}

// Storage errors. Both layouts return these rather than their own, so the HTTP
// layer maps them to status codes in one place.
var (
	// ErrNotFound: the path or content address does not exist.
	ErrNotFound = errors.New("storage: not found")
	// ErrUnsafePath: the name cannot be represented on disk safely.
	ErrUnsafePath = errors.New("storage: unsafe path component")
	// ErrSizeMismatch: the payload length differs from the declared length.
	ErrSizeMismatch = errors.New("storage: size mismatch")
	// ErrInvalidNode: the operation is not meaningful for this node (for
	// example, writing a directory, or moving across owners).
	ErrInvalidNode = errors.New("storage: invalid node for operation")
	// ErrWalkUnsupported: this layout cannot enumerate without metadata help.
	ErrWalkUnsupported = errors.New("storage: walk unsupported by this layout")
)

// windowsIllegal are characters Windows forbids in a filename. The colon is the
// dangerous one: "C:evil" is a drive-relative path and "file:stream" addresses
// an NTFS alternate data stream, so a name containing it is never just a name.
const windowsIllegal = `<>:"|?*`

// windowsReserved are device names Windows refuses, with or without an
// extension. A file called CON.txt syncs to a Windows client and then cannot be
// opened, which users report as data loss.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ValidateSegment checks one path component.
//
// Separators and traversal sequences are rejected rather than silently cleaned:
// "cleaned" input that differs from what the user submitted is how two distinct
// files end up sharing one name.
func ValidateSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." {
		return fmt.Errorf("%w: empty or traversal segment", ErrUnsafePath)
	}
	if !utf8.ValidString(seg) {
		return fmt.Errorf("%w: invalid UTF-8", ErrUnsafePath)
	}
	if len(seg) > 255 {
		return fmt.Errorf("%w: longer than 255 bytes", ErrUnsafePath)
	}
	for _, r := range seg {
		switch {
		case r == 0 || r == '/' || r == '\\':
			return fmt.Errorf("%w: separator or NUL", ErrUnsafePath)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("%w: control character", ErrUnsafePath)
		case strings.ContainsRune(windowsIllegal, r):
			return fmt.Errorf("%w: character %q is not portable", ErrUnsafePath, r)
		}
	}
	// Windows strips trailing dots and spaces, so "report." and "report" would
	// collide after a round trip through a Windows client.
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return fmt.Errorf("%w: trailing dot or space", ErrUnsafePath)
	}
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return fmt.Errorf("%w: reserved device name", ErrUnsafePath)
	}
	return nil
}

// ValidatePath checks a slash-separated relative path and returns its segments.
// An empty path is valid and denotes the root.
func ValidatePath(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("%w: absolute path", ErrUnsafePath)
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if err := ValidateSegment(seg); err != nil {
			return nil, err
		}
	}
	return segs, nil
}

// Join builds a relative path from segments, validating each one.
func Join(segs ...string) (string, error) {
	for _, s := range segs {
		if err := ValidateSegment(s); err != nil {
			return "", err
		}
	}
	return strings.Join(segs, "/"), nil
}
