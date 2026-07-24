package store

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// splitPath validates a slash-separated relative path into its segments.
//
// Validation is delegated to layout.ValidateSegment so the metadata layer and
// both storage layouts agree on exactly what a legal name is. Two independent
// validators would eventually disagree, and the difference would be a file that
// can be created through one path but not read through another.
func splitPath(p string) ([]string, error) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil, nil
	}
	segs, err := layout.ValidatePath(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	return segs, nil
}

// joinPath appends a child name to a directory path.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// parseOwner converts the storage layer's owner string back to an ID.
//
// The storage interface is string-keyed because it must also serve anonymous
// uploads (file requests), which have no user row. The metadata layer keys on
// UUIDs, so the conversion happens here, explicitly, at the boundary.
func parseOwner(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: bad owner id %q", ErrInvalidName, s)
	}
	return id, nil
}

// validateName rejects names that cannot be represented on disk or portably.
func validateName(name string) error {
	if err := layout.ValidateSegment(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	return nil
}
