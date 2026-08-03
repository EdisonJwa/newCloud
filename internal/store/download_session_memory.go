package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// In-memory implementation of DownloadSessionTx.
//
// It mirrors the PostgreSQL behaviour exactly, including the reuse rule. The
// conformance suite runs both, because a counter that behaves differently in tests
// and in production is precisely the failure mode this project's shared suite
// exists to prevent.

func (t *memTx) AuthorizeDownload(ctx context.Context, s DownloadSession, reuseExisting bool) (DownloadSession, bool, error) {
	if t.m.downloads == nil {
		t.m.downloads = map[string]*DownloadSession{}
	}
	key := downloadSessionKey(s.ShareID, s.NodeID, s.GrantHash)

	existing, found := t.m.downloads[key]
	if found && !existing.ExpiresAt.After(t.m.now()) {
		delete(t.m.downloads, key)
		existing, found = nil, false
	}

	if found && reuseExisting && existing.ETag == s.ETag {
		return *existing, false, nil
	}
	if found {
		delete(t.m.downloads, key)
	}

	if err := t.IncrementDownload(ctx, s.ShareID); err != nil {
		return DownloadSession{}, false, err
	}

	created := s
	if created.ID == uuid.Nil {
		created.ID = newID()
	}
	created.CreatedAt = t.m.now()
	if created.ExpiresAt.IsZero() {
		created.ExpiresAt = created.CreatedAt.Add(DownloadSessionTTL)
	}
	cp := created
	t.m.downloads[key] = &cp
	return created, true, nil
}

func (t *memTx) ExpireDownloadSessions(ctx context.Context) (int, error) {
	now := t.m.now()
	n := 0
	for k, s := range t.m.downloads {
		if !s.ExpiresAt.After(now) {
			delete(t.m.downloads, k)
			n++
		}
	}
	return n, nil
}

func downloadSessionKey(shareID, nodeID uuid.UUID, grant []byte) string {
	return shareID.String() + "/" + nodeID.String() + "/" + string(grant)
}

var _ = time.Now
