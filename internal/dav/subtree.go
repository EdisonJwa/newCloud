package dav

// Recursive permission checks for WebDAV (§4.2).
//
// MOVE and COPY on a collection move or duplicate everything inside it, and DELETE on a
// collection trash the whole subtree. In each case the requirement is that the operation
// must not be approved on the strength of the root alone: an inner node may carry a narrower
// grant, and acting on it because its parent was permitted is privilege escalation by
// nesting.
//
// This mirrors the JSON API's check deliberately. The two surfaces have independent code
// paths, so a rule enforced in only one of them is a rule a WebDAV client can bypass.

import (
	"context"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/authz"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// subtreeWalkLimit bounds how many descendants are inspected.
//
// A subtree beyond it is refused rather than partially approved: the node that was not
// inspected may be the protected one, which is exactly the failure the rule prevents.
const subtreeWalkLimit = 10_000

// subtreeAllows reports whether the actor holds perm on every node below rootID.
//
// An error is returned rather than folded into `false` so the caller can distinguish "the
// store could not be read" (a 5xx) from "the permission was refused" (a 4xx).
func (h *Handler) subtreeAllows(ctx context.Context, a authz.Actor,
	rootID uuid.UUID, perm authz.Perm) (bool, error) {

	var (
		nodes    []store.Node
		complete bool
		walkErr  error
	)
	err := h.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		nodes, complete, walkErr = tx.Descendants(ctx, rootID, subtreeWalkLimit)
		return walkErr
	})
	if err != nil {
		return false, err
	}
	if !complete {
		h.logf("recursive operation refused: subtree larger than %d nodes root=%s",
			subtreeWalkLimit, rootID)
		return false, nil
	}

	for _, n := range nodes {
		cap, cerr := h.authz.Authorize(a, n.ID)
		if cerr != nil {
			return false, cerr
		}
		if !cap.Can(perm) {
			return false, nil
		}
	}
	return true, nil
}
