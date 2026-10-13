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
}

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
}

type LocalAdminConfig struct {
	Username     string   `yaml:"username"`
	PasswordFile string   `yaml:"password_argon2_file"`
	AllowedCIDRs []string `yaml:"allowed_cidrs"`
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
	// ArchiveDir is where `newcloud audit prune` writes the events it is about to remove.
	//
	// Empty means ./audit-archive. It should point at the same kind of place as the anchor —
	// somewhere the database cannot rewrite — because the archive is the only remaining copy of
	// that part of the chain, and the prune mark records its path as the answer to "where did it go".
	ArchiveDir string `yaml:"archive_dir"`
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
		},
		Audit: AuditConfig{RetentionDays: 365},
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
	if sameOrigin(pub, uc) {
		return invalid("server.usercontent_url must differ from public_url in scheme, host or port (got the same origin)")
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

	if len([]byte(c.Auth.SessionSecret)) < 32 {
		return invalid("auth.session_secret must be at least 32 bytes")
	}

	for name, role := range c.Auth.GroupRoleMap {
		switch role {
		case RoleAdmin, RoleUser, RoleGuest:
		default:
			return invalid("auth.group_role_map[%q]: unknown role %q", name, role)
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
		if c.Mail.Port < 0 || c.Mail.Port > 65535 {
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
