package server

// Recursive permission checks (§4.2).
//
// The requirement is specific: a directory-recursive operation must not check only the root
// and ignore an inner node that carries a more restrictive grant. Every recursive handler —
// trash, archive, move, copy — needs the same answer, so it is one helper rather than four
// slightly different walks.
//
// Two decisions worth stating:
//
//   - An *incomplete* walk is a refusal. If the subtree is larger than the walk bound, the
//     node that was not inspected may be the protected one, and approving the part that was
//     seen would be the exact failure the rule exists to prevent.
//   - The check is per node against the authorizer, not against the root's capability. A
//     grant that covers a folder does not automatically cover a child whose own grant is
//     narrower, which is what `inherit=false` means.

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// subtreeCheckLimit bounds how many descendants are inspected.
//
// It is generous for a folder tree and small enough that one request cannot pin a
// connection while walking a million rows. A subtree beyond it is refused rather than
// partially approved.
const subtreeCheckLimit = 10_000

// checkSubtreePerm reports whether the actor holds perm on every node below rootID.
//
// A store error is treated as a refusal: an operation that cannot verify the subtree must
// not proceed on the assumption that it would have been allowed.
func (s *Server) checkSubtreePerm(r *http.Request, actor authz.Actor,
	rootID uuid.UUID, perm authz.Perm) bool {
	return s.checkSubtreePermCtx(r.Context(), actor, rootID, perm)
}

// checkSubtreePermCtx is the same answer for a caller that has only a context.
//
// The transfer helpers are reached both from a handler and from a batch loop that has already
// flattened N requests into one; threading a *http.Request through them only to reach its context
// would suggest the request were the input when the actor and the tree are.
func (s *Server) checkSubtreePermCtx(ctx context.Context, actor authz.Actor,
	rootID uuid.UUID, perm authz.Perm) bool {

	var (
		nodes    []store.Node
		complete bool
		err      error
	)
	walkErr := s.deps.Meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		nodes, complete, err = tx.Descendants(ctx, rootID, subtreeCheckLimit)
		return err
	})
	if walkErr != nil {
		s.deps.Log.Warn("subtree walk failed; refusing the recursive operation",
			"root", rootID.String(), "error", walkErr)
		return false
	}

	if !complete {
		// The requirement's intent is that nothing is deleted or moved without its
		// permission having been checked. A truncated walk cannot promise that.
		s.deps.Log.Warn("subtree too large to verify; refusing the recursive operation",
			"root", rootID.String(), "limit", subtreeCheckLimit)
		return false
	}

	for _, n := range nodes {
		cap, aerr := s.deps.Authz.Authorize(actor, n.ID)
		if aerr != nil {
			return false
		}
		if !cap.Can(perm) {
			return false
		}
	}
	return true
}
