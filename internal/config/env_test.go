package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setServingEnv sets the minimum NEWCLOUD_* set that validates, so the tests below can
// override single keys on top of a legal configuration rather than fighting validation
// errors that have nothing to do with the variable under test.
func setServingEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NEWCLOUD_PUBLIC_URL", "https://cloud.example.com")
	t.Setenv("NEWCLOUD_USERCONTENT_URL", "https://usercontent.example.com")
	t.Setenv("NEWCLOUD_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("NEWCLOUD_OIDC_CLIENT_ID", "newcloud")
	t.Setenv("NEWCLOUD_OIDC_CLIENT_SECRET", "not-a-real-secret")
	// 32 bytes exactly: the floor validation enforces, so the helper cannot hide a
	// longer-than-needed secret that would pass for the wrong reason.
	t.Setenv("NEWCLOUD_SESSION_SECRET", "0123456789abcdef0123456789abcdef")
}

// The file-free deployment: Load("") with environment alone must produce a
// configuration that validates, because that is what `docker run` without a mounted
// YAML does.
func TestEnvironmentAloneIsAServingConfiguration(t *testing.T) {
	setServingEnv(t)
	t.Setenv("NEWCLOUD_USERCONTENT_ADDR", ":8081")
	t.Setenv("NEWCLOUD_STORAGE_ROOT", "/var/lib/newcloud/blobs")
	t.Setenv("NEWCLOUD_STORAGE_UPLOAD_DIR", "/var/lib/newcloud/uploads")
	t.Setenv("NEWCLOUD_TRUSTED_PROXIES", "172.16.0.0/12, 192.168.0.0/16")
	t.Setenv("NEWCLOUD_NOTIFY_QUOTA_WARNING_PERCENT", "75")
	t.Setenv("NEWCLOUD_ANTIVIRUS_ENABLED", "true")
	t.Setenv("NEWCLOUD_ANTIVIRUS_TIMEOUT_SECONDS", "20")
	t.Setenv("NEWCLOUD_ANTIVIRUS_MAX_SCAN_BYTES", "1048576")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("environment alone must configure a valid deployment: %v", err)
	}
	if cfg.Server.UserContentAddr != ":8081" {
		t.Errorf("UserContentAddr = %q, want :8081", cfg.Server.UserContentAddr)
	}
	if cfg.Storage.Local.Root != "/var/lib/newcloud/blobs" || cfg.Storage.UploadDir != "/var/lib/newcloud/uploads" {
		t.Errorf("storage paths = %q/%q, want the env values", cfg.Storage.Local.Root, cfg.Storage.UploadDir)
	}
	if len(cfg.Server.TrustedProxies) != 2 || cfg.Server.TrustedProxies[0] != "172.16.0.0/12" {
		t.Errorf("TrustedProxies = %v, want the two CIDRs without the stray spaces", cfg.Server.TrustedProxies)
	}
	if cfg.Notify.QuotaWarningPercent != 75 {
		t.Errorf("QuotaWarningPercent = %d, want 75", cfg.Notify.QuotaWarningPercent)
	}
	if !cfg.AV.Enabled || cfg.AV.TimeoutSec != 20 || cfg.AV.MaxScanBytes != 1048576 {
		t.Errorf("antivirus = enabled:%v timeout:%d max:%d, want true/20/1048576",
			cfg.AV.Enabled, cfg.AV.TimeoutSec, cfg.AV.MaxScanBytes)
	}
}

// The environment corrects the file; it does not race it. An operator overriding one
// key from a compose file is fixing the mounted YAML, and the file must not win.
func TestEnvironmentBeatsTheFile(t *testing.T) {
	setServingEnv(t)
	t.Setenv("NEWCLOUD_PUBLIC_URL", "https://env-wins.example.com")

	path := filepath.Join(t.TempDir(), "config.yml")
	yaml := "server:\n  public_url: https://file-loses.example.com\n" +
		"  usercontent_url: https://uc-file.example.com\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a file plus one override must still validate: %v", err)
	}
	if cfg.Server.PublicURL != "https://env-wins.example.com" {
		t.Errorf("public_url = %q, want the environment's value", cfg.Server.PublicURL)
	}
}

// A set-but-unparsable variable is refused, not skipped: "accepted and ignored" is the
// failure mode this package exists to stop, and a port named "smtp" silently keeping
// the default is exactly that.
func TestAnUnparsableValueIsRefused(t *testing.T) {
	setServingEnv(t)
	t.Setenv("NEWCLOUD_MAIL_PORT", "smtp")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "NEWCLOUD_MAIL_PORT") {
		t.Fatalf("err = %v, want it to name NEWCLOUD_MAIL_PORT", err)
	}

	t.Setenv("NEWCLOUD_MAIL_PORT", "587")
	t.Setenv("NEWCLOUD_ANTIVIRUS_ENABLED", "yes")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "NEWCLOUD_ANTIVIRUS_ENABLED") {
		t.Fatalf("err = %v, want it to name NEWCLOUD_ANTIVIRUS_ENABLED", err)
	}
}

// Parsing and validation stay separate on purpose: applyEnv accepts any integer, and
// the bound check stays in Validate where every other bound check lives. This pins
// that an out-of-range env value still cannot start the server.
func TestAnOutOfRangeValueIsRefusedByValidation(t *testing.T) {
	setServingEnv(t)
	t.Setenv("NEWCLOUD_NOTIFY_QUOTA_WARNING_PERCENT", "150")

	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "0-100") {
		t.Fatalf("err = %v, want the quota bound to refuse 150", err)
	}
}

// The proxy list tolerates the noise an operator's shell leaves behind: spaces around
// entries and an accidental double comma.
func TestTheProxyListIgnoresEmptyEntries(t *testing.T) {
	cfg := Default()
	t.Setenv("NEWCLOUD_TRUSTED_PROXIES", "10.0.0.0/8,, ,172.16.0.0/12")

	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Server.TrustedProxies) != 2 {
		t.Fatalf("TrustedProxies = %v, want the two real CIDRs", cfg.Server.TrustedProxies)
	}
}
