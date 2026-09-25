package config_test

// server.usercontent_url is required and inert.
//
// Validate insists the key is an absolute URL that differs from public_url, and the server serves
// user content from public_url regardless. So a deployment is forced to configure a separation it
// does not get: content shares the application's origin, and with it that origin's cookies.
//
// Until origin separation is implemented, the least the server can do is say so at startup. These
// tests pin both halves: the key is reported when it is configured to differ, and not reported when
// it is absent or equal (where it would be a false alarm about a setting that changes nothing).

import (
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

func TestUnusedKeysReportsTheContentOrigin(t *testing.T) {
	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = "https://content.example.test"

	keys := cfg.UnusedKeys()
	if len(keys) != 1 {
		t.Fatalf("want exactly the content-origin key reported, got %d: %+v", len(keys), keys)
	}
	if keys[0].Key != "server.usercontent_url" {
		t.Fatalf("reported %q, want server.usercontent_url", keys[0].Key)
	}
	if keys[0].Why == "" {
		t.Fatal("an inert key must be reported with the reason it is inert, or the warning is " +
			"indistinguishable from noise")
	}
}

func TestUnusedKeysIsQuietWhenThereIsNothingToSay(t *testing.T) {
	// Not configured: nothing to report.
	cfg := config.Default()
	cfg.Server.PublicURL = "https://cloud.example.test"
	cfg.Server.UserContentURL = ""
	if keys := cfg.UnusedKeys(); len(keys) != 0 {
		t.Fatalf("reported %+v for an unset key", keys)
	}

	// Set to the same origin: it promises nothing that public_url does not already do.
	cfg.Server.UserContentURL = cfg.Server.PublicURL
	if keys := cfg.UnusedKeys(); len(keys) != 0 {
		t.Fatalf("reported %+v when the key equals public_url", keys)
	}
}
