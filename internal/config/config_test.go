package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// valid returns a configuration that passes every check, so individual tests can
// break exactly one thing.
func valid() Config {
	c := Default()
	c.Server.PublicURL = "https://cloud.example.com"
	c.Server.UserContentURL = "https://usercontent.example.com"
	c.Auth.Issuer = "https://id.example.com"
	c.Auth.ClientID = "newcloud"
	c.Auth.ClientSecret = "s3cret"
	c.Auth.SessionSecret = strings.Repeat("k", 32)
	return c
}

func TestValidConfigPasses(t *testing.T) {
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatalf("baseline config must be valid: %v", err)
	}
}

// TestContentOriginMustDiffer guards the single most important deployment
// invariant: user-supplied bytes must not be served from the authenticated app
// origin, or stored XSS in a shared file becomes session theft.
func TestContentOriginMustDiffer(t *testing.T) {
	cases := map[string]string{
		"same host and scheme":  "https://cloud.example.com",
		"same host, other path": "https://cloud.example.com/files",
		"same host, uppercase":  "HTTPS://CLOUD.EXAMPLE.COM",
	}
	for name, ucurl := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid()
			c.Server.UserContentURL = ucurl
			err := c.Validate()
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
			if !strings.Contains(err.Error(), "usercontent_url") {
				t.Fatalf("error should name the field: %v", err)
			}
		})
	}
}

func TestContentOriginDifferenceIsAccepted(t *testing.T) {
	for _, ucurl := range []string{
		"https://usercontent.example.com",
		"https://cloud.example.com:8443", // different port is a different origin
		"http://cloud.example.com",       // different scheme is a different origin
	} {
		c := valid()
		c.Server.UserContentURL = ucurl
		if err := c.Validate(); err != nil {
			t.Fatalf("%s should be accepted: %v", ucurl, err)
		}
	}
}

// TestGroupsScopeRequired: Pocket ID only releases group membership when the
// groups scope is requested, so omitting it silently gives every user the
// lowest role. Fail at startup instead.
func TestGroupsScopeRequired(t *testing.T) {
	c := valid()
	c.Auth.Scopes = []string{"openid", "profile", "email"}
	err := c.Validate()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "groups") {
		t.Fatalf("error should mention groups: %v", err)
	}
}

// TestBreakglassRequiresCIDR: an unrestricted local admin is a
// password-protected root login reachable from the internet.
func TestBreakglassRequiresCIDR(t *testing.T) {
	c := valid()
	c.Auth.AllowLocalAdmin = true
	c.Auth.LocalAdmin = LocalAdminConfig{Username: "bg", PasswordFile: "/etc/bg"}
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unrestricted break-glass must be rejected, got %v", err)
	}

	c.Auth.LocalAdmin.AllowedCIDRs = []string{"10.0.0.0/8"}
	if err := c.Validate(); err != nil {
		t.Fatalf("scoped break-glass should be accepted: %v", err)
	}
}

func TestShortSessionSecretRejected(t *testing.T) {
	c := valid()
	c.Auth.SessionSecret = "short"
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want rejection, got %v", err)
	}
}

func TestStorageBackendValidation(t *testing.T) {
	c := valid()
	c.Storage.Backend = "smb"
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("unknown backend must be rejected: only local and s3 are supported")
	}

	c = valid()
	c.Storage.Backend = "s3"
	c.Storage.S3.Bucket = ""
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("s3 without a bucket must be rejected")
	}

	c.Storage.S3.Bucket = "cloud"
	if err := c.Validate(); err != nil {
		t.Fatalf("s3 with a bucket should pass: %v", err)
	}
}

func TestChunkSizeCannotExceedFileLimit(t *testing.T) {
	c := valid()
	c.Limits.MaxFileBytes = 100
	c.Limits.UploadChunkBytes = 200
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("chunk larger than the file limit must be rejected")
	}
}

// TestRoleForGroupsIsFailClosed: unknown users must land on guest, never user.
func TestRoleForGroupsFailClosed(t *testing.T) {
	c := valid()
	c.Auth.GroupRoleMap = map[string]Role{
		"cloud-admins": RoleAdmin,
		"cloud-users":  RoleUser,
	}

	cases := []struct {
		groups []string
		want   Role
	}{
		{nil, RoleGuest},
		{[]string{"unknown-group"}, RoleGuest},
		{[]string{"cloud-users"}, RoleUser},
		{[]string{"cloud-admins"}, RoleAdmin},
		{[]string{"cloud-users", "cloud-admins"}, RoleAdmin}, // highest wins
		{[]string{"unknown", "cloud-users"}, RoleUser},
	}
	for _, tc := range cases {
		if got := c.RoleForGroups(tc.groups); got != tc.want {
			t.Fatalf("groups %v: want %v, got %v", tc.groups, tc.want, got)
		}
	}
}

func TestLoadFromYAMLAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	// Deliberately omits scopes, session_ttl, limits: defaults must survive.
	yaml := `
server:
  public_url: https://cloud.example.com
  usercontent_url: https://usercontent.example.com
auth:
  issuer: https://id.example.com
  client_id: newcloud
  client_secret: s3cret
  session_secret: kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Auth.SessionTTL != 12*time.Hour {
		t.Fatalf("default session_ttl lost: %v", c.Auth.SessionTTL)
	}
	if len(c.Auth.Scopes) != 4 {
		t.Fatalf("default scopes lost: %v", c.Auth.Scopes)
	}
	if c.Limits.MaxProfindChildren != 100_000 {
		t.Fatalf("default propfind limit lost: %d", c.Limits.MaxProfindChildren)
	}
	if c.Limits.MaxFileBytes != 1<<40 {
		t.Fatalf("default max file size lost: %d", c.Limits.MaxFileBytes)
	}
}

func TestLoadReadsSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "client_secret")
	if err := os.WriteFile(secretPath, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yml")
	yaml := `
server:
  public_url: https://cloud.example.com
  usercontent_url: https://usercontent.example.com
auth:
  issuer: https://id.example.com
  client_id: newcloud
  client_secret_file: ` + secretPath + `
  session_secret: kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Auth.ClientSecret != "from-file" {
		t.Fatalf("secret file not trimmed/read: %q", c.Auth.ClientSecret)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	yaml := `
server:
  public_url: https://file.example.com
  usercontent_url: https://usercontent.example.com
auth:
  issuer: https://id.example.com
  client_id: newcloud
  client_secret: s3cret
  session_secret: kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NEWCLOUD_PUBLIC_URL", "https://env.example.com")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Server.PublicURL != "https://env.example.com" {
		t.Fatalf("env override ignored: %q", c.Server.PublicURL)
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatal("missing config file must be an error")
	}
}
