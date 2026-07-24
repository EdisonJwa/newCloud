package dav

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// lockRequest is the LOCK body.
type lockRequest struct {
	XMLName xml.Name `xml:"lockinfo"`
	Scope   struct {
		Exclusive *struct{} `xml:"exclusive"`
		Shared    *struct{} `xml:"shared"`
	} `xml:"lockscope"`
	Type struct {
		Write *struct{} `xml:"write"`
	} `xml:"locktype"`
	OwnerRaw string `xml:"owner"`
}

// handleLock creates or refreshes a lock.
//
// Locks are durable rows, not process memory. The stdlib's in-memory lock system
// breaks under a restart or a second process, and a broken lock is how Office
// co-authoring silently overwrites someone else's edits.
func (h *Handler) handleLock(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	ctx := r.Context()

	cap, node, err := h.resolvePath(ctx, a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	timeout := h.requestedTimeout(r)
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "infinity"
	}
	if depth != "0" && depth != "infinity" {
		writeXMLError(w, http.StatusBadRequest, "invalid-depth")
		return
	}

	// A refresh is a LOCK with an If header and no body.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}

	refreshToken := lockTokenFromIf(r.Header.Get("If"))
	if len(strings.TrimSpace(string(body))) == 0 && refreshToken != "" {
		tok, expires, ok := h.refreshLock(ctx, node.ID, refreshToken, a.ID(), timeout)
		if !ok {
			writeXMLError(w, http.StatusPreconditionFailed, "lock-token-matches-request-uri")
			return
		}
		h.writeLockResponse(w, http.StatusOK, tok, node, expires)
		return
	}

	var req lockRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}

	// An existing lock held by someone else blocks a new exclusive lock. The
	// database enforces this too, but failing here gives the correct status code.
	if existing, ok := h.activeLock(ctx, node.ID); ok {
		writeXMLError(w, http.StatusLocked, "no-conflicting-lock")
		_ = existing
		return
	}

	token := newLockToken()
	expires := h.now().Add(timeout)
	err = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if lerr := tx.CreateLock(ctx, store.Lock{
			Token: token, NodeID: node.ID, UserID: a.Ptr(),
			OwnerXML: req.OwnerRaw, Depth: depth,
			TimeoutSec: int(timeout.Seconds()), ExpiresAt: expires,
		}); lerr != nil {
			return lerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.lock", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok", Metadata: map[string]any{"depth": depth},
		})
	})
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}

	h.writeLockResponse(w, http.StatusOK, token, node, expires)
}

// handleUnlock removes a lock.
func (h *Handler) handleUnlock(w http.ResponseWriter, r *http.Request, a authz.Actor) {
	cap, node, err := h.resolvePath(r.Context(), a, r.URL.Path)
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !cap.Can(authz.PermWrite) {
		writeXMLError(w, http.StatusNotFound, "")
		return
	}

	token := strings.Trim(r.Header.Get("Lock-Token"), "<>")
	if token == "" {
		writeXMLError(w, http.StatusBadRequest, "")
		return
	}

	removed := false
	err = h.meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		ok, lerr := tx.DeleteLock(ctx, node.ID, token, a.ID())
		if lerr != nil {
			return lerr
		}
		removed = ok
		if !ok {
			return nil
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: a.Ptr(), ActorEmail: a.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "file.unlock", TargetKind: "node", TargetID: node.ID.String(),
			Result: "ok",
		})
	})
	if err != nil {
		writeXMLError(w, statusForError(err), "")
		return
	}
	if !removed {
		// The lock is not ours (or is gone). RFC says 409, and clients handle it.
		writeXMLError(w, http.StatusConflict, "lock-token-matches-request-uri")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeLockResponse(w http.ResponseWriter, status int, token string, n store.Node, expires time.Time) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Lock-Token", "<"+token+">")
	w.WriteHeader(status)

	secs := int(time.Until(expires).Seconds())
	if secs < 0 {
		secs = 0
	}
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock>
<D:locktype><D:write/></D:locktype>
<D:lockscope><D:exclusive/></D:lockscope>
<D:depth>%s</D:depth>
<D:timeout>Second-%d</D:timeout>
<D:locktoken><D:href>%s</D:href></D:locktoken>
</D:activelock></D:lockdiscovery></D:prop>`,
		"infinity", secs, xmlEscape(token))
}

// requestedTimeout parses the client's Timeout header.
//
// It is clamped: a client asking for Infinite would pin a row forever, and a
// client asking for one second would lose its lock mid-edit.
func (h *Handler) requestedTimeout(r *http.Request) time.Duration {
	const def = 30 * time.Minute
	const maxTTL = 24 * time.Hour

	raw := r.Header.Get("Timeout")
	if raw == "" {
		return def
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if strings.EqualFold(part, "Infinite") {
			return maxTTL
		}
		if secs, ok := strings.CutPrefix(part, "Second-"); ok {
			n, err := strconv.Atoi(secs)
			if err != nil || n <= 0 {
				continue
			}
			d := time.Duration(n) * time.Second
			if d > maxTTL {
				return maxTTL
			}
			return d
		}
	}
	return def
}

// lockTokenFromIf extracts a token from an If header.
//
// The header is a small grammar; the only form that matters here is the
// "(<opaquelocktoken:...>)" wrapper clients send on refresh.
func lockTokenFromIf(ifHeader string) string {
	start := strings.Index(ifHeader, "<")
	end := strings.Index(ifHeader, ">")
	if start < 0 || end <= start+1 {
		return ""
	}
	return ifHeader[start+1 : end]
}

// newLockToken mints an opaquelocktoken as required by RFC 4918.
func newLockToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Locking without randomness would produce guessable tokens, which is
		// worse than failing: a guessed token lets anyone steal a lock.
		panic("dav: crypto/rand failed: " + err.Error())
	}
	return "opaquelocktoken:" + base64.RawURLEncoding.EncodeToString(b[:])
}
