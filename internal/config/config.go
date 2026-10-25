// Package config loads, validates and self-checks runtime configuration.
//
// Every check in Validate exists because a misconfiguration would otherwise be
// silent: a wrong origin breaks the content-isolation guarantee, a split
// filesystem turns atomic rename into a copy, and an unscoped break-glass
// account is an unauthenticated root shell.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Role is the local role a user maps to.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
	RoleGuest Role = "guest"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Auth    AuthConfig    `yaml:"auth"`
	Storage StorageConfig `yaml:"storage"`
	Limits  LimitsConfig  `yaml:"limits"`
	Audit   AuditConfig   `yaml:"audit"`
	Mail    MailConfig    `yaml:"mail"`
	Mounts  MountsConfig  `yaml:"mounts"`
	Search  SearchConfig  `yaml:"search"`

	// warnings collects the non-fatal findings Validate produces (a legal but
	// discouraged configuration, such as an http:// public URL). They are reported by
	// the serving process at startup, which is the one moment an operator is looking at
	// configuration; Validate itself cannot print, because it also runs from commands
	// whose output is machine-read.
	warnings []string
}

// Warnings returns the non-fatal findings from the last Validate, in the order found.
//
// Empty for a configuration that raised nothing. It is a method rather than part of the
// error so a legal configuration still loads: a warning must never become a reason a
// deployment cannot start.
func (c *Config) Warnings() []string { return c.warnings }

// MailConfig configures the transport for share recipient verification codes.
//
// Email shares need it: without a transport the server still issues codes but can only
// log that it could not send them, which is a visible gap rather than a silent one.
type MailConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// PasswordFile is preferred over Password in a container: a file can be mounted from
	// a secret store and never appears in `docker inspect`.
	PasswordFile string `yaml:"password_file"`
	From         string `yaml:"from"`
	ImplicitTLS  bool   `yaml:"implicit_tls"`
	StartTLS     bool   `yaml:"start_tls"`
	// InsecureSkipVerify exists for a relay with a self-signed certificate on a private
	// network. It removes the only protection against an active attacker on that path, so
	// enabling it is logged at startup.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type ServerConfig struct {
	Addr           string `yaml:"addr"`
	PublicURL      string `yaml:"public_url"`
	UserContentURL string `yaml:"usercontent_url"`
	// UserContentAddr is the listen address for the content origin.
	//
	// Empty means content is served by the main listener and the separation is the deployment's
	// (a reverse proxy routing usercontent_url to the same process). Setting it makes the split real
	// inside the application: the content routes move to this listener and the main one refuses them,
	// so the app origin cannot serve user bytes even by accident.
	UserContentAddr string `yaml:"usercontent_addr"`
	// TrustProxy honours X-Forwarded-For from ANY peer.
	//
	// Deprecated in favour of TrustedProxies, and unsafe on a listener that is reachable
	// directly: the header is client-controlled, so a caller can choose its own address and
	// with it the outcome of every address-derived decision — the break-glass CIDR
	// restriction, the per-IP throttles, and the ActorIP in the audit log.
	TrustProxy bool `yaml:"trust_proxy"`
	// TrustedProxies lists the CIDRs allowed to set X-Forwarded-For (§9.4 可信源列表).
	//
	// Empty means no peer is trusted and the header is ignored entirely, which is the correct
	// default for a server not behind a reverse proxy. This is the knob the requirement asks
	// for; TrustProxy is the blunt version kept so an existing deployment does not break.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// AllowSameHostContent permits usercontent_url to share a hostname with public_url.
	//
	// Off by default, because browsers key cookies by HOSTNAME and not by port or scheme: a
	// content origin on the same host receives the app's session cookie on every request, which
	// is the cookie-isolation property the separate origin exists to provide. Set it only when
	// the deployment already strips Cookie at the proxy for content paths and that fact is
	// written down; startup then logs the exception so it is not a silent choice.
	AllowSameHostContent bool `yaml:"allow_same_host_content"`
}

type AuthConfig struct {
	Issuer           string          `yaml:"issuer"`
	ClientID         string          `yaml:"client_id"`
	ClientSecret     string          `yaml:"client_secret"`
	ClientSecretFile string          `yaml:"client_secret_file"`
	Scopes           []string        `yaml:"scopes"`
	GroupRoleMap     map[string]Role `yaml:"group_role_map"`
	SessionTTL       time.Duration   `yaml:"session_ttl"`
	SessionSecret    string          `yaml:"session_secret"`
	// JIT controls what happens the first time an unknown IdP identity signs in (ID-04).
	//
	// The default is "open" because that is what the project has always done, and
	// changing the default would silently lock existing deployments out on upgrade. The
	// point of the switch is that a deployment handling other people's data can close it.
	JIT             JITConfig        `yaml:"jit"`
	AllowLocalAdmin bool             `yaml:"allow_local_admin"`
	LocalAdmin      LocalAdminConfig `yaml:"local_admin"`
	SCIMToken       string           `yaml:"scim_token"`
	// RevalidateMinutes bounds how long a session or App Password may outlive an IdP-side change
	// (ID-06, WC-GAP-001).
	//
	// Role and group membership are reconciled only at login, because that is the only moment the
	// server sees the IdP's claims. A user removed from the admin group who never signs in again
	// keeps the old role, and their WebDAV App Password — which never talks to the IdP at all —
	// keeps it indefinitely. "Takes effect at the next login" is not a bound; it is the absence of
	// one.
	//
	// With a non-zero value, a request presenting a credential whose last confirmation is older
	// than this window is refused until the account signs in again. That converts an unbounded
	// window into a documented, testable one, at the cost of a periodic interactive login. Zero
	// disables the check and preserves the previous behaviour, which startup warns about.
	//
	// This is not an IdP event feed. A feed (SCIM, push, or a poll) is strictly better because it
	// revokes at the moment of the change; this bounds the exposure using only what the existing
	// OIDC flow already provides.
	RevalidateMinutes int `yaml:"revalidate_minutes"`
}

type LocalAdminConfig struct {
	Username     string   `yaml:"username"`
	PasswordFile string   `yaml:"password_argon2_file"`
	AllowedCIDRs []string `yaml:"allowed_cidrs"`
}

