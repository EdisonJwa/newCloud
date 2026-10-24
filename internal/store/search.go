package store

// Search.
//
// A name search across the whole tree. It is an optional capability for the same reason as
// RecentFiles: a backend that cannot answer it cheaply should lose search, not make every other
// call slower.
//
// This file holds TWO interfaces, because there are two questions and they have different answers:
//
//   - Searcher is the original name-only search. It stays because it is the honest fallback for a
//     backend that cannot filter, and because the conformance suite pins it.
//
//   - FilterSearcher is what core-scope §7.1 asks for: the same name match plus the filter, sort
//     and pagination vocabulary a file manager needs, and a cursor rather than a single unbounded
//     page. Both shipped backends implement it; a third backend may implement only Searcher and
//     the HTTP layer then degrades to name-only results rather than failing.
//
// Content search (§7.2) is deliberately NOT here. It is served by an external engine that is a
// cache of content this server can already read, and it never becomes the source of truth for what
// exists — see internal/search. Keeping it out of the store interface is what makes "the engine is
// down" a degradation rather than a data-availability problem.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SearchSort is the order a result set is returned in (§7.1).
type SearchSort string

const (
	// SearchSortModified is the default, and the one that answers "what did I touch lately".
	SearchSortModified SearchSort = "modified"
	// SearchSortName is what a palette wants: predictable, and easy to scan.
	SearchSortName SearchSort = "name"
	// SearchSortSize answers "what is taking the space".
	SearchSortSize SearchSort = "size"
)

// Valid reports whether s is a supported sort key.
func (s SearchSort) Valid() bool {
	return s == SearchSortModified || s == SearchSortName || s == SearchSortSize
}

// ErrInvalidSearch reports a filter combination this server will not answer.
//
// Distinct from a bad cursor: one is a request the caller can fix by removing a filter, the other
// is a token that is not ours. Both are 400, but they name different things in the message.
var ErrInvalidSearch = errors.New("store: invalid search")

// SearchQuery is one search request.
//
// Every filter is optional and they compose with AND. The zero value is "everything the caller can
// see, newest first" — an empty query with no filters is a listing, which is what a file manager's
// filter bar produces when the user clears it.
type SearchQuery struct {
	// Text matches the node's NAME, case-insensitively, as a literal substring.
	//
	// It is not a pattern and is not interpreted: callers pass user input, and a query of "%" must
	// match files whose names contain a percent sign.
	Text string
	// Kind restricts to files or directories. Empty means both.
	Kind Kind
	// MimePrefix restricts to a content type prefix, e.g. "image/" or "text/". A directory has no
	// MIME type, so this filter implies files.
	MimePrefix string
	// SizeMin/SizeMax bound the byte size, inclusive. Zero means unbounded.
	SizeMin int64
	SizeMax int64
	// ModifiedAfter/ModifiedBefore bound the modification time. Zero means unbounded.
	ModifiedAfter  time.Time
	ModifiedBefore time.Time
	// TagID restricts to nodes carrying one tag.
	TagID *uuid.UUID
	// Favorite restricts to the caller's starred nodes.
	Favorite bool
	// Sort selects the ordering. Empty means SearchSortModified.
	Sort SearchSort
	// Limit bounds one page. Zero means the store's default.
	Limit int
	// Cursor is the opaque `cursor` from the previous page. Empty starts at the first page.
	Cursor string
}

// SearchPage is one page of results plus the cursor for the next one.
type SearchPage struct {
	Items []RecentFile
	// Cursor is the token for the next page, or "" when this page is the last.
	//
	// An empty cursor means "there is no more", which the HTTP layer renders as `truncated: false`.
	// Returning a cursor on the last page would make a client ask for one more empty page, which is
	// harmless but wrong.
	Cursor string
}

// Searcher is implemented by stores that can search node names.
type Searcher interface {
	// Search returns at most limit non-trashed nodes owned by ownerID whose name contains
	// query, newest first, with the path each was found at.
	Search(ctx context.Context, ownerID uuid.UUID, query string, limit int) ([]RecentFile, error)
}

// FilterSearcher is implemented by stores that can answer a filtered, sorted, paged search.
type FilterSearcher interface {
	// SearchNodes returns one page of results and the cursor for the next one.
	SearchNodes(ctx context.Context, ownerID uuid.UUID, q SearchQuery) (SearchPage, error)
}

// searchCursor is the decoded form of a search page cursor.
//
// It carries the sort key so a cursor issued for one ordering is refused under another: page 2 of a
// name sort keyed on a name is meaningless as page 2 of a size sort, and silently reinterpreting it
// would produce results the client cannot explain.
type searchCursor struct {
	Sort string
	Key  string
	ID   string
}

