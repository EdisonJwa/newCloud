package server

import (
	"strings"
	"testing"
)

// TestSanitizeSegmentAcceptsNormalNames covers the names real users create.
func TestSanitizeSegmentAcceptsNormalNames(t *testing.T) {
	ok := []string{
		"report.pdf",
		"2026-10-21 会议纪要.docx",
		"a",
		"file with spaces.txt",
		"UPPER.TXT",
		"emoji-📄.png",
		".hidden",
		"v1.2.3.tar.gz",
		"console.log", // starts with a reserved prefix but is not the device name
	}
	for _, name := range ok {
		if _, err := SanitizeSegment(name); err != nil {
			t.Fatalf("%q should be accepted: %v", name, err)
		}
	}
}

// TestSanitizeSegmentRejectsTraversal is the regression guard for the entire
// path-traversal class: separators are rejected, not normalised away.
func TestSanitizeSegmentRejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"a/b",
		"a\\b",
		"../etc/passwd",
		"..\\windows\\system32",
		"/etc/shadow",
		"C:evil",
		"file\x00.txt",
		"line\nbreak",
		"tab\there",
		strings.Repeat("a", 256), // over the 255-byte limit
	}
	for _, name := range bad {
		if _, err := SanitizeSegment(name); err == nil {
			t.Fatalf("%q must be rejected", name)
		}
	}
}

// TestSanitizeSegmentRejectsWindowsReserved: a file named CON.txt syncs to a
// Windows client and then cannot be opened, which users report as data loss.
func TestSanitizeSegmentRejectsWindowsReserved(t *testing.T) {
	bad := []string{"CON", "con", "CON.txt", "PRN", "AUX", "NUL", "COM1", "LPT9", "con.tar.gz"}
	for _, name := range bad {
		if _, err := SanitizeSegment(name); err == nil {
			t.Fatalf("reserved device name %q must be rejected", name)
		}
	}
}

// TestSanitizeSegmentRejectsTrailingDotOrSpace: Windows silently strips these,
// so "report." and "report" would collide after a round trip.
func TestSanitizeSegmentRejectsTrailingDotOrSpace(t *testing.T) {
	for _, name := range []string{"report.", "report ", "report. "} {
		if _, err := SanitizeSegment(name); err == nil {
			t.Fatalf("%q must be rejected", name)
		}
	}
}

func TestSplitPath(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"/", nil, false},
		{"", nil, false},
		{"/a/b/c.txt", []string{"a", "b", "c.txt"}, false},
		{"a/b", []string{"a", "b"}, false},
		{"/a/../b", []string{"b"}, false},        // path.Clean resolves before validation
		{"/a/../../etc", []string{"etc"}, false}, // clean clamps at root
		{"..", nil, false},                       // clean turns this into "/"
		{"/a/\x00/b", nil, true},
	}
	for _, c := range cases {
		got, err := SplitPath(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%q: want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%q: want %v, got %v", c.in, c.want, got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%q: want %v, got %v", c.in, c.want, got)
			}
		}
	}
}
