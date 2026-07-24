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
	TrustProxy     bool   `yaml:"trust_proxy"`
}

type AuthConfig struct {
	Issuer           string           `yaml:"issuer"`
	ClientID         string           `yaml:"client_id"`
	ClientSecret     string           `yaml:"client_secret"`
	ClientSecretFile string           `yaml:"client_secret_file"`
	Scopes           []string         `yaml:"scopes"`
	GroupRoleMap     map[string]Role  `yaml:"group_role_map"`
	SessionTTL       time.Duration    `yaml:"session_ttl"`
	SessionSecret    string           `yaml:"session_secret"`
	AllowLocalAdmin  bool             `yaml:"allow_local_admin"`
	LocalAdmin       LocalAdminConfig `yaml:"local_admin"`
	SCIMToken        string           `yaml:"scim_token"`
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
}

type LocalStorage struct {
	Root string `yaml:"root"`
}

type S3Storage struct {
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
	Region   string `yaml:"region"`
}

type LimitsConfig struct {
	MaxFileBytes          int64 `yaml:"max_file_bytes"`
	DefaultFileQuotaBytes int64 `yaml:"default_file_quota_bytes"`
	MaxProfindChildren    int   `yaml:"max_propfind_children"`
	UploadChunkBytes      int64 `yaml:"upload_chunk_bytes"`
}

type AuditConfig struct {
	RetentionDays int `yaml:"retention_days"`
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
	if cfg.Mail.Password == "" && cfg.Mail.PasswordFile != "" {
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
