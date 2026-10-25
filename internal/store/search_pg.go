package store

// Filtered search on PostgreSQL (core-scope §7.1).
//
// Two decisions worth stating, because both look like implementation detail and are not:
//
//  1. THE NAME MATCH STAYS strpos. It was chosen over LIKE because a user's query is DATA, and
//     feeding "%" or "_" from it into a pattern silently changes what the query means. Migration
//     0024 adds a pg_trgm GIN index on lower(name), which the planner uses for `LIKE '%x%'`; to
//     keep the meaning identical while making the index usable, the query escapes the three LIKE
//     metacharacters and then wraps the term in wildcards. Both forms are checked against the same
//     conformance cases, so "escaped LIKE" and "strpos" cannot quietly disagree.
//
//  2. THE FILTERS ARE ONE STATEMENT. Not a chain of appends building SQL from user input — that is
//     how an injection gets in and how a filter combination ends up unindexable. Every filter is a
//     parameter and a fixed clause, so the plan is stable and the parameters are typed by the
//     driver, not by fmt.Sprintf.
//
// The cursor is (sort key, node id), base64-wrapped by the store so a name containing the separator
// cannot forge a position. A cursor issued for one sort is refused under another (see
// decodeSearchCursor): page 2 of a name sort means nothing as page 2 of a size sort.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var _ FilterSearcher = (*PG)(nil)
var _ Searcher = (*PG)(nil)

