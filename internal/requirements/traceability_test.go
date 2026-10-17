package requirements

// Requirement traceability, as a ratchet.
//
// The gap analysis numbers its requirements, and those numbers are the only stable way to ask "is this
// requirement implemented?" — the prose moves, the IDs do not. The code cites many of them in comments,
// which makes the mapping greppable. Nothing enforced that, so a refactor could quietly delete the only
// pointer from a requirement to the code that satisfies it, and the loss would be invisible.
//
// This test knows the list from the document and requires each P0/P1 requirement to be cited somewhere
// in production source. It is bidirectional, like the other ratchets here: a deferred ID that starts
// being cited fails too, because a stale allowlist is a lie of its own kind.
//
// What it is NOT: proof of implementation. A comment citing SHARE-07 says a person believed the code
// addresses it; the behaviour is proven by tests. This instrument only keeps the MAPPING alive, so an
// auditor (or a future maintainer) can find the claim in the first place.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// deferred lists requirements that are deliberately NOT implemented, with the reason. An entry here
// means "no code cites this because there is no code"; adding one without a reason is how a gap becomes
// permanent.
var deferred = map[string]string{
	"SHARE-11": "team-owned shared spaces (P2): shares are personal; there is no team entity, so a " +
		"space that outlives an individual account does not exist. ID-09's provisioning and group " +
		"membership cover the collaboration needs that are implemented",
}

// required is every P0/P1 requirement ID from the document (v0.2), plus the invariants, which the
// document treats as binding without a P-level.
var required = []string{
	// Identity (P0).
	"ID-01", "ID-02", "ID-03", "ID-04", "ID-05", "ID-06", "ID-07", "ID-08",
	// Invariants: binding, no P-level in the document.
	"INV-01", "INV-02", "INV-03", "INV-04", "INV-05", "INV-06", "INV-07", "INV-08", "INV-09",
	// Sharing: P0 for 01-07, P1 for 08-10.
	"SHARE-01", "SHARE-02", "SHARE-03", "SHARE-04", "SHARE-05", "SHARE-06", "SHARE-07",
	"SHARE-08", "SHARE-09", "SHARE-10",
}

// lowestPriority lists the IDs present in the document that are NOT required above, so the list cannot
// silently omit a P2 requirement: if one is added to the document, this test has to be updated.
var deferredByPriority = []string{
	"ID-09",    // P2: SCIM or a management API. The management API half is implemented; see admin_users.go.
	"SHARE-11", // P2: team spaces, unimplemented (see deferred).
}

func TestEveryRequiredRequirementIsCited(t *testing.T) {
	root := repoRoot(t)
	sources := productionSources(t, root)

	var missing []string
	for _, id := range required {
		if _, isDeferred := deferred[id]; isDeferred {
			t.Fatalf("%s is both required and deferred; pick one", id)
		}
		if !cited(sources, id) {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("requirement(s) with no citation in production code:\n  %s\n\n"+
			"Either the requirement is implemented and the comment that names it was lost — restore it "+
			"where the behaviour lives, which is how an auditor finds the code — or it is not "+
			"implemented, in which case add it to `deferred` with the reason it is not.",
			strings.Join(missing, "\n  "))
	}
}

func TestADeferredRequirementThatIsNowCitedIsRemovedFromTheList(t *testing.T) {
	root := repoRoot(t)
	sources := productionSources(t, root)

	var stale []string
	for id := range deferred {
		if cited(sources, id) {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("%s is deferred but now cited in production code; remove it from `deferred` so the "+
			"ratchet records the improvement and can catch a regression", strings.Join(stale, ", "))
	}
}

func TestTheDeferredListCoversEveryP2Requirement(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range required {
		seen[id] = true
	}
	for _, id := range deferredByPriority {
		if seen[id] {
			t.Fatalf("%s is both required and deferred", id)
		}
		seen[id] = true
	}
	// The document's P2 set is exactly these two. If a third P2 arrives, this test has to be touched,
	// which is the point: the list is a decision, not an omission.
	for id := range deferred {
		if !contains(deferredByPriority, id) {
			t.Fatalf("%s is deferred but not in deferredByPriority", id)
		}
	}
	for _, id := range deferredByPriority {
		if id == "ID-09" {
			// ID-09 is satisfied through the management API, not deferred to nothing: its citation
			// lives in admin_users.go, so it is required in practice.
			if !cited(productionSources(t, repoRoot(t)), id) {
				t.Fatalf("ID-09 is the management-API half of a P2 requirement and should be cited " +
					"where the endpoint lives")
			}
		}
	}
}

func cited(sources map[string]string, id string) bool {
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\b`)
	for _, body := range sources {
		if pattern.MatchString(body) {
			return true
		}
	}
	return false
}

// productionSources reads every non-test Go file outside this package.
//
// This package IS the list, so it is excluded: counting its own table as a citation would make the test
// pass no matter what the rest of the repository said.
func productionSources(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "requirements" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			out[path] = string(body)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(out) < 50 {
		t.Fatalf("found only %d production files; the walk is probably broken and this check would "+
			"pass vacuously", len(out))
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find go.mod above the working directory")
	return ""
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
