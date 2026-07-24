package blobstore

import (
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/layouttest"
)

// TestConformance runs the shared suite against the blob layout, proving the
// WebDAV and API layers can stay layout-agnostic.
func TestConformance(t *testing.T) {
	layouttest.Run(t, func(t *testing.T) layout.ContentStore {
		bs, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s, err := New(bs, newResolver())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
