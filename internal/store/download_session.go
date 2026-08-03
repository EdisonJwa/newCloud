package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// DownloadSession is one authorised logical download of a shared file.
//
// The requirement (§6.2, D-04) is specific about why a plain request counter is
// wrong: a media player issues several Range requests for one playback, and a
// resumed download is another request for the same bytes. Counting requests
// therefore consumes a limited share far too fast, while a presigned URL reused
// within its lifetime consumes it too slowly. A session is the unit that matches
// what a user means by "I downloaded this file".
//
// The session is bound to (share, node, visitor grant) and carries the node's ETag,
// so re-authorising a later Range request cannot be used to read a different
// version of the file.
type DownloadSession struct {
	ID        uuid.UUID
	ShareID   uuid.UUID
	NodeID    uuid.UUID
	GrantHash []byte
	ETag      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DownloadSessionTTL bounds how long one paid download stays authorised. Long
// enough for a large file on a slow link and for seeking around a video; short
// enough that a leaked grant cannot be replayed indefinitely.
const DownloadSessionTTL = time.Hour

// DownloadSessionTx is the session surface.
type DownloadSessionTx interface {
	// AuthorizeDownload authorises one request against the share's allowance.
	//
	// reuseExisting distinguishes the two cases the requirement separates:
	//   - a Range request (or a retry) continues an existing download: join the
	//     live session and charge nothing;
	//   - a fresh full request is a new download: end the old session, charge one,
	//     and record a new one.
	//
	// Charging and recording are one operation because the counter and the session
	// must agree. A session that exists without having consumed a download is a
	// free download; a charge without a session means every Range request pays
	// again.
	AuthorizeDownload(ctx context.Context, s DownloadSession, reuseExisting bool) (DownloadSession, bool, error)
	// ExpireDownloadSessions deletes sessions past their TTL.
	ExpireDownloadSessions(ctx context.Context) (int, error)
}
