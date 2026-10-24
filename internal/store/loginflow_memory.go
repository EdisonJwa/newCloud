package store

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// In-memory implementation of LoginFlowTx (ADR-0015).
//
// The map is keyed by flow id and looked up by token hash. The flows are a handful of
// rows with a twenty-minute life, so a scan is cheaper than maintaining two more indexes
// — and it cannot drift out of step with the rows, which the PostgreSQL indexes cannot
// either but which a second map here could.

func (t *memTx) CreateLoginFlow(ctx context.Context, f LoginFlow, pollHash, loginHash [32]byte) error {
	if _, exists := t.findLoginFlow(func(r *memLoginFlow) bool {
		return r.pollHash == pollHash || r.loginHash == loginHash
	}); exists {
		return ErrConflict
	}
	t.m.loginFlows[f.ID] = &memLoginFlow{flow: f, pollHash: pollHash, loginHash: loginHash}
	return nil
}

func (t *memTx) LoginFlowByLoginHash(ctx context.Context, loginHash [32]byte) (LoginFlow, error) {
	rec, ok := t.findLoginFlow(func(r *memLoginFlow) bool { return r.loginHash == loginHash })
	if !ok || !t.m.now().Before(rec.flow.ExpiresAt) {
		return LoginFlow{}, ErrNotFound
	}
	return rec.flow, nil
}

func (t *memTx) ApproveLoginFlow(ctx context.Context, loginHash [32]byte, userID uuid.UUID,
	at time.Time) (LoginFlow, error) {

	rec, ok := t.findLoginFlow(func(r *memLoginFlow) bool { return r.loginHash == loginHash })
	if !ok || !at.Before(rec.flow.ExpiresAt) {
		return LoginFlow{}, ErrNotFound
	}
	if rec.flow.UserID != nil {
		if *rec.flow.UserID != userID {
			return LoginFlow{}, ErrLoginFlowConflict
		}
		return rec.flow, nil
	}
	rec.flow.UserID = &userID
	approved := at
	rec.flow.ApprovedAt = &approved
	return rec.flow, nil
}

func (t *memTx) ConsumeLoginFlow(ctx context.Context, pollHash [32]byte, at time.Time) (LoginFlow, error) {
	rec, ok := t.findLoginFlow(func(r *memLoginFlow) bool { return r.pollHash == pollHash })
	if !ok || !rec.flow.Approved() || !at.Before(rec.flow.ExpiresAt) {
		return LoginFlow{}, ErrNotFound
	}
	delete(t.m.loginFlows, rec.flow.ID)
	return rec.flow, nil
}

func (t *memTx) DeleteExpiredLoginFlows(ctx context.Context, now time.Time) (int, error) {
	var ids []uuid.UUID
	for id, rec := range t.m.loginFlows {
		if !now.Before(rec.flow.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		delete(t.m.loginFlows, id)
	}
	return len(ids), nil
}

// findLoginFlow returns the first record matching pred, deterministically.
//
// The map iteration order is random, so a predicate that could match two rows would make
// the behaviour depend on the run. "First" is therefore defined by creation order, which
// is also the order PostgreSQL's unique index would resolve to (there can only be one).
func (t *memTx) findLoginFlow(pred func(*memLoginFlow) bool) (*memLoginFlow, bool) {
	var matches []*memLoginFlow
	for _, rec := range t.m.loginFlows {
		if pred(rec) {
			matches = append(matches, rec)
		}
	}
	if len(matches) == 0 {
		return nil, false
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].flow.CreatedAt.Before(matches[j].flow.CreatedAt)
	})
	return matches[0], true
}
