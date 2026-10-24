package consistency

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/fence"
)

type fakeLive struct {
	owner       uuid.UUID
	node        uuid.UUID
	resolveNode uuid.UUID
	ancestors   []uuid.UUID
	resolveAnc  []uuid.UUID
	mu          sync.Mutex
	opens       int
	authOK      bool
	notVisible  bool
	openStarted chan struct{}
	allowOpen   chan struct{}
}

func (f *fakeLive) PlanLive(ctx context.Context, path string) (LivePlan, error) {
	intents := []fence.Intent{fence.Read(fence.NodeKey(f.owner, f.node))}
	for _, a := range f.ancestors {
		intents = append(intents, fence.Read(fence.NodeKey(f.owner, a)))
	}
	return LivePlan{Intents: intents, ExpectedNodeID: f.node, ExpectedAncestors: f.ancestors}, nil
}

func (f *fakeLive) ResolveLive(ctx context.Context, path string) (LiveVersion, error) {
	n := f.node
	if f.resolveNode != uuid.Nil {
		n = f.resolveNode
	}
	anc := f.ancestors
	if f.resolveAnc != nil {
		anc = f.resolveAnc
	}
	return LiveVersion{NodeID: n, AncestorIDs: anc, ExternallyVisible: !f.notVisible,
		Revision: 7, ETag: `"v7"`, Size: 3}, nil
}

func (f *fakeLive) AuthorizeLive(ctx context.Context, v LiveVersion) error {
	if !f.authOK {
		return errors.New("denied")
	}
	return nil
}

func (f *fakeLive) OpenLive(ctx context.Context, v LiveVersion) (io.ReadSeekCloser, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	if f.openStarted != nil {
		close(f.openStarted)
	}
	if f.allowOpen != nil {
		select {
		case <-f.allowOpen:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &memReadSeekCloser{Reader: bytes.NewReader([]byte("abc"))}, nil
}

type memReadSeekCloser struct{ *bytes.Reader }

func (m *memReadSeekCloser) Close() error { return nil }

func TestOpenLivePinsVersionAndHoldsFenceThroughOpen(t *testing.T) {
	owner, node := uuid.New(), uuid.New()
	f := &fakeLive{owner: owner, node: node, authOK: true,
		openStarted: make(chan struct{}), allowOpen: make(chan struct{})}
	l := fence.New()
	ctx := context.Background()

	opened := make(chan *LiveHandle, 1)
	go func() {
		h, err := OpenLive(ctx, l, f, f, "file.txt")
		if err != nil {
			t.Errorf("OpenLive: %v", err)
			return
		}
		opened <- h
	}()

	// The fake blocks inside OpenLive after the shared lock is held. This is a
	// deterministic boundary, not a sleep: the writer is attempted only after the
	// open has started.
	select {
	case <-f.openStarted:
	case <-time.After(time.Second):
		t.Fatal("OpenLive never reached the open")
	}
	ctxWriter, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctxWriter, fence.Write(fence.NodeKey(owner, node))); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer acquired while OpenLive held the fence: %v", err)
	}

	close(f.allowOpen)
	h := <-opened
	defer h.Content.Close()
	if h.Version.Revision != 7 || h.Version.ETag != `"v7"` || h.Version.Size != 3 {
		t.Fatalf("pinned version = %+v", h.Version)
	}
	body, _ := io.ReadAll(h.Content)
	if string(body) != "abc" {
		t.Fatalf("content = %q", body)
	}

	// After OpenLive returns, the fence must be released.
	rel, err := l.Acquire(ctx, fence.Write(fence.NodeKey(owner, node)))
	if err != nil {
		t.Fatalf("writer after open: %v", err)
	}
	rel()
}

func TestOpenLiveRefusesAuthzAndTrashed(t *testing.T) {
	owner, node := uuid.New(), uuid.New()
	f := &fakeLive{owner: owner, node: node, authOK: false}
	l := fence.New()
	if _, err := OpenLive(context.Background(), l, f, f, "file.txt"); err == nil {
		t.Fatal("unauthorized open succeeded")
	}
	// A denied open must release the fence.
	rel, err := l.Acquire(context.Background(), fence.Write(fence.NodeKey(owner, node)))
	if err != nil {
		t.Fatalf("fence leaked after denial: %v", err)
	}
	rel()
}

// R12-002: a stale Plan must not open the node that resolution returned under
// different locks. The helper must refuse or re-plan, never call OpenLive.
func TestOpenLiveRejectsAStalePlanIdentity(t *testing.T) {
	owner, planned, actual := uuid.New(), uuid.New(), uuid.New()
	f := &fakeLive{owner: owner, node: planned, resolveNode: actual, authOK: true}
	l := fence.New()
	// A writer already holds the real node exclusively. If OpenLive ignored the
	// plan mismatch it would open B without contending on B's write fence.
	held, err := l.Acquire(context.Background(), fence.Write(fence.NodeKey(owner, actual)))
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	_, err = OpenLive(context.Background(), l, f, f, "path")
	if !errors.Is(err, ErrLivePlanStale) {
		t.Fatalf("stale plan = %v, want ErrLivePlanStale", err)
	}
	if f.opens != 0 {
		t.Fatalf("OpenLive opened a node outside its held fence (%d opens)", f.opens)
	}
}

func TestOpenLiveRejectsAnAncestorChainChange(t *testing.T) {
	owner, node, oldParent, newParent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	f := &fakeLive{
		owner: owner, node: node, ancestors: []uuid.UUID{oldParent},
		resolveAnc: []uuid.UUID{newParent}, authOK: true,
	}
	l := fence.New()
	_, err := OpenLive(context.Background(), l, f, f, "path")
	if !errors.Is(err, ErrLivePlanStale) {
		t.Fatalf("reparented path = %v, want ErrLivePlanStale", err)
	}
	if f.opens != 0 {
		t.Fatalf("OpenLive opened after an ancestor change (%d opens)", f.opens)
	}
}

func TestOpenLiveRejectsEffectiveVisibilityFalse(t *testing.T) {
	owner, node := uuid.New(), uuid.New()
	f := &fakeLive{owner: owner, node: node, authOK: true, notVisible: true}
	l := fence.New()
	_, err := OpenLive(context.Background(), l, f, f, "path")
	if !errors.Is(err, ErrLiveUnavailable) {
		t.Fatalf("not-visible node = %v, want ErrLiveUnavailable", err)
	}
	if f.opens != 0 {
		t.Fatalf("OpenLive opened a not-visible node (%d opens)", f.opens)
	}
}
