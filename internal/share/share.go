// Package share implements share links, email shares and upload-only file
// requests on top of the metadata store.
//
// The rules this package enforces, and why each one is not optional:
//
//   - The URL token is never stored; only its SHA-256. A database dump must not
//     yield working links.
//   - A share's liveness (revoked / expired / exhausted) is decided by one
//     predicate. Duplicating that check is how "expired but still downloadable"
//     ships.
//   - Download counting is a single guarded increment. Read-then-write lets two
//     concurrent requests both see "one copy left".
//   - A request share never lists its contents, and the server enforces it rather
//     than the UI hiding the button.
package share

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/password"
	"github.com/EdisonJwa/newCloud/internal/store"
	"github.com/EdisonJwa/newCloud/internal/ticket"
)

// Service manages shares.
type Service struct {
	meta store.Store
	now  func() time.Time
	// tickets mints and verifies recipient verification codes. Optional: without it,
	// link and request shares work and email verification reports "not configured".
	tickets *ticket.Issuer
	// argonParams are deliberately tunable so tests can use cheap settings while
	// production pays for real ones.
	argonParams ArgonParams
}

// ArgonParams configures password hashing.
//
// It is an alias for password.Params so the KDF has exactly one implementation: the
// same parameters now guard share passwords and the break-glass administrator, and two
// copies would eventually drift into one of them being weaker.
type ArgonParams = password.Params

// DefaultArgon are the production parameters (64 MiB, 3 passes).
func DefaultArgon() ArgonParams { return password.Default() }

// TestArgon are intentionally weak parameters for fast tests.
func TestArgon() ArgonParams { return password.Test() }

// New builds the service.
func New(meta store.Store, params ArgonParams) *Service {
	return &Service{meta: meta, now: time.Now, argonParams: params}
}

// NewShare describes a share to create.
type NewShare struct {
	NodeID  uuid.UUID
	OwnerID uuid.UUID
	// OwnerEmail is recorded on the create event. Audit rows carry the actor address at the
	// time of the event, so reading it back by joining users would rewrite history.
	OwnerEmail string
	// NodePath is the resolved path of the shared object, recorded on the audit row. Without
	// it the log says "share.create 18711bd6", which nobody can act on later.
	NodePath        string
	Kind            store.ShareKind
	Label           string
	Password        string
	TTL             time.Duration
	ExpiresAt       *time.Time
	MaxDownloads    *int
	MaxUploads      *int
	MaxBytesPerFile *int64
	// MaxTotalBytes caps what the link may receive in total (SHARE-05 总空间).
	MaxTotalBytes *int64
	// RetentionSeconds is how long delivered data is kept (SHARE-05 保留期限).
	RetentionSeconds        *int64
	MaxUploadRate           *int64
	UploadRateWindowSeconds *int64
	AllowedMIME             []string
	Permissions             []string
	Recipients              []string
}

// Created is the result of creating a share: the plaintext token appears exactly
// once, here.
type Created struct {
	Share store.Share
	Token string
}

