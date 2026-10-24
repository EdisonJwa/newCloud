package dav

// A COPY must not publish its bytes at a new name before its metadata row owns that name.
//
// On the tree layout the content address IS the path, so writing the destination first means
// a concurrent PUT that already returned success has its bytes replaced by a copy that then
// fails to publish. The winner never sees an error: its file simply becomes someone else's
// content, or nothing. This is the 2026-10-24 review's C3; the reproduction lives in
// phase0_red_test.go behind a build tag, and this file pins the same property in the suite
// that CI actually runs.
//
// Both layouts are covered because the fix is layout-aware: where paths follow content the
// copy stages under a reserved sibling name and installs only after the row commits, and
// where they do not (the blob layout) the write is non-destructive and stays direct.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
)

// interleavingPut runs a competing request at the moment a COPY writes its bytes.
//
// The match is by substring because the copy may write a reserved staging name
// (`<rand>.newcloud-staged-dst.txt`) rather than the destination itself — and the moment that
// matters is when bytes are written, wherever that is.
type interleavingPut struct {
	layout.ContentStore
	armed  bool
	fired  bool
	target string
	run    func()
}

func (c *interleavingPut) Put(ctx context.Context, n layout.Node, r io.Reader, size int64) (layout.PutResult, error) {
	if c.armed && strings.Contains(n.Path, c.target) && !n.IsDir {
		c.fired = true
		c.armed = false
		c.run()
	}
	return c.ContentStore.Put(ctx, n, r, size)
}

// TestCopyToANewNameDoesNotLoseAConcurrentWinnersBytes is C3.
func TestCopyToANewNameDoesNotLoseAConcurrentWinnersBytes(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		if rec := e.do(t, http.MethodPut, e.davPath("src.txt"),
			strings.NewReader("SOURCE BYTES"), nil); rec.Code != http.StatusCreated {
			t.Fatalf("seed source: %d (%s)", rec.Code, rec.Body.String())
		}

		inter := &interleavingPut{ContentStore: e.h.content, target: "dst.txt"}
		var winnerCode int
		inter.run = func() {
			winnerCode = e.do(t, http.MethodPut, e.davPath("dst.txt"),
				strings.NewReader("B WINS"), nil).Code
		}
		e.h.content = inter
		inter.armed = true

		rec := e.do(t, "COPY", e.davPath("src.txt"), nil,
			map[string]string{"Destination": e.davPath("dst.txt")})
		if !inter.fired {
			t.Fatal("the concurrent PUT was never injected; the test would prove nothing")
		}
		if winnerCode != http.StatusCreated && winnerCode != http.StatusNoContent {
			t.Fatalf("the concurrent PUT failed (%d); the interleaving did not happen", winnerCode)
		}
		// The copy lost the name, so it must report a failure rather than a create.
		if rec.Code == http.StatusCreated || rec.Code == http.StatusNoContent {
			t.Fatalf("COPY reported %d even though the destination name was taken", rec.Code)
		}

		got := e.do(t, http.MethodGet, e.davPath("dst.txt"), nil, nil)
		if got.Code != http.StatusOK || got.Body.String() != "B WINS" {
			t.Fatalf("after the lost COPY, GET dst.txt = %d %q, want 200 with B's bytes. "+
				"The copy published over a winner whose success was already returned.",
				got.Code, got.Body.String())
		}

		// And the losing copy must not leave its staged bytes behind, where they would hold
		// quota and clutter a filesystem that an operator inspects with `ls`.
		list, err := e.h.content.List(context.Background(), layout.Node{
			Owner: e.user.ID.String(), Path: "", IsDir: true,
		}, 100)
		if err == nil {
			for _, n := range list {
				if strings.Contains(n.Path, ".newcloud-staged-") {
					t.Fatalf("the failed copy left a staged file behind: %q", n.Path)
				}
			}
		}
	})
}
