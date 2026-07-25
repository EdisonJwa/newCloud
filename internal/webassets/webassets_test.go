package webassets

import "testing"

// The embedded client must be servable, and must carry a validator.
//
// Without one, a browser that already has app.js keeps using it for the whole max-age
// window after an upgrade — the deployed interface looks unchanged and the operator
// concludes the release did not happen.

func TestAssetsAreEmbedded(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, name := range []string{"app.js", "style.css", "index.html"} {
		if _, ok := h.hashes[name]; !ok {
			t.Errorf("%s has no hash; it would be cached without a validator", name)
		}
	}

	// The index must reference the assets it needs, or the page loads and does nothing.
	if len(h.index) == 0 {
		t.Fatal("index.html is empty")
	}
	if !containsAll(string(h.index), "/assets/app.js", "/assets/style.css") {
		t.Error("index.html does not reference the client assets")
	}
}

func TestHashesAreDistinct(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}
	for name, hash := range h.hashes {
		if hash == `""` || hash == "" {
			t.Errorf("%s has an empty ETag", name)
		}
		if prev, ok := seen[hash]; ok {
			t.Errorf("%s and %s share the ETag %s; a content hash must differ", prev, name, hash)
		}
		seen[hash] = name
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
		{`W/"abc"`, `"abc"`, false}, // a weak validator is not this strong one
		{`*`, `"abc"`, true},
		{``, `"abc"`, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, tc.etag); got != tc.want {
			t.Errorf("etagMatches(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(haystack); i++ {
			if haystack[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
