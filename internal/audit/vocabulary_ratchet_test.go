package audit_test

// A ratchet on the audit vocabulary.
//
// `internal/audit` declares 40 actions. Twelve of them are never emitted by any non-test code — no
// constant reference, no string literal. That is a specific kind of dishonesty in an audit system:
// the vocabulary tells an operator or an exporter that `auth.session.revoke` is a thing that
// happens, and filtering on it returns nothing, forever, with no indication whether the absence means
// "no revocations" or "this event was never wired".
//
// Twelve is too many to fix in one change and too easy to leave uncounted. So this pins the CURRENT
// dead set: it fails when the set GROWS (a new declaration nobody emits) and it fails when a listed
// name becomes USED (which means the list is stale and must shrink). The second half is what makes
// this a ratchet rather than a snapshot — otherwise the entries rot in place and the test becomes a
// formality.
//
// Removing a name is how the debt is paid: emit the event, then delete the line here.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// knownUnemitted lists actions declared in internal/audit with no emitter outside tests.
//
// Two kinds are in here and they are not equally bad:
//
//	Missing auditing for something that happens — auth.session.revoke, admin.setting.change.
//	    These are requirement gaps (§10.1 asks for 会话撤销 and 管理配置变更), not naming choices.
//
//	Names reserved for paths that do not exist yet — security.anomaly, admin.privileged_read.
//	    Declaring them early is defensible; leaving them uncounted is not.
var knownUnemitted = []string{
	"admin.privileged_read",
	"admin.setting.change",
	"file.download_zip",
	"security.anomaly",
}

// declaredActions parses the Action constants out of internal/audit/audit.go.
func declaredActions(t *testing.T, root string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "internal", "audit", "audit.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*(Action\w+)\s+Action\s*=\s*"([^"]+)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[m[2]] = m[1]
	}
	if len(out) == 0 {
		t.Fatal("parsed no Action constants; the declaration shape changed and this guard is " +
			"silently checking nothing")
	}
	return out
}

// repoRoot walks up to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}

// emittedActions finds every action VALUE that some non-test source refers to, either by the
// constant name or by the literal string.
func emittedActions(t *testing.T, root string) map[string]bool {
	t.Helper()
	declared := declaredActions(t, root)
	used := map[string]bool{}

	auditFile := filepath.Join(root, "internal", "audit", "audit.go")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip hidden and vendored trees wholesale.
			//
			// The original version listed ".git" and "node_modules" by name and was defeated by a
			// stray ".rename-tmp" directory holding 404 duplicated files, one of which was a stale
			// copy of internal/audit/audit.go. The scan then "found" emitters for five actions
			// because their strings appeared in a duplicate that is not part of the build. A
			// source-scanning guard must not depend on no stray directory existing: any directory
			// whose name starts with a dot is not module source, and Go ignores it too.
			if name := info.Name(); name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if path == auditFile {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Comments are stripped before matching. A mention of an action in prose is not an
		// emitter, and counting one makes this ratchet report coverage where there is none —
		// which is exactly how share.download looked wired while the only emitter was a method
		// nothing called. The scan still cannot see reachability; it can at least insist that the
		// reference is code.
		text := stripComments(string(body))
		for value, name := range declared {
			if strings.Contains(text, `"`+value+`"`) || strings.Contains(text, name) {
				used[value] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return used
}

// TestEveryDeclaredAuditActionIsEmittedOrCounted keeps the dead set from growing.
func TestEveryDeclaredAuditActionIsEmittedOrCounted(t *testing.T) {
	root := repoRoot(t)
	declared := declaredActions(t, root)
	used := emittedActions(t, root)

	known := map[string]bool{}
	for _, v := range knownUnemitted {
		known[v] = true
		if _, ok := declared[v]; !ok {
			t.Fatalf("%q is listed as unemitted but is no longer declared in internal/audit; "+
				"the list has gone stale", v)
		}
	}

	// Grown: a declaration nothing emits, and nobody recorded it.
	var grown []string
	for value := range declared {
		if !used[value] && !known[value] {
			grown = append(grown, value)
		}
	}
	sort.Strings(grown)
	if len(grown) > 0 {
		t.Fatalf("these audit actions are declared but never emitted:\n  %s\n\n"+
			"An audit vocabulary is a promise that these events can be filtered for. Emit the "+
			"event, or declare it deliberately by adding it to knownUnemitted — but do not let "+
			"the set grow unnoticed.", strings.Join(grown, "\n  "))
	}

	// Shrunk: a listed action now has an emitter, so the entry is stale. This is the half that
	// makes the guard a ratchet; without it the list silently becomes a lie of its own.
	var stale []string
	for _, v := range knownUnemitted {
		if used[v] {
			stale = append(stale, v)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("these actions now have an emitter but are still listed as unemitted:\n  %s\n\n"+
			"Delete them from knownUnemitted so the count stays true.", strings.Join(stale, "\n  "))
	}
}

// stripComments removes line and block comments so a prose mention is not mistaken for an emitter.
//
// Deliberately lexical rather than go/ast: this guard reads source it did not compile, and a parse
// error in an unrelated file must not make the check fail for the wrong reason.
func stripComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	for i := 0; i < len(src); i++ {
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(src[i])
	}
	return out.String()
}
