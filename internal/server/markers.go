package server

// Provenance markers for file rows.
//
// A row should say, without being opened, whether the object is shared and whether earlier
// versions exist. That is the audit capability made visible in the place people actually
// look — and it is the difference between "we have an audit log" and "you can see the state
// of your data".
//
// It is done with one batch call per listing, never per row: a directory of five hundred
// files would otherwise issue a thousand queries to draw one screen. Backends that cannot
// answer cheaply are simply not asked, and the decoration is absent rather than slow.

import (
	"context"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/google/uuid"
)

// nodeMarkers returns markers keyed by node id, or nil when the backend cannot answer.
//
// The error is swallowed on purpose: a missing marker is a cosmetic loss, while failing the
// whole listing because a decoration could not be computed would be a real regression.
func (s *Server) nodeMarkers(ctx context.Context, owner uuid.UUID, nodes []store.Node) map[uuid.UUID]store.Marker {
	reader, ok := s.deps.Meta.(store.MarkersFor)
	if !ok || len(nodes) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		// Only files can be shared by link or carry versions; directories are skipped to
		// keep the query parameter list to what can actually match.
		if n.Kind == store.KindFile {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	markers, err := reader.MarkersFor(ctx, owner, ids)
	if err != nil {
		s.deps.Log.Warn("provenance markers unavailable", "error", err)
		return nil
	}
	return markers
}
