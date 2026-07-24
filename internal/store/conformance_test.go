package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/storetest"
)

// TestConformance runs the shared metadata suite against the in-memory store.
//
// It lives in the external test package (store_test) so it can import the suite
// without an import cycle.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (store.Store, uuid.UUID) {
		return store.NewMemory(), uuid.New()
	}, storetest.Options{
		Versions: true,
		AuditCount: func(t *testing.T, s store.Store) int {
			m, ok := s.(*store.Memory)
			if !ok {
				t.Fatalf("unexpected store type %T", s)
			}
			return len(m.AuditLog())
		},
		NewBlob: func(t *testing.T) uuid.UUID { return uuid.New() },
		SetQuota: func(t *testing.T, s store.Store, owner uuid.UUID, limit int64) {
			m, ok := s.(*store.Memory)
			if !ok {
				t.Fatalf("unexpected store type %T", s)
			}
			m.SetQuota(owner, limit)
		},
	})
}
