package layout_test

// Fuzzing the path validator.
//
// ValidateSegment and ValidatePath are the boundary between "a name a user typed" and "a name that
// reaches the filesystem". Everything downstream trusts them: the tree layout joins a validated path
// onto a storage root, so a validator that accepts "../.." or an absolute path is a traversal, and one
// that accepts two different inputs which collapse to the same segment is a name collision reported by
// users as data loss.
//
// The properties below are the ones the rest of the code assumes. They are ordinary Go tests as well
// as fuzz targets, so they run on every `go test` with an empty corpus and explore further under
// `go test -fuzz`.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// FuzzValidateSegment checks the properties every accepted segment must have.
func FuzzValidateSegment(f *testing.F) {
	for _, seed := range []string{
		"", ".", "..", "/", "\\", "a/b", "a\\b", "report.", "report ", "CON", "con.txt",
		"aux", "NUL", "name\x00", "tab\tname", "newline\nname", "ok.txt", "Ünïcode.txt",
		strings.Repeat("a", 255), strings.Repeat("a", 256), "\xff\xfe", "..hidden", ".hidden",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, seg string) {
		if err := layout.ValidateSegment(seg); err != nil {
			return // rejected: nothing below applies
		}

		// What was accepted must be safe to put on a filesystem.
		if seg == "" || seg == "." || seg == ".." {
			t.Fatalf("accepted %q, which is a traversal or an empty name", seg)
		}
		if !utf8.ValidString(seg) {
			t.Fatalf("accepted invalid UTF-8 %q", seg)
		}
		if len(seg) > 255 {
			t.Fatalf("accepted a %d-byte name", len(seg))
		}
		if strings.ContainsAny(seg, "/\\") {
			t.Fatalf("accepted %q, which contains a separator: it would escape its parent", seg)
		}
		if strings.ContainsRune(seg, 0) {
			t.Fatalf("accepted %q, which contains a NUL", seg)
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("accepted %q, which contains a control character", seg)
			}
		}
		if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			t.Fatalf("accepted %q: Windows strips the suffix, so it would collide with %q",
				seg, strings.TrimRight(seg, ". "))
		}

		// And it must round-trip through a path unchanged.
		p, err := layout.Join(seg)
		if err != nil {
			t.Fatalf("a segment ValidateSegment accepted was rejected by Join(%q): %v", seg, err)
		}
		segs, err := layout.ValidatePath(p)
		if err != nil {
			t.Fatalf("a path Join produced from an accepted segment was rejected: %v", err)
		}
		if len(segs) != 1 || segs[0] != seg {
			t.Fatalf("round trip through Join/ValidatePath changed %q into %q", seg, segs)
		}
	})
}

// FuzzValidatePath checks the properties of an accepted path.
func FuzzValidatePath(f *testing.F) {
	for _, seed := range []string{
		"", "/", "a", "a/b", "../a", "a/../b", "./a", "a//b", "a/", "/a", "a/b/c",
		"a/./b", "CON/a", "a/report.", strings.Repeat("a/", 200) + "z", "a\\b",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, p string) {
		segs, err := layout.ValidatePath(p)
		if err != nil {
			return
		}

		// An accepted path is relative and cannot climb out of its root.
		if strings.HasPrefix(p, "/") {
			t.Fatalf("accepted the absolute path %q", p)
		}
		if strings.Contains(p, "\\") {
			t.Fatalf("accepted %q, which contains a backslash", p)
		}
		if p != "" && strings.Contains(p, "//") {
			t.Fatalf("accepted %q, which has an empty segment", p)
		}

		// Every segment it yields is itself acceptable, and joining them reproduces the path.
		// The second half is what keeps two spellings from naming one file.
		for _, seg := range segs {
			if err := layout.ValidateSegment(seg); err != nil {
				t.Fatalf("path %q was accepted but segment %q is not: %v", p, seg, err)
			}
		}
		rejoined, jerr := layout.Join(segs...)
		if jerr != nil {
			t.Fatalf("path %q was accepted but its own segments do not join: %v", p, jerr)
		}
		if rejoined != p {
			t.Fatalf("path %q rejoins as %q: two spellings would name one file", p, rejoined)
		}
	})
}
