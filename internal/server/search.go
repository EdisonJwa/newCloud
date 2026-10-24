package server

// Search across the tree (core-scope §7.1).
//
// Two things changed from the original name-only search, and both are about honesty:
//
//   - The FILTERS (kind, mime, size, modified, tag, favorite) plus sort and cursor pagination are
//     the §7.1 vocabulary. An empty q with filters is a LISTING, not an error: it is what the file
//     manager's filter bar produces when the user clears the search box, and treating it as "no
//     query, no results" would make the filters unusable.
//
//   - The response says `mode`. The request can ask for `mode=content` — full-text — and this build
//     answers `mode: name` with `degraded: true` when no engine is configured (§7.2). That is the
//     whole posture of §7.2 in one field: search is best-effort telemetry, not evidence, and a
//     deployment without Elasticsearch still has working name+filter search while SAYING so rather
//     than pretending.
//
// An empty q with NO filters still returns nothing. That is not an inconsistency with the paragraph
// above: a search box that dumps the whole library while you are typing its first character is
// noise, and a client that wants the library is asking for a listing, which it already has.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

const searchMax = 200

// searchMode is what the caller asked for.
type searchMode string

const (
	// searchModeName is name-and-filter matching, which needs no engine.
	searchModeName searchMode = "name"
	// searchModeContent is full-text matching, which needs the engine (§7.2).
	searchModeContent searchMode = "content"
	// searchModeAll is name OR content: the union, so a document is found by its filename and by a
	// phrase inside it in one query.
	searchModeAll searchMode = "all"
)

// valid reports whether m is a mode this server understands.
func (m searchMode) valid() bool {
	return m == searchModeName || m == searchModeContent || m == searchModeAll
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())
	q, err := parseSearchQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_search", err.Error())
		return
	}

	// One character matches most of the library. Requiring two keeps the result meaningful and the
	// scan bounded — but only when there is a TEXT. A filtered listing with no text is legitimate.
	if len([]rune(q.Text)) == 1 {
		writeJSON(w, http.StatusOK, s.emptySearchBody(q, searchModeName, false))
		return
	}

	wanted := searchMode(strings.ToLower(r.URL.Query().Get("mode")))
	if wanted == "" {
		wanted = searchModeName
	}
	if !wanted.valid() {
		writeError(w, http.StatusBadRequest, "invalid_mode", "mode must be name, content or all")
		return
	}

	// Content search needs the engine. Without one, the request still answers — with name results
	// and an explicit `degraded: true` — because a search that silently returns a subset would let
	// a user conclude a document does not exist.
	mode := wanted
	degraded := false
	if mode != searchModeName {
		if s.deps.Search == nil || !s.deps.Search.Available() {
			mode = searchModeName
			degraded = true
		}
	}

	if mode != searchModeName {
		s.handleContentSearch(w, r, actor, q, mode)
		return
	}

	searcher, ok := s.deps.Meta.(store.FilterSearcher)
	if !ok {
		// A backend that cannot filter loses the filters, not the endpoint. It is the same
		// optional-capability shape RecentFiles and MarkersFor use, and the response says which
		// mode actually answered.
		writeJSON(w, http.StatusOK, s.emptySearchBody(q, searchModeName, degraded))
		return
	}

	page, serr := searcher.SearchNodes(r.Context(), actor.ID(), q)
	if serr != nil {
		if errors.Is(serr, store.ErrInvalidCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor",
				"the cursor is not one this server issued for this sort; drop it to start over")
			return
		}
		if errors.Is(serr, store.ErrInvalidSearch) {
			writeError(w, http.StatusBadRequest, "invalid_search", serr.Error())
			return
		}
		s.writeStoreError(w, serr)
		return
	}

	s.writeSearchPage(w, r, actor, page, q, mode, degraded)
}

