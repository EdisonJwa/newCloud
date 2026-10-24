package consistency

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/fence"
)

// LiveVersion is the metadata pinned for one content open. It deliberately
// carries revision and ETag separately: headers use ETag, conditional writes use
// revision, and a reader must not mix a newer ETag with older bytes.
type LiveVersion struct {
	NodeID uuid.UUID
	// AncestorIDs is the complete parent chain from the user root to the direct
	// parent. OpenLive uses it to prove the fence plan actually covers the node
	// that resolution returned, not just the node it saw when planning.
	AncestorIDs []uuid.UUID
	// ExternallyVisible is the resolver's EffectiveExternalVisibility result. It
	// must be true for a live read; a caller cannot assume it from Trashed alone.
	ExternallyVisible bool
	Revision          Revision
	ETag              string
	Size              int64
	ModTime           time.Time
	Trashed           bool
	Quarantined       bool
}

// LivePlan is the fence the resolver says this path needs, plus the identity it
// expects to resolve. A rename/reparent between planning and resolution makes the
// plan stale; OpenLive must discard it and re-plan rather than open another node
// under the wrong locks.
type LivePlan struct {
	Intents           []fence.Intent
	ExpectedNodeID    uuid.UUID
	ExpectedAncestors []uuid.UUID
}

// LiveResolver resolves and authorizes a live path. ResolveLive runs while the
// shared fence is held; AuthorizeLive must re-check the pinned identity.
type LiveResolver interface {
	PlanLive(ctx context.Context, path string) (LivePlan, error)
	ResolveLive(ctx context.Context, path string) (LiveVersion, error)
	AuthorizeLive(ctx context.Context, v LiveVersion) error
}

// LiveOpener opens the pinned content. It must not resolve the path again: the
// handle is the content authority after the fence is released.
type LiveOpener interface {
	OpenLive(ctx context.Context, v LiveVersion) (io.ReadSeekCloser, error)
}

// LiveHandle is an opened, pinned live resource.
type LiveHandle struct {
	Content io.ReadSeekCloser
	Version LiveVersion
}

// ErrLiveUnavailable means the resource is not externally visible or failed a
// pre-open check. It is intentionally coarse: a caller must not turn it into an
// oracle for staged/quarantined node existence.
var ErrLiveUnavailable = errors.New("consistency: live resource unavailable")

// ErrLivePlanStale means repeated Plan/Resolve attempts kept disagreeing, so no
// handle was opened. This is a safe refusal, not a retryable success.
var ErrLivePlanStale = errors.New("consistency: live fence plan could not cover the resolved node")

const openLiveAttempts = 3

// OpenLive implements the read-side publication fence (ADR-0013 §4.5):
//
//  1. resolve the fence plan and expected identity;
//  2. acquire the shared path/ancestor intents;
//  3. resolve the authoritative version and prove the held keys cover its node
//     and complete ancestor chain; a rename/reparent makes the plan stale;
//  4. authorize and open the content, releasing the fence after the handle exists.
//
// The caller streams from Content and uses Version for headers/range checks. It
// must not re-resolve the path for the rest of the request.
func OpenLive(ctx context.Context, f *fence.Locker, r LiveResolver, o LiveOpener, path string) (*LiveHandle, error) {
	for attempt := 0; attempt < openLiveAttempts; attempt++ {
		plan, err := r.PlanLive(ctx, path)
		if err != nil {
			return nil, err
		}
		if plan.ExpectedNodeID == uuid.Nil {
			return nil, fmt.Errorf("%w: no expected node identity", ErrLivePlanStale)
		}
		release, err := f.Acquire(ctx, plan.Intents...)
		if err != nil {
			return nil, err
		}
		v, err := r.ResolveLive(ctx, path)
		if err != nil {
			release()
			return nil, err
		}
		if !planCovers(plan, v) {
			release()
			continue
		}
		if !v.ExternallyVisible || v.Trashed || v.Quarantined {
			release()
			return nil, ErrLiveUnavailable
		}
		if err := r.AuthorizeLive(ctx, v); err != nil {
			release()
			return nil, err
		}
		content, err := o.OpenLive(ctx, v)
		if err != nil {
			release()
			return nil, err
		}
		release()
		return &LiveHandle{Content: content, Version: v}, nil
	}
	return nil, ErrLivePlanStale
}

// planCovers proves the plan's expected identity and every ancestor intent match
// the version actually resolved under the lock. Node identity alone is not
// enough: a directory MOVE/reparent can keep a node ID while changing ancestors.
func planCovers(plan LivePlan, v LiveVersion) bool {
	if plan.ExpectedNodeID != v.NodeID {
		return false
	}
	if len(plan.ExpectedAncestors) != len(v.AncestorIDs) {
		return false
	}
	for i := range v.AncestorIDs {
		if plan.ExpectedAncestors[i] != v.AncestorIDs[i] {
			return false
		}
	}
	held := make(map[uuid.UUID]bool, len(plan.Intents))
	for _, in := range plan.Intents {
		if in.Key.Kind == fence.Node {
			held[in.Key.NodeID] = true
		}
	}
	if !held[v.NodeID] {
		return false
	}
	for _, anc := range v.AncestorIDs {
		if !held[anc] {
			return false
		}
	}
	return true
}