// RevalidateWindow is how long a credential may go unconfirmed before it must be re-established
// interactively (ID-06, WC-GAP-001). Zero means no bound, which is the previous behaviour.
func (a AuthConfig) RevalidateWindow() time.Duration {
	if a.RevalidateMinutes <= 0 {
		return 0
	}
	return time.Duration(a.RevalidateMinutes) * time.Minute
}

// Layout selects how files map onto physical storage. It is a deployment
// decision, not a per-user toggle: the two layouts have different backup,
// deduplication and inspection properties, and switching after data exists
// requires a migration.
type Layout string

const (
	// LayoutBlob stores content by SHA-256 under <root>/ab/cd/<hash>. The
	// directory tree exists only in PostgreSQL. Deduplicates, cheap versions,
	// but not inspectable with ls.
	LayoutBlob Layout = "blob"
	// LayoutTree mirrors the user-visible hierarchy as <root>/<owner>/<path>.
	// Inspectable, rsync-friendly, but no deduplication and each version is a
	// full copy.
	LayoutTree Layout = "tree"
)

func (l Layout) Valid() bool { return l == LayoutBlob || l == LayoutTree }

// StorageConfig chooses the physical storage backend and the on-disk layout.
type StorageConfig struct {
	// Backend is where the bytes live: local disk or an S3-compatible service.
	Backend string `yaml:"backend"`
	// Layout is how they are arranged within that backend.
	Layout Layout       `yaml:"layout"`
	Local  LocalStorage `yaml:"local"`
	S3     S3Storage    `yaml:"s3"`
	// UploadDir stages resumable uploads. Empty falls back to the system temporary directory.
	UploadDir string `yaml:"upload_dir"`
}

type LocalStorage struct {
	Root string `yaml:"root"`
}

// UploadDir is where resumable uploads are staged before publishing.
//
// It is separate from the content root on purpose: with the tree layout the content root is the
// user-visible tree, and half-written uploads must never appear there. Empty means the process
// temporary directory, which works everywhere but is worth setting explicitly on a real
// deployment — /tmp is often a small tmpfs.
const DefaultUploadDir = ""

// S3Storage configures the object-store backend.
//
// Credentials are optional on purpose: when they are absent the standard AWS chain is used
// (environment, shared credentials file, instance role). A deployment in a cloud account
// should not need to put a key in a config file to start.
type S3Storage struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// Secure selects https and defaults to true. Sending object data in the clear must be an
	// explicit decision rather than something inherited from a zero value.
	Secure *bool  `yaml:"secure"`
	Prefix string `yaml:"prefix"`
	// SSE is the server-side encryption mode (§9.4): "", "sse-s3" or "sse-kms".
	//
	// Empty leaves it to the bucket policy. Anything else is validated at startup, and an unknown
	// value is a refusal to start rather than a silent fallback to plaintext.
	SSE string `yaml:"sse"`
	// SSEKMSKeyID names the KMS key for sse-kms.
	SSEKMSKeyID string `yaml:"sse_kms_key_id"`
	// VerifyAfterPut reads every object back and re-hashes it before the upload is reported as
	// successful (WC-GAP-003, INV-03).
	//
	// The pre-upload hash proves the payload matched its address when it left this process. It does
	// not prove what the store now holds: a retried multipart part can be assembled from a buffer
	// being rewritten concurrently (a known minio-go race, documented in the implementation record),
	// producing an object of the right length at the right key holding the wrong bytes. Only a
	// read-back sees that.
	//
	// Off by default because it doubles the bytes read from the store on every upload. Turn it on
	// for any deployment where the object store is not trusted to return what it acknowledged.
	VerifyAfterPut bool `yaml:"verify_after_put"`
	// VerifyMaxBytes skips the read-back above this size. Zero verifies everything.
	VerifyMaxBytes int64 `yaml:"verify_max_bytes"`
}

// UseTLS reports whether the connection must use https.
func (s S3Storage) UseTLS() bool {
	if s.Secure == nil {
		return true
	}
	return *s.Secure
}

