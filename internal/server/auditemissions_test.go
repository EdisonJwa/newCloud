package server_test

// Three audit events that were declared and never written.
//
// §10.1 requires audit coverage of sensitive operations, and the vocabulary is what an operator
// filters on. A name that exists and never fires is worse than a missing name: the filter returns
// nothing, and nothing says whether that means "it did not happen" or "it was never wired".
//
// Each case here asserts the row appears for the event it describes AND does not appear for the
// adjacent event it must not be confused with — an account creation that fires on every login, or an
// expiry that fires for a revocation, would be as misleading as no row at all.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/fakeidp"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func actionsIn(t *testing.T, m *store.Memory, action string) []store.AuditEntry {
	t.Helper()
	var out []store.AuditEntry
	for _, e := range m.AuditLog() {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// TestUserCreateIsRecordedOncePerAccount: the creation is its own event, not a property of the first
// login that happens to produce it.
func TestUserCreateIsRecordedOncePerAccount(t *testing.T) {
	h := newHarness(t)
	if rows := actionsIn(t, h.meta, "user.create"); len(rows) != 0 {
		t.Fatalf("the fixture starts with %d user.create rows", len(rows))
	}

	// The harness user was created directly in the store, so this is the JIT path: a fresh OIDC
	// login for an identity the server has never seen.
	oidc := newOIDCHarness(t, fakeidp.User{
		Subject: "audit-jit", Username: "audit-jit", Email: "audit-jit@example.test",
	})
	oidc.login(t)

	rows := actionsIn(t, oidc.meta, "user.create")
	if len(rows) != 1 {
		t.Fatalf("a first login wrote %d user.create rows, want 1", len(rows))
	}
	if rows[0].Metadata["via"] != "oidc" {
		t.Fatalf("the row does not say how the account appeared: %v", rows[0].Metadata)
	}

	// A second login must not claim a second creation.
	oidc.b.resetJar(t)
	oidc.login(t)
	if rows := actionsIn(t, oidc.meta, "user.create"); len(rows) != 1 {
		t.Fatalf("a repeat login added another user.create row (%d total). The event marks the "+
			"account's creation; firing per login makes it a second, noisier login log.", len(rows))
	}
}

// TestExpiredShareIsRecordedAndRevokedIsNot: the visitor sees one answer, the owner's log has the
// reason.
func TestExpiredShareIsRecordedAndRevokedIsNot(t *testing.T) {
	h := newHarness(t)
	token := shareToken(t, h)

	// Age the share past its expiry directly in the metadata layer: the create API has no way to
	// produce an already-expired link, which is the correct design and inconvenient here.
	if err := h.meta.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		hash := sha256.Sum256([]byte(token))
		sh, err := tx.ShareByTokenHash(ctx, hash[:])
		if err != nil {
			return err
		}
		past := time.Now().Add(-time.Hour)
		sh.ExpiresAt = &past
		_, uerr := tx.UpdateShare(ctx, sh)
		return uerr
	}); err != nil {
		t.Fatalf("expire the share: %v", err)
	}

	resp := h.do(http.MethodGet, "/s/"+token, nil, nil)
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("an expired link returned %d, want the same 404 every other refusal gives", code)
	}

	if rows := actionsIn(t, h.meta, "share.expired"); len(rows) != 1 {
		t.Fatalf("an expired link was used and wrote %d share.expired rows, want 1. The owner "+
			"cannot tell 'my link expired' from 'somebody revoked it' without this.", len(rows))
	}
	if rows := actionsIn(t, h.meta, "share.limit_reached"); len(rows) != 0 {
		t.Fatalf("expiry was recorded as an exhausted allowance: %v", rows)
	}
}

// TestExhaustedShareLimitIsRecorded: the other reason a link stops working.
func TestExhaustedShareLimitIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.uploadFile(t, "shared.txt", "bytes")
	create := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"shared.txt","kind":"link","max_downloads":1}`),
		map[string]string{"Content-Type": "application/json"})
	payload := readBody(create)
	create.Body.Close()
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create share: %d %s", create.StatusCode, payload)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(payload), &created); err != nil || created.Token == "" {
		t.Fatalf("no token in %s", payload)
	}
	token := created.Token

	first := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("the first download was refused: %d", first.StatusCode)
	}

	second := h.do(http.MethodGet, "/s/"+token+"/download", nil, nil)
	code := second.StatusCode
	second.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("the second download with a one-download link returned %d", code)
	}

	rows := actionsIn(t, h.meta, "share.limit_reached")
	if len(rows) != 1 {
		t.Fatalf("an exhausted link was used and wrote %d share.limit_reached rows, want 1", len(rows))
	}
	if rows := actionsIn(t, h.meta, "share.expired"); len(rows) != 0 {
		t.Fatalf("an exhausted allowance was recorded as an expiry: %v", rows)
	}
}