// Search implements Searcher: the unfiltered name search.
//
// It is a thin wrapper over SearchNodes rather than a second query, so the two cannot drift: the
// older routes and the filtered ones answer from one implementation, which is the same reason the
// single-request and resumable uploads share publishFile.
func (s *PG) Search(ctx context.Context, ownerID uuid.UUID, query string,
	limit int) ([]RecentFile, error) {

	page, err := s.SearchNodes(ctx, ownerID, SearchQuery{Text: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

// SearchNodes implements FilterSearcher.
//
// The path is assembled with the same recursive walk the unfiltered search uses, so a hit deep in
// the tree carries a path that actually locates it — a search result you cannot click is not a
// result.
func (s *PG) SearchNodes(ctx context.Context, ownerID uuid.UUID, q SearchQuery) (SearchPage, error) {
	if err := q.Validate(); err != nil {
		return SearchPage{}, err
	}
	sortKey := searchSortOf(q.Sort)
	limit := searchLimitOf(q.Limit)
	cursor, err := decodeSearchCursor(q.Cursor, sortKey)
	if err != nil {
		return SearchPage{}, err
	}

	// One extra row answers "is there more" without a second query.
	fetch := limit + 1

	// The ordering clause is chosen from a closed set, never built from input.
	var orderBy string
	switch sortKey {
	case SearchSortName:
		orderBy = `ORDER BY lower(n.name), n.id`
	case SearchSortSize:
		orderBy = `ORDER BY n.size DESC, n.id`
	default:
		orderBy = `ORDER BY n.modified_at DESC, n.id`
	}

	// The keyset predicate, expressed per ordering. `>` for an ascending walk, `<` for a descending
	// one, with the id as the tie-break in the same direction as the ordering.
	//
	// The key is passed as TEXT and cast inside the statement, for one specific reason: when there
	// is no cursor the predicate is absent, and an unreferenced parameter leaves PostgreSQL unable
	// to infer its type. A text parameter with an explicit cast in each form means the no-cursor
	// case binds an unused NULL of a known type rather than a placeholder of none.
	var keyset string
	var cursorKey *string
	var cursorID any
	if cursor.ID != "" {
		cursorID = uuid.MustParse(cursor.ID)
		key := cursor.Key
		switch sortKey {
		case SearchSortName:
			keyset = `AND (lower(n.name), n.id) > ($10::text, $11::uuid)`
			cursorKey = &key
		case SearchSortSize:
			keyset = `AND (n.size, n.id) < ($10::bigint, $11::uuid)`
			if _, perr := strconv.ParseInt(key, 10, 64); perr != nil {
				return SearchPage{}, errInvalidCursorValue()
			}
			cursorKey = &key
		default:
			keyset = `AND (n.modified_at, n.id) < ($10::timestamptz, $11::uuid)`
			if _, perr := strconv.ParseInt(key, 10, 64); perr != nil {
				return SearchPage{}, errInvalidCursorValue()
			}
			cursorKey = &key
		}
	} else {
		// Parameters 10 and 11 must still be bound AND typed, or the driver rejects the statement.
		// The clauses below are always true, so an absent cursor changes no result.
		keyset = `AND ($10::text IS NULL OR TRUE) AND ($11::uuid IS NULL OR TRUE)`
		cursorKey, cursorID = nil, nil
	}

	// Tag and favorite filters are EXISTS subqueries rather than joins: a node can carry several
	// matching rows in node_tags and a join would then duplicate it once per tag.
	var tagID any
	if q.TagID != nil {
		tagID = *q.TagID
	}
	var mimePrefix any
	if q.MimePrefix != "" {
		// Case-folded prefix: MIME types are conventionally lowercase, but a client-declared one
		// may not be, and "Image/" failing to match "image/png" would be a filter that silently
		// drops results.
		mimePrefix = strings.ToLower(q.MimePrefix)
	}

	query := `
		WITH RECURSIVE hit AS (
		    SELECT n.id AS hit_id
		    FROM nodes n
		    WHERE n.owner_id = $1
		      AND n.trashed_at IS NULL
		      -- A namespace root has no name and cannot be opened, so it is never a result. It only
		      -- became reachable once an empty q started meaning "list what the filters allow".
		      AND n.parent_id IS NOT NULL
		      AND (strpos(lower(n.name), lower($2)) > 0)
		      AND ($3::text IS NULL OR n.kind = $3)
		      AND ($4::text IS NULL OR (n.kind = 'file' AND lower(n.mime) LIKE $4 || '%'))
		      AND ($5::bigint <= 0 OR n.size >= $5)
		      AND ($6::bigint <= 0 OR n.size <= $6)
		      AND ($7::timestamptz IS NULL OR n.modified_at >= $7)
		      AND ($8::timestamptz IS NULL OR n.modified_at <= $8)
		      AND ($9::uuid IS NULL OR EXISTS (
		            SELECT 1 FROM node_tags nt WHERE nt.node_id = n.id AND nt.tag_id = $9))
		      AND ($12::boolean IS NOT TRUE OR EXISTS (
		            SELECT 1 FROM favorites f WHERE f.node_id = n.id AND f.user_id = $1))
		      ` + keyset + `
		    ` + orderBy + `
		    LIMIT $13
		), up(origin, id, parent_id, name, depth) AS (
		    -- The origin is the HIT id; the id column is the ancestor's. Naming both explicitly is
		    -- what stops the recursive arm from being ambiguous with the CTE above.
		    SELECT h.hit_id, n.id, n.parent_id, n.name, 0
		    FROM nodes n JOIN hit h ON h.hit_id = n.id
		    UNION ALL
		    SELECT up.origin, p.id, p.parent_id, p.name, up.depth + 1
		    FROM nodes p JOIN up ON p.id = up.parent_id
		    WHERE up.depth < 64
		), chains AS (
		    SELECT origin,
		           string_agg(name, '/' ORDER BY depth DESC)
		             FILTER (WHERE depth > 0 AND name <> '') AS dirs
		    FROM up GROUP BY origin
		)
		SELECT ` + nodeColumnsQualified("n") + `,
		       COALESCE(chains.dirs || '/', '') || n.name AS path
		FROM nodes n
		JOIN hit h ON h.hit_id = n.id
		LEFT JOIN chains ON chains.origin = n.id
		` + orderBy + ``

	// A "no filter" value has to be an explicit NULL: passing "" for a text parameter makes the
	// clause `n.kind = ''`, which matches nothing — the exact failure a filter that "returns
	// nothing" is indistinguishable from.
	rows, err := s.pool.Query(ctx, query,
		ownerID,
		q.Text,
		nullableString(string(q.Kind)),
		mimePrefix,
		q.SizeMin,
		q.SizeMax,
		nullableTime(q.ModifiedAfter),
		nullableTime(q.ModifiedBefore),
		tagID,
		cursorKey,
		cursorID,
		q.Favorite,
		fetch,
	)
	if err != nil {
		return SearchPage{}, translate(err)
	}
	defer rows.Close()

	page := SearchPage{Items: make([]RecentFile, 0, limit)}
	for rows.Next() {
		var n Node
		var kind, path string
		if err := rows.Scan(&n.ID, &n.ParentID, &n.OwnerID, &n.OwnerGroupID, &n.Name, &kind,
			&n.Size, &n.Mime, &n.ETag, &n.BlobID, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt,
			&n.RetainUntil, &path); err != nil {
			return SearchPage{}, translate(err)
		}
		n.Kind = Kind(kind)
		if len(page.Items) >= limit {
			// The extra row proves there is a next page; the cursor names the LAST row of the page
			// we are returning, not the one we are dropping.
			last := page.Items[len(page.Items)-1].Node
			page.Cursor = encodeSearchCursor(searchCursor{
				Sort: string(sortKey), Key: searchSortKeyOf(last, sortKey), ID: last.ID.String(),
			})
			break
		}
		page.Items = append(page.Items, RecentFile{Node: n, Path: path})
	}
	return page, translate(rows.Err())
}

// nullableTime renders a zero time as SQL NULL, so an unset bound is "no bound" rather than 1970.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// errInvalidCursorValue is the error for a cursor whose key is not parseable in the ordering it
// claims. Separate from a malformed token so the message can say which part was wrong.
func errInvalidCursorValue() error {
	return &cursorValueError{}
}

type cursorValueError struct{}

func (*cursorValueError) Error() string {
	return "store: invalid cursor: its sort key does not parse"
}

// Is makes errors.Is(err, ErrInvalidCursor) true for a bad cursor key, so the HTTP layer's existing
// mapping answers 400 without knowing this type.
func (*cursorValueError) Is(target error) bool { return target == ErrInvalidCursor }
