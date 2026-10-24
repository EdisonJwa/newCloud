package deadcode_test

// A ratchet on unexported functions nothing calls.
//
// This started as a probe for the shape that appeared twice in this work: a function that looked
// like production wiring and had no caller. Service.RecordDownload wrote the share.download audit
// event and was called by nothing, so the event was never written while the vocabulary and the tests
// both said it was. The same probe found destinationHostMatches: written for exactly one purpose,
// documented as "checked by the caller", and no caller checked it — so a WebDAV MOVE naming another
// server was applied locally and the documented defence did not exist. That one is now wired in and
// tested; it disappeared from this list as a result.
//
// The scope is deliberately narrow: unexported FREE functions. Methods are mostly interface
// implementations or test affordances in production files, and flagging them produced seventy
// findings that were all noise — a ratchet nobody can read is a ratchet nobody maintains.
//
// An unexported free function cannot be reached from another package, so "no reference in non-test
// code" means dead, not "used by a test helper elsewhere".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// knownUncalled pins the unexported functions with no caller, each with the reason it is still here.
var knownUncalled = map[string]string{
	"checkInteractions": "tools/demoshot: a developer script, not part of the server",
	"constantTimeEqual": "internal/auth: a convenience wrapper around subtle.ConstantTimeCompare " +
		"that nothing uses, because both call sites compare directly. Dead, harmless, and cheap to " +
		"delete whenever that file is next touched",
	"sniffMediaType": "internal/server: used only by tests in its own package. It should live in a " +
		"_test.go file rather than being compiled into the server",
	"spoolDirFor": "internal/storage/blobstore: the spool path is computed inline; this helper is " +
		"left over from an earlier shape",
	"encodeAttrs": "internal/external: the SFTP v3 attribute encoder. The client never sends " +
		"attrs (its writes carry only pflags/perms), but the loopback test server answers with " +
		"them, and both sides sharing one codec is what keeps the wire format honest",
}

func TestNoNewUncalledFunctions(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	declared := map[string]bool{}
	used := map[string]int{}

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			switch name {
			case "node_modules", "web", "design-studio":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			// A file the build would reject is not this check's business, and failing here
			// would make a syntax error look like a dead-code finding.
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Recv == nil && x.Name.Name != "main" && x.Name.Name != "init" &&
					!ast.IsExported(x.Name.Name) {
					declared[x.Name.Name] = true
				}
			case *ast.SelectorExpr:
				used[x.Sel.Name]++
			case *ast.Ident:
				used[x.Name]++
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(declared) < 100 {
		t.Fatalf("found only %d unexported functions; the walk is broken and this check is "+
			"testing nothing", len(declared))
	}

	var uncalled []string
	for name := range declared {
		// One reference is the declaration itself.
		if used[name] <= 1 {
			uncalled = append(uncalled, name)
		}
	}
	sort.Strings(uncalled)

	var unexpected, stale []string
	for _, name := range uncalled {
		if _, ok := knownUncalled[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	for name := range knownUncalled {
		found := false
		for _, u := range uncalled {
			if u == name {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)

	if len(unexpected) > 0 {
		t.Fatalf("unexported function(s) nothing calls:\n  %s\n\n"+
			"Either wire them up, delete them, or pin them in knownUncalled with the reason. The "+
			"function behind the missing share-download audit and the one behind the unchecked "+
			"WebDAV destination were both found this way.", strings.Join(unexpected, "\n  "))
	}
	if len(stale) > 0 {
		t.Fatalf("knownUncalled lists function(s) that are now called or gone: %s\n\n"+
			"Remove them so the list records the improvement and catches a regression.",
			strings.Join(stale, ", "))
	}
}

func moduleRoot(t *testing.T) string {
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
	t.Fatal("no module root")
	return ""
}
