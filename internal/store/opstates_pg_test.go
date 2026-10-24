package store_test

// The journal's states exist twice: as constants, and as the CHECK constraint that refuses anything
// else. A state added to one and not the other is either dead code or a write the database rejects at
// 3am, and neither shows up in a test that only drives the states it already knows.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

func TestOpStatesMatchTheSchemaCheck(t *testing.T) {
	body, err := os.ReadFile("../../migrations/0016_consistency.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}

	start := strings.Index(string(body), "state           text NOT NULL")
	if start < 0 {
		t.Fatal("the migration no longer declares the state column the way this test reads it; " +
			"update the test rather than deleting it, because the constraint is what makes a bad " +
			"state unwritable")
	}
	rest := string(body)[start:]
	end := strings.Index(rest, "))")
	if end < 0 {
		t.Fatal("could not find the end of the CHECK")
	}
	quoted := regexp.MustCompile(`'([A-Z_]+)'`).FindAllStringSubmatch(rest[:end], -1)

	inSchema := map[string]bool{}
	for _, m := range quoted {
		inSchema[m[1]] = true
	}
	inCode := map[string]bool{}
	for _, s := range store.OpStates() {
		inCode[s] = true
	}

	for s := range inCode {
		if !inSchema[s] {
			t.Errorf("state %q exists in the code and not in the CHECK: the database would reject a "+
				"transition the code believes it can make", s)
		}
	}
	for s := range inSchema {
		if !inCode[s] {
			t.Errorf("state %q exists in the CHECK and not in store.OpStates(): recovery would meet "+
				"a state it does not know", s)
		}
	}
}
