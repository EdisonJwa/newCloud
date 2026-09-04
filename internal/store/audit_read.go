package store

// Audit read surface on the in-memory store.
//
// An important limitation, stated rather than papered over: the in-memory store records
// audit *entries*, not a sealed hash chain. Only the PostgreSQL store computes prev_hash
// and a per-row hash (see store.PG.AuditRows). That means the in-memory store cannot
// support anchoring in any meaningful sense — it can hand back rows, but there is no
// chain head to publish.
//
// Rather than fabricate a head by hashing whatever it has (which would produce an anchor
// that verifies against itself and proves nothing), the in-memory store reports the
// absence honestly: AuditHead returns an error naming the limitation. Anchoring then
// fails loudly in a development deployment that asked for it, instead of silently
// claiming tamper-evidence it does not have.

import (
	"context"
	"errors"

	"github.com/EdisonJwa/newCloud/internal/audit"
)

// ErrNoChain is returned by a store that does not maintain a sealed hash chain.
var ErrNoChain = errors.New("store: this backend does not maintain a sealed audit hash chain")

// AuditRows returns the recorded entries, oldest first.
//
// The rows carry no prev_hash or hash: the memory backend does not compute them. They are
// still useful for the admin export and for tests that assert "this action was audited",
// which is why this method exists at all.
func (m *Memory) AuditRows(ctx context.Context, afterID int64, limit int) ([]audit.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.isBroken() {
		return nil, ErrUnavailable
	}
	if limit <= 0 {
		limit = 100
	}

	// Every attribution field the entry carries is mapped through.
	//
	// Four were missing: ActorUserID, UserAgent, ShareID and Bytes. The first is the serious one —
	// the audit trail's central question is "who did this", and the answer was an email string that
	// an account can change, with the immutable user id dropped. A share-mediated action also lost
	// which share carried it, so the same event was attributable on PostgreSQL and not on the
	// in-memory backend, which is the backend the tests run against: the gap could not be caught by
	// any test that used it.
	out := make([]audit.Event, 0, limit)
	for i, e := range m.audit {
		id := int64(i + 1)
		if id <= afterID {
			continue
		}
		out = append(out, audit.Event{
			ID:          id,
			Time:        m.auditTimes[i],
			ActorKind:   audit.ActorKind(e.ActorKind),
			ActorUserID: e.ActorUserID,
			ActorEmail:  e.ActorEmail,
			ActorIP:     e.ActorIP,
			UserAgent:   e.UserAgent,
			Action:      audit.Action(e.Action),
			TargetKind:  audit.TargetKind(e.TargetKind),
			TargetID:    e.TargetID,
			ShareID:     e.ShareID,
			Result:      audit.Result(e.Result),
			Bytes:       e.Bytes,
			Metadata:    e.Metadata,
			// Hash and PrevHash are intentionally zero: this backend does not seal, and
			// inventing them here would make verification meaningless.
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// AuditHead reports that the memory backend has no sealed chain.
func (m *Memory) AuditHead(ctx context.Context) ([]byte, error) {
	return nil, ErrNoChain
}

// AuditCount returns how many entries were recorded.
func (m *Memory) AuditCount(ctx context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.audit)), nil
}

// ChainCapable reports whether this backend maintains a real hash chain.
//
// Callers use it to decide whether anchoring is even possible, rather than discovering
// it from a failed publish later.
func (m *Memory) ChainCapable() bool { return false }