type LimitsConfig struct {
	MaxFileBytes          int64 `yaml:"max_file_bytes"`
	DefaultFileQuotaBytes int64 `yaml:"default_file_quota_bytes"`
	MaxProfindChildren    int   `yaml:"max_propfind_children"`
	UploadChunkBytes      int64 `yaml:"upload_chunk_bytes"`
	// BlobScrubPerPass is how many blobs the integrity scrub reads per cycle. Zero disables it.
	//
	// This is a count rather than a schedule because the cost is proportional to bytes read, not
	// to elapsed time: a small store is fully covered every cycle, and a large one is sampled. Set
	// it to roughly "how many blobs you are willing to read per sweep interval", which for a
	// multi-terabyte store is a few hundred.
	//
	// Zero is the default and means no scrub. That is deliberate: reading stored content back is
	// real I/O, and a deployment should opt into it rather than discover it.
	BlobScrubPerPass int `yaml:"blob_scrub_per_pass"`
	// MaxTrashBytes caps RETAINED trash usage per account (§4.3). Zero means unlimited, which is
	// the safe default: a deployment that never set this must not discover a limit by being unable
	// to delete a file.
	MaxTrashBytes int64 `yaml:"max_trash_bytes"`
	// MaxPendingUploadBytes caps the DECLARED size of one account's unfinished uploads (§4.3). Zero
	// means unlimited. Declared rather than written: the point is to stop a client reserving
	// unbounded staging space, and a session that never receives its bytes is reclaimed by the TTL.
	MaxPendingUploadBytes int64 `yaml:"max_pending_upload_bytes"`
	// MaxRetainedVersionBytes caps retained HISTORY per account in bytes (§4.3). Zero means
	// unlimited. Enforced by a sweep, not on the write path: refusing a save because the HISTORY is
	// full would lose the content the user is trying to write.
	MaxRetainedVersionBytes int64 `yaml:"max_retained_version_bytes"`
	// BlobScrubIntervalSeconds is how often the scrub runs. Zero uses the sweep cadence (6h).
	//
	// Separate from the sweep because corrupted content is urgent in a way unreferenced content is
	// not: a user cannot open the file either way, and the question is only how long until somebody
	// knows. Six hours is the floor set by the default; an hour is a reasonable production value.
	BlobScrubIntervalSeconds int `yaml:"blob_scrub_interval_seconds"`

	// Retention (§8.2). Every value here was previously either missing or a
	// constant inside a handler, which meant an operator could not state a data
	// retention policy at all — the requirement's own parameter list was
	// unmeetable.
	//
	// Zero means "keep forever". That is the safe default: a deployment that has
	// not thought about retention must not silently delete data.
	VersionRetentionDays int `yaml:"version_retention_days"`
	VersionCountLimit    int `yaml:"version_count_limit"`
	TrashRetentionDays   int `yaml:"trash_retention_days"`
	UploadSessionTTLHrs  int `yaml:"upload_session_ttl_hours"`

	// UploadCleanupGraceHours is how old a partial upload must be before it is discarded.
	//
	// §9.3: 清理未完成 Multipart Upload. It must exceed the longest upload the deployment
	// accepts, or the sweep aborts transfers that are still running. Zero disables the sweep
	// deliberately rather than by omission: there is no grace period that is safe for every
	// deployment, because "safe" depends on how large a file this one expects to receive.
	UploadCleanupGraceHours int `yaml:"upload_cleanup_grace_hours"`

	// ThumbnailIsolation controls where image decoding happens (§6.3, WC-GAP-004).
	//
	//   "process" (default): decode in a child process that can be killed.
	//   "inline":            decode in the server process, bounded only by the pixel cap and a
	//                        deadline that cannot stop the decoder.
	//
	// The default is "process" because in-process decoding lets a crafted file keep consuming CPU
	// in the request-serving process after the timeout has been returned to the client, which is
	// the one resource a hostile upload can take without ever completing a request.
	ThumbnailIsolation string `yaml:"thumbnail_isolation"`

	// CommentRatePerHour bounds how many comments one account may post in a rolling hour
	// (core-scope §6).
	//
	// The anonymous-abuse lesson from the share OTP endpoint applies to every user-writable append
	// surface: a table anybody can write to without a bound is a table somebody will fill. A rate
	// rather than a count because the failure is a BURST, and a total cap would either be too low
	// for a normal year or too high to stop a spam run.
	//
	// 60 is a generous ceiling for a human (a comment a minute for an hour) and far below what a
	// script needs. Zero or negative disables the limit, which an operator might do on a small
	// trusted deployment — the startup log does not warn about it, because unlike a security
	// control it is a policy about noise rather than about access.
	CommentRatePerHour int `yaml:"comment_rate_per_hour"`
}

// VersionRetention is how long an old version is kept. Zero means forever.
func (l LimitsConfig) VersionRetention() time.Duration {
	return time.Duration(l.VersionRetentionDays) * 24 * time.Hour
}

// UploadCleanupGrace is how old a partial upload must be before it is discarded.
func (l LimitsConfig) UploadCleanupGrace() time.Duration {
	return time.Duration(l.UploadCleanupGraceHours) * time.Hour
}

// TrashRetention is how long a deleted item stays restorable. Zero means forever.
func (l LimitsConfig) TrashRetention() time.Duration {
	return time.Duration(l.TrashRetentionDays) * 24 * time.Hour
}

// UploadSessionTTL is how long an unfinished resumable upload may sit idle.
func (l LimitsConfig) UploadSessionTTL() time.Duration {
	if l.UploadSessionTTLHrs <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(l.UploadSessionTTLHrs) * time.Hour
}

// Thumbnail isolation modes (§6.3, WC-GAP-004).
const (
	// ThumbnailIsolationProcess decodes in a child process that a deadline can kill.
	ThumbnailIsolationProcess = "process"
	// ThumbnailIsolationInline decodes in the server process. The pixel cap and the deadline still
	// apply, but the deadline cannot stop a running decoder.
	ThumbnailIsolationInline = "inline"
)

// ThumbnailIsolated reports whether image decoding should run in a child process.
//
// Any value other than the explicit opt-out isolates, so a typo or a missing key gets the safe
// behaviour. The opposite default would mean a single misspelling silently restored the in-process
// decoder on a deployment that asked for isolation.
func (l LimitsConfig) ThumbnailIsolated() bool {
	return !strings.EqualFold(strings.TrimSpace(l.ThumbnailIsolation), ThumbnailIsolationInline)
}

// MaxVersionsFor reports how many versions to keep for a layout.
//
// The cap is layout-aware because the cost differs by an order of magnitude: the
// tree layout stores each version as a full copy, the blob layout stores
// differences. It is a method rather than two constants in two handlers — which is
// what it used to be, and those constants could not be configured.
func (l LimitsConfig) MaxVersionsFor(cheapVersions bool) int {
	if l.VersionCountLimit > 0 {
		return l.VersionCountLimit
	}
	if cheapVersions {
		return 10
	}
	return 3
}

// JITConfig governs just-in-time provisioning of accounts on first login.
//
// Three modes rather than a boolean, because the middle one is what most organisations
// actually want: nobody is admitted silently, but an administrator does not have to
// pre-create every account either.
type JITConfig struct {
	// Mode is "open", "pending" or "deny".
	//
	//   open    - any identity the IdP asserts gets an account immediately (the old
	//             behaviour, and the default)
	//   pending - the account is created disabled with status "pending"; an
	//             administrator must approve it before it can be used
	//   deny    - the identity is refused; accounts must be pre-created
	Mode string `yaml:"mode"`
	// AllowedDomains limits which email domains may be provisioned at all, in every
	// mode. Empty means any domain.
	//
	// It is not a substitute for the IdP: an IdP that lets anyone register is the real
	// gate. It is a second lock for the case where the IdP's tenant is wider than the
	// set of people who should reach this deployment.
	AllowedDomains []string `yaml:"allowed_domains"`
}

// JIT mode constants.
const (
	JITOpen    = "open"
	JITPending = "pending"
	JITDeny    = "deny"
)

