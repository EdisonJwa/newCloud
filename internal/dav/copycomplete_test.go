package dav

// P0-DAV-COPY-001 / P0-DAV-COPY-002: a COPY must cover the whole tree or fail.
//
// The independent review reproduced both halves on 2026-10-23 with limits.max_profind_children = 1:
//
//	COPY src/ -> copied/      answered 201, copied one of two files
//	COPY src/ -> dst/         answered 204, replaced a complete destination with an incomplete copy
//
// The cause was that copyTree recursed through h.listChildren, which applies the PROPFIND page size
// and says nothing when it truncates. The listing limit is a UI budget; it has no business deciding
// whether a data operation contained a file.
//
// These tests are permanent: they exercise the real DAV handler on BOTH content layouts, and they run
// the refusal branch (a source too large to prove complete) as well as the success branch. A test
// that could only observe the success path would not notice a regression back to truncation.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// seedCopySource builds the folder shape from the review: two files at the top and a nested folder,
// so a per-level truncation is visible at more than one depth.
func seedCopySource(t *testing.T, e *testEnv) {
	t.Helper()
	if rec := e.do(t, "MKCOL", e.davPath("srcfolder"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol srcfolder: %d", rec.Code)
	}
	for _, f := range []struct{ name, body string }{
		{"a.txt", "AAA"},
		{"b.txt", "BBB"},
	} {
		if rec := e.do(t, http.MethodPut, e.davPath("srcfolder/"+f.name),
			strings.NewReader(f.body), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed srcfolder/%s: %d", f.name, rec.Code)
		}
	}
	if rec := e.do(t, "MKCOL", e.davPath("srcfolder/nested"), nil, nil); rec.Code != http.StatusCreated {
		t.Fatalf("mkcol nested: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPut, e.davPath("srcfolder/nested/c.txt"),
		strings.NewReader("CCC"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed nested/c.txt: %d", rec.Code)
	}
}

// assertCopyContents requires every file of the source, at the destination, with its exact bytes.
func assertCopyContents(t *testing.T, e *testEnv, prefix string, mode layout.Mode) {
	t.Helper()
	for _, f := range []struct{ name, body string }{
		{"a.txt", "AAA"},
		{"b.txt", "BBB"},
		{"nested/c.txt", "CCC"},
	} {
		got := e.do(t, http.MethodGet, e.davPath(prefix+"/"+f.name), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: %s is %d after a COPY that reported success. A copy missing a file is "+
				"worse than a copy that failed: the client believes it has its data.", mode,
				prefix+"/"+f.name, got.Code)
		}
		if body := got.Body.String(); body != f.body {
			t.Fatalf("%s: %s = %q, want %q", mode, prefix+"/"+f.name, body, f.body)
		}
	}

	// The structure, not merely the files that were asked about: an incomplete copy or a leftover
	// staging sibling would change the child list.
	var got []string
	dstID := nodeIDAt(t, e, prefix)
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		kids, kerr := tx.Children(ctx, dstID, 100)
		for _, k := range kids {
			got = append(got, k.Name)
		}
		return kerr
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a.txt" || got[1] != "b.txt" || got[2] != "nested" {
		t.Fatalf("%s: %s contains %v, want exactly [a.txt b.txt nested]", mode, prefix, got)
	}
}

// TestCopyFolderIsCompleteEvenWhenTheListingPageIsOne is the review's first reproduction.
//
// The listing page size is set to 1, exactly as in the report. The old implementation answered 201
// and copied only "a.txt"; the assertion below cannot be satisfied by a partial tree.
func TestCopyFolderIsCompleteEvenWhenTheListingPageIsOne(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		// The PROPFIND page size, deliberately smaller than every directory in the source.
		e.h.cfg.Limits.MaxProfindChildren = 1
		seedCopySource(t, e)

		rec := e.do(t, "COPY", e.davPath("srcfolder"), nil,
			map[string]string{"Destination": e.davPath("copiedfolder")})
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: COPY = %d, want 201 (%s)", mode, rec.Code, rec.Body.String())
		}

		assertCopyContents(t, e, "copiedfolder", mode)
	})
}

// TestCopyOverwriteIsCompleteEvenWhenTheListingPageIsOne is the review's second reproduction.
//
// An overwrite must never replace a complete destination with an incomplete copy while answering 204.
// The old destination is expected to move to the trash (that is overwrite semantics), but the new one
// has to be the whole source.
func TestCopyOverwriteIsCompleteEvenWhenTheListingPageIsOne(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		e.h.cfg.Limits.MaxProfindChildren = 1
		seedCopySource(t, e)

		if rec := e.do(t, "MKCOL", e.davPath("dstfolder"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("mkcol dstfolder: %d", rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("dstfolder/precious.txt"),
			strings.NewReader("OLD VALUABLE DATA"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed precious.txt: %d", rec.Code)
		}

		rec := e.do(t, "COPY", e.davPath("srcfolder"), nil,
			map[string]string{"Destination": e.davPath("dstfolder")})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: COPY overwrite = %d, want 204 (%s)", mode, rec.Code, rec.Body.String())
		}

		assertCopyContents(t, e, "dstfolder", mode)

		// The displaced original is gone from the live tree — the overwrite did happen, so this is
		// not a test that passes by refusing to replace anything.
		if old := e.do(t, http.MethodGet, e.davPath("dstfolder/precious.txt"), nil, nil); old.Code == http.StatusOK {
			t.Fatalf("%s: the previous destination's file survived an overwrite that answered 204", mode)
		}
	})
}

