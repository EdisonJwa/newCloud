package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/lease"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestRuntimeProtectionRefusesASecondTreeProcess(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Layout = config.LayoutTree
	cfg.Storage.Local.Root = t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	meta := store.NewMemory()
	ctx := context.Background()
	stop := func() {}

	_, release, err := acquireRuntimeProtection(ctx, cfg, meta, stop, log)
	if err != nil {
		t.Fatalf("first process: %v", err)
	}
	_, second, err := acquireRuntimeProtection(ctx, cfg, meta, stop, log)
	if !errors.Is(err, lease.ErrTreeLocked) {
		if second != nil {
			second()
		}
		t.Fatalf("second process = %v, want ErrTreeLocked", err)
	}
	release()
	_, third, err := acquireRuntimeProtection(ctx, cfg, meta, stop, log)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	third()
}

func TestRuntimeProtectionPGLeaseExcludesASecondProcess(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL lease wiring test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Storage.Layout = config.LayoutBlob
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pg := store.NewPG(pool)
	stop := func() {}
	_, release, err := acquireRuntimeProtection(ctx, cfg, pg, stop, log)
	if err != nil {
		t.Fatalf("first process: %v", err)
	}
	_, second, err := acquireRuntimeProtection(ctx, cfg, pg, stop, log)
	if !errors.Is(err, lease.ErrHeld) {
		if second != nil {
			second()
		}
		t.Fatalf("second process = %v, want ErrHeld", err)
	}
	release()
	_, third, err := acquireRuntimeProtection(ctx, cfg, pg, stop, log)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	third()
}
