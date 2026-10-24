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
	"crypto/sha256"
	"encoding/hex"
	"path"

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

	ids := idsOf(nodes)
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

// idsOf returns the ids of the nodes that can carry markers.
//
// Only files can be shared by link or carry versions, and a directory has no content address, so
// the parameter list is kept to what can actually match.
func idsOf(nodes []store.Node) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == store.KindFile {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// nodeSocial returns the per-viewer annotations (star, tags, checksum) keyed by node id.
//
// Same contract as nodeMarkers: one batch call for the whole page, nil when the backend cannot
// answer, and an error is a COSMETIC loss rather than a failed listing. The alternative — failing a
// directory listing because a decoration could not be computed — is how a database hiccup becomes
// "all my files are gone".
//
// Directories are included, unlike the marker path: a directory can be starred and tagged, it just
// has no checksum.
func (s *Server) nodeSocial(ctx context.Context, viewer uuid.UUID, nodes []store.Node) map[uuid.UUID]store.NodeSocial {
	reader, ok := s.deps.Meta.(store.SocialReader)
	if !ok || len(nodes) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}

	social, err := reader.SocialFor(ctx, viewer, ids)
	if err != nil {
		s.deps.Log.Warn("social annotations unavailable", "error", err)
		return nil
	}
	return social
}

// annotate applies the provenance and social annotations to one page of rows.
//
// It exists as one function because the listing, the search results, the recents, the favorites
// list and the shared-with-me view all need exactly this, and the first version of this codebase
// had three of them each remembering a different subset. Rows whose annotations are unknown keep
// the field absent, which the client renders the same as "no marker".
func (s *Server) annotate(ctx context.Context, viewer uuid.UUID, items []fileJSON,
	nodes []store.Node, byID map[uuid.UUID]string) {

	markers := s.nodeMarkers(ctx, viewer, nodes)
	social := s.nodeSocial(ctx, viewer, nodes)

	for i := range items {
		id := nodes[i].ID
		if len(byID) > 0 {
			if rel, ok := byID[id]; ok {
				items[i].Path = path.Join("/", rel)
			}
		}
		if m, ok := markers[id]; ok {
			items[i].ShareCount = m.ShareCount
			items[i].HasVersions = m.HasVersions
		}
		if v, ok := social[id]; ok {
			items[i].Starred = v.Starred
			items[i].Tags = toTagJSONs(v.Tags)
			if len(v.SHA256) == sha256.Size {
				items[i].SHA256 = hex.EncodeToString(v.SHA256)
			}
		}
	}
}
