package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

// TestExampleConfig keeps deploy/config.example.yml honest.
//
// An example configuration that fails validation is worse than no example: operators
// copy it verbatim, hit a startup error whose cause is not in their own file, and lose
// trust in the error messages. This test loads the shipped example with the
// file-based secrets substituted, so the rest of the document must actually parse.
func TestExampleConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "config.example.yml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}

	body := string(raw)

	// The example references secrets from files and a placeholder session secret, as
	// it should: shipping a real secret in an example is how one ends up in
	// production. Substitute concrete values here so validation can run.
	body = strings.ReplaceAll(body,
		`client_secret_file: "/run/secrets/oidc_client_secret"`,
		`client_secret: "example-client-secret"`)
	body = strings.ReplaceAll(body,
		`password_argon2_file: "/etc/newcloud/breakglass.pass"`,
		`password_argon2: "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHQ$c29tZWhhc2g"`)
	body = strings.ReplaceAll(body,
		`session_secret: "REPLACE-WITH-32-BYTES-OF-RANDOM"`,
		`session_secret: "0123456789abcdef0123456789abcdef"`)
	// Mail is disabled in the example, but the loader still resolves password_file when it
	// is present, so the substitution is needed for the document to load at all.
	body = strings.ReplaceAll(body,
		`password_file: "/run/secrets/smtp_password"`,
		`password: "example-smtp-password"`)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the shipped example must be a valid configuration: %v", err)
	}

	// Spot-check the values that carry design decisions, so a careless edit to the
	// example cannot silently change a default a reader relies on.
	if cfg.Storage.Layout != config.LayoutBlob {
		t.Errorf("example should ship an explicit layout, got %q", cfg.Storage.Layout)
	}
	if cfg.Storage.Backend != "local" {
		t.Errorf("example backend: %q", cfg.Storage.Backend)
	}
	if len(cfg.Auth.Scopes) == 0 {
		t.Error("scopes must be set: Pocket ID only releases groups when asked")
	} else if !contains(cfg.Auth.Scopes, "groups") {
		t.Error("the groups scope is required for role mapping")
	}
	if cfg.Server.UserContentURL == cfg.Server.PublicURL {
		t.Error("the content origin must differ from the app origin")
	}
	if cfg.Auth.AllowLocalAdmin {
		t.Error("break-glass must be off in the example")
	}
	if cfg.Auth.SessionSecret == "REPLACE-WITH-32-BYTES-OF-RANDOM" {
		t.Error("the example must not ship a usable session secret")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
