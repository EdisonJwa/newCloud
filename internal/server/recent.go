package server

// Recent files for the home screen.
//
// The home view leads with what somebody last worked on. It cannot be derived on the client:
// the files worth showing are scattered across the tree, and walking directories to find them
// would be both slow and wrong. So the server answers it directly, in one query.
//
// A backend that does not implement the capability returns an empty list rather than an
// error. The home screen then shows its empty state, which is honest, instead of failing.

import (
	"net/http"
	"strconv"

	"github.com/EdisonJwa/newCloud/internal/store"
)

const (
	recentDefault = 12
	recentMax     = 60
)

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorFrom(r.Context())

	limit := recentDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, cerr := strconv.Atoi(raw); cerr == nil {
			switch {
			case n < 1:
				limit = recentDefault
			case n > recentMax:
				limit = recentMax
			default:
				limit = n
			}
		}
	}

	reader, ok := s.deps.Meta.(store.RecentReader)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"items": []fileJSON{}})
		return
	}

	recents, err := reader.RecentFiles(r.Context(), actor.ID(), limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	nodes := make([]store.Node, 0, len(recents))
	items := make([]fileJSON, 0, len(recents))
	for _, recent := range recents {
		items = append(items, toFileJSON(recent.Node, recent.Path))
		nodes = append(nodes, recent.Node)
	}

	// The same provenance marks the file listing carries, so a recent file that is shared
	// says so here too.
	if markers := s.nodeMarkers(r.Context(), actor.ID(), nodes); markers != nil {
		for i, recent := range recents {
			if m, ok := markers[recent.Node.ID]; ok {
				items[i].ShareCount = m.ShareCount
				items[i].HasVersions = m.HasVersions
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
