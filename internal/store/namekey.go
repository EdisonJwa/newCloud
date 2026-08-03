package store

// Sibling-name identity (T-12).
//
// T-12 names "大小写/Unicode 名称冲突" as an attack: a name that normalises or case-folds to an
// existing sibling's name must not create a second, shadowing node. Two names that a user
// cannot tell apart must be one name.
//
// The rule has to live in one function, because it is enforced in three places that must agree:
// sibling uniqueness, lookup, and the (parent, name) uniqueness in each backend. When they
// disagreed, the failure was quiet: a client created "Readme.md" and then could not find
// "README.md" — or created both, and a case-insensitive client (Windows, macOS) showed one and
// silently overwrote the other.

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// nameKey is how two filenames are compared for identity.
//
// Three normalisations, each for a concrete failure it prevents:
//
//  1. Unicode NFC. "café" written as precomposed U+00E9 and as "e" + combining U+0301 are
//     different byte strings but the same filename to a user, and to every filesystem that
//     normalises. Without this, two files appear identical in a directory listing.
//  2. Case folding, which is broader than ToLower: it handles scripts whose upper and lower
//     forms are not a simple mapping (ß/ss, Turkish dotted I, Greek final sigma). Plain
//     ToLower leaves pairs that a case-insensitive client still treats as equal.
//  3. Explicitly stripping the Unicode "default ignorable" characters that render as nothing —
//     zero-width joiners, soft hyphens, bidirectional overrides. A name made entirely of RTL
//     overrides displays as nothing at all, so it can hide inside a listing and be mistaken for
//     a file the user recognises.
//
// The stored name keeps its original bytes and case: this key is for comparison only. Losing the
// user's capitalisation would be a different bug.
func nameKey(name string) string {
	// Normalise first. Folding before composing would map the combining mark separately and
	// produce a key that no other spelling of the name can reach.
	s := norm.NFC.String(name)
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r) && r == '\u00AD' {
			// Cf = format characters (ZWJ, ZWNJ, bidi overrides, soft hyphen). Dropping them
			// makes "pass\u202Efdp.txt" collide with "passfdp.txt" instead of displaying as
			// "passfdp.txt" while being a different file.
			return -1
		}
		return r
	}, s)

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(r)
	}

	return norm.NFC.String(foldCase(b.String()))
}

// foldCase applies case folding, including the expansions simple folding misses.
//
// unicode.SimpleFold cycles an equivalence class, which handles ordinary pairs like A/a. It does
// NOT handle full folding, where one character maps to several: ß folds to "ss", the Greek final
// sigma to σ, the ligature ﬁ to "fi". Those are exactly the pairs a user cannot distinguish, so
// leaving them unfolded means "straße" and "STRASSE" are two files.
func foldCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Full-fold expansions first. This table is short on purpose: it covers the characters
		// where simple folding is visibly wrong for a filename, and anything missing still folds
		// case-insensitively rather than not at all.
		if exp, ok := fullFold[r]; ok {
			b.WriteString(exp)
			continue
		}

		// Case folding maps a character to its lowercase form, not to the smallest rune in its
		// equivalence class: {A, a} folds to "a", and picking the minimum ("A") would make the key
		// uppercase — self-consistent, but wrong for matching the lowercase input a client sends.
		//
		// SimpleFold is still needed to reach the class, because ToLower alone misses the pairs
		// where the loop starts from an unusual member (e.g. the Kelvin sign K, which folds to k).
		lower := unicode.ToLower(r)
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if l := unicode.ToLower(f); l < lower {
				lower = l
			}
		}
		b.WriteRune(lower)
	}
	return b.String()
}

// fullFold lists the characters whose case folding expands to more than one rune.
//
// Derived from Unicode's CaseFolding.txt "F" (full) mappings for the characters that can appear
// in a filename and whose simple fold differs from their full fold. Keeping it a table rather
// than pulling in a full case-folding package is a size decision: the package would add a large
// generated table for characters this server will never see in a document name.
var fullFold = map[rune]string{
	'ß': "ss", 'ẞ': "ss",
	'ﬀ': "ff", 'ﬁ': "fi", 'ﬂ': "fl", 'ﬃ': "ffi", 'ﬄ': "ffl", 'ﬅ': "st", 'ﬆ': "st",
	'ς': "σ", // Greek final sigma vs medial sigma
	'ΐ': "ΐ", 'ΰ': "ΰ",
	'ﬔ': "me", 'ﬕ': "mi", 'ﬖ': "mn", 'ﬗ': "m", // Armenian ligatures
	'ŉ': "ʼn",
}

// NameKeyForTest exposes the key function to the migration-drift checker.
//
// The checker lives in a separate module path (tools/), so it cannot reach an unexported
// function. Exposing it rather than duplicating the logic is deliberate: a copy would drift,
// and drift is exactly what the checker exists to measure.
func NameKeyForTest(name string) string { return nameKey(name) }