// encodeSearchCursor renders a cursor as an opaque token.
//
// Base64 so that a name containing the separator cannot forge a position, and so that the shape is
// not something a client might decide to construct itself (ADR-0012: clients echo cursors, they do
// not build them).
func encodeSearchCursor(c searchCursor) string {
	raw := c.Sort + "\x00" + c.Key + "\x00" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeSearchCursor parses a cursor and checks it was issued for this ordering.
func decodeSearchCursor(token string, sort SearchSort) (searchCursor, error) {
	if token == "" {
		return searchCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return searchCursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidCursor)
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		return searchCursor{}, fmt.Errorf("%w: malformed cursor", ErrInvalidCursor)
	}
	c := searchCursor{Sort: parts[0], Key: parts[1], ID: parts[2]}
	if c.Sort != string(sort) {
		return searchCursor{}, fmt.Errorf(
			"%w: this cursor was issued for a %q sort, not %q", ErrInvalidCursor, c.Sort, sort)
	}
	if _, err := uuid.Parse(c.ID); err != nil {
		return searchCursor{}, fmt.Errorf("%w: malformed cursor id", ErrInvalidCursor)
	}
	return c, nil
}

// searchSortOf normalises an empty sort to the default.
func searchSortOf(s SearchSort) SearchSort {
	if s.Valid() {
		return s
	}
	return SearchSortModified
}

// searchLimitOf clamps a page size.
//
// Unclamped, `?limit=1000000` is a one-line denial of service against the database and the browser
// — the same reasoning as parseLimit in the HTTP layer, applied where the query is built.
func searchLimitOf(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// Validate rejects a filter combination that cannot mean anything, before it reaches a backend.
//
// The cases are ones where the alternative is a silently empty result: asking for directories with
// a MIME prefix has no answer, and returning nothing would make the UI report "no matches" for a
// contradiction rather than for an absence.
func (q SearchQuery) Validate() error {
	if q.Kind != "" && q.Kind != KindFile && q.Kind != KindDir {
		return fmt.Errorf("%w: kind must be %q or %q", ErrInvalidSearch, KindFile, KindDir)
	}
	if q.Kind == KindDir && q.MimePrefix != "" {
		return fmt.Errorf("%w: a directory has no MIME type, so kind=dir cannot take a mime filter",
			ErrInvalidSearch)
	}
	if q.SizeMin < 0 || q.SizeMax < 0 {
		return fmt.Errorf("%w: size bounds cannot be negative", ErrInvalidSearch)
	}
	if q.SizeMax > 0 && q.SizeMin > q.SizeMax {
		return fmt.Errorf("%w: size_min is larger than size_max", ErrInvalidSearch)
	}
	if !q.ModifiedAfter.IsZero() && !q.ModifiedBefore.IsZero() &&
		q.ModifiedAfter.After(q.ModifiedBefore) {
		return fmt.Errorf("%w: modified_after is later than modified_before", ErrInvalidSearch)
	}
	if q.Sort != "" && !q.Sort.Valid() {
		return fmt.Errorf("%w: sort must be modified, name or size", ErrInvalidSearch)
	}
	return nil
}

// matchesSearch reports whether a node satisfies every filter, for backends that filter in Go.
//
// It exists so the in-memory backend and the SQL predicates cannot drift in SPIRIT: the SQL is
// written clause by clause against this function's comments, and the conformance suite holds the
// two to the same answers.
//
// The name match is a literal, case-insensitive substring — not a pattern. A query of "%" matches a
// name containing "%", which is what a user typing into a search box means.
func matchesSearch(n Node, q SearchQuery, starred bool, tagIDs map[uuid.UUID]bool) bool {
	// A NAMESPACE ROOT is not a search result.
	//
	// It has no name and cannot be opened, so returning it would put an untitled row at the top of
	// the palette. It only became reachable when an empty `q` started meaning "list everything the
	// filters allow" (§7.1's filter bar): with a non-empty query the empty name never matched, which
	// is why this was not needed before.
	if n.ParentID == nil {
		return false
	}
	if n.TrashedAt != nil {
		return false
	}
	if q.Text != "" && !strings.Contains(strings.ToLower(n.Name), strings.ToLower(q.Text)) {
		return false
	}
	if q.Kind != "" && n.Kind != q.Kind {
		return false
	}
	if q.MimePrefix != "" {
		if n.Kind != KindFile {
			return false
		}
		if !strings.HasPrefix(strings.ToLower(n.Mime), strings.ToLower(q.MimePrefix)) {
			return false
		}
	}
	if q.SizeMin > 0 && n.Size < q.SizeMin {
		return false
	}
	if q.SizeMax > 0 && n.Size > q.SizeMax {
		return false
	}
	if !q.ModifiedAfter.IsZero() && n.ModifiedAt.Before(q.ModifiedAfter) {
		return false
	}
	if !q.ModifiedBefore.IsZero() && n.ModifiedAt.After(q.ModifiedBefore) {
		return false
	}
	if q.TagID != nil && !tagIDs[*q.TagID] {
		return false
	}
	if q.Favorite && !starred {
		return false
	}
	return true
}
