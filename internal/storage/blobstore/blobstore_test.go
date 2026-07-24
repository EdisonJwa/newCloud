package blobstore

import (
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/blob"
	"github.com/EdisonJwa/newCloud/internal/blob/local"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// memResolver is an in-memory stand-in for the metadata layer. The real one
// queries PostgreSQL; the contract is the same.
type memResolver struct {
	mu    sync.RWMutex
	files map[string]blob.Hash // "owner/path" -> hash
	dirs  map[string]bool      // "owner/path" -> is dir
}

func newResolver() *memResolver {
	return &memResolver{
		files: map[string]blob.Hash{},
		dirs:  map[string]bool{},
	}
}

func key(owner, p string) string { return owner + "\x00" + p }

func (r *memResolver) HashFor(_ context.Context, owner, p string) (blob.Hash, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.files[key(owner, p)]
	return h, ok, nil
}

func (r *memResolver) Record(_ context.Context, owner, p string, h blob.Hash, isDir bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if isDir {
		r.dirs[key(owner, p)] = true
		return nil
	}
	r.files[key(owner, p)] = h
	return nil
}

func (r *memResolver) Forget(_ context.Context, owner, p string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, key(owner, p))
	delete(r.dirs, key(owner, p))
	return nil
}

func (r *memResolver) IsDir(_ context.Context, owner, p string) (bool, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.dirs[key(owner, p)] {
		return true, true, nil
	}
	if _, ok := r.files[key(owner, p)]; ok {
		return false, true, nil
	}
	return false, false, nil
}