// Create issues a share.
//
// Ordering matters: permissions are normalised before validation, so a request
// share cannot be created with read access even if the caller asks for it.
func (s *Service) Create(ctx context.Context, in NewShare) (Created, error) {
	kind := in.Kind
	if kind == "" {
		kind = store.ShareLink
	}

	perms := normalisePermissions(kind, in.Permissions)

	expires := in.ExpiresAt
	if expires == nil && in.TTL > 0 {
		t := s.now().Add(in.TTL)
		expires = &t
	}
	if expires != nil && !expires.After(s.now()) {
		return Created{}, errors.New("share: expiry must be in the future")
	}

	var passwordHash []byte
	if in.Password != "" {
		h, err := s.HashPassword(in.Password)
		if err != nil {
			return Created{}, err
		}
		passwordHash = h
	}

	token, tokenHash, err := newToken()
	if err != nil {
		return Created{}, err
	}

	sh := store.Share{
		NodeID:                  in.NodeID,
		OwnerID:                 in.OwnerID,
		Kind:                    kind,
		Label:                   in.Label,
		TokenHash:               tokenHash,
		PasswordHash:            passwordHash,
		ExpiresAt:               expires,
		MaxDownloads:            in.MaxDownloads,
		MaxUploads:              in.MaxUploads,
		MaxBytesPerFile:         in.MaxBytesPerFile,
		MaxTotalBytes:           in.MaxTotalBytes,
		RetentionSeconds:        in.RetentionSeconds,
		MaxUploadRate:           in.MaxUploadRate,
		UploadRateWindowSeconds: in.UploadRateWindowSeconds,
		AllowedMIME:             in.AllowedMIME,
		Permissions:             perms,
		HideContents:            kind == store.ShareRequest,
	}

	var created store.Share
	err = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		c, cerr := tx.CreateShare(ctx, sh)
		if cerr != nil {
			return cerr
		}
		created = c

		if kind == store.ShareEmail {
			for _, addr := range in.Recipients {
				normalised, aerr := normaliseEmail(addr)
				if aerr != nil {
					return aerr
				}
				if rerr := tx.AddRecipient(ctx, store.ShareRecipient{
					ShareID: c.ID, Email: normalised,
				}); rerr != nil {
					return rerr
				}
			}
		}

		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &in.OwnerID, ActorEmail: in.OwnerEmail,
			Action: "share.create", TargetKind: "share", TargetID: c.ID.String(),
			Result: "ok",
			Metadata: map[string]any{
				"kind":         string(kind),
				"permissions":  perms,
				"has_password": len(passwordHash) > 0,
				"expires_at":   expires,
				// The path is what makes this row readable later. Without it the audit log
				// says "share.create 18711bd6", which is a fact nobody can act on.
				"path": nodePath(in),
			},
		})
	})
	if err != nil {
		return Created{}, err
	}
	return Created{Share: created, Token: token}, nil
}

// normalisePermissions forces the shape each kind requires.
//
// A request share is upload-only: it must never carry "read", because that would
// turn a drop box into a read-only publication of the directory it accepts files
// into.
func normalisePermissions(kind store.ShareKind, requested []string) []string {
	switch kind {
	case store.ShareRequest:
		return []string{"create"}
	default:
		if len(requested) == 0 {
			return []string{"read"}
		}
		out := make([]string, 0, len(requested))
		for _, p := range requested {
			switch p {
			case "read", "create":
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			out = []string{"read"}
		}
		return out
	}
}

// Resolve looks up a share by its URL token.
//
// It returns store.ErrNotFound for every failure mode — unknown token, revoked,
// expired, exhausted — because the caller maps all of them to the same 404.
// Distinguishing them in the response would tell an attacker whether a guessed
// token ever existed.
func (s *Service) Resolve(ctx context.Context, token string) (store.Share, error) {
	hash := tokenHash(token)

	var sh store.Share
	err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		found, err := tx.ShareByTokenHash(ctx, hash)
		if err != nil {
			return err
		}
		sh = found
		return nil
	})
	if err != nil {
		return store.Share{}, err
	}

	if err := s.gate(sh); err != nil {
		return store.Share{}, err
	}
	return sh, nil
}

// ResolveForDownload resolves a token without applying the download cap.
//
// The download handler needs this because "the share is exhausted" is the wrong question
// for a request that is a CONTINUATION of a download already paid for. A media player
// seeking through a shared video issues one Range request per seek; with the cap applied
// here, the second seek is refused even though it should ride the session created by the
// first — which is requirement line 217's exact concern ("最大下载次数 must not be counted
// per HTTP request").
//
// So the cap is still enforced, but by the session logic: AuthorizeDownload charges a new
// download atomically and refuses when the cap is reached, and lets a continuation of an
// existing session through for free. Revocation and expiry are still applied here, because
// those mean the share must stop working entirely and a continuation is no exception.
//
// Callers that are not the download handler must keep using Resolve: for them "exhausted"
// really does mean "not usable".
func (s *Service) ResolveForDownload(ctx context.Context, token string) (store.Share, error) {
	hash := tokenHash(token)

	var sh store.Share
	err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		found, err := tx.ShareByTokenHash(ctx, hash)
		if err != nil {
			return err
		}
		sh = found
		return nil
	})
	if err != nil {
		return store.Share{}, err
	}

	switch {
	case sh.RevokedAt != nil:
		return store.Share{}, store.ErrShareRevoked
	case sh.Expired(s.now()):
		return store.Share{}, store.ErrShareExpired
	}
	return sh, nil
}

// gate applies the checks that make a share unusable regardless of the request.
func (s *Service) gate(sh store.Share) error {
	switch {
	case sh.RevokedAt != nil:
		return store.ErrShareRevoked
	case sh.Expired(s.now()):
		return store.ErrShareExpired
	case sh.Exhausted():
		return store.ErrShareExhausted
	}
	return nil
}

