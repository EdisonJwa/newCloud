package server_test

// Fail-closed authorisation (§4, T-01).
//
// Every call to Authorize must either return its error to the caller or refuse. None may discard
// it, and none may treat "the check could not be made" as "the check passed".
//
// This is checked structurally rather than case by case because the failure mode is a missing
// branch in whichever handler somebody added last. A reviewer reading one handler will not notice
// that the twenty-fifth call site swallows its error; that is precisely how a permission check
// becomes decorative.
//
// The property held when this test was written — all twenty-four call sites were correct — so this
// round found nothing. That is the point of writing it anyway: the audit that produced that number
// lived only in a conversation, and the next handler added will not know about it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEveryAuthorizeCallSiteFailsClosed(t *testing.T) {
	root := repoRoot(t)

	// `cap, err := something.Authorize(` or a direct `return something.Authorize(`.
	call := regexp.MustCompile(`(\w+)\s*,\s*(\w+)\s*:?=\s*\S*\.?Authorize\(`)

	count := 0
	var problems []string

	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(src), "\n")
		rel, _ := filepath.Rel(root, p)

		for i, line := range lines {
			m := call.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			count++
			errVar := m[2]

			// Directly returning the result is fine: the caller decides.
			if strings.Contains(line, "return") {
				continue
			}
			// A discarded error is the defect this test exists for.
			if errVar == "_" {
				problems = append(problems, rel+":"+itoa(i+1)+": discards the Authorize error")
				continue
			}
			// Otherwise the error must be consulted within the next few lines. Six is enough for a
			// multi-line condition and short enough that a distant, unrelated `if` cannot satisfy
			// it by accident.
			// The whole line on the first iteration, not just the part after the call:
			// `if _, cerr := ...Authorize(...); cerr != nil {` checks the error BEFORE the call
			// textually, and scanning only forwards called that a failure.
			seen := false
			for j := i; j < len(lines) && j < i+6; j++ {
				rest := lines[j]
				if strings.Contains(rest, errVar) &&
					(strings.Contains(rest, "if ") || strings.Contains(rest, "switch ")) {
					seen = true
					break
				}
			}
			if !seen {
				problems = append(problems, rel+":"+itoa(i+1)+": never checks "+errVar+
					", so a failed permission lookup reads as a granted one")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A floor, so a walk that stops finding call sites cannot report success over nothing.
	if count < 20 {
		t.Fatalf("found only %d Authorize call sites; the walk is probably broken and this check "+
			"is not testing anything", count)
	}
	if len(problems) > 0 {
		t.Fatalf("%d authorisation call site(s) do not fail closed:\n  %s\n\n"+
			"An unreadable permission must refuse the request. Treating it as granted turns a "+
			"database problem into an authorisation bypass.",
			len(problems), strings.Join(problems, "\n  "))
	}
}
