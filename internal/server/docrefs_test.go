package server_test

// Documentation integrity: every test name a document cites must exist.
//
// This exists because I wrote `TestOIDCNonceMismatchIsRefused` and
// `TestCallbackRejectsForgedState` into a policy document from memory. Neither test exists; the
// real ones are `TestMismatchedNonceIsRejected` and `TestForgedStateRejected`. A document naming a
// test that does not exist is worse than one naming none: it manufactures confidence, and the
// reader has no reason to check.
//
// The check is deliberately simple — an exact `func <Name>(` match in any _test.go under cmd/ or
// internal/ — because that is exactly the claim the documents make. It is not a coverage check; a
// test that exists but asserts nothing still passes here, and that is a different problem.
//
// Names may legitimately appear in prose that is *about* a mistake ("the first version was called
// X and did not exist"). Those are excluded by requiring the name to be introduced as a current
// reference: the marker `docIgnore:` on the same line. A blanket exclusion would defeat the point.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docTestRef finds backtick-quoted test names.
var docTestRef = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")

// docIgnoreMarker exempts a line from the check, for prose that discusses a name that no longer
// exists.
const docIgnoreMarker = "docIgnore:"

// TestDocumentsCiteOnlyRealTests.
func TestDocumentsCiteOnlyRealTests(t *testing.T) {
	root := repoRoot(t)

	// Collect every test function that exists.
	defined := map[string]bool{}
	for _, dir := range []string{"cmd", "internal"} {
		walkTests(t, filepath.Join(root, dir), defined)
	}
	if len(defined) == 0 {
		t.Fatal("found no test functions; the walk is broken and this check would pass vacuously")
	}

	// Collect every test name the documents cite.
	cited := map[string][]string{} // name -> files that cite it
	docs := docFiles(t, root)
	if len(docs) == 0 {
		t.Fatal("found no documents; the check would pass vacuously")
	}
	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, docIgnoreMarker) {
				continue
			}
			for _, m := range docTestRef.FindAllStringSubmatch(line, -1) {
				cited[m[1]] = append(cited[m[1]], filepath.Base(path))
			}
		}
	}

	var missing []string
	for name, where := range cited {
		if !defined[name] {
			missing = append(missing, name+" (cited in "+strings.Join(uniq(where), ", ")+")")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("documentation cites %d test(s) that do not exist:\n  %s\n\n"+
			"Either rename the reference to the test that exists, or — if the sentence is "+
			"discussing a name that is deliberately gone — mark the line with `%s`.",
			len(missing), strings.Join(missing, "\n  "), docIgnoreMarker)
	}
}

// repoRoot walks up until it finds go.mod.
func repoRoot(t *testing.T) string {
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
	t.Fatal("could not find the repository root (no go.mod above the test's directory)")
	return ""
}

// walkTests records every `func TestXxx(` in the tree under dir.
func walkTests(t *testing.T, dir string, into map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not this test's problem
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range regexp.MustCompile(`func (Test[A-Za-z0-9_]+)\(`).
			FindAllStringSubmatch(string(raw), -1) {
			into[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// docFiles returns the documents whose citations are checked.
//
// Only documentation is checked. The requirements document is excluded: it is the input, not our
// output, and it may reasonably name tests we have not written yet.
func docFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, rel := range []string{"docs", "deploy"} {
		_ = filepath.WalkDir(filepath.Join(root, rel), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			if strings.Contains(filepath.Base(path), "0010-functional-requirements") {
				return nil
			}
			out = append(out, path)
			return nil
		})
	}
	out = append(out, filepath.Join(root, "README.md"))
	return out
}

// uniq removes duplicates, preserving order.
func uniq(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
