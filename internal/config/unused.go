package config

// Keys that are accepted, validated, and do nothing.
//
// A key like this is the quiet kind of wrong. The deployment is forced to set it, or sets it
// expecting an effect, the operator concludes the protection or policy it implies is in place, and
// nothing says otherwise until someone reads the code. Reporting them at startup is the least the
// server can do short of implementing them.
//
// The list is deliberately in the config package rather than the CLI: the check that every key is
// read lives here too, and both should agree about which keys are inert. UnusedKeys mentions those
// keys in order to report them, which is not a use — TestEveryConfigKeyIsReadOrPinned excludes it
// for exactly that reason.

// UnusedKey is one configured-but-inert setting.
type UnusedKey struct {
	// Key is the dotted configuration path, as an operator would type it.
	Key string
	// Why says what the key appears to promise and what actually happens.
	Why string
}

// UnusedKeys reports the configured settings that have no effect.
//
// Each entry is conditional on the key actually being set: warning about a key nobody configured
// is noise, and noise is how a real warning gets ignored.
func (c *Config) UnusedKeys() []UnusedKey {
	var out []UnusedKey

	if c.Auth.SCIMToken != "" {
		out = append(out, UnusedKey{
			Key: "auth.scim_token",
			Why: "no SCIM endpoint is served. Accounts are created on first OIDC login and by the " +
				"local administrator; there is no provisioning API for an identity provider to call",
		})
	}

	return out
}
