package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// In-memory audit read path.
//
// It reads the same `audit` slice the writer appends to, rather than a parallel
// structure: two copies of the audit log would be two things to keep in step, and the one
// that drifts is always the one nobody queries.
//
// The filter semantics match the SQL implementation so a test written against one store
// is meaningful for the other.

// ReadAudit returns events matching f.
func (m *Memory) ReadAudit(ctx context.Context, f AuditFilter) ([]AuditRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	scope := m.shareOwnersLocked(f.ScopeToOwner)

	out := make([]AuditRow, 0, len(m.audit))
	for i, e := range m.audit {
		row := memoryAuditRow(e, i, m.auditTimes[i])
		if matchesAudit(row, f, scope) {
			out = append(out, row)
		}
	}

	if f.Descending {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}

	// Offset before Limit: the two compose as "skip, then take", matching SQL.
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// CountAudit reports the number of matching events.
func (m *Memory) CountAudit(ctx context.Context, f AuditFilter) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	scope := m.shareOwnersLocked(f.ScopeToOwner)

	n := 0
	for i, e := range m.audit {
		if matchesAudit(memoryAuditRow(e, i, m.auditTimes[i]), f, scope) {
			n++
		}
	}
	return n, nil
}

// shareOwnersLocked builds the share-id-to-owner index the scope filter needs.
//
// It returns nil when no owner scope is requested, so the common case does no work. The
// caller must hold at least a read lock.
func (m *Memory) shareOwnersLocked(scope *uuid.UUID) map[string]string {
	if scope == nil {
		return nil
	}
	index := make(map[string]string, len(m.shares))
	for id, sh := range m.shares {
		index[id.String()] = sh.OwnerID.String()
	}
	return index
}

// memoryAuditRow projects the write-side entry onto the read-side shape.
//
// The timestamp comes from the parallel auditTimes slice because AuditEntry deliberately
// carries none: on the SQL side the column defaults to now() inside the same transaction,
// which is the honest single source of truth. Reproducing that here means stamping at
// append time rather than inventing a time during the read.
func memoryAuditRow(e AuditEntry, index int, at time.Time) AuditRow {
	return AuditRow{
		ID:          int64(index + 1),
		Time:        at,
		ActorKind:   e.ActorKind,
		ActorUserID: e.ActorUserID,
		ActorEmail:  e.ActorEmail,
		ActorIP:     e.ActorIP,
		UserAgent:   e.UserAgent,
		Action:      e.Action,
		TargetKind:  e.TargetKind,
		TargetID:    e.TargetID,
		ShareID:     e.ShareID,
		Result:      e.Result,
		Bytes:       e.Bytes,
		Metadata:    e.Metadata,
	}
}

// matchesAudit applies the filter to one row.
//
// shareOwners maps share id to owner id and may be nil when the filter does not scope by
// owner; passing it in avoids rebuilding the index per row.
func matchesAudit(r AuditRow, f AuditFilter, shareOwners map[string]string) bool {
	if f.Since != nil && r.Time.Before(*f.Since) {
		return false
	}
	if f.Until != nil && !r.Time.Before(*f.Until) {
		return false
	}
	if len(f.Actions) > 0 && !containsFold(f.Actions, r.Action) {
		return false
	}
	if f.ActorUserID != nil {
		if r.ActorUserID == nil || *r.ActorUserID != *f.ActorUserID {
			return false
		}
	}
	if f.ShareID != nil {
		if r.ShareID == nil || *r.ShareID != *f.ShareID {
			return false
		}
	}
	if f.Result != "" && r.Result != f.Result {
		return false
	}
	if f.ScopeToOwner != nil {
		// Mirror the SQL predicate: the caller either caused the event or owns the share
		// it is about. Keep the two in step, or a test against one store says nothing
		// about the other.
		owner := f.ScopeToOwner.String()
		caused := r.ActorUserID != nil && r.ActorUserID.String() == owner
		if !caused {
			if r.ShareID == nil || shareOwners[r.ShareID.String()] != owner {
				return false
			}
		}
	}
	return true
}

// containsFold reports whether list contains want, case-insensitively.
//
// Action names are lowercase everywhere, but a query string from a human is not: making
// the filter case-insensitive avoids "audit.export with no rows" being reported as an
// empty log.
func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

var _ AuditReader = (*Memory)(nil)