type AuditConfig struct {
	RetentionDays int `yaml:"retention_days"`
	// AnchorDir is where daily chain-head checkpoints are published (INV-09).
	//
	// Empty disables anchoring. That is a real choice with a real consequence: a chain
	// that is never anchored cannot detect its own truncation, because an attacker who
	// rewrites the tail leaves a log that still verifies.
	//
	// It must point somewhere with *different credentials* from the database. A
	// directory on the same volume as the metadata store makes the anchor decorative,
	// and the deployment documentation says so.
	AnchorDir string `yaml:"anchor_dir"`
	// AnchorSigningKeyFile is the Ed25519 private key that signs published checkpoints (§10.2).
	//
	// Empty publishes UNSIGNED checkpoints, which still detect truncation as long as the anchor
	// directory is write-protected — the location carries that property. A signed commit is the
	// stronger one: it survives an attacker who can write to the anchor directory but does not hold
	// the key, which is exactly the threat an unsigned file cannot answer.
	AnchorSigningKeyFile string `yaml:"anchor_signing_key_file"`
	// ArchiveDir is where `newcloud audit prune` writes the events it is about to remove.
	//
	// Empty means ./audit-archive. It should point at the same kind of place as the anchor —
	// somewhere the database cannot rewrite — because the archive is the only remaining copy of
	// that part of the chain, and the prune mark records its path as the answer to "where did it go".
	ArchiveDir string `yaml:"archive_dir"`
}

// MountsConfig governs external storage mounts (ADR-0014): SMB/SFTP/FTP/WebDAV-backed folders
// that are browsed in place, never copied.
//
// The whole section is disabled by default, because a mount is a credential the server spends
// on the operator's behalf: enabling it is the decision to let the file service talk to hosts
// it did not choose itself.
type MountsConfig struct {
	// Enabled turns on the mount surface: the /api/mounts routes and the virtual resolution
	// below mount points. Disabled, existing mounts resolve as ordinary (empty) folders and
	// the routes answer 503 — the data behind them was never local, so there is nothing to
	// migrate off.
	Enabled bool `yaml:"enabled"`
	// SecretFile is the hex-encoded 32-byte key that encrypts mount credentials at rest
	// (AES-256-GCM). Required when Enabled: mount credentials are live secrets to external
	// systems, and storing them in plaintext in the metadata store would make every future
	// database leak a credential leak too.
	SecretFile string `yaml:"secret_file"`
	// AllowUserMounts lets any signed-in user create mounts in their own tree. Off by default:
	// creating a mount is an operator action, because it points the server at a host of the
	// creator's choosing (ADR-0014, security posture).
	AllowUserMounts bool `yaml:"allow_user_mounts"`
	// DenyPrivateAddresses refuses user-created mounts whose endpoint resolves to loopback,
	// link-local or private address space — the SSRF guard for the user-mount mode. Admin
	// mounts are exempt: an administrator already controls the process.
	DenyPrivateAddresses bool `yaml:"deny_private_addresses"`
	// TimeoutSec bounds every dial and per-operation round trip to a mount. Slow remote
	// storage must degrade into 502s, not hold request goroutines.
	TimeoutSec int `yaml:"timeout_seconds"`
	// MaxListEntries caps one directory listing fetched from a mount. The store's listings
	// page; a remote listing arrives whole, so the cap is the honest alternative — refuse
	// with "too many entries" rather than silently truncating, which would look like an
	// empty bottom half of the folder.
	MaxListEntries int `yaml:"max_list_entries"`
	// MaxFileBytes caps a single write into a mount. Local writes have limits.max_file_bytes;
	// external writes must have their own, because they consume a request goroutine and a
	// remote handle for the whole transfer. Zero means "use limits.max_file_bytes".
	MaxFileBytes int64 `yaml:"max_file_bytes"`
	// AllowedSchemes is the dial allowlist. Default: the schemes with a provider compiled in
	// today. An operator can narrow it; widening it to "smb" changes nothing until an SMB
	// provider exists (the dial refuses with a clear error rather than 500ing).
	AllowedSchemes []string `yaml:"allowed_schemes"`
}

// DefaultMountSchemes are the schemes a provider exists for out of the box (ADR-0014).
var DefaultMountSchemes = []string{"webdav", "ftp", "sftp"}

// SearchConfig governs the OPTIONAL content-search engine (core-scope §7.2, ADR-0016).
//
// The default is `none`, and that is a decision rather than an omission: a deployment without
// Elasticsearch still has working name-and-filter search, and the response says `mode: name` so a
// user is never told a document does not exist when the truth is that nothing looked inside it.
// The single-binary principle is preserved by making the engine optional, not by pretending
// full-text search does not exist.
type SearchConfig struct {
	// Engine is "none" or "elasticsearch".
	Engine string `yaml:"engine"`
	// Elasticsearch is the cluster configuration. Validated only when Engine says so: a
	// deployment that turned the engine off must not fail to start because a URL it does not use
	// is malformed.
	Elasticsearch ElasticsearchConfig `yaml:"elasticsearch"`
	// ContentTypes is the set of document types whose TEXT is extracted and indexed (§7.2 v1:
	// text, markdown, pdf).
	//
	// Office extraction is deliberately out: it needs an extractor service, and promising it
	// without one is worse than not offering it. A file whose type is not here is still indexed by
	// its METADATA, so it stays findable by name, kind, tag and date.
	ContentTypes []string `yaml:"content_types"`
	// IndexBytes is the largest body the indexer reads (limits.search_index_bytes). A document
	// above it is indexed without content: the metadata still answers "what is this file", and
	// reading a two-gigabyte PDF during a sweep is not worth it.
	IndexBytes int64 `yaml:"index_bytes"`
}

