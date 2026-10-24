package tree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

func testNode(owner, p string) layout.Node { return layout.Node{Owner: owner, Path: p} }

func mustStage(t *testing.T, s *Store, op uuid.UUID, p, body string) {
	t.Helper()
	if _, err := s.StagePut(context.Background(), op, p, bytes.NewReader([]byte(body)), int64(len(body))); err != nil {
		t.Fatalf("stage %s: %v", p, err)
	}
}

func readLive(t *testing.T, s *Store, n layout.Node) string {
	t.Helper()
	f, err := s.Open(context.Background(), n)
	if err != nil {
		t.Fatalf("open %s: %v", n.Path, err)
	}
	defer f.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(f); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestStageIsInvisibleUntilPublish(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	op := uuid.New()
	final := testNode("alice", "dir/file.txt")
	mustStage(t, s, op, "dir/file.txt", "staged bytes")

	if _, err := s.Stat(context.Background(), final); !errors.Is(err, layout.ErrNotFound) {
		t.Fatalf("live path is visible before publish: %v", err)
	}
	staged, err := s.StagePath(op, "dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("stage bytes missing: %v", err)
	}

	if err := s.PublishNoReplace(context.Background(), op, "dir/file.txt", final); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := readLive(t, s, final); got != "staged bytes" {
		t.Fatalf("published bytes = %q", got)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage path still exists after publish: %v", err)
	}
}

func TestPublishNoReplaceRefusesAnExistingLivePath(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	owner := "alice"
	if _, err := s.Put(ctx, testNode(owner, "file.txt"), bytes.NewReader([]byte("old")), 3); err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	mustStage(t, s, op, "file.txt", "new")

	if err := s.PublishNoReplace(ctx, op, "file.txt", testNode(owner, "file.txt")); err == nil {
		t.Fatal("PublishNoReplace replaced an existing live path")
	}
	if got := readLive(t, s, testNode(owner, "file.txt")); got != "old" {
		t.Fatalf("existing bytes changed to %q", got)
	}
	staged, _ := s.StagePath(op, "file.txt")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged bytes were consumed by a refused publish: %v", err)
	}
}

func TestPublishReplaceRetainsOldBytesInTheOperationStage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	owner := "alice"
	final := testNode(owner, "file.txt")
	if _, err := s.Put(ctx, final, bytes.NewReader([]byte("old")), 3); err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	mustStage(t, s, op, "file.txt", "new")

	if err := s.PublishReplace(ctx, op, "file.txt", final, testNode(owner, "backup")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := readLive(t, s, final); got != "new" {
		t.Fatalf("live bytes = %q, want new", got)
	}
	backup, err := s.StagePath(op, "backup")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(backup); err != nil || string(b) != "old" {
		t.Fatalf("old bytes not retained at %s: %q %v", backup, b, err)
	}
}

func TestDiscardRemovesOnlyTheOperationStage(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	op := uuid.New()
	mustStage(t, s, op, "a.txt", "a")
	mustStage(t, s, op, "b.txt", "b")

	if err := s.Discard(ctx, op); err != nil {
		t.Fatalf("discard: %v", err)
	}
	for _, p := range []string{"a.txt", "b.txt"} {
		staged, _ := s.StagePath(op, p)
		if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staged %s still exists: %v", p, err)
		}
	}
}

func TestStageRejectsTraversalAndEmptyOperation(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.StagePut(context.Background(), uuid.Nil, "a", bytes.NewReader(nil), 0); err == nil {
		t.Fatal("empty operation id was accepted")
	}
	for _, p := range []string{"../escape", "/absolute", "a/../../escape"} {
		if _, err := s.StagePut(context.Background(), uuid.New(), p, bytes.NewReader(nil), 0); !errors.Is(err, layout.ErrUnsafePath) {
			t.Fatalf("stage %q = %v, want ErrUnsafePath", p, err)
		}
	}
}

func TestProbeNoReplaceOnTheTargetVolume(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ProbeNoReplace(); err != nil {
		t.Fatalf("no-replace probe: %v", err)
	}
}
