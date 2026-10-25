package server_test

// The content-search engine, exercised through its real interface (core-scope §7.2).
//
// What these check is the part of §7.2 that is a CLAIM rather than a feature:
//
//	the index never authorises ??a stale or wrong index returns nothing it should not
//	a write never waits on the index ??publishing succeeds with an engine that always fails
//	the degraded response is honest ??it says which mode answered and why
//
// A fake engine rather than Elasticsearch itself, because the interesting contract is between the
// server and the ENGINE INTERFACE, not between the server and one product. The HTTP client's own
// behaviour is covered by internal/search's tests against an httptest server.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	// Aliased because this package's test files already have a `search` helper (the name search
	// fixture), and a package name and a function name cannot share a scope.
	enginesearch "github.com/EdisonJwa/newCloud/internal/search"
	"github.com/EdisonJwa/newCloud/internal/server"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// fakeEngine is a scriptable enginesearch.Engine.
//
// It records what it was asked and answers from a fixed list, which is what lets a test assert BOTH
// halves of the posture: what the server sent (the scope) and what it did with what came back (the
// re-check).
type fakeEngine struct {
	mu sync.Mutex
	// docs is what the engine will report as hits, in order.
	docs []uuid.UUID
	// scopes records every Scope the server computed.
	scopes []enginesearch.Scope
	// queries records every Query so a test can assert the filter reached the engine.
	queries []enginesearch.Query
	// indexed records the documents written.
	indexed []enginesearch.Doc
	// deleted records the node ids removed.
	deleted []uuid.UUID
	// failIndex makes every write fail, so a test can prove a publish does not depend on it.
	failIndex bool
	// failSearch makes every query fail, so a test can prove the degraded path.
	failSearch bool
}

func (f *fakeEngine) Available() bool { return !f.failSearch }

func (f *fakeEngine) Search(_ context.Context, scope enginesearch.Scope, q enginesearch.Query) ([]enginesearch.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, scope)
	f.queries = append(f.queries, q)
	if f.failSearch {
		return nil, enginesearch.ErrUnavailable
	}
	out := make([]enginesearch.Hit, 0, len(f.docs))
	for _, id := range f.docs {
		out = append(out, enginesearch.Hit{NodeID: id})
	}
	return out, nil
}

func (f *fakeEngine) Index(_ context.Context, doc enginesearch.Doc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIndex {
		return enginesearch.ErrUnavailable
	}
	f.indexed = append(f.indexed, doc)
	return nil
}

func (f *fakeEngine) Delete(_ context.Context, nodeID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, nodeID)
	return nil
}

// indexingHarness builds a harness whose server has the given engine wired.
func indexingHarness(t *testing.T, engine enginesearch.Engine) *harness {
	t.Helper()
	return newHarnessTuned(t, nil, func(d *server.Deps) { d.Search = engine })
}

// TestTheIndexNeverAuthorises is §7.2's central security property, stated as a test.
//
// The engine is told to return a node the caller may not read. The response must not contain it,
// must not mention it, and must not reveal that it exists.
func TestTheIndexNeverAuthorises(t *testing.T) {
	engine := &fakeEngine{}
	h := indexingHarness(t, engine)

	// The victim's file, which the caller has no grant on.
	victim, _ := h.intruder(t, "index-victim")
	victimFile := h.foreignFile(t, victim.ID, "secret-plan.txt", "classified")

	// The caller's own file, so the response has something legitimate in it.
	h.uploadFile(t, "mine.txt", "x")

	// The engine claims BOTH are hits ??which is exactly what a stale or poisoned index would do.
	engine.docs = []uuid.UUID{victimFile, h.resolveNode(t, "mine.txt").ID}

	resp := h.do(http.MethodGet, "/api/files/search?q=plan&mode=content", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content search: %d %s", resp.StatusCode, body)
	}

	// The victim's name and id must not appear. A 200 that lists it would be the enumeration oracle
	// §7.2 forbids; a 200 that mentions the id in an error field would be the same leak.
	for _, secret := range []string{"secret-plan.txt", victimFile.String()} {
		if contains(body, secret) {
			t.Fatalf("the search response leaked %q from a hit the caller cannot read: %s",
				secret, body)
		}
	}
	// And the caller's own file IS there, so the drop above was a decision rather than a failure.
	if !contains(body, "mine.txt") {
		t.Fatalf("the caller's own hit was dropped too: %s", body)
	}
}

// TestTheSearchScopeIsTheCallersOwnSubjects checks step 1 of the three-step flow: the engine is
// asked about the caller's subjects and nobody else's.
func TestTheSearchScopeIsTheCallersOwnSubjects(t *testing.T) {
	engine := &fakeEngine{}
	h := indexingHarness(t, engine)
	h.uploadFile(t, "scoped.txt", "x")

	resp := h.do(http.MethodGet, "/api/files/search?q=scoped&mode=content", nil, nil)
	resp.Body.Close()

	if len(engine.scopes) == 0 {
		t.Fatal("the engine was never asked")
	}
	scope := engine.scopes[0]
	if len(scope.Owners) != 1 || scope.Owners[0] != h.user.ID {
		t.Fatalf("the scope owners are %v, want just the caller %s: an unscoped query is the "+
			"enumeration oracle §7.2 rules out", scope.Owners, h.user.ID)
	}
}

