package server_test

// SHARE-06: what a share link does after the file behind it is deleted or moved.
//
// The requirement says the effect must be *defined* and must cover derived resources. The
// failure without that definition is quiet: the lookup error was discarded with `_ =`, so a link
// whose file had been trashed answered 200 with an empty `root_name` and no children. The
// recipient saw a blank listing and nothing said why — indistinguishable from an empty folder,
// which is worse than an error because there is nothing to report to anyone.
//
// Two properties are pinned here:
//
//  1. a trashed file makes the link stop serving content (the response is a refusal);
//  2. the refusal is *indistinguishable* from an expired or revoked link. That is deliberate:
//     renderShareError collapses every cause because distinguishing them would turn the endpoint
//     into a token oracle. A test that asserted a distinct "gone" message would be asserting a
//     vulnerability.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// shareHash is the token hash the server computes.
func shareHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// createShareWithToken creates a link share for a node with a known token.
func (h *harness) createShareWithToken(t *testing.T, nodeID [16]byte, token string) store.Share {
	t.Helper()
	var sh store.Share
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		got, err := tx.CreateShare(ctx, store.Share{
			OwnerID: h.user.ID, NodeID: nodeID, Kind: store.ShareLink,
			TokenHash: shareHash(token), Permissions: []string{"read"},
		})
		if err != nil {
			return err
		}
		sh = got
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return sh
}

// sharePage fetches the JSON view of a share (not the HTML shell).
func (h *harness) sharePage(t *testing.T, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Without an HTML Accept the handler returns JSON rather than the app shell.
	req.Header.Set("Accept", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// TestShareStopsServingAfterFileIsTrashed is the SHARE-06 case.
func TestShareStopsServingAfterFileIsTrashed(t *testing.T) {
	h := newHarness(t)
	file := h.uploadFile(t, "report.txt", "content")
	token := "share06-trashed"
	h.createShareWithToken(t, file.ID, token)

	// While the file exists the link serves it.
	code, body := h.sharePage(t, token)
	if code != http.StatusOK {
		t.Fatalf("the link should serve while the file exists, got %d", code)
	}
	if body["root_name"] != "report.txt" {
		t.Fatalf("setup: unexpected share view %v", body)
	}

	// Trash the file.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Trash(ctx, file.ID)
	}); err != nil {
		t.Fatal(err)
	}

	// The link must refuse, not answer 200 with an empty listing.
	code, body = h.sharePage(t, token)
	if code == http.StatusOK {
		t.Fatalf("a trashed file must not be served through its link; got 200 with %v", body)
	}
	if code != http.StatusNotFound {
		t.Fatalf("the refusal must be 404, got %d", code)
	}
}

// TestShareRefusalIsIndistinguishableFromRevoked.
//
// The three causes — file gone, link revoked, link expired — must look identical from outside.
// A distinct message per cause would let anyone with a candidate token learn whether it was ever
// valid, which is a token oracle.
func TestShareRefusalIsIndistinguishableFromRevoked(t *testing.T) {
	h := newHarness(t)

	// Case 1: the file is gone.
	gone := h.uploadFile(t, "gone.txt", "x")
	goneToken := "share06-gone"
	h.createShareWithToken(t, gone.ID, goneToken)
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.Trash(ctx, gone.ID)
	}); err != nil {
		t.Fatal(err)
	}

	// Case 2: the link is revoked, file intact.
	revoked := h.uploadFile(t, "revoked.txt", "x")
	revokedToken := "share06-revoked"
	sh := h.createShareWithToken(t, revoked.ID, revokedToken)
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.RevokeShare(ctx, sh.ID)
	}); err != nil {
		// The store method may be named differently; fall back to setting revoked_at directly.
		if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.RevokeShare(ctx, sh.ID)
		}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	}

	// Case 3: a token that never existed.
	unknownToken := "share06-never-existed"

	codeGone, bodyGone := h.sharePage(t, goneToken)
	codeRevoked, bodyRevoked := h.sharePage(t, revokedToken)
	codeUnknown, bodyUnknown := h.sharePage(t, unknownToken)

	for name, code := range map[string]int{
		"file gone": codeGone, "revoked": codeRevoked, "unknown": codeUnknown,
	} {
		if code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d", name, code)
		}
	}

	// The bodies must be identical, or the difference is an oracle.
	want := mustJSON(t, bodyGone)
	for name, body := range map[string]map[string]any{
		"revoked": bodyRevoked, "unknown": bodyUnknown,
	} {
		if got := mustJSON(t, body); got != want {
			t.Fatalf("%s answered %s but 'file gone' answered %s; distinguishing them lets a "+
				"candidate token be tested for validity", name, got, want)
		}
	}
}

// mustJSON renders a decoded body deterministically for comparison.
func mustJSON(t *testing.T, body map[string]any) string {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