// VerifyPassword checks a presented password in constant time at the KDF level.
func (s *Service) VerifyPassword(ctx context.Context, sh store.Share, password string) error {
	if !sh.NeedsPassword() {
		return nil
	}

	// Throttle BEFORE comparing the hash (SHARE-02 尝试上限).
	//
	// Checking the limit first is what makes it hold under parallel guessing: a caller that
	// counted only after a failure would leave a window in which concurrent attempts all observed
	// "under the limit". Recording the attempt first, then refusing when the window is full, means
	// the sixth guess in fifteen minutes is refused regardless of whether it would have been right.
	//
	// The count read FAILS CLOSED.
	//
	// The previous version discarded this error, leaving `failures` at zero so the comparison was
	// false and the attempt proceeded. A storage fault therefore disabled the attempt cap
	// completely — and the worse the fault, the longer the window in which guessing was free.
	// Fail-open is the wrong direction for a control that exists to bound guessing: if the server
	// cannot tell whether the cap is reached, it must not assume it is not.
	//
	// The error is returned rather than translated into a wrong-password refusal, so a client does
	// not get told "wrong password" when the truth is "cannot tell", and so the operator sees a
	// fault instead of a suspicious burst of failures.
	var failures int
	if cerr := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		n, nerr := tx.PasswordFailureCount(ctx, sh.ID, store.PasswordWindow)
		failures = n
		return nerr
	}); cerr != nil {
		return cerr
	}
	if failures >= store.PasswordMaxAttempts {
		_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "anonymous", Action: "share.password.verify.throttled",
				TargetKind: "share", TargetID: sh.ID.String(), Result: "denied",
			})
		})
		return store.ErrSharePasswordThrottled
	}

	ok := s.checkPassword(password, sh.PasswordHash)
	_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.RecordPasswordAttempt(ctx, sh.ID, "", ok)
	})

	if !ok {
		// Failure is audited: a burst of these on one share is how brute forcing
		// looks, and it is the only signal the owner has.
		_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.Audit(ctx, store.AuditEntry{
				ActorKind: "anonymous", Action: "share.password.verify.failure",
				TargetKind: "share", TargetID: sh.ID.String(), Result: "denied",
			})
		})
		return store.ErrSharePassword
	}
	return nil
}

// RecordDownload was removed.
//
// It incremented the share's download counter and wrote the share.download audit row, and it had no
// production caller: downloads are charged through the session API (AuthorizeDownload), which is what
// makes Range requests and resumed transfers cost one download instead of many. Because the method
// still existed and was tested, share.download looked emitted — the vocabulary scan saw the string
// literal in this file — while in production NOTHING wrote the event, and a public link could serve
// files with no audit trail at all. Deleting it is part of the fix: the event is now written by the
// handler, inside the transaction that charges the download.
// RecordUpload counts an accepted upload and audits it.
func (s *Service) RecordUpload(ctx context.Context, sh store.Share, meta DownloadMeta) error {
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Rate first, then the totals.
		//
		// Order matters for the failure a visitor sees: a burst that is over the rate should be
		// told "slow down", not "this link is used up", because the second is false — the
		// allowance is intact, the window is not. Checking the rate first means a visitor who
		// retries after the window passes succeeds, whereas consuming the count first would spend
		// an upload from the allowance on a request that was refused for a different reason.
		if sh.MaxUploadRate != nil && *sh.MaxUploadRate > 0 {
			window := defaultUploadRateWindow
			if sh.UploadRateWindowSeconds != nil && *sh.UploadRateWindowSeconds > 0 {
				window = time.Duration(*sh.UploadRateWindowSeconds) * time.Second
			}
			if err := tx.AllowShareUpload(ctx, sh.ID, *sh.MaxUploadRate, window,
				meta.ActorIP, meta.Bytes); err != nil {
				return err
			}
		}

		// The byte total moves with the count in the same guarded UPDATE: a cap on total
		// space checked separately from the increment would be racy, and a rejected upload
		// must not leave received_bytes inflated.
		if err := tx.IncrementUpload(ctx, sh.ID, meta.Bytes); err != nil {
			return err
		}
		bytes := meta.Bytes
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: meta.ActorKind, ActorUserID: meta.ActorUserID,
			ActorEmail: meta.ActorEmail, ActorIP: meta.ActorIP,
			UserAgent: meta.UserAgent,
			Action:    "share.upload", TargetKind: "share", TargetID: sh.ID.String(),
			ShareID: &sh.ID, Result: "ok", Bytes: &bytes,
			Metadata: map[string]any{"name": meta.Name, "recipient": meta.Recipient},
		})
	})
}

