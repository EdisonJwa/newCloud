package server_test

// Name search.
//
// The command palette is the only place a user types a filename, so this endpoint either makes
// that typing useful or makes it a lie. The tests pin the parts that decide: the scope is the
// whole tree, the match is on the name, wildcards are text, and a one-character query returns
// nothing rather than the whole library.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

type searchHit struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func search(t *testing.T, h *harness, query string) []searchHit {
	t.Helper()
	resp := h.do(http.MethodGet, "/api/files/search?q="+url.QueryEscape(query), nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search %q: %d %s", query, resp.StatusCode, readBody(resp))
	}
	var payload struct {
		Items []searchHit `json:"items"`
	}
	body := readBody(resp)
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return payload.Items
}

// TestSearchFindsFilesAnywhere covers the case a per-folder filter cannot: the file is not in
// the folder you are looking at.
func TestSearchFindsFilesAnywhere(t *testing.T) {
	h := newHarness(t)
	mkdirPath(t, h, "reports")
	mkdirPath(t, h, "reports/2026")
	uploadPath(t, h, "reports/2026/quarterly-budget.txt", "numbers")
	uploadPath(t, h, "other.txt", "unrelated")

	hits := search(t, h, "budget")
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Path != "/reports/2026/quarterly-budget.txt" {
		t.Fatalf("path must locate the hit, got %q", hits[0].Path)
	}
}

// TestSearchIsCaseInsensitive: people do not type exact case.
func TestSearchIsCaseInsensitive(t *testing.T) {
	h := newHarness(t)
	uploadPath(t, h, "Quarterly-Report.txt", "x")

	if got := search(t, h, "quarterly"); len(got) != 1 {
		t.Fatalf("lowercase query must match, got %d", len(got))
	}
	if got := search(t, h, "QUARTERLY"); len(got) != 1 {
		t.Fatalf("uppercase query must match, got %d", len(got))
	}
}

// TestSearchTreatsWildcardsAsText: the query is data, not a pattern. Both cases below are two
// characters long, so they reach the matcher instead of being rejected as too short — which is
// what makes them worth testing.
func TestSearchTreatsWildcardsAsText(t *testing.T) {
	h := newHarness(t)
	uploadPath(t, h, "plain.txt", "x")
	// "media" ends in "a", so a leaked LIKE pattern would match it. Without this file the
	// "%a" case below would pass whether or not the escaping was right.
	uploadPath(t, h, "media", "x")

	// Under LIKE semantics "__" matches every two-character name; under strpos it matches none.
	if got := search(t, h, "__"); len(got) != 0 {
		t.Fatalf(`"__" must be literal, got %d hits`, len(got))
	}
	// Under LIKE semantics "%a" matches anything ending in "a"; here there is no "a" file.
	if got := search(t, h, "%a"); len(got) != 0 {
		t.Fatalf(`"%%a" must be literal, got %d hits`, len(got))
	}
	// Sanity: a literal substring still matches.
	if got := search(t, h, "la"); len(got) != 1 {
		t.Fatalf("literal substring must still match, got %d", len(got))
	}
}

// TestSearchRequiresTwoCharacters: one character matches most of the library.
func TestSearchRequiresTwoCharacters(t *testing.T) {
	h := newHarness(t)
	uploadPath(t, h, "alpha.txt", "x")

	if got := search(t, h, "a"); len(got) != 0 {
		t.Fatalf("a single character must not search, got %d hits", len(got))
	}
}

// TestSearchExcludesTrash: a deleted file is not a search result.
func TestSearchExcludesTrash(t *testing.T) {
	h := newHarness(t)
	uploadPath(t, h, "disposable.txt", "x")

	resp := h.do(http.MethodDelete, "/api/files/?path=disposable.txt", nil, nil)
	resp.Body.Close()

	if got := search(t, h, "disposable"); len(got) != 0 {
		t.Fatalf("trashed files must not be searchable, got %d", len(got))
	}
}

// TestSearchFindsDirectories: a folder is a legitimate destination, and the palette should be
// able to jump to one.
func TestSearchFindsDirectories(t *testing.T) {
	h := newHarness(t)
	mkdirPath(t, h, "design-archive")

	hits := search(t, h, "design")
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].Path != "/design-archive" {
		t.Fatalf("path = %q", hits[0].Path)
	}
}
