package webassets

// The embedded bundle must be servable, self-describing, and cacheable.

import (
	"strings"
	"testing"
)

func TestBundleIsEmbedded(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if len(h.index) == 0 {
		t.Fatal("index.html is empty")
	}
	if !strings.Contains(string(h.index), "/assets/") {
		t.Error("the shell must reference the hashed bundle, or nothing loads")
	}
	if len(h.assets) == 0 {
		t.Fatal("no assets were embedded")
	}

	var js, css bool
	for name := range h.assets {
		if strings.HasSuffix(name, ".js") {
			js = true
		}
		if strings.HasSuffix(name, ".css") {
			css = true
		}
	}
	if !js {
		t.Error("the bundle has no JavaScript")
	}
	if !css {
		t.Error("the bundle has no stylesheet")
	}
}

// TestEveryAssetHasAValidator: without one, an upgraded hashed bundle could still be served
// stale by an intermediary that ignores the file name.
func TestEveryAssetHasAValidator(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}
	for name, hash := range h.hashes {
		if hash == `""` || hash == "" {
			t.Errorf("%s has an empty ETag", name)
		}
		if prev, dup := seen[hash]; dup {
			t.Errorf("%s and %s share the ETag %s", prev, name, hash)
		}
		seen[hash] = name
	}
	if len(h.hashes) != len(h.assets) {
		t.Errorf("%d assets but %d hashes", len(h.assets), len(h.hashes))
	}
}

// TestContentTypesAreSelfOnly: the handler serves a closed set of build outputs, and must
// never return a type the browser would render as a document.
func TestContentTypesAreSelfOnly(t *testing.T) {
	cases := map[string]string{
		"assets/index-abc.js":  "text/javascript; charset=utf-8",
		"assets/index-abc.css": "text/css; charset=utf-8",
		"assets/logo.svg":      "image/svg+xml",
		"assets/font.woff2":    "font/woff2",
	}
	for name, want := range cases {
		if got := contentTypeOf(name); got != want {
			t.Errorf("contentTypeOf(%q) = %q, want %q", name, got, want)
		}
	}

	// Anything else is opaque rather than guessable.
	for _, name := range []string{"assets/x.html", "assets/x.htm", "assets/x.txt"} {
		if got := contentTypeOf(name); got != "application/octet-stream" {
			t.Errorf("contentTypeOf(%q) = %q; unknown types must not be renderable", name, got)
		}
	}
}

func TestEtagMatches(t *testing.T) {
	cases := []struct {
		header string
		etag   string
		want   bool
	}{
		{`"abc"`, `"abc"`, true},
		{`"abc"`, `"def"`, false},
		{`"def", "abc"`, `"abc"`, true},
		{`*`, `"abc"`, true},
		{``, `"abc"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, tc.etag); got != tc.want {
			t.Errorf("etagMatches(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}
