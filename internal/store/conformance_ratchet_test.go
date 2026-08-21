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
	"AllRoots", "AppPasswordByHash", "BlobHash", "CollectableBlobs",
	"DeadProps", "DeleteBlob", "DeleteDeadProp", "DeleteLock",
	"Descendants", "ExpireDownloadSessions", "GroupByID", "GroupByName",
	"GroupMembers", "GroupsForUser", "IncrementDownload", "IncrementUpload", "LocksForUser",
	"MarkRecipientAccess", "PruneOTPAttempts",
	"PurgeTrash", "ReapExpiredLeases", "RecipientsFor", "RefreshLock", "RemoveGroupMember", "RevokeAppPassword", "RevokeSession",
	"RevokeShare", "RevokeUserAppPasswords", "RevokeUserSessions", "SampleBlobHashes",
	"SetDeadProp", "ShareByTokenHash", "SharesByOwner", "SharesForNode", "TouchAppPassword",
	"UpdateShare", "UserByEmail", "UserByIdPSubject", "UserByUsername",
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