// ElasticsearchConfig is the cluster connection (search.elasticsearch.*).
type ElasticsearchConfig struct {
	// URLs is the cluster's HTTP endpoints, tried in order.
	URLs []string `yaml:"urls"`
	// Username and Password authenticate. PasswordFile is preferred in a container: a mounted
	// secret never appears in `docker inspect`.
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	// IndexPrefix namespaces this deployment's index so two deployments can share a cluster.
	IndexPrefix string `yaml:"index_prefix"`
	// InsecureSkipVerify is for a self-signed certificate on a private network. It removes the
	// only protection against an active attacker on that path, so startup logs it.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// TimeoutSeconds bounds every engine request. A search that hangs is a page that hangs.
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// SearchEnabled reports whether a content-search engine is configured.
func (c *Config) SearchEnabled() bool { return c.Search.Engine == "elasticsearch" }

// SearchTimeout is the per-request bound for the engine.
func (e ElasticsearchConfig) Timeout() time.Duration {
	if e.TimeoutSeconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(e.TimeoutSeconds) * time.Second
}

// Default returns the safe baseline. Everything that can be wrong by omission
// is set here rather than left to zero values.
func Default() Config {
	return Config{
		Server: ServerConfig{Addr: ":8080"},
		Auth: AuthConfig{
			Scopes:     []string{"openid", "profile", "email", "groups"},
			SessionTTL: 12 * time.Hour,
			GroupRoleMap: map[string]Role{
				"cloud-admins": RoleAdmin,
				"cloud-users":  RoleUser,
			},
			JIT: JITConfig{Mode: JITOpen},
		},
		Storage: StorageConfig{
			Backend: "local",
			Layout:  LayoutBlob,
			Local:   LocalStorage{Root: "/var/lib/newcloud/blobs"},
		},
		Limits: LimitsConfig{
			MaxFileBytes:          1 << 40,  // 1 TiB
			DefaultFileQuotaBytes: 10 << 30, // 10 GiB
			MaxProfindChildren:    100_000,
			UploadChunkBytes:      256 << 20, // 256 MiB
			// Decode in a child process by default (§6.3, WC-GAP-004).
			ThumbnailIsolation: ThumbnailIsolationProcess,
			// A comment a minute, on average, over an hour. See LimitsConfig.CommentRatePerHour.
			CommentRatePerHour: 60,
		},
		Audit: AuditConfig{RetentionDays: 365},
		Mounts: MountsConfig{
			// Private-address refusal is the safe default for the day user mounts are
			// turned on; it is only reachable then, but a default that must be remembered
			// to be safe is a default that will be forgotten.
			DenyPrivateAddresses: true,
			TimeoutSec:           30,
			MaxListEntries:       5_000,
			AllowedSchemes:       DefaultMountSchemes,
		},
		Search: SearchConfig{
			// "none" is the default: optional means off unless an operator decides otherwise, and
			// name+filter search works without it.
			Engine: "none",
			// §7.2 names exactly these three. Office extraction needs an extractor service and is
			// a separate decision, not a silent gap.
			ContentTypes: []string{"text", "markdown", "pdf"},
			// 10 MiB of extracted text is far more than any document whose body is worth searching,
			// and small enough that a sweep cannot read a whole archive in one pass.
			IndexBytes: 10 << 20,
			Elasticsearch: ElasticsearchConfig{
				IndexPrefix:    "newcloud",
				TimeoutSeconds: 5,
			},
		},
	}
}

// Load reads path (YAML), overlays environment variables, then validates.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		// Decode onto the defaults so omitted keys keep their safe values.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}

	applyEnv(&cfg)

	if cfg.Auth.ClientSecret == "" && cfg.Auth.ClientSecretFile != "" {
		secret, err := os.ReadFile(cfg.Auth.ClientSecretFile)
		if err != nil {
			return cfg, fmt.Errorf("read client_secret_file: %w", err)
		}
		cfg.Auth.ClientSecret = strings.TrimSpace(string(secret))
	}

	// The same file-over-inline preference applies to the mail password: a mounted
	// secret never appears in `docker inspect` or a process listing.
	//
	// Only read when mail is actually enabled. A disabled transport still names a secret
	// path in every example configuration, and demanding that the file exist turns the
	// shipped example into one that cannot start - which is how an operator concludes the
	// documentation is wrong rather than that they misconfigured something.
	if cfg.Mail.Enabled && cfg.Mail.Password == "" && cfg.Mail.PasswordFile != "" {
		secret, err := os.ReadFile(cfg.Mail.PasswordFile)
		if err != nil {
			return cfg, fmt.Errorf("read mail.password_file: %w", err)
		}
		cfg.Mail.Password = strings.TrimSpace(string(secret))
	}

	// The search engine's credential, on the same rule as mail: read only when the engine is
	// actually enabled, because an example configuration that names a secret path must not make a
	// deployment fail to start when the engine is off.
	if cfg.SearchEnabled() && cfg.Search.Elasticsearch.Password == "" &&
		cfg.Search.Elasticsearch.PasswordFile != "" {
		secret, err := os.ReadFile(cfg.Search.Elasticsearch.PasswordFile)
		if err != nil {
			return cfg, fmt.Errorf("read search.elasticsearch.password_file: %w", err)
		}
		cfg.Search.Elasticsearch.Password = strings.TrimSpace(string(secret))
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(c *Config) {
	set := func(dst *string, key string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	set(&c.Server.Addr, "NEWCLOUD_ADDR")
	set(&c.Server.PublicURL, "NEWCLOUD_PUBLIC_URL")
	set(&c.Server.UserContentURL, "NEWCLOUD_USERCONTENT_URL")
	set(&c.Auth.Issuer, "NEWCLOUD_OIDC_ISSUER")
	set(&c.Auth.ClientID, "NEWCLOUD_OIDC_CLIENT_ID")
	set(&c.Auth.ClientSecret, "NEWCLOUD_OIDC_CLIENT_SECRET")
	set(&c.Auth.SessionSecret, "NEWCLOUD_SESSION_SECRET")
	set(&c.Auth.LocalAdmin.Username, "NEWCLOUD_BREAKGLASS_USER")
	set(&c.Auth.SCIMToken, "NEWCLOUD_SCIM_TOKEN")

	if v := os.Getenv("NEWCLOUD_STORAGE_LAYOUT"); v != "" {
		c.Storage.Layout = Layout(v)
	}

	if v := os.Getenv("NEWCLOUD_ALLOW_LOCAL_ADMIN"); v == "1" || strings.EqualFold(v, "true") {
		c.Auth.AllowLocalAdmin = true
	}
}

// ErrInvalidConfig wraps every validation failure so callers can distinguish
// "bad deployment" from "runtime error".
var ErrInvalidConfig = errors.New("invalid configuration")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

// Validate rejects configurations that are structurally unsafe.
func (c *Config) Validate() error {
	// Fresh findings per run: Validate can be called more than once (Load does, and tests
	// call it directly), and a warning that survived from a previous pass would be
	// reported for a configuration that no longer has the problem.
	c.warnings = nil

	if c.Server.PublicURL == "" {
		return invalid("server.public_url is required (used for share links and OIDC redirect)")
	}
	pub, err := url.Parse(c.Server.PublicURL)
	if err != nil || pub.Scheme == "" || pub.Host == "" {
		return invalid("server.public_url must be an absolute URL, got %q", c.Server.PublicURL)
	}

	// Content isolation: user-supplied bytes must never be served from the same
	// origin as the authenticated app shell, otherwise stored XSS in a shared
	// file becomes session theft.
	if c.Server.UserContentURL == "" {
		return invalid("server.usercontent_url is required; user content must be served from a separate origin")
	}
	uc, err := url.Parse(c.Server.UserContentURL)
	if err != nil || uc.Scheme == "" || uc.Host == "" {
		return invalid("server.usercontent_url must be an absolute URL, got %q", c.Server.UserContentURL)
	}

	// The scheme has to be one the browser actually treats as a secure context or plain web
	// origin. Anything else (a typo like "https:/cloud", or an invented scheme) parses as an
	// absolute URL above and passes, and the deployment only notices when a redirect or a
	// share link is built from it.
	//
	// Plain http is legal — dev deployments and air-gapped LANs — but it is a decision, so
	// it is reported as a warning rather than passed in silence (see Warnings).
	for name, u := range map[string]*url.URL{"server.public_url": pub, "server.usercontent_url": uc} {
		switch strings.ToLower(u.Scheme) {
		case "http":
			c.warnings = append(c.warnings, name+" uses plain http: share links, OIDC redirects and "+
				"session cookies will not be encrypted. Acceptable for development only")
		case "https":
		default:
			return invalid("%s must use http or https, got scheme %q", name, u.Scheme)
		}
	}
	if sameOrigin(pub, uc) {
		return invalid("server.usercontent_url must differ from public_url in scheme, host or port (got the same origin)")
	}

	// The content origin must be a different HOSTNAME, not merely a different port or scheme.
	//
	// The origin check above is about the browser's same-origin policy: it is what stops an
	// uploaded HTML file from reading the app DOM. It is not the whole isolation story, because
	// cookies are keyed by hostname, PORT AND SCHEME ARE NOT PART OF THE KEY. So
	// app https://cloud.example.com and content https://cloud.example.com:8443 are two web
	// origins that still share one cookie jar: a request to the content listener carries
	// __Host-wc_session, and every proxy, log and sidecar on that host sees a live session
	// credential for a request that should never have needed one.
	//
	// The content route does not authenticate from the cookie, so this is not by itself a
	// session-theft path; it is the reason the "a separate origin protects the session cookie"
	// claim was too strong. Requiring a distinct host is the configuration that makes the
	// claim true. A deployment that genuinely needs the same host can set
	// server.allow_same_host_content and accept the documented consequence.
	if strings.EqualFold(contentHost(pub), contentHost(uc)) && !c.Server.AllowSameHostContent {
		return invalid("server.usercontent_url must use a different HOSTNAME from public_url, got %q on both. "+
			"A different port or scheme is a different browser origin but the SAME cookie jar, so the "+
			"session cookie would be sent to the content listener. Use a separate host (ideally a "+
			"separate registrable domain), or set server.allow_same_host_content: true and document "+
			"that the cookie reaches the content origin",
			pub.Hostname())
	}

	// §9.4 可信源列表. A malformed entry is rejected rather than skipped: an operator who writes
	// 10.0.0.0/8 expecting a whole network and silently gets no trusted proxies at all would see
	// every client arrive as the reverse proxy's address, and would have no way to tell why the
	// per-IP limits had collapsed into one.
	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return invalid("server.trusted_proxies entry %q is not a CIDR (%v)", cidr, err)
		}
	}

	if c.Auth.Issuer == "" {
		return invalid("auth.issuer is required (Pocket ID base URL)")
	}
	iss, err := url.Parse(c.Auth.Issuer)
	if err != nil || iss.Scheme == "" || iss.Host == "" {
		return invalid("auth.issuer must be an absolute URL, got %q", c.Auth.Issuer)
	}
	if c.Auth.ClientID == "" {
		return invalid("auth.client_id is required")
	}
	if c.Auth.ClientSecret == "" {
		return invalid("auth.client_secret (or client_secret_file) is required")
	}

	// Pocket ID only emits the groups claim when the scope is requested.
	// Without it nobody has a role, so fail loudly instead of silently
	// downgrading every user to "no access".
	if !contains(c.Auth.Scopes, "groups") {
		return invalid("auth.scopes must include \"groups\": Pocket ID only releases group membership when the scope is requested")
	}
	// openid and email are the two scopes the login itself depends on, not merely
	// enriches: without openid the provider issues no ID token, so the OAuth callback
	// has nothing to verify and LOGIN CANNOT WORK AT ALL; without email there is no
	// verified address, so account provisioning cannot name the person and share
	// invitations by email have no recipient. A deployment that starts without them
	// looks healthy until the first sign-in fails, which is the wrong time to learn it.
	for _, need := range []struct{ scope, why string }{
		{"openid", "without it the provider issues no ID token and login cannot work"},
		{"email", "without it no account has a verified address and provisioning cannot complete"},
	} {
		if !contains(c.Auth.Scopes, need.scope) {
			return invalid("auth.scopes must include %q: %s", need.scope, need.why)
		}
	}

	if len([]byte(c.Auth.SessionSecret)) < 32 {
		return invalid("auth.session_secret must be at least 32 bytes")
	}

	// The session lifetime was the one duration nothing checked. Zero reaches
	// Sessions.Mint as a zero expiry: the ticket is minted already expired, so the login
	// appears to succeed and the very next request is refused — a failure that looks like
	// a broken IdP rather than a misconfigured duration.
	if c.Auth.SessionTTL <= 0 {
		return invalid("auth.session_ttl must be positive (how long a browser session lives), got %s",
			c.Auth.SessionTTL)
	}

	// An unknown isolation mode is rejected rather than defaulted. Defaulting would be safe (any
	// value other than "inline" isolates), but it would also silently accept "proces" and leave an
	// operator believing they had turned something on that they had not.
	switch strings.ToLower(strings.TrimSpace(c.Limits.ThumbnailIsolation)) {
	case "", ThumbnailIsolationProcess, ThumbnailIsolationInline:
	default:
		return invalid("limits.thumbnail_isolation must be %q or %q, got %q",
			ThumbnailIsolationProcess, ThumbnailIsolationInline, c.Limits.ThumbnailIsolation)
	}

	for name, role := range c.Auth.GroupRoleMap {
		switch role {
		case RoleAdmin, RoleUser, RoleGuest:
		default:
			return invalid("auth.group_role_map[%q]: unknown role %q", name, role)
		}
	}

	// The search engine (§7.2). Validated here for the same reason mail is: a key that is accepted,
	// documented and silently ignored is the failure mode the configuration ratchet exists to stop,
	// and an engine that is configured but unreachable must be a startup ERROR rather than a
	// deployment that quietly never has content search.
	switch strings.ToLower(strings.TrimSpace(c.Search.Engine)) {
	case "", "none":
		c.Search.Engine = "none"
	case "elasticsearch":
		c.Search.Engine = "elasticsearch"
		if len(c.Search.Elasticsearch.URLs) == 0 {
			return invalid("search.engine=elasticsearch requires search.elasticsearch.urls")
		}
		for _, raw := range c.Search.Elasticsearch.URLs {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || u.Scheme == "" || u.Host == "" {
				return invalid("search.elasticsearch.urls: %q is not an absolute URL", raw)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return invalid("search.elasticsearch.urls: %q must be http or https", raw)
			}
			// An engine reached over plain HTTP on a public address hands the credentials and the
			// document bodies to anyone on the path. On a private network, which is where most
			// single-host deployments put it, that is a normal choice; saying so is not.
			if u.Scheme == "http" && !isLocallyRouted(u.Hostname()) {
				c.warnings = append(c.warnings, fmt.Sprintf(
					"search.elasticsearch.urls[%q] uses plain http; document text and the engine "+
						"credential travel unencrypted", raw))
			}
		}
		if c.Search.Elasticsearch.InsecureSkipVerify {
			c.warnings = append(c.warnings, "search.elasticsearch.insecure_skip_verify is set: the "+
				"engine's TLS certificate is not verified, so an active attacker on that path can "+
				"read the indexed document text")
		}
		// The index prefix becomes part of a URL path and an index name; Elasticsearch restricts
		// it to lowercase, digits and a few separators, and a prefix with a slash would address a
		// different endpoint entirely.
		prefix := strings.TrimSpace(c.Search.Elasticsearch.IndexPrefix)
		if prefix == "" {
			c.Search.Elasticsearch.IndexPrefix = "newcloud"
		} else if !validIndexPrefix(prefix) {
			return invalid("search.elasticsearch.index_prefix must be lowercase letters, digits, "+
				"- or _, got %q", prefix)
		}
	default:
		// Refused, not defaulted. The whole reason this switch is validated is that a key which is
		// accepted and ignored is worse than one that does not exist: `engine: opensearch` would
		// otherwise start cleanly, report content search as disabled, and be indistinguishable from
		// an operator who chose `none` — with the user-facing symptom being "my document is not
		// there". (Found by TestAnUnknownEngineIsRefused.)
		return invalid("search.engine must be %q or %q, got %q", "none", "elasticsearch",
			c.Search.Engine)
	}

	if c.Search.IndexBytes < 0 {
		return invalid("search.index_bytes must not be negative, got %d", c.Search.IndexBytes)
	}
	for _, ct := range c.Search.ContentTypes {
		switch strings.ToLower(strings.TrimSpace(ct)) {
		case "", "text", "markdown", "pdf":
		default:
			// Refused rather than ignored: an operator who added "docx" would otherwise believe
			// Office documents were searchable, which is the "silently worse than promised"
			// outcome §7.2 names.
			return invalid("search.content_types: %q has no extractor in this build; supported "+
				"types are text, markdown and pdf", ct)
		}
	}

	// JIT defaults to open when unset, so an existing configuration file keeps working.
	// The modes are validated rather than guessed: an unrecognised value here decides who
	// gets an account, and silently falling back to the permissive one is the wrong guess.
	if c.Auth.JIT.Mode == "" {
		c.Auth.JIT.Mode = JITOpen
	}
	switch c.Auth.JIT.Mode {
	case JITOpen, JITPending, JITDeny:
	default:
		return invalid("auth.jit.mode: want %q, %q or %q, got %q",
			JITOpen, JITPending, JITDeny, c.Auth.JIT.Mode)
	}

	if c.Auth.AllowLocalAdmin {
		// A break-glass account without an address restriction is a
		// password-protected root login reachable from the internet.
		if len(c.Auth.LocalAdmin.AllowedCIDRs) == 0 {
			return invalid("auth.allow_local_admin=true requires auth.local_admin.allowed_cidrs")
		}
		if c.Auth.LocalAdmin.Username == "" || c.Auth.LocalAdmin.PasswordFile == "" {
			return invalid("auth.allow_local_admin=true requires username and password_argon2_file")
		}
	}

	if c.Limits.MaxFileBytes <= 0 {
		return invalid("limits.max_file_bytes must be positive")
	}
	if c.Limits.UploadChunkBytes <= 0 || c.Limits.UploadChunkBytes > c.Limits.MaxFileBytes {
		return invalid("limits.upload_chunk_bytes must be positive and <= max_file_bytes")
	}
	if c.Limits.MaxProfindChildren <= 0 {
		return invalid("limits.max_propfind_children must be positive")
	}

	if c.Mounts.Enabled {
		if c.Mounts.SecretFile == "" {
			return invalid("mounts.enabled=true requires mounts.secret_file (the key that encrypts mount credentials)")
		}
		if c.Mounts.TimeoutSec < 5 || c.Mounts.TimeoutSec > 300 {
			return invalid("mounts.timeout_seconds must be between 5 and 300")
		}
		if c.Mounts.MaxListEntries <= 0 {
			return invalid("mounts.max_list_entries must be positive")
		}
		if c.Mounts.MaxFileBytes < 0 {
			return invalid("mounts.max_file_bytes must not be negative")
		}
		if len(c.Mounts.AllowedSchemes) == 0 {
			return invalid("mounts.allowed_schemes must name at least one scheme (the default is webdav, ftp, sftp)")
		}
	}

	switch c.Storage.Backend {
	case "local":
		if c.Storage.Local.Root == "" {
			return invalid("storage.local.root is required when backend=local")
		}
	case "s3":
		if c.Storage.S3.Bucket == "" {
			return invalid("storage.s3.bucket is required when backend=s3")
		}
		if c.Storage.S3.Endpoint == "" {
			return invalid("storage.s3.endpoint is required when backend=s3")
		}
		if (c.Storage.S3.AccessKey == "") != (c.Storage.S3.SecretKey == "") {
			return invalid("storage.s3.access_key and secret_key must be set together, or both left empty to use the environment chain")
		}
	default:
		return invalid("storage.backend must be \"local\" or \"s3\", got %q", c.Storage.Backend)
	}

	// The layout is independent of the backend, but not every combination is
	// implemented. Fail at startup rather than at the first upload.
	switch c.Storage.Layout {
	case LayoutBlob:
		// Supported on both backends.
	case LayoutTree:
		if c.Storage.Backend != "local" {
			return invalid("storage.layout \"tree\" requires backend \"local\": " +
				"an object store has no directory hierarchy to mirror")
		}
	case "":
		return invalid("storage.layout is required (\"blob\" for content-addressed storage, " +
			"\"tree\" to mirror the user-visible hierarchy on disk)")
	default:
		return invalid("storage.layout must be \"blob\" or \"tree\", got %q", c.Storage.Layout)
	}

	// Mail is optional, but a half-configured relay must not start: it would accept
	// invitations by email and silently fail to deliver every one of them.
	if c.Mail.Enabled {
		if c.Mail.Host == "" {
			return invalid("mail.host is required when mail.enabled is true")
		}
		if c.Mail.From == "" {
			return invalid("mail.from is required when mail.enabled is true")
		}
		// `<= 0`, not `< 0`: the message below has always said "between 1 and 65535",
		// and a zero port slipped past it. The transport happens to assume 587 for a
		// zero port, which is a silent guess about a production relay — exactly the
		// kind of half-configured value this block exists to name.
		if c.Mail.Port <= 0 || c.Mail.Port > 65535 {
			return invalid("mail.port must be between 1 and 65535, got %d", c.Mail.Port)
		}
		if c.Mail.ImplicitTLS && c.Mail.StartTLS {
			return invalid("mail.implicit_tls and mail.start_tls are mutually exclusive")
		}
	}

	return nil
}

// sameOrigin reports whether two URLs share an origin.
//
// Origin is scheme + host + port only. The path is deliberately ignored: two
// URLs on the same origin share cookies and can reach each other from the DOM,
// which is exactly the property content isolation must break. Serving user
// content under a different path prefix provides no isolation at all.
func sameOrigin(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	return strings.EqualFold(canonicalHost(a), canonicalHost(b))
}

// contentHost normalises a URL's hostname for the cookie-jar comparison.
//
// The trailing dot is the one that bites: "cloud.example.com." and "cloud.example.com" resolve to
// the same place and, per RFC 6265, a browser treats them as the same cookie domain. Comparing the
// raw Hostname() strings would accept the dotted form as a separate host and hand back exactly the
// shared cookie jar the check exists to refuse.
func contentHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func canonicalHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return host + ":" + port
}

