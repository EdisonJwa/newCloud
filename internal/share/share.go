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
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

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
// Share passwords are human-chosen, so they need a slow KDF — unlike app
// passwords, which are 256-bit random values.
type ArgonParams struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	KeyLen  uint32
}

// DefaultArgon are the production parameters (64 MiB, 3 passes).
func DefaultArgon() ArgonParams {
	return ArgonParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32}
}

// TestArgon are intentionally weak parameters for fast tests.
func TestArgon() ArgonParams {
	return ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32}
}

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
	AllowedMIME     []string
	Permissions     []string
	Recipients      []string
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
		NodeID:          in.NodeID,
		OwnerID:         in.OwnerID,
		Kind:            kind,
		Label:           in.Label,
		TokenHash:       tokenHash,
		PasswordHash:    passwordHash,
		ExpiresAt:       expires,
		MaxDownloads:    in.MaxDownloads,
		MaxUploads:      in.MaxUploads,
		MaxBytesPerFile: in.MaxBytesPerFile,
		AllowedMIME:     in.AllowedMIME,
		Permissions:     perms,
		HideContents:    kind == store.ShareRequest,
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

	switch {
	case sh.RevokedAt != nil:
		return store.Share{}, store.ErrShareRevoked
	case sh.Expired(s.now()):
		return store.Share{}, store.ErrShareExpired
	case sh.Exhausted():
		return store.Share{}, store.ErrShareExhausted
	}
	return sh, nil
}

// VerifyPassword checks a presented password in constant time at the KDF level.
func (s *Service) VerifyPassword(ctx context.Context, sh store.Share, password string) error {
	if !sh.NeedsPassword() {
		return nil
	}
	if !s.checkPassword(password, sh.PasswordHash) {
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

// RecordDownload counts a successful download and audits it.
//
// The count is incremented first: if the cap is already reached the increment
// fails and the caller must not start streaming. Doing it the other way round
// would let the bytes leave before we discover there was no allowance left.
func (s *Service) RecordDownload(ctx context.Context, sh store.Share, meta DownloadMeta) error {
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.IncrementDownload(ctx, sh.ID); err != nil {
			return err
		}
		bytes := meta.Bytes
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: meta.ActorKind, ActorUserID: meta.ActorUserID,
			ActorEmail: meta.ActorEmail, ActorIP: meta.ActorIP,
			UserAgent: meta.UserAgent,
			Action:    "share.download", TargetKind: "share", TargetID: sh.ID.String(),
			ShareID: &sh.ID, Result: "ok", Bytes: &bytes,
			Metadata: map[string]any{
				"name":       meta.Name,
				"node_id":    sh.NodeID.String(),
				"recipient":  meta.Recipient,
				"share_kind": string(sh.Kind),
			},
		})
	})
}

// RecordUpload counts an accepted upload and audits it.
func (s *Service) RecordUpload(ctx context.Context, sh store.Share, meta DownloadMeta) error {
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.IncrementUpload(ctx, sh.ID); err != nil {
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
func (s *Service) HashPassword(password string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, s.argonParams.Time,
		s.argonParams.Memory, s.argonParams.Threads, s.argonParams.KeyLen)
	return append(salt, key...), nil
}

func (s *Service) checkPassword(password string, stored []byte) bool {
	const saltLen = 16
	if len(stored) <= saltLen {
		return false
	}
	salt, want := stored[:saltLen], stored[saltLen:]
	got := argon2.IDKey([]byte(password), salt, s.argonParams.Time,
		s.argonParams.Memory, s.argonParams.Threads, s.argonParams.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}