// writeSearchPage renders one page of name-search results with the provenance and social
// annotations a listing row carries.
//
// The markers are the same ones the file listing shows (§7.1: "results carry the same markers as
// listings"), fetched in one batch for the whole page rather than per row.
func (s *Server) writeSearchPage(w http.ResponseWriter, r *http.Request, actor authz.Actor,
	page store.SearchPage, q store.SearchQuery, mode searchMode, degraded bool) {

	items := make([]fileJSON, 0, len(page.Items))
	nodes := make([]store.Node, 0, len(page.Items))
	for _, hit := range page.Items {
		items = append(items, toFileJSON(hit.Node, hit.Path))
		nodes = append(nodes, hit.Node)
	}
	s.annotate(r.Context(), actor.ID(), items, nodes, nil)

	body := map[string]any{
		"items": items,
		"query": q.Text,
		// `truncated` mirrors the listing's flag and `cursor` is what actually continues the walk.
		"truncated": page.Cursor != "",
		"cursor":    page.Cursor,
		// The mode that actually answered, which is not always the one asked for.
		"mode": mode,
	}
	if degraded {
		body["degraded"] = true
		body["degraded_reason"] = "no content-search engine is configured on this deployment; " +
			"the results are name matches"
	}
	if tags := s.tagsOfQuery(r, actor); tags != nil {
		body["tags"] = tags
	}
	writeJSON(w, http.StatusOK, body)
}

// emptySearchBody is the shape every "nothing to show" answer uses, so a client has one decoder.
func (s *Server) emptySearchBody(q store.SearchQuery, mode searchMode, degraded bool) map[string]any {
	body := map[string]any{
		"items": []fileJSON{}, "query": q.Text, "truncated": false, "cursor": "", "mode": mode,
	}
	if degraded {
		body["degraded"] = true
		body["degraded_reason"] = "no content-search engine is configured on this deployment"
	}
	return body
}

// parseSearchQuery reads the §7.1 filter vocabulary off the request.
//
// Every parsing failure is returned rather than ignored: a client that sends `size_min=abc` and
// gets unfiltered results would conclude the filter works and that nothing matches it.
func parseSearchQuery(r *http.Request) (store.SearchQuery, error) {
	v := r.URL.Query()
	q := store.SearchQuery{
		Text:       strings.TrimSpace(v.Get("q")),
		Kind:       store.Kind(strings.ToLower(strings.TrimSpace(v.Get("kind")))),
		MimePrefix: strings.TrimSpace(v.Get("mime")),
		Cursor:     v.Get("cursor"),
		Sort:       store.SearchSort(strings.ToLower(strings.TrimSpace(v.Get("sort")))),
	}

	if raw := strings.TrimSpace(v.Get("size_min")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return q, errBadSizeBound
		}
		q.SizeMin = n
	}
	if raw := strings.TrimSpace(v.Get("size_max")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return q, errBadSizeBound
		}
		q.SizeMax = n
	}
	for _, spec := range []struct {
		name string
		dst  *time.Time
	}{
		{"modified_after", &q.ModifiedAfter},
		{"modified_before", &q.ModifiedBefore},
	} {
		raw := strings.TrimSpace(v.Get(spec.name))
		if raw == "" {
			continue
		}
		at, err := parseSearchTime(raw)
		if err != nil {
			return q, errBadTimeBound
		}
		*spec.dst = at
	}
	if raw := strings.TrimSpace(v.Get("tag")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return q, errBadTag
		}
		q.TagID = &id
	}
	if raw := strings.TrimSpace(v.Get("favorite")); raw != "" {
		// Accept the shapes a browser and a script send: "1", "true", "yes" for on; anything else
		// off. An unparseable value is off rather than an error: a filter switch that cannot be
		// turned off is worse than one that ignores a typo.
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "on":
			q.Favorite = true
		}
	}
	if raw := v.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			q.Limit = min(n, searchMax)
		}
	}
	return q, q.Validate()
}

// parseSearchTime reads a bound in either of the two forms a client sends.
//
// RFC 3339 is what a JSON API client produces; a bare date is what a date picker in a browser
// produces. Both are accepted because refusing the second is a support ticket, and the result is
// unambiguous: a bare date means midnight UTC of that day.
func parseSearchTime(raw string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return at, nil
	}
	if at, err := time.Parse("2006-01-02", raw); err == nil {
		return at.UTC(), nil
	}
	return time.Time{}, errBadTimeBound
}

// Validation errors, so the handler answers 400 with a reason rather than "could not parse".
var (
	errBadSizeBound = errors.New("size_min and size_max must be whole numbers of bytes")
	errBadTimeBound = errors.New("modified_after and modified_before must be RFC 3339 timestamps " +
		"or YYYY-MM-DD dates")
	errBadTag = errors.New("tag must be a tag id")
)