// TestAWriteNeverWaitsOnTheIndex proves §7.2's "no write fails because the index is down".
//
// The engine rejects every write, and the upload must still succeed. That is the whole posture: the
// index is a cache, and losing it loses convenience rather than data.
func TestAWriteNeverWaitsOnTheIndex(t *testing.T) {
	engine := &fakeEngine{failIndex: true}
	h := indexingHarness(t, engine)

	// The upload succeeded even though the engine refused everything.
	h.uploadFile(t, "unindexed.txt", "the bytes are still here")

	get := h.do(http.MethodGet, "/api/files/download?path=unindexed.txt", nil, nil)
	body := readBody(get)
	get.Body.Close()
	if body != "the bytes are still here" {
		t.Fatalf("the file does not serve its bytes after a failed index write: %q", body)
	}
}

// TestAContentSearchFailureDegradesRatherThanFailing: an unavailable engine must not turn a search
// into an error, and must say so.
func TestAContentSearchFailureDegradesRatherThanFailing(t *testing.T) {
	engine := &fakeEngine{failSearch: true}
	h := indexingHarness(t, engine)
	h.uploadFile(t, "fallback.txt", "x")

	resp := h.do(http.MethodGet, "/api/files/search?q=fallback&mode=content", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a failed engine answered %d (%s); §7.2 says it degrades, not fails",
			resp.StatusCode, body)
	}
	if !contains(body, `"degraded":true`) {
		t.Fatalf("the degradation is not reported: %s", body)
	}
	// mode=content has no name half, so the honest answer is "nothing, and here is why".
	if !contains(body, `"mode":"name"`) {
		t.Fatalf("the response does not say which mode answered: %s", body)
	}
}

// TestModeAllFallsBackToNameResults: with mode=all the name half still works when the engine does
// not, so the user gets the results the server CAN produce.
func TestModeAllFallsBackToNameResults(t *testing.T) {
	engine := &fakeEngine{failSearch: true}
	h := indexingHarness(t, engine)
	h.uploadFile(t, "stillfindable.txt", "x")

	resp := h.do(http.MethodGet, "/api/files/search?q=stillfindable&mode=all", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mode=all with a failed engine: %d %s", resp.StatusCode, body)
	}
	if !contains(body, "stillfindable.txt") {
		t.Fatalf("mode=all lost its name results when the engine failed: %s", body)
	}
	if !contains(body, `"degraded":true`) {
		t.Fatalf("mode=all did not report the degradation: %s", body)
	}
}

// TestTheFilterReachesTheEngine checks that the §7.1 filters compose with content mode, which §7.2
// requires: "tag and favorite filters compose with content mode".
func TestTheFilterReachesTheEngine(t *testing.T) {
	engine := &fakeEngine{}
	h := indexingHarness(t, engine)
	h.mkdir(t, "filters")
	h.uploadAt(t, "filters/inside.txt", "x")

	resp := h.do(http.MethodGet,
		"/api/files/search?q=phrase&mode=content&kind=file&mime=text/&size_min=2&sort=name", nil, nil)
	resp.Body.Close()

	if len(engine.queries) == 0 {
		t.Fatal("the engine was never asked")
	}
	q := engine.queries[0]
	if q.Filter.Kind != "file" {
		t.Fatalf("kind did not reach the engine: %+v", q.Filter)
	}
	if q.Filter.MimePrefix != "text/" {
		t.Fatalf("mime did not reach the engine: %+v", q.Filter)
	}
	if q.Filter.SizeMin != 2 {
		t.Fatalf("size_min did not reach the engine: %+v", q.Filter)
	}
	if q.Text != "phrase" {
		t.Fatalf("the phrase did not reach the engine: %q", q.Text)
	}
}

// TestNoEngineIsTheDefaultShape: with no engine wired at all, a content request degrades exactly as
// it does when the engine is down. A deployment without the feature and one whose feature is
// broken must not be distinguishable to a client.
func TestNoEngineIsTheDefaultShape(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "plain.txt", "x")

	resp := h.do(http.MethodGet, "/api/files/search?q=plain&mode=content", nil, nil)
	body := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content search with no engine: %d %s", resp.StatusCode, body)
	}
	if !contains(body, `"mode":"name"`) || !contains(body, `"degraded":true`) {
		t.Fatalf("an engine-less deployment did not report the degradation: %s", body)
	}
}

// foreignFile creates a node in another account's tree, bypassing the HTTP surface.
//
// The API can only write into the caller's own namespace ??that is the point ??so a test that needs
// a node the caller has no grant on has to make one at the store level, exactly as a second
// account's real uploads would.
//
// The content address is registered but no bytes are stored: these cases are about what the SEARCH
// path does with a hit, and the fake engine never reads content. Writing real bytes would only make
// the fixture slower.
func (h *harness) foreignFile(t *testing.T, owner uuid.UUID, name, content string) uuid.UUID {
	t.Helper()
	root, err := h.meta.EnsureRoot(context.Background(), owner)
	if err != nil {
		t.Fatalf("ensure the victim's root: %v", err)
	}

	sum := sha256.Sum256([]byte(content))
	var made uuid.UUID
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		n, cerr := tx.CreateFile(ctx, store.NewFile{
			OwnerID: owner, ParentID: root.ID, Name: name,
			Size: int64(len(content)), Mime: "text/plain", ETag: `"victim"`,
			BlobID: h.meta.BlobIDFor(ctx, sum, int64(len(content))), BlobHash: sum,
		})
		if cerr != nil {
			return cerr
		}
		made = n.ID
		return nil
	}); err != nil {
		t.Fatalf("create the victim's node: %v", err)
	}
	return made
}

// contains is a tiny helper so the assertions read as English.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
