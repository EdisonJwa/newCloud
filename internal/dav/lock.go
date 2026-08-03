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

	"github.com/google/uuid"

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

// submittedTokens returns every lock token named in the request's If header.
//
// A request may submit several tagged lists, one per resource. Ownership is
// decided by matching any of them against the lock on the target, which is what
// RFC 4918 §10.4 describes; treating the header as a single opaque string (as an
// earlier version did) means a client that tags its lists differently gets a
// spurious 423 on its own lock.
func submittedTokens(ifHeader string) []string {
	var out []string
	for {
		start := strings.IndexByte(ifHeader, '<')
		if start < 0 {
			return out
		}
		rest := ifHeader[start+1:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return out
		}
		tok := strings.TrimSpace(rest[:end])
		if tok != "" {
			out = append(out, tok)
		}
		ifHeader = rest[end+1:]
	}
}

// lockBlocks reports whether a live lock prevents this actor from writing nodeID.
//
// It is the one place that answers "is this write allowed past the lock", called
// from every mutating method. Before this existed, locks were enforced only in the
// LOCK handler itself: a client could take a lock, and then any other writer —
// including the same client through a different entry point — would overwrite the
// file without ever being told the resource was locked. That is precisely the
// silent-corruption case RFC 4918 §6 exists to prevent.
func (h *Handler) lockBlocks(ctx context.Context, a authz.Actor, nodeID [16]byte, ifHeader string) bool {
	return h.lockBlocksAtDepth(ctx, a, nodeID, ifHeader, true)
}

// lockBlocksAtDepth is lockBlocks with an explicit answer to "does a Depth: 0 lock count here".
//
// anyDepth=false is used on the ancestor walk for content writes, where only a Depth: infinity
// lock covers the child. anyDepth=true is used for the node itself, where the lock covers its
// own content regardless of depth: a Depth: 0 lock on a file still protects that file's bytes.
func (h *Handler) lockBlocksAtDepth(ctx context.Context, a authz.Actor, nodeID [16]byte,
	ifHeader string, anyDepth bool) bool {
	lock, held := h.activeLock(ctx, nodeID)
	if !held {
		return false
	}
	// A Depth: 0 lock on an ancestor protects membership, not this child's content. Only
	// Depth: infinity reaches down into the subtree (RFC 4918 §7.4).
	if !anyDepth && lock.Depth != "infinity" {
		return false
	}
	// The holder may write through its own lock, and only by submitting the
	// token. Accepting the write without the token would make the lock advisory,
	// which is worse than no lock: the client believes it is protected.
	if lock.UserID != nil && *lock.UserID == a.ID() {
		for _, tok := range submittedTokens(ifHeader) {
			if tok == lock.Token {
				return false
			}
		}
	}
	return true
}

// requireUnlocked writes the 423 response when a lock blocks the write.
//
// 423 Locked with a DAV:lock-token-submitted body is what makes Windows and
// Office show "the file is in use by another user" instead of a generic failure.
func (h *Handler) requireUnlocked(w http.ResponseWriter, ctx context.Context, a authz.Actor,
	nodeID [16]byte, ifHeader string) bool {

	if h.lockBlocks(ctx, a, nodeID, ifHeader) {
		writeXMLError(w, http.StatusLocked, "lock-token-submitted")
		return false
	}
	return true
}

// lockAncestors reports whether a live lock on any ancestor blocks the write.
//
// Depth: infinity locks cover the whole subtree per RFC 4918 §9.10.3, so creating
// or replacing a child of a locked collection must be refused. Without this a
// client can lock a folder, and another client can still write into it.
func (h *Handler) lockAncestors(ctx context.Context, a authz.Actor, parentID [16]byte, ifHeader string) bool {
	return h.lockAncestorsDepth(ctx, a, parentID, ifHeader, true)
}

// lockAncestorsInfinityOnly is the same walk, restricted to Depth: infinity locks.
//
// The distinction is RFC 4918 §7.4 and it is not cosmetic. A Depth: 0 lock on a collection
// protects the collection's *membership* — you may not add or remove children — but it says
// nothing about the content of an existing child. A Depth: infinity lock covers the whole
// subtree, content included.
//
// The two call sites need different answers:
//
//   - PUT over an existing member changes that member's content. Only a Depth: infinity ancestor
//     lock blocks it. Using the membership rule here makes a client that locked a folder unable
//     to edit any file inside it, which is not what a Depth: 0 lock means and not what the
//     client asked for.
//   - MKCOL/DELETE change membership. A Depth: 0 parent lock blocks them, because that is
//     exactly the right it protects.
//
// Before this split one function answered both, so Depth was ignored entirely and a Depth: 0
// lock behaved like Depth: infinity on the ancestor path — over-blocking, and silently, because
// the client gets 423 with no indication that its own Depth: 0 lock is the cause.
func (h *Handler) lockAncestorsInfinityOnly(ctx context.Context, a authz.Actor, parentID [16]byte,
	ifHeader string) bool {
	return h.lockAncestorsDepth(ctx, a, parentID, ifHeader, false)
}

// lockAncestorsDepth walks the ancestor chain and reports whether a lock blocks.
//
// anyDepth reports whether a Depth: 0 lock on an ancestor counts. It is the parameter that makes
// the membership/content distinction explicit at every call site.
func (h *Handler) lockAncestorsDepth(ctx context.Context, a authz.Actor, parentID [16]byte,
	ifHeader string, anyDepth bool) bool {
	for depth := 0; depth < 256; depth++ {
		if h.lockBlocksAtDepth(ctx, a, parentID, ifHeader, anyDepth) {
			return true
		}
		var parent *uuid.UUID
		_ = h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, err := tx.Node(ctx, parentID)
			if err != nil {
				return nil
			}
			parent = n.ParentID
			return nil
		})
		if parent == nil {
			return false
		}
		parentID = *parent
	}
	return false
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