// TestCopyIsRefusedWhenTheSourceCannotBeProvenComplete is the other half of the rule.
//
// "Copy the whole tree or fail" has two branches. The budget is lowered so the refusal itself is
// exercised: the destination must not exist afterwards, and the source must be intact. Before the
// fix this scenario answered 201 with a partial destination, because nothing checked completeness at
// all.
func TestCopyIsRefusedWhenTheSourceCannotBeProvenComplete(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		copyWalkBudgetForTest = 2
		t.Cleanup(func() { copyWalkBudgetForTest = 0 })

		seedCopySource(t, e) // three descendants, budget two

		rec := e.do(t, "COPY", e.davPath("srcfolder"), nil,
			map[string]string{"Destination": e.davPath("copiedfolder")})
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: a source too large to prove complete must be refused with 413, got %d "+
				"(%s). A 2xx here means the server copied a tree it could not prove it had seen "+
				"entirely.", mode, rec.Code, rec.Body.String())
		}

		// No destination may be left behind, not even a partial one.
		if got := e.do(t, http.MethodGet, e.davPath("copiedfolder/a.txt"), nil, nil); got.Code == http.StatusOK {
			t.Fatalf("%s: a refused COPY created a partial destination", mode)
		}
		// And the source is untouched.
		assertCopyContents(t, e, "srcfolder", mode)
	})
}

// nodeIDAt resolves a path to its node id, so a test can prove the destination is the SAME node
// afterwards rather than merely a directory with the same name.
func nodeIDAt(t *testing.T, e *testEnv, rel string) uuid.UUID {
	t.Helper()
	nodes, err := e.meta.Resolve(context.Background(), e.user.ID, rel)
	if err != nil {
		t.Fatalf("resolve %q: %v", rel, err)
	}
	return nodes[len(nodes)-1].ID
}

// trashedCount counts the account's trash.
func trashedCount(t *testing.T, e *testEnv) int {
	t.Helper()
	var n int
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		items, err := tx.Trashed(ctx, e.user.ID, 1000)
		n = len(items)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCopyOverwriteIsRefusedBeforeTheDestinationIsTouched pins the ordering that matters most: the
// completeness proof happens BEFORE the swap begins, so a refusal cannot have displaced, staged or
// trashed the destination.
func TestCopyOverwriteIsRefusedBeforeTheDestinationIsTouched(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, mode layout.Mode) {
		copyWalkBudgetForTest = 1
		t.Cleanup(func() { copyWalkBudgetForTest = 0 })

		seedCopySource(t, e)

		if rec := e.do(t, "MKCOL", e.davPath("dstfolder"), nil, nil); rec.Code != http.StatusCreated {
			t.Fatalf("mkcol dstfolder: %d", rec.Code)
		}
		if rec := e.do(t, http.MethodPut, e.davPath("dstfolder/precious.txt"),
			strings.NewReader("OLD VALUABLE DATA"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed precious.txt: %d", rec.Code)
		}
		beforeID := nodeIDAt(t, e, "dstfolder")
		beforeTrash := trashedCount(t, e)

		rec := e.do(t, "COPY", e.davPath("srcfolder"), nil,
			map[string]string{"Destination": e.davPath("dstfolder")})
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: an overwrite whose source cannot be proven complete must be refused with "+
				"413, got %d (%s)", mode, rec.Code, rec.Body.String())
		}

		// The destination's identity and the trash are unchanged: a refusal must not have begun
		// the swap at all.
		if afterID := nodeIDAt(t, e, "dstfolder"); afterID != beforeID {
			t.Fatalf("%s: a refused overwrite replaced the destination node (%s -> %s)", mode,
				beforeID, afterID)
		}
		if afterTrash := trashedCount(t, e); afterTrash != beforeTrash {
			t.Fatalf("%s: a refused overwrite changed the trash (%d -> %d items)", mode,
				beforeTrash, afterTrash)
		}

		// The original destination is exactly where and what it was.
		got := e.do(t, http.MethodGet, e.davPath("dstfolder/precious.txt"), nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: a refused overwrite removed the destination (GET = %d)", mode, got.Code)
		}
		if body := got.Body.String(); body != "OLD VALUABLE DATA" {
			t.Fatalf("%s: a refused overwrite changed the destination: %q", mode, body)
		}
	})
}
