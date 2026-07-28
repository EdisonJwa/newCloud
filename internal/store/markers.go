package store

// Per-node provenance markers.
//
// The interface wants to answer, for every row in a listing, two questions that make the
// audit capability visible without opening anything:
//
//	is this node shared, and by how many active links?
//	does it have earlier versions?
//
// Doing that through SharesForNode and Versions would be two queries per row. A directory of
// 500 files would issue a thousand round trips to render one screen, which is the classic
// N+1 that makes a listing feel broken on a remote database.
//
// MarkersFor exists so the two questions are answered in two queries for the whole page,
// regardless of row count. It is a separate optional interface rather than part of Tx: a
// backend that cannot answer it cheaply should return nothing and lose a decoration, not
// fail the listing.

import (
	"context"

	"github.com/google/uuid"
)

// Marker is the provenance summary for one node.
type Marker struct {
	// ShareCount is the number of active (unrevoked) shares pointing at the node.
	ShareCount int
	// HasVersions reports whether at least one earlier version is stored.
	HasVersions bool
}

// MarkersFor returns markers for the given nodes, owned by ownerID.
//
// Nodes with no markers are omitted from the result rather than returned zeroed: the caller
// treats a missing key and a zero marker the same, and omitting keeps the map small on a
// directory where almost nothing is shared.
type MarkersFor interface {
	MarkersFor(ctx context.Context, ownerID uuid.UUID, nodeIDs []uuid.UUID) (map[uuid.UUID]Marker, error)
}
