package tree

import (
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/storage/layouttest"
)

// TestConformance runs the shared suite against the tree layout.
func TestConformance(t *testing.T) {
	layouttest.Run(t, func(t *testing.T) layout.ContentStore {
		s, err := New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
