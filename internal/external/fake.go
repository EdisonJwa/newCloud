package external

// The in-memory Storage used by the route and handler tests.
//
// A fake for the mount surface has to be a real filesystem-shaped object rather than a list
// of stubbed answers, because the property under test is usually a sequence — write then
// list then rename then delete — and a stub that cannot hold state proves the caller asked
// for something, not that the caller did anything.

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Storage. Zero value is an empty filesystem with a root directory.
type Fake struct {
	mu sync.Mutex
	// entries maps clean path -> entry. The root is "" and always present.
	entries map[string]Entry
	// bodies holds file contents, parallel to entries with IsDir false.
	bodies map[string][]byte

	// Err, when set, is returned by every operation. Tests set it to prove the
	// 502-not-empty-folder property: an unavailable remote must surface as an error,
	// because an empty directory and a dead server are different facts.
	Err error
}

// NewFake returns an empty fake filesystem.
func NewFake() *Fake {
	return &Fake{
		entries: map[string]Entry{"": {Name: "", IsDir: true}},
		bodies:  map[string][]byte{},
	}
}

// AddFile seeds a file (creating nothing else). Test-fixture helper.
func (f *Fake) AddFile(path string, body []byte, mod time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[path] = Entry{Name: base(path), IsDir: false, Size: int64(len(body)), ModTime: mod}
	f.bodies[path] = body
}

// AddDir seeds a directory, and any missing ancestors the remote would have had.
func (f *Fake) AddDir(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for p := path; ; p = parent(p) {
		if _, ok := f.entries[p]; ok {
			break
		}
		f.entries[p] = Entry{Name: base(p), IsDir: true}
		if p == "" {
			break
		}
	}
}

func (f *Fake) List(ctx context.Context, dir string) ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	e, ok := f.entries[dir]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.IsDir {
		return nil, ErrNotADirectory
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	var out []Entry
	for p, entry := range f.entries {
		if p == dir || !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if strings.Contains(rest, "/") {
			continue // a grandchild: belongs to a child's listing
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Fake) Stat(ctx context.Context, path string) (Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return Entry{}, f.Err
	}
	e, ok := f.entries[path]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

type fakeReader struct {
	body []byte
	read int
}

func (r *fakeReader) Read(p []byte) (int, error) {
	if r.read >= len(r.body) {
		return 0, io.EOF
	}
	n := copy(p, r.body[r.read:])
	r.read += n
	return n, nil
}

func (r *fakeReader) Close() error { return nil }

func (f *Fake) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	e, ok := f.entries[path]
	if !ok {
		return nil, ErrNotFound
	}
	if e.IsDir {
		return nil, ErrNotADirectory
	}
	return &fakeReader{body: f.bodies[path]}, nil
}

func (f *Fake) Create(ctx context.Context, path string, r io.Reader) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return 0, ErrUnavailable
	}
	if e, ok := f.entries[path]; ok && e.IsDir {
		return 0, ErrNotADirectory
	}
	now := time.Now()
	f.entries[path] = Entry{Name: base(path), IsDir: false, Size: int64(len(body)), ModTime: now}
	f.bodies[path] = body
	return int64(len(body)), nil
}

func (f *Fake) Mkdir(ctx context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if _, ok := f.entries[path]; ok {
		return ErrExists
	}
	f.entries[path] = Entry{Name: base(path), IsDir: true, ModTime: time.Now()}
	return nil
}

func (f *Fake) Remove(ctx context.Context, path string, recursive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	e, ok := f.entries[path]
	if !ok {
		return ErrNotFound
	}
	if e.IsDir && !recursive {
		prefix := path + "/"
		for p := range f.entries {
			if strings.HasPrefix(p, prefix) {
				return ErrExists // the protocol's "directory not empty"
			}
		}
	}
	prefix := path + "/"
	for p := range f.entries {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(f.entries, p)
			delete(f.bodies, p)
		}
	}
	return nil
}

func (f *Fake) Rename(ctx context.Context, oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	e, ok := f.entries[oldPath]
	if !ok {
		return ErrNotFound
	}
	if _, exists := f.entries[newPath]; exists {
		return ErrExists
	}
	f.entries[newPath] = e
	delete(f.entries, oldPath)
	if !e.IsDir {
		f.bodies[newPath] = f.bodies[oldPath]
		delete(f.bodies, oldPath)
	}
	return nil
}

func (f *Fake) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Err
}

func base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func parent(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}