func (r *memResolver) Children(_ context.Context, owner, dir string, limit int) ([]layout.Node, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]layout.Node{}

	add := func(p string, isDir bool, size int64) {
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" {
			return
		}
		first, _, nested := strings.Cut(rest, "/")
		if first == "" {
			return
		}
		childPath := first
		if dir != "" {
			childPath = path.Join(dir, first)
		}
		n := layout.Node{Owner: owner, Path: childPath, IsDir: nested, Size: size}
		if nested {
			n.Size = 0
		}
		seen[childPath] = n
	}

	for k, h := range r.files {
		o, p, ok := strings.Cut(k, "\x00")
		if !ok || o != owner || !strings.HasPrefix(p, prefix) {
			continue
		}
		add(p, false, int64(len(h)))
	}
	for k := range r.dirs {
		o, p, ok := strings.Cut(k, "\x00")
		if !ok || o != owner || !strings.HasPrefix(p, prefix) || p == dir {
			continue
		}
		add(p, true, 0)
	}

	out := make([]layout.Node, 0, len(seen))
	for _, n := range seen {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func newStore(t *testing.T) (*Store, *memResolver) {
	t.Helper()
	bs, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := newResolver()
	s, err := New(bs, r)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func node(owner, p string, isDir bool) layout.Node {
	return layout.Node{Owner: owner, Path: p, IsDir: isDir}
}

func put(t *testing.T, s *Store, n layout.Node, content string) layout.PutResult {
	t.Helper()
	res, err := s.Put(context.Background(), n, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("put %s/%s: %v", n.Owner, n.Path, err)
	}
	return res
}

func read(t *testing.T, s *Store, n layout.Node) string {
	t.Helper()
	rc, err := s.Open(context.Background(), n)
	if err != nil {
		t.Fatalf("open %s: %v", n.Path, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPutOpenRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, node("alice", "docs/a.txt", false), "hello blob")
	if got := read(t, s, node("alice", "docs/a.txt", false)); got != "hello blob" {
		t.Fatalf("content %q", got)
	}
}

// TestDeduplicationAcrossUsers is the defining property of this layout: two users
// storing identical content consume disk once, while each is billed for their
// own quota at the metadata layer.
func TestDeduplicationAcrossUsers(t *testing.T) {
	s, r := newStore(t)
	content := strings.Repeat("shared content ", 1000)

	r1 := put(t, s, node("alice", "a.txt", false), content)
	r2 := put(t, s, node("bob", "b.txt", false), content)

	if r1.Hash != r2.Hash {
		t.Fatal("identical content must hash identically")
	}
	if r2.Deduplicated != true {
		t.Fatal("the second write of identical content must report deduplication")
	}

	// Both users read the same bytes under their own path.
	if got := read(t, s, node("alice", "a.txt", false)); got != content {
		t.Fatal("alice content mismatch")
	}
	if got := read(t, s, node("bob", "b.txt", false)); got != content {
		t.Fatal("bob content mismatch")
	}

	// Only one entry in the resolver's file map per hash would be ideal, but the
	// path->hash mapping is per-user by design; the dedup happens at the blob
	// layer, which stores one object.
	if len(r.files) != 2 {
		t.Fatalf("expected two metadata entries, got %d", len(r.files))
	}
}

// TestOverwriteWithSameContentDoesNotGrow is the versioning-friendly case: saving
// an unchanged file must not create a second copy.
func TestOverwriteWithSameContentDoesNotGrow(t *testing.T) {
	s, _ := newStore(t)
	first := put(t, s, node("alice", "f.txt", false), "stable content")
	second := put(t, s, node("alice", "f.txt", false), "stable content")

	if first.Hash != second.Hash {
		t.Fatal("content address must be stable")
	}
	if !second.ReplacedExisting {
		t.Fatal("overwrite must be reported")
	}
	if !second.Deduplicated {
		t.Fatal("re-saving identical content must not write new bytes")
	}
}

func TestOverwriteWithDifferentContent(t *testing.T) {
	s, _ := newStore(t)
	first := put(t, s, node("alice", "f.txt", false), "v1")
	second := put(t, s, node("alice", "f.txt", false), "v2 is longer")

	if first.Hash == second.Hash {
		t.Fatal("different content must have different addresses")
	}
	if second.Deduplicated {
		t.Fatal("new content is not deduplicated")
	}
	if got := read(t, s, node("alice", "f.txt", false)); got != "v2 is longer" {
		t.Fatalf("content after overwrite: %q", got)
	}
}

func TestOpenIsSeekable(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, node("alice", "big.txt", false), strings.Repeat("0123456789", 1000))

	rc, err := s.Open(context.Background(), node("alice", "big.txt", false))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if _, err := rc.Seek(5000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "0123" {
		t.Fatalf("seek landed wrong: %q", buf)
	}
}

func TestMkdirAndList(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Mkdir(context.Background(), node("alice", "docs", true)); err != nil {
		t.Fatal(err)
	}
	put(t, s, node("alice", "docs/a.txt", false), "a")
	put(t, s, node("alice", "docs/b.txt", false), "b")
	if err := s.Mkdir(context.Background(), node("alice", "docs/sub", true)); err != nil {
		t.Fatal(err)
	}
	put(t, s, node("alice", "docs/sub/deep.txt", false), "deep")

	children, err := s.List(context.Background(), node("alice", "docs", true), 0)
	if err != nil {
		t.Fatal(err)
	}
	// a.txt, b.txt, sub — deep.txt is one level down and must not appear.
	if len(children) != 3 {
		t.Fatalf("want 3 direct children, got %v", children)
	}
	for _, c := range children {
		if strings.HasSuffix(c.Path, "deep.txt") {
			t.Fatal("List must not recurse into subdirectories")
		}
	}
}

func TestListLimit(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Mkdir(context.Background(), node("alice", "d", true)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		put(t, s, node("alice", "d/"+name+".txt", false), name)
	}

	children, err := s.List(context.Background(), node("alice", "d", true), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("limit ignored: got %d", len(children))
	}
}

func TestMoveIsMetadataOnly(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, node("alice", "a.txt", false), "content")

	if err := s.Move(context.Background(), node("alice", "a.txt", false), node("alice", "dir/b.txt", false)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, node("alice", "dir/b.txt", false)); got != "content" {
		t.Fatal("content must survive the move")
	}
}

// TestDeleteRemovesMetadataNotBytes: the bytes are shared and refcounted by the
// database, so the storage adapter must not delete them.
func TestDeleteRemovesMetadataNotBytes(t *testing.T) {
	s, r := newStore(t)
	put(t, s, node("alice", "a.txt", false), "shared")
	put(t, s, node("bob", "b.txt", false), "shared")

	if err := s.Delete(context.Background(), node("alice", "a.txt", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(context.Background(), node("alice", "a.txt", false)); !errors.Is(err, layout.ErrNotFound) {
		t.Fatal("alice's entry should be gone")
	}
	// Bob is unaffected.
	if got := read(t, s, node("bob", "b.txt", false)); got != "shared" {
		t.Fatal("bob's file must be unaffected by alice's delete")
	}
	_ = r
}

// TestWalkUnsupportedIsExplicit: returning an empty result would make a caller
// believe the subtree is empty and silently skip work (like a quota
// recalculation or a virus scan pass).
func TestWalkUnsupportedIsExplicit(t *testing.T) {
	s, _ := newStore(t)
	err := s.Walk(context.Background(), node("alice", "", true), func(layout.Node) error { return nil })
	if !errors.Is(err, layout.ErrWalkUnsupported) {
		t.Fatalf("want ErrWalkUnsupported, got %v", err)
	}
}

// TestCorruptMetadataIsLoud: metadata referencing missing bytes must not be
// reported as a plain 404, because that hides data loss.
func TestCorruptMetadataIsLoud(t *testing.T) {
	dir := t.TempDir()
	bs, err := local.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newResolver()
	s, err := New(bs, r)
	if err != nil {
		t.Fatal(err)
	}

	put(t, s, node("alice", "gone.txt", false), "will vanish")
	h, _, _ := r.HashFor(context.Background(), "alice", "gone.txt")
	if err := bs.Delete(context.Background(), h); err != nil {
		t.Fatal(err)
	}

	_, err = s.Stat(context.Background(), node("alice", "gone.txt", false))
	if err == nil {
		t.Fatal("missing content must be an error")
	}
	if errors.Is(err, layout.ErrNotFound) {
		// A wrapped ErrNotFound is acceptable only with the corruption detail;
		// the message must name the missing content address.
		if !strings.Contains(err.Error(), "missing content") {
			t.Fatalf("corruption should be distinguishable from absence: %v", err)
		}
	}
}

func TestUnsafePathsRejected(t *testing.T) {
	s, _ := newStore(t)
	bad := []string{"../etc/passwd", "/abs", "a\x00b", "a\\b"}
	for _, p := range bad {
		if _, err := s.Put(context.Background(), node("alice", p, false), strings.NewReader("x"), 1); err == nil {
			t.Fatalf("path %q must be rejected", p)
		}
	}
}

func TestEmptyOwnerRejected(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(context.Background(), node("", "a.txt", false), strings.NewReader("x"), 1); err == nil {
		t.Fatal("empty owner must be rejected")
	}
}

func TestPutDirectoryRejected(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(context.Background(), node("alice", "d", true), strings.NewReader("x"), 1); err == nil {
		t.Fatal("Put on a directory must be rejected")
	}
}

func TestCapabilitiesReflectLayout(t *testing.T) {
	s, _ := newStore(t)
	caps := s.Capabilities()
	if !caps.Deduplicated {
		t.Fatal("blob layout deduplicates")
	}
	if caps.DirectFSInspect {
		t.Fatal("blob layout is not human-inspectable on disk")
	}
	if !caps.EfficientRename {
		t.Fatal("blob layout rename is a metadata update")
	}
}

func TestSizeMismatchRejected(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Put(context.Background(), node("alice", "f.txt", false), strings.NewReader("abc"), 999)
	if err == nil {
		t.Fatal("size mismatch must fail")
	}
}

func TestManyConcurrentWrites(t *testing.T) {
	s, _ := newStore(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := strings.Repeat("x", 100+i)
			p := "f" + string(rune('a'+i)) + ".txt"
			_, err := s.Put(context.Background(), node("alice", p, false), strings.NewReader(content), int64(len(content)))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
