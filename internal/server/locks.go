package server

// Cross-interface write arbitration (§5.3).
//
// The requirement is explicit: "网页/API 和 WebDAV 必须进入相同的写入仲裁层" and "Lock 的语义需适用于
// 跨接口操作；只有服务器内部可理解的锁不算完整兼容".
//
// Before this file the rule was violated in one direction only, which is the worst kind of
// half-implemented guarantee: WebDAV honoured locks, and the JSON API did not. A browser could
// overwrite a file a WebDAV client had locked, and the locking client would never know — it
// would write its own copy afterwards and silently lose the browser's edit, or the reverse.
// A lock that only one of two write paths respects is not a lock; it is a false promise to
// whichever client bothered to take one.
//
// So every mutating JSON handler now asks the same question the DAV handler asks, through the
// same store operation (`ActiveLock`). The two paths cannot drift because the decision is one
// function.

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// lockBlocksWrite reports whether a live lock prevents this actor writing the node.
//
// It mirrors dav.Handler.lockBlocks deliberately, including the two rules that make a lock
// meaningful rather than advisory:
//
//   - the holder may write through its own lock, but only by submitting the token. Accepting
//     the holder's write without the token would let a client believe a file is protected while
//     a stale tab overwrites it;
//   - anyone else is blocked regardless of what they submit.
func (s *Server) lockBlocksWrite(ctx context.Context, actor authz.Actor,
	nodeID uuid.UUID, lockToken string) (bool, error) {
	// anyDepth is true here: a lock ON this node covers this node content whatever its Depth.
	// Depth only distinguishes ancestor locks (see lockBlocksAncestors).
	return s.lockBlocksAtDepth(ctx, actor, nodeID, lockToken, true)
}

// lockBlocksAncestors reports whether a lock on an ancestor blocks a membership change.
//
// This is the API-side mirror of dav.lockAncestors. It differs from lockBlocksWrite in the same
// way MKCOL differs from PUT: adding or removing a child changes the parent's membership, so a
// Depth: 0 lock on the parent is enough to block it, while changing an existing child's content
// needs a Depth: infinity lock to reach down.
//
// Before this existed the API had no ancestor walk at all: a client could lock a folder and a
// browser could still upload a new file into it, which on the DAV side is refused. §5.3 asks for
// one arbitration layer, so the two must agree on this too.
func (s *Server) lockBlocksAncestors(ctx context.Context, actor authz.Actor,
	nodeID uuid.UUID, lockToken string) (bool, error) {

	// Check the node passed in before walking up. The caller passes the *parent collection*, and a
	// lock on that very collection is the case this function exists for; starting the loop with a
	// move to its parent would skip it entirely.
	blocked, err := s.lockBlocksAtDepth(ctx, actor, nodeID, lockToken, true)
	if err != nil {
		return false, err
	}
	if blocked {
		return true, nil
	}

	seen := 0
	for {
		seen++
		if seen > 256 {
			// A cycle or an absurdly deep tree. Refusing to loop forever is the right answer;
			// treating it as "no lock" would make a malformed path a way past a lock.
			return true, nil
		}

		var parent *uuid.UUID
		err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, nerr := tx.Node(ctx, nodeID)
			if nerr != nil {
				if nerr == store.ErrNotFound {
					return nil
				}
				return nerr
			}
			parent = n.ParentID
			return nil
		})
		if err != nil {
			return false, err
		}
		if parent == nil {
			return false, nil
		}
		nodeID = *parent

		blocked, berr := s.lockBlocksAtDepth(ctx, actor, nodeID, lockToken, true)
		if berr != nil {
			return false, berr
		}
		if blocked {
			return true, nil
		}
	}
}

// lockBlocksAtDepth is the shared body: anyDepth says whether a Depth: 0 lock on this node
// counts, matching the RFC distinction between membership and content.
func (s *Server) lockBlocksAtDepth(ctx context.Context, actor authz.Actor,
	nodeID uuid.UUID, lockToken string, anyDepth bool) (bool, error) {

	var (
		lock  store.Lock
		found bool
	)
	err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		l, lerr := tx.ActiveLock(ctx, nodeID)
		if lerr != nil {
			// No live lock is the common case and must not be an error.
			if lerr == store.ErrNotFound {
				return nil
			}
			return lerr
		}
		lock, found = l, true
		return nil
	})
	if err != nil || !found {
		return false, err
	}

	// A Depth: 0 lock protects membership, not the content of a child. Only Depth: infinity
	// reaches into the subtree (RFC 4918 §7.4).
	if !anyDepth && lock.Depth != "infinity" {
		return false, nil
	}

	if lock.UserID != nil && *lock.UserID == actor.ID() {
		if lockToken != "" && strings.TrimSpace(lockToken) == lock.Token {
			return false, nil
		}
	}
	return true, nil
}

