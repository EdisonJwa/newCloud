package main

// A guard on the two places an S3 store is built.
//
// `serve` constructs one for the running server. `openBlobStore` constructs another for the blob-size
// backfill, which needs the layer below the layout adapter. They take the same s3blob.Config, and
// every field that changes what the backend DOES has to be listed in both.
//
// The omission is silent and asymmetric. This call site was already missing SSEMode and SSEKMSKeyID,
// so the backfill path wrote unencrypted objects while the serving path encrypted them — with no
// error, no log line, and no way to notice short of reading both literals side by side. Adding the
// WC-GAP-003 verification fields to serve alone would have repeated it.
//
// So this test parses the two composite literals out of main.go and requires their key sets to match.
// It is deliberately a source scan rather than a refactor into a shared constructor: the literals are
// in two different functions with different lifetimes and error handling, and collapsing them would
// make the backfill depend on serve's connection setup. Asserting they agree is the smaller change and
// catches the thing that actually goes wrong.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// s3ConfigLiteralKeys returns the field names set in every s3blob.Config composite literal in main.go,
// keyed by the line the literal starts on.
func s3ConfigLiteralKeys(t *testing.T) map[int][]string {
	t.Helper()

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	// Match `s3blob.Config{` and capture to the matching closing brace. The literals are flat —
	// no nested braces — so a non-greedy run to the first `}` is exact here, and the test fails
	// loudly below if that assumption ever stops holding.
	start := regexp.MustCompile(`s3blob\.Config\{`)
	locs := start.FindAllStringIndex(text, -1)
	if len(locs) < 2 {
		t.Fatalf("found %d s3blob.Config literals in main.go, expected at least 2 (serve and "+
			"openBlobStore). If the stores are now constructed elsewhere, this guard is checking "+
			"the wrong file and would pass vacuously.", len(locs))
	}

	keyRe := regexp.MustCompile(`(?m)^\s*([A-Z]\w*):`)
	out := map[int][]string{}
	for _, loc := range locs {
		end := strings.Index(text[loc[0]:], "}")
		if end < 0 {
			t.Fatalf("unterminated s3blob.Config literal at byte %d", loc[0])
		}
		body := text[loc[0] : loc[0]+end]
		line := strings.Count(text[:loc[0]], "\n") + 1
		var keys []string
		for _, m := range keyRe.FindAllStringSubmatch(body, -1) {
			keys = append(keys, m[1])
		}
		if len(keys) == 0 {
			t.Fatalf("the s3blob.Config literal at line %d sets no fields; the parse is broken", line)
		}
		sort.Strings(keys)
		out[line] = keys
	}
	return out
}

// TestEveryS3ConfigFieldReachesBothStores fails when one construction site learns a field the other
// did not.
func TestEveryS3ConfigFieldReachesBothStores(t *testing.T) {
	literals := s3ConfigLiteralKeys(t)
	if len(literals) < 2 {
		t.Fatalf("expected at least two s3blob.Config literals, found %d", len(literals))
	}

	// Take the largest key set as the reference: a site that sets fewer fields is the suspect one.
	lines := make([]int, 0, len(literals))
	for line := range literals {
		lines = append(lines, line)
	}
	sort.Ints(lines)

	ref := literals[lines[0]]
	for _, l := range lines[1:] {
		if len(literals[l]) > len(ref) {
			ref = literals[l]
		}
	}

	for _, line := range lines {
		got := map[string]bool{}
		for _, k := range literals[line] {
			got[k] = true
		}
		var missing []string
		for _, k := range ref {
			if !got[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("the s3blob.Config literal at main.go:%d does not set %s.\n\n"+
				"Both construction sites must configure the same behaviour, or the backfill path "+
				"silently differs from the serving path — that is how SSEMode came to be missing "+
				"here, writing plaintext from one path while the other encrypted.",
				line, strings.Join(missing, ", "))
		}
	}
}

// TestTheVerificationFieldsAreWiredIntoAtLeastOneStore: the fields exist in config, so something has
// to read them. A field that is validated, documented, and never passed on is the "implemented and
// not wired" defect this whole review is about.
func TestTheVerificationFieldsAreWiredIntoAtLeastOneStore(t *testing.T) {
	literals := s3ConfigLiteralKeys(t)

	// s3.Config has these two fields; every literal in main.go must forward them, which the test
	// above enforces across sites. Here the question is whether they reach the store at all.
	for _, want := range []string{"VerifyAfterPut", "VerifyMaxBytes"} {
		found := false
		for _, keys := range literals {
			for _, k := range keys {
				if k == want {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("no s3blob.Config literal sets %s. The configuration key would be validated, "+
				"documented and read by nothing, so an operator turning it on would see no effect "+
				"and no error.", want)
		}
	}
}
