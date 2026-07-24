package store

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// In-memory share implementation.
//
// The atomicity semantics mirror the SQL: IncrementDownload is a single guarded
// mutation under the transaction, so "at most N downloads" holds under concurrency
// in both implementations.

func (t *memTx) CreateShare(ctx context.Context, s Share) (Share, error) {
	if _, ok := t.m.nodes[s.NodeID]; !ok {
		return Share{}, ErrNotFound
	}
	for _, existing := range t.m.shares {
		if len(existing.TokenHash) == len(s.TokenHash) && string(existing.TokenHash) == string(s.TokenHash) {
			return Share{}, ErrConflict
		}
	}
	if s.ID == uuid.Nil {
		s.ID = newID()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = t.m.now()
	}
	// A request share is upload-only by construction; enforcing it here means no
	// caller can create one that also lists its contents.
	if s.Kind == ShareRequest {
		s.HideContents = true
		if len(s.Permissions) == 0 {
			s.Permissions = []string{"create"}
		}
	}
	cp := s
	t.m.shares[s.ID] = &cp
	return cp, nil
}

func (t *memTx) ShareByTokenHash(ctx context.Context, hash []byte) (Share, error) {
	for _, s := range t.m.shares {
		if len(s.TokenHash) == len(hash) && string(s.TokenHash) == string(hash) {
			return *s, nil
		}
	}
	return Share{}, ErrNotFound
}

func (t *memTx) ShareByID(ctx context.Context, id uuid.UUID) (Share, error) {
	s, ok := t.m.shares[id]
	if !ok {
		return Share{}, ErrNotFound
	}
	return *s, nil
}

func (t *memTx) UpdateShare(ctx context.Context, s Share) (Share, error) {
	cur, ok := t.m.shares[s.ID]
	if !ok || cur.RevokedAt != nil {
		return Share{}, ErrNotFound
	}
	cur.Label = s.Label
	cur.PasswordHash = s.PasswordHash
	cur.ExpiresAt = s.ExpiresAt
	cur.MaxDownloads = s.MaxDownloads
	cur.MaxUploads = s.MaxUploads
	cur.MaxBytesPerFile = s.MaxBytesPerFile
	cur.AllowedMIME = s.AllowedMIME
	cur.Permissions = s.Permissions
	cur.HideContents = s.HideContents
	return *cur, nil
}

func (t *memTx) RevokeShare(ctx context.Context, id uuid.UUID) error {
	s, ok := t.m.shares[id]
	if !ok {
		return ErrNotFound
	}
	if s.RevokedAt == nil {
		at := t.m.now()
		s.RevokedAt = &at
	}
	return nil
}

// IncrementDownload is the guarded counter increment.
func (t *memTx) IncrementDownload(ctx context.Context, id uuid.UUID) error {
	s, ok := t.m.shares[id]
	if !ok {
		return ErrNotFound
	}
	now := t.m.now()
	if s.RevokedAt != nil {
		return ErrShareRevoked
	}
	if s.Expired(now) {
		return ErrShareExpired
	}
	if s.MaxDownloads != nil && s.DownloadCount >= *s.MaxDownloads {
		return ErrShareExhausted
	}
	s.DownloadCount++
	return nil
}

func (t *memTx) IncrementUpload(ctx context.Context, id uuid.UUID) error {
	s, ok := t.m.shares[id]
	if !ok {
		return ErrNotFound
	}
	now := t.m.now()
	if s.RevokedAt != nil {
		return ErrShareRevoked
	}
	if s.Expired(now) {
		return ErrShareExpired
	}
	if s.MaxUploads != nil && s.UploadCount >= *s.MaxUploads {
		return ErrShareExhausted
	}
	s.UploadCount++
	return nil
}

func (t *memTx) SharesForNode(ctx context.Context, nodeID uuid.UUID) ([]Share, error) {
	var out []Share
	for _, s := range t.m.shares {
		if s.NodeID == nodeID && s.RevokedAt == nil {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (t *memTx) SharesByOwner(ctx context.Context, ownerID uuid.UUID) ([]Share, error) {
	var out []Share
	for _, s := range t.m.shares {
		if s.OwnerID == ownerID {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (t *memTx) AddRecipient(ctx context.Context, r ShareRecipient) error {
	if _, ok := t.m.shares[r.ShareID]; !ok {
		return ErrNotFound
	}
	key := [2]string{r.ShareID.String(), lower(r.Email)}
	if _, exists := t.m.recipients[key]; exists {
		return nil // ON CONFLICT DO NOTHING
	}
	if r.ID == uuid.Nil {
		r.ID = newID()
	}
	if r.InvitedAt.IsZero() {
		r.InvitedAt = t.m.now()
	}
	cp := r
	t.m.recipients[key] = &cp
	return nil
}

func (t *memTx) RecipientsFor(ctx context.Context, shareID uuid.UUID) ([]ShareRecipient, error) {
	var out []ShareRecipient
	for k, r := range t.m.recipients {
		if k[0] == shareID.String() {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InvitedAt.Before(out[j].InvitedAt) })
	return out, nil
}

func (t *memTx) RecipientFor(ctx context.Context, shareID uuid.UUID, email string) (ShareRecipient, error) {
	r, ok := t.m.recipients[[2]string{shareID.String(), lower(email)}]
	if !ok {
		return ShareRecipient{}, ErrNotFound
	}
	return *r, nil
}

func (t *memTx) MarkRecipientVerified(ctx context.Context, shareID uuid.UUID, email string) error {
	r, ok := t.m.recipients[[2]string{shareID.String(), lower(email)}]
	if !ok {
		return ErrNotFound
	}
	if r.VerifiedAt == nil {
		now := t.m.now()
		r.VerifiedAt = &now
	}
	return nil
}

func (t *memTx) MarkRecipientAccess(ctx context.Context, shareID uuid.UUID, email string) error {
	r, ok := t.m.recipients[[2]string{shareID.String(), lower(email)}]
	if !ok {
		return ErrNotFound
	}
	now := t.m.now()
	if r.FirstAccessAt == nil {
		r.FirstAccessAt = &now
	}
	r.LastAccessAt = &now
	return nil
}

// AllShares returns every share, for tests.
func (m *Memory) AllShares() []Share {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Share, 0, len(m.shares))
	for _, s := range m.shares {
		out = append(out, *s)
	}
	return out
}

var _ = time.Now
