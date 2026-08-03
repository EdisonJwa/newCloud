package store_test

import (
	"context"
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
		m := store.NewMemory()
		// The owner must exist as an account, not just as an id. The PostgreSQL
		// factory inserts a users row for the same reason: identity operations
		// (role changes, sessions, credentials) key on it, and a factory that only
		// made an id would let those operations fail in tests while working in
		// production — or the reverse.
		owner := m.AddUser(store.User{
			Username: "owner", Email: "owner@example.test", Role: "user",
		})
		return m, owner.ID
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
		BlobIDFor: func(t *testing.T, s store.Store, hash [32]byte) uuid.UUID {
			return s.BlobIDFor(context.Background(), hash, 0)
		},
		SetQuota: func(t *testing.T, s store.Store, owner uuid.UUID, limit int64) {
			m, ok := s.(*store.Memory)
			if !ok {
				t.Fatalf("unexpected store type %T", s)
			}
			m.SetQuota(owner, limit)
		},
	})
}
