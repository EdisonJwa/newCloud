package server_test

// Every metric family must be fed by production code.
//
// This exists because three of them were not. ObserveUpload, ObserveThumbnail and ObserveBlobGC
// were called only from tests: the exposition printed perfectly well-formed series that stayed at
// zero forever, the eleven unit tests in internal/metrics all passed, and the alert rules written
// against them could never fire. A metric nobody writes is indistinguishable from a metric that
// is healthy — and worse than a missing one, because it looks like coverage.
//
// The check is deliberately structural rather than behavioural: it reads the source for a call
// site outside the metrics package and outside tests. A behavioural test per family would be
// better, and several exist (see metricsroute_test.go for requests, audit gauges and the route
// itself), but the failure mode here is a family nobody wired at all, which no per-family test
// can catch because nobody wrote it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryMetricFamilyIsFedByProductionCode.
func TestEveryMetricFamilyIsFedByProductionCode(t *testing.T) {
	root := repoRoot(t)

	// The mutating methods on the registry, taken from its own source so a new one is covered
	// automatically rather than needing to be added to a list here.
	src, err := os.ReadFile(filepath.Join(root, "internal", "metrics", "metrics.go"))
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	methodRe := regexp.MustCompile(`func \(r \*Registry\) (Observe\w+|Set\w+)\(`)
	var methods []string
	for _, m := range methodRe.FindAllStringSubmatch(string(src), -1) {
		methods = append(methods, m[1])
	}
	if len(methods) == 0 {
		t.Fatal("found no registry methods; the regex is broken and this test passes vacuously")
	}

	// Collect the production call sites (everything outside the metrics package and outside
	// _test.go files).
	callers := map[string][]string{}
	for _, tree := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// The defining package is not a caller.
			if strings.Contains(filepath.ToSlash(path), "/internal/metrics/") {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			text := string(body)
			rel, _ := filepath.Rel(root, path)
			for _, m := range methods {
				if strings.Contains(text, "."+m+"(") {
					callers[m] = append(callers[m], filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}

	var unfed []string
	for _, m := range methods {
		if len(callers[m]) == 0 {
			unfed = append(unfed, m)
		}
	}
	if len(unfed) > 0 {
		t.Fatalf("%d metric method(s) are never called outside the metrics package and its "+
			"tests:\n  %s\n\n"+
			"An unwritten metric prints a valid series that stays at zero forever. Either wire it "+
			"at the place the event happens, or delete it — and if you delete it, remove the alert "+
			"rule in deploy/monitoring/alerts.yml that refers to it.",
			len(unfed), strings.Join(unfed, "\n  "))
	}
}
