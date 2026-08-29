package store_test

// A ratchet on shared-suite coverage.
//
// Three times in three rounds a method was implemented, tested against the in-memory backend, and
// shipped with the PostgreSQL version never executed under test: the integrity scrub's sample
// query, the retention stamp, and the upload rate limiter. The rate limiter's two backends then
// disagreed on a prune boundary, and the retention stamp was silently dropped by the memory
// backend's CreateFile. Each was found late, by luck of writing the right server test.
//
// A blanket "every Tx method must appear in the shared suite" would fail today — 47 of 93 methods
// are exercised only by backend-specific tests or not at all — and a guard that fails on arrival
// gets deleted. So this pins the CURRENT uncovered set and fails only when that set GROWS. The
// effect is a ratchet: existing debt is allowed, new debt is not. Adding a method to Tx without
// covering it in storetest now fails here, which is the moment somebody still remembers whether
// the two backends can diverge.
//
// Removing a name from the list is how the debt is paid down. The test does not force that; it just
// refuses to let it grow while nobody is looking.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// knownUncovered lists Tx methods with no reference in internal/storetest.
//
// Each entry is a method whose behaviour is currently verified by at most one backend. Paying this
// down means writing a case in the shared suite and deleting the name here.
var knownUncovered = []string{
	// Empty. Every Tx method is now exercised by the shared conformance suite, which runs each
	// backend against the same cases — the only thing that catches a PostgreSQL statement and an
	// in-memory implementation disagreeing.
	//
	// The list reached zero by being paid down: 47 methods when the ratchet was written, several
	// of which turned out to be broken on one backend (a restore stub, a prune boundary that
	// emptied a table the other backend kept, a double-revoke that succeeded silently, and a
	// collector whose comment claimed to report a gap it did not). Keeping the list here, empty,
	// is what makes the guard fire on the NEXT uncovered method rather than only on a growing one.
}

// TestSharedSuiteCoversEveryNewTxMethod is the ratchet.
func TestSharedSuiteCoversEveryNewTxMethod(t *testing.T) {
	root := repoRootOf(t)
	iface, err := os.ReadFile(filepath.Join(root, "internal", "store", "store.go"))
	if err != nil {
		t.Fatal(err)
	}
	suite, err := os.ReadFile(filepath.Join(root, "internal", "storetest", "storetest.go"))
	if err != nil {
		t.Fatal(err)
	}

	// Only the Tx interface: Store embeds it and a few wrappers, and the question here is what a
	// backend must implement.
	text := string(iface)
	start := strings.Index(text, "type Tx interface")
	if start < 0 {
		t.Fatal("could not find the Tx interface; this check would pass vacuously")
	}
	end := strings.Index(text[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of the Tx interface")
	}
	decl := text[start : start+end]

	methods := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\t([A-Z]\w+)\(`).FindAllStringSubmatch(decl, -1) {
		methods[m[1]] = true
	}
	if len(methods) < 50 {
		t.Fatalf("found only %d Tx methods, which is far fewer than expected — the parse is "+
			"probably broken and this check is not testing anything", len(methods))
	}

	allowed := map[string]bool{}
	for _, name := range knownUncovered {
		allowed[name] = true
	}

	body := string(suite)
	var grew []string
	for name := range methods {
		if allowed[name] {
			continue
		}
		if !strings.Contains(body, name+"(") {
			grew = append(grew, name)
		}
	}
	sort.Strings(grew)

	if len(grew) > 0 {
		t.Fatalf("Tx method(s) with no exercise in the shared conformance suite:\n  %s\n\n"+
			"Add a case to internal/storetest — that suite runs every backend against the same "+
			"cases, which is the only thing that catches a PostgreSQL statement and an in-memory "+
			"implementation disagreeing. Three rounds in a row shipped a method tested on one "+
			"backend only; two of them were broken on the other.\n\n"+
			"If the method genuinely cannot be exercised there yet, add it to knownUncovered in "+
			"this file — deliberately, with a reason — rather than letting the set grow silently.",
			strings.Join(grew, "\n  "))
	}

	// Report debt that has been paid but not recorded, so the ratchet stays honest rather than
	// merely permissive.
	var stale []string
	for name := range allowed {
		if !methods[name] {
			stale = append(stale, name+" (no longer on Tx)")
			continue
		}
		if strings.Contains(body, name+"(") {
			stale = append(stale, name+" (now covered)")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("knownUncovered is out of date:\n  %s\n\nRemove these names so the ratchet "+
			"records the improvement and can catch a regression later.", strings.Join(stale, "\n  "))
	}
}

// repoRootOf walks up to the module root.
func repoRootOf(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the module root")
	return ""
}

// TestCaseFoldingColumnsAreCompatibleWithTheGoComparison guards a pairing that is invisible.
//
// The in-memory backend resolves accounts with strings.EqualFold — case-insensitive. PostgreSQL
// resolves them with `WHERE email = $1`, which is case-insensitive ONLY because the column is
// citext. Change that column to text and the two backends silently disagree: an ACL grant written
// as "Erin@Example.Test" would resolve in tests and fail in production, granting access to nobody.
//
// The shared suite catches the disagreement, but only if somebody runs it against a real database.
// This check reads the migration that defines the columns, so a schema change is caught by a test
// that needs no database at all — and the message names the Go function it has to stay compatible
// with.
func TestCaseFoldingColumnsAreCompatibleWithTheGoComparison(t *testing.T) {
	root := repoRootOf(t)

	// The Go side: which columns are compared case-insensitively in memory.
	memorySrc, err := os.ReadFile(filepath.Join(root, "internal", "store", "acl_memory.go"))
	if err != nil {
		t.Fatal(err)
	}
	goText := string(memorySrc)
	if !strings.Contains(goText, "strings.EqualFold(u.Email") ||
		!strings.Contains(goText, "strings.EqualFold(u.Username") {
		// The comparison changed. If it became exact, the columns should become text; either way
		// this test's premise is gone and it must not silently pass.
		t.Fatal("the in-memory backend no longer compares email/username with strings.EqualFold; " +
			"update this guard to match the new comparison rather than leaving it vacuous")
	}

	// The SQL side: those columns must be citext for the comparison to agree.
	dir := filepath.Join(root, "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var definitions []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		definitions = append(definitions, string(body))
	}
	if len(definitions) == 0 {
		t.Fatal("found no migrations; this check would pass vacuously")
	}
	all := strings.Join(definitions, "\n")

	for _, col := range []string{"email", "username"} {
		// Match the column declaration anywhere in the CREATE TABLE, allowing other columns and
		// whitespace between.
		re := regexp.MustCompile(`\b` + col + `\s+citext`)
		if !re.MatchString(strings.ToLower(all)) {
			t.Fatalf("users.%s is not declared citext in any migration. The in-memory backend "+
				"compares it with strings.EqualFold, so PostgreSQL's `WHERE %s = $1` must also be "+
				"case-insensitive — with a text column the two backends disagree, and an ACL grant "+
				"written with different capitalisation resolves in tests and fails in production.",
				col, col)
		}
	}
}
