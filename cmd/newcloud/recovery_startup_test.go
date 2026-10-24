package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/consistency"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestRecoveryBeforeServingRefusesPersistentQuarantine(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	id := uuid.New()
	if _, err := m.Create(ctx, consistency.Operation{ID: id, OwnerID: uuid.New(), Kind: "upload"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, id, consistency.StateIntent, consistency.StateStaging); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, id, consistency.StateStaging, consistency.StatePublishing); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := recoverBeforeServing(ctx, m, nil, log); err == nil {
		t.Fatal("startup accepted a quarantined PUBLISHING operation")
	}
	// The quarantine survives a second startup even though there are no
	// non-terminal operations left to recover.
	if err := recoverBeforeServing(ctx, m, nil, log); err == nil {
		t.Fatal("second startup forgot the NEEDS_OPERATOR operation (R12-003)")
	}
}