// isLoopbackHost reports whether a URL host names this machine.
//
// Used by the search-engine warning: plain HTTP to localhost is a normal single-host choice, and
// plain HTTP to a public address is credentials and document text on the wire.
//
// The name says "locally routed" rather than "loopback" because the distinction the caller needs is
// not "is this the same host" but "can somebody off this machine read the traffic". A cluster at
// 10.0.0.5, or inside a container network at 172.17.0.2, is reached over a link nobody else is on —
// which is why plain HTTP to it is a normal choice, and why the earlier loopback-only version of
// this function contradicted the comment above its own call site.
//
// The ranges are IPv4 private (RFC 1918), IPv6 unique-local (RFC 4193) and link-local, because a
// container is routinely reached at 169.254.x.x or fe80::.
func isLocallyRouted(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// validIndexPrefix reports whether a string is a legal Elasticsearch index-name prefix.
//
// The name ends up in a URL path, so a prefix containing "/" or "?" would address a different
// endpoint than the one intended. Elasticsearch itself forbids uppercase, so accepting it here
// would produce a startup that succeeds and an index that never exists.
func validIndexPrefix(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return s != "" && s != "." && s != ".."
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}

// RoleForGroups maps Pocket ID group names to a local role.
//
// It is deliberately fail-closed: an unknown user lands on RoleGuest, never on
// RoleUser. Pocket ID sends group *names* (not display names), which is what
// the map keys are.
func (c *Config) RoleForGroups(groups []string) Role {
	best := RoleGuest
	for _, g := range groups {
		role, ok := c.Auth.GroupRoleMap[g]
		if !ok {
			continue
		}
		if roleRank(role) > roleRank(best) {
			best = role
		}
	}
	return best
}

func roleRank(r Role) int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleUser:
		return 2
	case RoleGuest:
		return 1
	default:
		return 0
	}
}

// BlobScrubInterval is the scrub period, defaulting to zero (the worker's sweep cadence).
func (l LimitsConfig) BlobScrubInterval() time.Duration {
	if l.BlobScrubIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(l.BlobScrubIntervalSeconds) * time.Second
}
