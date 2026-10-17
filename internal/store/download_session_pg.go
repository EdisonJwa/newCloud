package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL implementation of DownloadSessionTx.

// AuthorizeDownload joins a live session or opens a new one, charging the share at
// most once per logical download.
//
// The two cases the requirement separates are handled differently on purpose:
//
//	reuseExisting (a Range request or a retry)
//	    A live session for this (share, node, grant) with the same ETag means the
//	    visitor already paid for these bytes; the request rides on it and the
//	    counter does not move. This is what stops a video seek from consuming a
//	    whole download.
//
//	fresh request
//	    Any session for this (share, node, grant) is ended and a new one is charged
//	    and recorded. A second full download is a second download, which is what
//	    the owner set a limit for.
//
// The insert is guarded by the unique index, so two concurrent first requests
// cannot both be charged: the loser's upsert lands on the winner's row.
// AuthorizeDownload opens or joins the download session for a share and charges it at most once.
//
// SHARE-07: the count is charged INSIDE the transaction that guards the cap, and a Range request that
// belongs to an existing session joins it rather than charging again — one media playback issues dozens
// of requests, and counting them would exhaust a limited link in seconds.
func (t *pgTx) AuthorizeDownload(ctx context.Context, s DownloadSession, reuseExisting bool) (DownloadSession, bool, error) {
	// A caller that omits the expiry gets the default rather than a session that
	// is already expired. A zero timestamp here used to mean every range request
	// created a new session and charged again — the exact over-counting this
	// mechanism exists to prevent.
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = t.now.Add(DownloadSessionTTL)
	}

	const find = `SELECT id, share_id, node_id, grant_hash, etag, created_at, expires_at
		FROM download_sessions
		WHERE share_id = $1 AND node_id = $2 AND grant_hash = $3 AND expires_at > now()`

	existing, err := scanDownloadSession(t.tx.QueryRow(ctx, find, s.ShareID, s.NodeID, s.GrantHash))
	found := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return DownloadSession{}, false, err
	}

	switch {
	case found && reuseExisting && existing.ETag == s.ETag:
		// Continuation of a download that has already been paid for.
		return existing, false, nil
	case found:
		// Either a fresh download, or a continuation against a newer version of
		// the file. Both start a new session.
		if _, derr := t.tx.Exec(ctx, `DELETE FROM download_sessions WHERE id = $1`, existing.ID); derr != nil {
			return DownloadSession{}, false, translate(derr)
		}
	}

	// The counter's own WHERE clause is what enforces the cap atomically: reading
	// then comparing would let two concurrent requests both see room for one more.
	if err := t.IncrementDownload(ctx, s.ShareID); err != nil {
		return DownloadSession{}, false, err
	}

	const insert = `INSERT INTO download_sessions
		(share_id, node_id, grant_hash, etag, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (share_id, node_id, grant_hash) DO UPDATE
			SET etag = EXCLUDED.etag, expires_at = EXCLUDED.expires_at, created_at = now()
		RETURNING id, share_id, node_id, grant_hash, etag, created_at, expires_at`
	created, err := scanDownloadSession(t.tx.QueryRow(ctx, insert,
		s.ShareID, s.NodeID, s.GrantHash, s.ETag, s.ExpiresAt))
	if err != nil {
		return DownloadSession{}, false, err
	}
	return created, true, nil
}

func (t *pgTx) ExpireDownloadSessions(ctx context.Context) (int, error) {
	tag, err := t.tx.Exec(ctx, `DELETE FROM download_sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

func scanDownloadSession(row pgx.Row) (DownloadSession, error) {
	var s DownloadSession
	err := row.Scan(&s.ID, &s.ShareID, &s.NodeID, &s.GrantHash, &s.ETag,
		&s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		return DownloadSession{}, translate(err)
	}
	return s, nil
}

var _ DownloadSessionTx = (*pgTx)(nil)
var _ = time.Now
var _ = uuid.Nil