// guardUnlocked answers 423 when a lock blocks the write, and reports whether the caller may
// proceed.
//
// The token is taken from the `If` header when present (the DAV convention) and otherwise from
// an `X-Lock-Token` header, so a browser client can present a lock it took through the API. Both
// are checked because the same file may be reached either way.
func (s *Server) guardUnlocked(w http.ResponseWriter, r *http.Request,
	actor authz.Actor, nodeID uuid.UUID) bool {

	token := submittedLockToken(r)
	blocked, err := s.lockBlocksWrite(r.Context(), actor, nodeID, token)
	if err != nil {
		s.writeStoreError(w, err)
		return false
	}
	if blocked {
		// 423 rather than 409: the file is not in a conflicting state, it is locked, and the
		// client needs to be told which. Windows and Office render 423 as "in use by another
		// user" instead of a generic failure, and the JSON API's clients get an error code they
		// can act on.
		writeError(w, http.StatusLocked, "locked",
			"the file is locked; submit its lock token to write")
		return false
	}
	return true
}

// submittedLockToken extracts a lock token from a request.
//
// The `If` header is the WebDAV spelling: a list of `<token>` entries, possibly with an
// `(<token>)` wrapper. The brackets are stripped so a caller can pass either form.
func submittedLockToken(r *http.Request) string {
	if f := r.Header.Get("If"); f != "" {
		for _, tok := range strings.Fields(f) {
			tok = strings.Trim(tok, "<>()")
			if strings.HasPrefix(tok, "opaquelocktoken:") || strings.HasPrefix(tok, "urn:") {
				return tok
			}
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Lock-Token"))
}

// guardParentUnlocked answers 423 when a lock on the parent collection blocks a membership
// change (creating or removing a child).
//
// Separate from guardUnlocked because the question differs: this one is about the parent's
// membership, so a Depth: 0 lock counts.
func (s *Server) guardParentUnlocked(w http.ResponseWriter, r *http.Request,
	actor authz.Actor, parentID uuid.UUID) bool {

	blocked, err := s.lockBlocksAncestors(r.Context(), actor, parentID, submittedLockToken(r))
	if err != nil {
		s.writeStoreError(w, err)
		return false
	}
	if blocked {
		writeError(w, http.StatusLocked, "locked",
			"the parent collection is locked; submit its lock token to add or remove entries")
		return false
	}
	return true
}

// guardAncestorContentUnlocked answers 423 when a Depth: infinity lock on an ancestor covers
// this node's content.
//
// Distinct from guardParentUnlocked: that one asks about the parent's membership and counts a
// Depth: 0 lock; this one asks about the node's content and must ignore Depth: 0, or a client
// that locked a folder to freeze its contents could no longer edit files inside it.
func (s *Server) guardAncestorContentUnlocked(w http.ResponseWriter, r *http.Request,
	actor authz.Actor, nodeID uuid.UUID) bool {

	blocked, err := s.ancestorLockInfinity(r.Context(), actor, nodeID, submittedLockToken(r))
	if err != nil {
		s.writeStoreError(w, err)
		return false
	}
	if blocked {
		writeError(w, http.StatusLocked, "locked",
			"an ancestor directory is locked with Depth: infinity")
		return false
	}
	return true
}

// ancestorLockInfinity walks up from nodeID's parent looking for a Depth: infinity lock.
func (s *Server) ancestorLockInfinity(ctx context.Context, actor authz.Actor,
	nodeID uuid.UUID, lockToken string) (bool, error) {

	current := nodeID
	for seen := 0; seen < 256; seen++ {
		var parent *uuid.UUID
		err := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			n, nerr := tx.Node(ctx, current)
			if nerr != nil {
				if nerr == store.ErrNotFound {
					return nil
				}
				return nerr
			}
			parent = n.ParentID
			return nil
		})
		if err != nil {
			return false, err
		}
		if parent == nil {
			return false, nil
		}
		current = *parent

		blocked, berr := s.lockBlocksAtDepth(ctx, actor, current, lockToken, false)
		if berr != nil {
			return false, berr
		}
		if blocked {
			return true, nil
		}
	}
	return true, nil
}
