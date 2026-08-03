package store

// In-memory implementation of account deletion.
//
// It follows the SQL step for step, including the parts that are easy to forget when writing a
// second implementation: releasing WebDAV locks, and removing grants the account *authored* as
// well as those naming it as subject.

import (
	"context"

	"github.com/google/uuid"
)

// DeleteUser applies the ID-08 policy.
func (t *memTx) DeleteUser(ctx context.Context, id uuid.UUID) (DeleteUserResult, error) {
	var res DeleteUserResult

	u, ok := t.m.users[id]
	if !ok {
		return res, ErrNotFound
	}
	if err := validateDeletable(u.Status); err != nil {
		return res, err
	}

	now := t.m.now()

	// 1. Files into the trash.
	for _, rec := range t.m.nodes {
		if rec.node.OwnerID != id || rec.node.TrashedAt != nil || rec.node.ParentID == nil {
			continue
		}
		trashed := now
		rec.node.TrashedAt = &trashed
		rec.node.ModifiedAt = now
		res.NodesTrashed++
	}

	// 2. Shares revoked.
	for _, sh := range t.m.shares {
		if sh.OwnerID == id && sh.RevokedAt == nil {
			revoked := now
			sh.RevokedAt = &revoked
			res.SharesRevoked++
		}
	}

	// 3 and 4. ACL grants: subject and author.
	for nodeID, entries := range t.m.acl {
		kept := entries[:0]
		for _, e := range entries {
			isSubject := e.UserID != nil && *e.UserID == id
			isAuthor := e.CreatedBy != nil && *e.CreatedBy == id
			switch {
			case isSubject:
				res.ACLGrantsRemoved++
			case isAuthor:
				res.ACLGrantsAuthored++
			}
			if isSubject || isAuthor {
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(t.m.acl, nodeID)
			continue
		}
		t.m.acl[nodeID] = kept
	}

	// 5. Credentials.
	for token, s := range t.m.sessions {
		if s.UserID == id && s.RevokedAt == nil {
			revoked := now
			s.RevokedAt = &revoked
			t.m.sessions[token] = s
			res.SessionsRevoked++
		}
	}
	for _, ap := range t.m.appPasswords {
		if ap.UserID == id && ap.RevokedAt == nil {
			revoked := now
			ap.RevokedAt = &revoked
			res.AppPasswordsRevoked++
		}
	}

	// 6. Locks released.
	for nodeID, l := range t.m.locks {
		if l.UserID != nil && *l.UserID == id {
			delete(t.m.locks, nodeID)
			res.LocksReleased++
		}
	}

	// 7. Audit history counted, not removed.
	for _, e := range t.m.audit {
		if e.ActorUserID != nil && *e.ActorUserID == id {
			res.AuditRowsKept++
		}
	}

	// 8. Tombstone.
	u.Status = StatusDeleted
	u.Username = deletedUsername(id)
	u.Email = deletedEmail(id)
	u.DisplayName = "deleted account"
	u.UpdatedAt = now

	return res, nil
}

var _ UserDeleteTx = (*memTx)(nil)
