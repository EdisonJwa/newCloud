package fsrename

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRenameNoReplaceRefusesAnExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RenameNoReplace(src, dst); !errors.Is(err, ErrExist) {
		t.Fatalf("RenameNoReplace onto an existing target = %v, want ErrExist", err)
	}
	if got, _ := os.ReadFile(src); string(got) != "source" {
		t.Fatalf("the source was moved despite the refusal: %q", got)
	}
	if got, _ := os.ReadFile(dst); string(got) != "destination" {
		t.Fatalf("the destination was modified: %q", got)
	}
}

func TestRenameNoReplacePublishesAFreeTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(src, dst); err != nil {
		t.Fatalf("free-target rename: %v", err)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still exists after publish: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "payload" {
		t.Fatalf("published bytes = %q", got)
	}
}

func TestRenameNoReplaceWorksForDirectories(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(filepath.Join(src, "child"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "child", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(src, dst); !errors.Is(err, ErrExist) {
		t.Fatalf("directory onto existing = %v, want ErrExist", err)
	}
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(src, dst); err != nil {
		t.Fatalf("directory free-target rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "child", "f")); err != nil {
		t.Fatalf("directory content did not move: %v", err)
	}
}

func TestConcurrentPublishersHaveOneWinner(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "winner")
	const n = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins int
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			src := filepath.Join(dir, fmt.Sprintf("src-%d", i))
			if err := os.WriteFile(src, []byte(fmt.Sprintf("payload-%d", i)), 0o600); err != nil {
				errs[i] = err
				return
			}
			if err := RenameNoReplace(src, dst); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrExist) {
				errs[i] = err
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("publisher %d: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d publishers won, want exactly 1", wins)
	}
}

func TestProbeUsesTheTargetDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := Probe(dir); err != nil {
		t.Fatalf("probe on the local filesystem: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left %d entries behind", len(entries))
	}
}
