package store

// Recent files.
//
// The home screen leads with the files somebody last worked on, so the server needs to answer
// "my most recently modified files" across the whole tree. Doing it by walking directories
// client-side would be wrong at any real size, and showing only the root folder would be a
// quiet lie about what the product can do.
//
// It is an optional capability (like MarkersFor) rather than part of Store: a backend that
// cannot answer it cheaply should lose the home screen's recents, not fail to compile or make
// every other call slow.

import (
	"context"

	"github.com/google/uuid"
)

// RecentFile is a node plus the path it was found at.
type RecentFile struct {
	Node Node
	// Path is slash-separated and relative to the owner's root, e.g. "合同/2026/q1.pdf".
	Path string
}

// RecentReader is implemented by stores that can list recently modified files.
type RecentReader interface {
	// RecentFiles returns at most limit non-trashed files owned by ownerID, newest first.
	RecentFiles(ctx context.Context, ownerID uuid.UUID, limit int) ([]RecentFile, error)
}
