package dav

// Conditional-PUT semantics, on both content layouts.
//
// These pin the defects the 2026-10-24 review reproduced (review §5, C1 and the
// conditional-semantics M2 group): the tree layout published the new bytes before
// evaluating If-Match, so a refused PUT destroyed the content it was refusing to
// accept; If-Match on a missing resource answered 201; a matching If-None-Match
// answered 204; a weak If-Match was honoured; and a chunked body bypassed
// limits.max_file_bytes entirely.
//
// Every scenario runs against BOTH layouts, because the failure only existed on the
// one where the content store publishes to the live path — a guarantee tested on the
// other layout alone is not a guarantee.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// chunked issues a request the way Go's server presents a chunked upload: no declared
// length (ContentLength -1) and Transfer-Encoding set. httptest.NewRequest cannot
// produce that shape on its own — it derives the length from the body — so the fields
// are set explicitly. This is the shape that once bypassed the size limit entirely.
func (e *testEnv) chunked(t *testing.T, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.SetBasicAuth("alice", e.secret)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// A refused conditional PUT must leave the resource exactly as it was. On the tree
// layout the old code had already replaced the live path by the time it answered 412,
// so a GET afterwards returned the NEW bytes under the OLD metadata.
func TestARefusedConditionalPutLeavesThePreviousContentIntact(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		rec := e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("v1 original"), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: want 201, got %d", rec.Code)
		}
		stale := rec.Header().Get("ETag")

		if rec := e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("v2 winner"), nil); rec.Code != http.StatusNoContent {
			t.Fatalf("second write: want 204, got %d", rec.Code)
		}

		rec = e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("stale loser"),
			map[string]string{"If-Match": stale})
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("stale If-Match: want 412, got %d", rec.Code)
		}

		get := e.do(t, http.MethodGet, e.davPath("doc.txt"), nil, nil)
		if get.Code != http.StatusOK {
			t.Fatalf("GET after refusal: want 200, got %d", get.Code)
		}
		if got := get.Body.String(); got != "v2 winner" {
			t.Fatalf("the refused write replaced the content: got %q", got)
		}
	})
}

// If-Match on a resource that does not exist fails the request (RFC 7232 §3.1). The
// old code fell through to the create path and answered 201, telling the client its
// guard had passed when the guard had not been evaluated at all.
func TestIfMatchOnAMissingResourceRefusesThePut(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		rec := e.do(t, http.MethodPut, e.davPath("absent.txt"), strings.NewReader("x"),
			map[string]string{"If-Match": "*"})
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("If-Match on a missing resource: want 412, got %d", rec.Code)
		}
		if rec := e.do(t, http.MethodGet, e.davPath("absent.txt"), nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("a refused create must not create: GET got %d", rec.Code)
		}
	})
}

// If-None-Match with the CURRENT tag must refuse a PUT (RFC 7232 §3.2). It answered
// 204 before, which a client reads as "saved".
func TestIfNoneMatchHoldingTheCurrentTagRefusesThePut(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		rec := e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("v1"), nil)
		current := rec.Header().Get("ETag")

		rec = e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("should not land"),
			map[string]string{"If-None-Match": current})
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("If-None-Match with the current tag: want 412, got %d", rec.Code)
		}
		if got := e.do(t, http.MethodGet, e.davPath("doc.txt"), nil, nil).Body.String(); got != "v1" {
			t.Fatalf("the refused write landed: %q", got)
		}
	})
}

// If-Match never matches a weakly-tagged entity (RFC 7232 §2.1). Accepting it let a
// client overwrite content it had only weakly validated.
func TestAWeakIfMatchNeverAuthorisesAPut(t *testing.T) {
	layouts(t, func(t *testing.T, e *testEnv, _ layout.Mode) {
		rec := e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("v1"), nil)
		strong := rec.Header().Get("ETag")
		weak := "W/" + strong

		rec = e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("weak overwrite"),
			map[string]string{"If-Match": weak})
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("weak If-Match: want 412, got %d", rec.Code)
		}
		if got := e.do(t, http.MethodGet, e.davPath("doc.txt"), nil, nil).Body.String(); got != "v1" {
			t.Fatalf("the weakly-authorised write landed: %q", got)
		}
	})
}

// A body with no declared length is capped by the read itself. The old code checked
// only ContentLength, which a chunked upload does not declare (it is -1), so
// `Transfer-Encoding: chunked` wrote without any limit at all.
//
// The env here carries a deliberately tiny limit, because the default is 1 TiB and no
// test wants to upload that much to trip it.
func TestAChunkedBodyBeyondTheLimitIsRefused(t *testing.T) {
	e := newEnvWithByteLimit(t, 16)
	if rec := e.do(t, http.MethodPut, e.davPath("big.bin"), strings.NewReader("original"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	oversized := strings.Repeat("x", 4096)
	rec := e.chunked(t, http.MethodPut, e.davPath("big.bin"), strings.NewReader(oversized))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked body over the limit: want 413, got %d (%s)",
			rec.Code, truncateForLog(rec.Body.String(), 120))
	}
	if got := e.do(t, http.MethodGet, e.davPath("big.bin"), nil, nil).Body.String(); got != "original" {
		t.Fatalf("the oversized write replaced the content: %d bytes", len(got))
	}

	// And the mirror case: a declared length beyond the limit is refused before a
	// single byte is read.
	rec = e.do(t, http.MethodPut, e.davPath("big.bin"), strings.NewReader(oversized), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared length over the limit: want 413, got %d", rec.Code)
	}
}

// putPrecondition is what hands the store the atomic half of the decision
// (UpdateFile's Expect). A list-valued If-Match must collapse to the one tag that
// matched, because the store compares a single exact value — anything else would
// re-evaluate a different condition than the one this function already decided.
func TestPutPreconditionNormalisesTheExpectationForTheStore(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPut, e.davPath("doc.txt"), strings.NewReader("v1"), nil)
	current := rec.Header().Get("ETag")

	var node store.Node
	if err := e.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		nodes, rerr := e.meta.Resolve(ctx, e.user.ID, "doc.txt")
		if rerr != nil {
			return rerr
		}
		node = nodes[len(nodes)-1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A list where the second member is the current tag.
	list := `"deadbeef", ` + current
	expect, ok := e.h.putPrecondition(&http.Request{Header: http.Header{
		"If-Match": []string{list},
	}}, &node)
	if !ok {
		t.Fatal("a list containing the current tag must pass")
	}
	if expect.IfMatch != current {
		t.Fatalf("Expect.IfMatch must collapse to the matched tag, got %q", expect.IfMatch)
	}

	// The wildcard must survive as the wildcard.
	expect, ok = e.h.putPrecondition(&http.Request{Header: http.Header{
		"If-Match": []string{"*"},
	}}, &node)
	if !ok || expect.IfMatch != "*" {
		t.Fatalf("If-Match * must normalise to *, got ok=%v IfMatch=%q", ok, expect.IfMatch)
	}

	// No conditionals at all: no expectation, unconditional write allowed.
	if expect, ok := e.h.putPrecondition(&http.Request{Header: http.Header{}}, &node); !ok || expect != nil {
		t.Fatalf("no conditionals: want nil,true got %v,%v", expect, ok)
	}
}

// newEnvWithByteLimit builds the blob-layout env with limits.max_file_bytes lowered,
// for tests that need to trip the cap without megabytes of fixture.
func newEnvWithByteLimit(t *testing.T, limit int64) *testEnv {
	t.Helper()
	e := newEnv(t)
	e.h.cfg.Limits.MaxFileBytes = limit
	return e
}

// truncateForLog keeps a failure message readable without dumping a whole error body.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
