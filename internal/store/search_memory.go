package store

// Filtered search in the in-memory store (core-scope §7.1).
//
// The linear scan is kept deliberately: this backend exists for tests and the pre-database wiring,
// where the whole tree is in RAM anyway. What matters is that it answers EXACTLY what the SQL does,
// because the shared conformance suite runs both against the same queries and any divergence would
// make a green test mean nothing about production.
//
// The cursor handling is the part worth reading: it is (sort key, node id) in both backends, so a
// page boundary that lands between two rows with identical timestamps cannot repeat or skip one.

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

var _ FilterSearcher = (*Memory)(nil)

// Search implements Searcher: the unfiltered name search the older routes use.
func (m *Memory) Search(ctx context.Context, ownerID uuid.UUID, query string,
	limit int) ([]RecentFile, error) {

	page, err := m.SearchNodes(ctx, ownerID, SearchQuery{Text: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// SearchNodes implements FilterSearcher.
func (m *Memory) SearchNodes(ctx context.Context, ownerID uuid.UUID, q SearchQuery) (SearchPage, error) {
	if err := ctx.Err(); err != nil {
		return SearchPage{}, err
	}
	if err := q.Validate(); err != nil {
		return SearchPage{}, err
	}
	sortKey := searchSortOf(q.Sort)
	limit := searchLimitOf(q.Limit)
	cursor, err := decodeSearchCursor(q.Cursor, sortKey)
	if err != nil {
		return SearchPage{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Per-node tag membership, built once: asking "does this node carry tag T" inside the filter
	// loop would be a scan of nodeTags per candidate.
	if q.TagID != nil {
		hasTag := false
		for key := range m.nodeTags {
			if key[1] == *q.TagID {
				hasTag = true
				break
			}
		}
		if !hasTag {
			// No node carries this tag. Answering "empty" here is not an optimisation: the
			// candidate loop below would otherwise have to distinguish "no such tag" from "tag
			// with no nodes", and the caller cannot act on the difference anyway.
			return SearchPage{Items: []RecentFile{}}, nil
		}
	}

	keep := make([]*memNode, 0, 16)
	for _, rec := range m.nodes {
		if rec.node.OwnerID != ownerID {
			continue
		}
		starred := false
		if q.Favorite {
			_, starred = m.favorites[[2]uuid.UUID{ownerID, rec.node.ID}]
		}
		var tagIDs map[uuid.UUID]bool
		if q.TagID != nil {
			tagIDs = map[uuid.UUID]bool{}
			if _, ok := m.nodeTags[[2]uuid.UUID{rec.node.ID, *q.TagID}]; ok {
				tagIDs[*q.TagID] = true
			}
		}
		if matchesSearch(rec.node, q, starred, tagIDs) {
			keep = append(keep, rec)
		}
	}

	// One ordering, stated once. The id tie-break is what makes the keyset cursor total: two rows
	// with the same mtime are otherwise interchangeable, and a page boundary between them can
	// return both or neither.
	sort.Slice(keep, func(i, j int) bool {
		a, b := keep[i].node, keep[j].node
		switch sortKey {
		case SearchSortName:
			la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
			if la != lb {
				return la < lb
			}
			return a.ID.String() < b.ID.String()
		case SearchSortSize:
			if a.Size != b.Size {
				return a.Size > b.Size
			}
			return a.ID.String() < b.ID.String()
		default: // SearchSortModified
			if !a.ModifiedAt.Equal(b.ModifiedAt) {
				return a.ModifiedAt.After(b.ModifiedAt)
			}
			return a.ID.String() < b.ID.String()
		}
	})

	// Apply the cursor: strictly after the position it names, in this ordering.
	if cursor.ID != "" {
		after := uuid.MustParse(cursor.ID)
		pos := sort.Search(len(keep), func(i int) bool {
			return searchAfter(keep[i].node, sortKey, cursor.Key, after)
		})
		keep = keep[pos:]
	}

	page := SearchPage{Items: make([]RecentFile, 0, limit)}
	for i, rec := range keep {
		if i >= limit {
			// One extra row is what tells "this is the last page" from "there is more" without a
			// second query or a count.
			last := keep[limit-1].node
			page.Cursor = encodeSearchCursor(searchCursor{
				Sort: string(sortKey), Key: searchSortKeyOf(last, sortKey), ID: last.ID.String(),
			})
			break
		}
		page.Items = append(page.Items, RecentFile{Node: rec.node, Path: m.pathLocked(rec.node)})
	}
	return page, nil
}

// searchAfter reports whether n sorts strictly after the (key, id) position in this ordering.
func searchAfter(n Node, sortKey SearchSort, key string, id uuid.UUID) bool {
	cmp := compareSortKey(n, sortKey, key)
	if cmp != 0 {
		return cmp > 0
	}
	return n.ID.String() > id.String()
}

// compareSortKey reports n's position against a cursor's stored key: <0 before, 0 same, >0 after.
func compareSortKey(n Node, sortKey SearchSort, key string) int {
	switch sortKey {
	case SearchSortName:
		return strings.Compare(strings.ToLower(n.Name), key)
	case SearchSortSize:
		want, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			// A malformed size key cannot be compared; treat it as "after" so the walk
			// terminates rather than looping on a position that never advances.
			return 1
		}
		switch {
		case n.Size > want:
			return -1 // larger first
		case n.Size < want:
			return 1
		}
		return 0
	default: // SearchSortModified
		want, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return 1
		}
		switch {
		case n.ModifiedAt.UnixNano() < want:
			return 1 // older, so later in a newest-first order
		case n.ModifiedAt.UnixNano() > want:
			return -1
		}
		return 0
	}
}

// searchSortKeyOf renders the sort key stored in a page cursor.
func searchSortKeyOf(n Node, sortKey SearchSort) string {
	switch sortKey {
	case SearchSortName:
		return strings.ToLower(n.Name)
	case SearchSortSize:
		return strconv.FormatInt(n.Size, 10)
	default:
		return strconv.FormatInt(n.ModifiedAt.UnixNano(), 10)
	}
}
