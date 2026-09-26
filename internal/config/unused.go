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

	if c.Server.UserContentURL != "" && c.Server.UserContentURL != c.Server.PublicURL {
		out = append(out, UnusedKey{
			Key: "server.usercontent_url",
			Why: "user content is still served from public_url, so it shares that origin's cookies " +
				"and cannot be isolated from it; serving content from a separate origin is not " +
				"implemented",
		})
	}

	if c.Audit.RetentionDays > 0 {
		out = append(out, UnusedKey{
			Key: "audit.retention_days",
			Why: "audit events are never pruned. Deleting rows from the middle of a hash-chained log " +
				"is indistinguishable from tampering, so retention cannot be implemented as a " +
				"delete: it needs an archive-and-anchor design. The value is validated and ignored, " +
				"and the log grows for as long as the deployment lives",
		})
	}

	if c.Auth.SCIMToken != "" {
		out = append(out, UnusedKey{
			Key: "auth.scim_token",
			Why: "no SCIM endpoint is served. Accounts are created on first OIDC login and by the " +
				"local administrator; there is no provisioning API for an identity provider to call",
		})
	}

	if c.Limits.UploadChunkBytes > 0 {
		out = append(out, UnusedKey{
			Key: "limits.upload_chunk_bytes",
			Why: "the value is validated and then never read: the resumable-upload endpoint accepts " +
				"whatever chunk size the client sends, and nothing tells the client what to use. " +
				"It is neither a limit nor advice",
		})
	}

	return out
}
