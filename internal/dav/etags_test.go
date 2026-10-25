package dav

// M1: the change-token contract at the PROPFIND surface.
//
// The store conformance suite (internal/storetest/etags.go) proves every mutation stamps the
// ancestor chain. What only this layer can prove is the half a sync client actually sees: the
// directory's getetag — which this server used to synthesize from a mtime nothing maintained,
// so it never changed and every walking client re-listed every subtree on every pass.

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var getetagRe = regexp.MustCompile(`<D:getetag>(.*?)</D:getetag>`)

// rootETag returns the user root's etag as PROPFIND Depth:0 reports it, with the XML
// entity escaping undone so the assertion below sees the token's real grammar.
func rootETag(t *testing.T, e *testEnv) string {
	t.Helper()
	rec := e.do(t, "PROPFIND", e.davPath(""), strings.NewReader(propfindAllProp),
		map[string]string{"Depth": "0"})
	body := rec.Body.String()
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND root: %d\n%s", rec.Code, body)
	}
	m := getetagRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the root reported no getetag:\n%s", body)
	}
	return strings.NewReplacer("&#34;", `"`, "&quot;", `"`, "&amp;", "&").Replace(m[1])
}

func TestDirectoryETagChangesWhenADeepChildChanges(t *testing.T) {
	e := newEnv(t)
	for _, dir := range []string{"sub", "sub/inner"} {
		rec := e.do(t, "MKCOL", e.davPath(dir), nil, nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("MKCOL %s: %d", dir, rec.Code)
		}
	}

	before := rootETag(t, e)
	if before == "" {
		t.Fatal("a freshly created tree reports an empty root etag")
	}

	// One write, two levels below the root. A walking client that listed the root before
	// must be able to learn "this subtree changed" from the root's etag alone — that is the
	// entire point of the change token.
	e.do(t, http.MethodPut, e.davPath("sub/inner/deep.txt"), strings.NewReader("changed"), nil)

	after := rootETag(t, e)
	if after == before {
		t.Fatalf("a write two levels deep did not change the root's etag (%q): every "+
			"walking sync client will re-list the whole tree", before)
	}

	// And the write must leave the root's etag a QUOTED opaque token, same grammar as a
	// file's — a client parsing one grammar from this server must not meet a second one.
	if !strings.HasPrefix(after, `"`) || !strings.HasSuffix(after, `"`) {
		t.Fatalf("the root's change token is not a quoted etag: %q", after)
	}
}
