package store

import (
	"context"

	"github.com/google/uuid"
)

// DeadProp is a WebDAV dead property: a name/value pair a client attached to a
// node.
//
// These are load-bearing. Windows Explorer stores Win32LastModifiedTime and
// macOS Finder stores AppleDouble metadata here; if they do not survive a
// reconnect, files show the wrong timestamps or appear to have changed on every
// mount.
type DeadProp struct {
	NodeID uuid.UUID
	Space  string
	Name   string
	Value  string
}

// DeadPropTx is the dead-property surface.
type DeadPropTx interface {
	// SetDeadProp upserts one property.
	SetDeadProp(ctx context.Context, nodeID uuid.UUID, space, name, value string) error
	// DeadProps returns every property of a node, ordered by namespace and name.
	DeadProps(ctx context.Context, nodeID uuid.UUID) ([]DeadProp, error)
	// DeadPropsFor returns the properties of several nodes in one query.
	//
	// A PROPFIND listing asks for the same non-DAV properties on every entry, and the
	// Nextcloud clients request about twenty of them. Answering each entry with its own
	// transaction made a 500-entry listing 500 round trips before the response started;
	// this is one. Nodes with no properties are absent from the map, so a caller must use
	// a two-value lookup rather than assuming an empty slice.
	DeadPropsFor(ctx context.Context, nodeIDs []uuid.UUID) (map[uuid.UUID][]DeadProp, error)
	// DeleteDeadProp removes one property.
	DeleteDeadProp(ctx context.Context, nodeID uuid.UUID, space, name string) error
}