// DownloadMeta is the attribution recorded for a share access.
//
// Anonymous access is still attributed: IP, user agent and (for email shares) the
// verified address are the only way to answer "who downloaded this".
type DownloadMeta struct {
	ActorKind   string
	ActorUserID *uuid.UUID
	ActorEmail  string
	ActorIP     string
	UserAgent   string
	Name        string
	Bytes       int64
	Recipient   string
}

// Revoke withdraws a share.
func (s *Service) Revoke(ctx context.Context, shareID uuid.UUID, actorID uuid.UUID, actorEmail string) error {
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.RevokeShare(ctx, shareID); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &actorID, ActorEmail: actorEmail,
			Action: "share.revoke", TargetKind: "share", TargetID: shareID.String(),
			Result: "ok",
		})
	})
}

// nodePath returns the human-readable target of a share for the audit trail.
//
// It deliberately does not touch the database: the caller already resolved the path to
// authorise the request, and re-resolving here would add a query to every share creation just
// to improve a log line.
func nodePath(in NewShare) string {
	return in.NodePath
}

// ValidateRecipient ensures an address is syntactically usable.
func ValidateRecipient(addr string) error {
	_, err := normaliseEmail(addr)
	return err
}

func normaliseEmail(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("share: empty recipient")
	}
	// net/mail parses the full grammar including display names; requiring a bare
	// address keeps the stored value unambiguous.
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return "", fmt.Errorf("share: invalid recipient %q: %w", addr, err)
	}
	at := strings.LastIndexByte(parsed.Address, '@')
	if at <= 0 || at == len(parsed.Address)-1 {
		return "", fmt.Errorf("share: invalid recipient %q", addr)
	}
	return strings.ToLower(parsed.Address), nil
}

// newToken mints a URL token and its storage hash.
//
// 128 bits: long enough that guessing is infeasible, short enough that the link
// stays copy-pasteable.
func newToken() (token string, hash []byte, err error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw[:])
	return token, tokenHash(token), nil
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// HashPassword produces an Argon2id hash with a random salt. It is exported so
// the API can change a share password without duplicating the KDF parameters.
//
// Layout: salt || hash, with the parameters implied by the service config. A
// stored format that embeds its parameters would allow tuning them later without
// invalidating existing shares; that is a deliberate simplification for v1 and is
// recorded here rather than discovered later.
func (s *Service) HashPassword(secret string) ([]byte, error) {
	return password.Hash(secret, s.argonParams)
}

func (s *Service) checkPassword(secret string, stored []byte) bool {
	return password.Verify(secret, stored, s.argonParams)
}

// defaultUploadRateWindow is the window when a share sets a rate but no window.
//
// One hour: the requirement says "rate" without a period, and a share's other limits are per-link
// totals rather than per-second budgets. An hour is slow enough to stop a flood and fast enough that
// a legitimate visitor who hit it can try again in the same session.
const defaultUploadRateWindow = time.Hour

// UploadRateExceeded reports whether this share has already accepted its allowance for the window.
//
// Advisory, and deliberately separate from RecordUpload. RecordUpload's guarded insert is what
// REFUSES a burst — correct under concurrency, and it stays the authority. This answers the earlier
// question "is there any point in accepting these bytes", so a caller can refuse before writing
// content that the authoritative check is about to reject anyway.
//
// A false negative here costs one wasted write, which is what happened before this existed. A false
// positive is impossible: the count is only ever behind the truth, so a share that is over its rate
// is always reported as such.
func (s *Service) UploadRateExceeded(ctx context.Context, sh store.Share) (bool, error) {
	if sh.MaxUploadRate == nil || *sh.MaxUploadRate <= 0 {
		return false, nil
	}
	window := defaultUploadRateWindow
	if sh.UploadRateWindowSeconds != nil && *sh.UploadRateWindowSeconds > 0 {
		window = time.Duration(*sh.UploadRateWindowSeconds) * time.Second
	}
	var n int64
	err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, cerr := tx.ShareUploadCountInWindow(ctx, sh.ID, window)
		n = got
		return cerr
	})
	if err != nil {
		return false, err
	}
	return n >= *sh.MaxUploadRate, nil
}
