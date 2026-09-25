package config

// Keys that are accepted, validated, and do nothing.
//
// A key like this is the quiet kind of wrong. The deployment is forced to set it, the operator
// concludes the protection it implies is in place, and nothing says otherwise until someone reads
// the code. Reporting them at startup is the least the server can do short of implementing them.
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
func (c *Config) UnusedKeys() []UnusedKey {
	var out []UnusedKey
	if c.Server.UserContentURL != "" && c.Server.UserContentURL != c.Server.PublicURL {
		out = append(out, UnusedKey{
			Key: "server.usercontent_url",
			Why: "user content is still served from public_url, so it shares that origin's cookies " +
				"and cannot be isolated from it; serving content from a separate origin is not " +
				"implemented",
		})
	}
	return out
}
