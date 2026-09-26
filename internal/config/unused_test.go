package config_test

// Configured keys that do nothing must be announced.
//
// Four settings are accepted, validated and read by nothing:
//
//   server.usercontent_url       user content is served from public_url, so the origin separation
//                                the key implies does not exist — while Validate REQUIRES the key
//   audit.retention_days         audit events are never pruned; a hash-chained log cannot delete
//                                from its middle without looking tampered
//   auth.scim_token              no SCIM endpoint is served
//   limits.upload_chunk_bytes    (implemented — the resumable endpoint now enforces it)
//
// Each is reported only when configured, because a warning about a key nobody set is noise, and
// noise is how a real warning gets ignored.

import (
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

func unusedByKey(t *testing.T, cfg config.Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, u := range cfg.UnusedKeys() {
		if u.Why == "" {
			t.Fatalf("%s is reported without a reason; the warning would be indistinguishable "+
				"from noise", u.Key)
		}
		if _, dup := out[u.Key]; dup {
			t.Fatalf("%s is reported twice", u.Key)
		}
		out[u.Key] = u.Why
	}
	return out
}

func TestUnusedKeysCoversEveryInertKey(t *testing.T) {
	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	cfg.Auth.SCIMToken = "scim-secret"
	// Default() already sets audit.retention_days and limits.upload_chunk_bytes.

	got := unusedByKey(t, cfg)
	for _, want := range []string{
		"server.usercontent_url",
		"audit.retention_days",
		"auth.scim_token",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("%s is not reported as inert; an operator setting it sees no effect and no "+
				"explanation. Reported: %v", want, keysOf(got))
		}
	}
	if len(got) != 3 {
		t.Fatalf("reported %d keys, want 3: %v", len(got), keysOf(got))
	}
}

func TestUnusedKeysIsQuietWhenThereIsNothingToSay(t *testing.T) {
	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"

	// Real configuration that works: no warning about any of it.
	cfg.Server.UserContentURL = cfg.Server.PublicURL
	cfg.Auth.SCIMToken = ""
	cfg.Audit.RetentionDays = 0

	if got := cfg.UnusedKeys(); len(got) != 0 {
		t.Fatalf("warned about configuration that has an effect: %v", keysOf(unusedByKey(t, cfg)))
	}
}

func TestUnusedKeysExplainsWhatIsMissing(t *testing.T) {
	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"
	got := unusedByKey(t, cfg)

	// A reason that does not name the consequence is not worth printing.
	if !strings.Contains(got["server.usercontent_url"], "public_url") {
		t.Fatalf("the content-origin reason should say where content is actually served from: %q",
			got["server.usercontent_url"])
	}
	if !strings.Contains(got["audit.retention_days"], "pruned") {
		t.Fatalf("the audit-retention reason should say the log is never pruned: %q",
			got["audit.retention_days"])
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
