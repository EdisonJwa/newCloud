package main

// A guard on how the serving process tears down resumable uploads (NC-RR-001).
//
// The defect the re-review found was not in the upload manager — its recovery logic was correct and
// unit-tested. It was one line in this package:
//
//	defer uploads.Close()
//
// `Close` aborts every live session and deletes its staging bytes AND its record. So an ordinary
// restart, deploy or upgrade destroyed every transfer in flight, while a SIGKILL recovered, because
// a crash leaves the directory untouched. The durability work was real and the documentation's
// "resumable across restarts" claim was still false for the lifecycle operators actually use.
//
// Nothing in internal/upload can catch that: both methods work exactly as documented, and the
// difference is which one `main` calls. internal/upload's own tests cannot see this package, and its
// end-to-end tests reach the manager through a harness rather than through this wiring. So the guard
// has to live here, and it is a source scan for the same reason s3wiring_test.go is: the fact under
// test is which identifier appears in the shutdown path.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// shutdownCallsUploadsShutdown requires the shutdown defer to call Shutdown, not Close.
func TestShutdownPreservesUploadsRatherThanAbortingThem(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Comments are stripped first. The comment that documents this very decision necessarily
	// mentions `defer uploads.Close()`, and a scan that matched it would forbid explaining the fix
	// — a test that makes its own rationale unwritable is the wrong test.
	text := stripGoComments(string(src))

	// The destructive call must not appear as a shutdown hook. It is allowed anywhere else — a test
	// helper, an operator command — but registering it with defer is what destroyed in-flight
	// transfers.
	closeDefer := regexp.MustCompile(`defer\s+uploads\.Close\(\)`)
	if closeDefer.MatchString(text) {
		t.Fatal("main.go registers `defer uploads.Close()`: a graceful stop would abort every " +
			"unfinished upload and delete its staging bytes, so a deploy destroys transfers that " +
			"a crash would have recovered (NC-RR-001). Use uploads.Shutdown(), which flushes and " +
			"releases the handles while preserving the recoverable state.")
	}

	// And the preserving call must be present, or the uploads would be left with open handles on a
	// Windows host and the next process could not reopen them.
	shutdownCall := regexp.MustCompile(`uploads\.Shutdown\(\)`)
	if !shutdownCall.MatchString(text) {
		t.Fatal("main.go never calls uploads.Shutdown(): unfinished uploads would keep their " +
			"staging file handles open across the restart, and on Windows the next process cannot " +
			"reopen a file this one still holds (NC-RR-001).")
	}

	// The error must be handled rather than discarded: a session that could not be flushed is
	// exactly what an operator needs to hear about after an upgrade.
	if !strings.Contains(text, `if err := uploads.Shutdown(); err != nil {`) {
		t.Fatal("the result of uploads.Shutdown() is not checked: a session that failed to flush " +
			"would vanish from the log, and an operator would have no way to know which transfers " +
			"did not resume")
	}
}

// stripGoComments removes //-comments and /* */ blocks, preserving line numbering well enough for a
// pattern scan. String literals are few enough in this file that a false positive inside one would
// be caught immediately by the test failing on its own message.
func stripGoComments(s string) string {
	var out strings.Builder
	out.Grow(len(s))

	lines := strings.Split(s, "\n")
	inBlock := false
	for _, line := range lines {
		if inBlock {
			if idx := strings.Index(line, "*/"); idx >= 0 {
				line = line[idx+2:]
				inBlock = false
			} else {
				out.WriteString("\n")
				continue
			}
		}
		// A block comment starting on this line: keep the code before it and drop the rest until
		// the closing marker.
		if start := strings.Index(line, "/*"); start >= 0 {
			rest := line[start:]
			if end := strings.Index(rest, "*/"); end >= 0 {
				line = line[:start] + rest[end+2:]
			} else {
				line = line[:start]
				inBlock = true
			}
		}
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// TestTheUploadManagerStillHasAPreservingShutdown keeps the two methods from being collapsed.
//
// A later cleanup that "simplified" Shutdown into Close, or removed it as redundant, would restore
// the defect silently. The source scan above would then fail too, but this test names the reason.
func TestTheUploadManagerStillHasAPreservingShutdown(t *testing.T) {
	src, err := os.ReadFile("../../internal/upload/upload.go")
	if err != nil {
		t.Fatalf("read upload.go: %v", err)
	}
	text := string(src)

	if !regexp.MustCompile(`func \(m \*Manager\) Shutdown\(\) error`).MatchString(text) {
		t.Fatal("internal/upload no longer has `func (m *Manager) Shutdown() error`: without it " +
			"the only way to stop the manager deletes in-flight work")
	}
	if !regexp.MustCompile(`func \(m \*Manager\) Close\(\)`).MatchString(text) {
		t.Fatal("internal/upload no longer has Close(): tests and an operator purge still need a " +
			"deliberate abort-everything path, and folding it into Shutdown would make shutdown " +
			"destructive again")
	}
}
