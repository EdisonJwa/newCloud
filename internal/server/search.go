package server

// Name search across the tree.
//
// The command palette is where people type a filename; before this endpoint it could only
// offer the files they had touched recently, which is not search. The scope is stated in the
// UI and here: names only, not contents.
//
// An empty query is not an error and not a full listing — it returns nothing. A search box
// that dumps the whole library while you are still typing its first character is noise.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/EdisonJwa/newCloud/internal/store"
)

const searchMax = 200

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, searchMax)
		}
	}

	// One character matches most of the library. Requiring two keeps the result meaningful and
	// the scan bounded.
	if len([]rune(query)) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"items": []fileJSON{}, "query": query})
		return
	}

	searcher, ok := s.deps.Meta.(store.Searcher)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": []fileJSON{}, "query": query})
		return
	}

	hits, err := searcher.Search(r.Context(), actor.ID(), query, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	nodes := make([]store.Node, 0, len(hits))
	items := make([]fileJSON, 0, len(hits))
	for _, hit := range hits {
		items = append(items, toFileJSON(hit.Node, hit.Path))
		nodes = append(nodes, hit.Node)
	}

	// The same provenance marks the listing carries, so a search hit that is shared says so.
	if markers := s.nodeMarkers(r.Context(), actor.ID(), nodes); markers != nil {
		for i, hit := range hits {
			if m, ok := markers[hit.Node.ID]; ok {
				items[i].ShareCount = m.ShareCount
				items[i].HasVersions = m.HasVersions
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "query": query})
}
