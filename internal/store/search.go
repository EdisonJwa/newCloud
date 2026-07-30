package store

// Search.
//
// A name search across the whole tree. It is an optional capability for the same reason as
// RecentFiles: a backend that cannot answer it cheaply should lose search, not fail to
// compile or make every other call slower.
//
// It matches on the name only. Content search would need an index maintained on write, which
// is a much larger commitment than this feature is worth today, and a half-built index that
// silently misses files is worse than an honest name search.

import (
	"context"

	"github.com/google/uuid"
)

// Searcher is implemented by stores that can search node names.
type Searcher interface {
	// Search returns at most limit non-trashed nodes owned by ownerID whose name contains
	// query, newest first, with the path each was found at.
	Search(ctx context.Context, ownerID uuid.UUID, query string, limit int) ([]RecentFile, error)
}
