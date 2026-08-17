package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ShareKind distinguishes the three share shapes.
type ShareKind string

const (
	// ShareLink is the classic public link: anyone with the URL, optionally
	// password-protected, expiring, or capped by download count.
	ShareLink ShareKind = "link"
	// ShareEmail targets specific recipients. For an internal recipient the grant
	// is attached to their account; for an external one it is gated on proving
	// control of the mailbox.
	ShareEmail ShareKind = "email"
	// ShareRequest is an upload-only drop box: the visitor can add files but can
	// never list, read or delete anything.
	ShareRequest ShareKind = "request"
)

// Share is one share record.
//
// TokenHash is the SHA-256 of the URL token; the plaintext is never stored, so a
// database dump cannot be replayed as working links.
type Share struct {
	ID              uuid.UUID
	NodeID          uuid.UUID
	OwnerID         uuid.UUID
	Kind            ShareKind
	Label           string
	TokenHash       []byte
	PasswordHash    []byte
	ExpiresAt       *time.Time
	MaxDownloads    *int
	DownloadCount   int
	MaxUploads      *int
	UploadCount     int
	MaxBytesPerFile *int64
	// MaxTotalBytes caps the sum of everything delivered through this link (SHARE-05 总空间).
	// Without it, a link with a per-file cap and a count cap accepts
	// max_uploads × max_bytes_per_file, which is not the number the owner set.
	MaxTotalBytes *int64
	// ReceivedBytes is the running total, maintained by the same guarded UPDATE that
	// increments UploadCount so the two cannot disagree.
	ReceivedBytes int64
	// RetentionSeconds is how long data delivered through this link is kept (SHARE-05 保留期限).
	// Nil means the owner keeps it until they delete it.
	RetentionSeconds *int64
	AllowedMIME      []string
	Permissions      []string // "read", "create"
	HideContents     bool
	CreatedAt        time.Time
	RevokedAt        *time.Time
}

// CanRead reports whether the share grants read access.
func (s Share) CanRead() bool { return contains(s.Permissions, "read") }

// CanCreate reports whether the share grants upload access.
func (s Share) CanCreate() bool { return contains(s.Permissions, "create") }

// NeedsPassword reports whether a password must be presented.
func (s Share) NeedsPassword() bool { return len(s.PasswordHash) > 0 }

// Expired reports whether the share is past its expiry.
func (s Share) Expired(now time.Time) bool {
	return s.ExpiresAt != nil && now.After(*s.ExpiresAt)
}

// Exhausted reports whether a counter has reached its cap.
func (s Share) Exhausted() bool {
	if s.MaxDownloads != nil && s.DownloadCount >= *s.MaxDownloads {
		return true
	}
	if s.MaxUploads != nil && s.UploadCount >= *s.MaxUploads {
		return true
	}
	return false
}

// Live reports whether the share may currently be used.
//
// It is deliberately one predicate used by every caller: a share that is revoked,
// expired or exhausted must behave identically everywhere, and duplicating the
// check is how the "expired but still downloadable" bug happens.
func (s Share) Live(now time.Time) bool {
	return s.RevokedAt == nil && !s.Expired(now) && !s.Exhausted()
}

// ShareRecipient is a named recipient of an email share.
type ShareRecipient struct {
	ID            uuid.UUID
	ShareID       uuid.UUID
	Email         string
	UserID        *uuid.UUID
	InvitedAt     time.Time
	VerifiedAt    *time.Time
	FirstAccessAt *time.Time
	LastAccessAt  *time.Time
	RevokedAt     *time.Time
	// ConsumedAt marks the moment the invitation's verification code was used.
	// A code is single-use: without this, it stayed valid for its whole TTL and
	// could be replayed by anyone who saw it once.
	ConsumedAt *time.Time
}

// Share errors.
var (
	// ErrShareExpired: the share exists but is past its window.
	ErrShareExpired = errors.New("store: share expired")
	// ErrShareExhausted: a download or upload cap was reached.
	ErrShareExhausted = errors.New("store: share limit reached")
	// ErrShareRevoked: the owner revoked it.
	ErrShareRevoked = errors.New("store: share revoked")
	// ErrShareGone: the share is valid but the file it points at was deleted (SHARE-06).
	//
	// Internal only. The recipient deliberately cannot tell this apart from "revoked",
	// "expired" or "never existed" — renderShareError collapses them all, because
	// distinguishing the causes would turn the endpoint into a token oracle. The value of this
	// sentinel is that the server stops silently serving an empty listing and can log the real
	// reason.
	ErrShareGone = errors.New("store: the shared file no longer exists")
	// ErrSharePassword: the presented password did not match.
	ErrSharePassword = errors.New("store: share password incorrect")
	// ErrRecipientDenied: the caller is not a recipient of an email share.
	ErrRecipientDenied = errors.New("store: not a share recipient")
)

// ShareTx is the share surface.
type ShareTx interface {
	CreateShare(ctx context.Context, s Share) (Share, error)
	// ShareByTokenHash resolves a share from the hash of the URL token.
	ShareByTokenHash(ctx context.Context, hash []byte) (Share, error)
	ShareByID(ctx context.Context, id uuid.UUID) (Share, error)
	// UpdateShare applies an owner's edit (password, expiry, caps, permissions).
	UpdateShare(ctx context.Context, s Share) (Share, error)
	RevokeShare(ctx context.Context, id uuid.UUID) error
	// IncrementDownload counts a completed download and fails if the cap is hit.
	//
	// It must be atomic: two concurrent downloads of the last permitted copy must
	// not both succeed, and doing it as read-then-write guarantees they would.
	IncrementDownload(ctx context.Context, id uuid.UUID) error
	// IncrementUpload counts an accepted upload against the cap.
	IncrementUpload(ctx context.Context, id uuid.UUID, size int64) error
	SharesForNode(ctx context.Context, nodeID uuid.UUID) ([]Share, error)
	SharesByOwner(ctx context.Context, ownerID uuid.UUID) ([]Share, error)

	AddRecipient(ctx context.Context, r ShareRecipient) error
	RecipientsFor(ctx context.Context, shareID uuid.UUID) ([]ShareRecipient, error)
	MarkRecipientVerified(ctx context.Context, shareID uuid.UUID, email string) error
	// RecipientFor finds a recipient record by share and email.
	RecipientFor(ctx context.Context, shareID uuid.UUID, email string) (ShareRecipient, error)
	MarkRecipientAccess(ctx context.Context, shareID uuid.UUID, email string) error
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
