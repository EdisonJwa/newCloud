package server

// Runtime settings (§10.1 管理配置变更).
//
// The settings table has existed since the first migration and no Go code ever touched it: every limit
// in this product was a config file and a restart, and the audit action for changing one was reserved
// because nothing could change one. This file is the smallest honest implementation: an allow-list of
// keys that genuinely take effect at runtime, validated on write, audited, and read through a short
// cache so the hot paths do not add a query per request.
//
// The allow-list is the design. A settings table that accepts arbitrary keys is a table nobody can
// reason about: the value is stored, every reader ignores it, and the operator believes they changed
// something. A key that is not on this list is refused by name, with the list in the message.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// The keys that take effect at runtime. Each one has a read point in a request path, which is what
// makes it a setting rather than a config file entry with a table row next to it.
const (
	// settingKeyMaxFileBytes caps a single upload.
	settingKeyMaxFileBytes = "limits.max_file_bytes"
	// settingKeyChunkBytes caps one ranged upload request.
	settingKeyChunkBytes = "limits.upload_chunk_bytes"
	// settingKeyMaxTrashBytes caps retained trash usage per account (§4.3).
	settingKeyMaxTrashBytes = "limits.max_trash_bytes"
	// settingKeyPendingUploadBytes caps the declared size of an account's unfinished uploads.
	settingKeyPendingUploadBytes = "limits.max_pending_upload_bytes"
	// settingKeyRetainedVersionBytes caps retained history in bytes.
	settingKeyRetainedVersionBytes = "limits.max_retained_version_bytes"
	// settingKeyDefaultQuota is the quota a NEW account is created with. It has no effect on existing
	// accounts; changing those is the per-user quota endpoint, and conflating the two would make
	// "change the default" quietly rewrite people's limits.
	settingKeyDefaultQuota = "limits.default_file_quota_bytes"
	// Share policy (T1c): the sharing posture keys. Grouped with the limits because they take
	// effect on the next request, like every other runtime setting, and because an operator
	// tightening a sharing posture mid-incident is exactly the case the runtime path exists for.
	settingKeyShareMaxExpiry     = "limits.share_max_expiry_seconds"
	settingKeyShareDefaultExpiry = "limits.share_default_expiry_seconds"
	settingKeyShareRequirePass   = "limits.share_require_password"
)

// runtimeSetting is one key that takes effect without a restart.
type runtimeSetting struct {
	key string
	// configured returns the value the config file supplies, which is also what DELETE restores.
	configured func(config.Config) int64
	// validate rejects values the product cannot honour.
	validate func(int64) error
}

// runtimeSettings is the allow-list. Everything else is refused by name.
var runtimeSettings = []runtimeSetting{
	{
		key:        settingKeyMaxFileBytes,
		configured: func(c config.Config) int64 { return c.Limits.MaxFileBytes },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyChunkBytes,
		configured: func(c config.Config) int64 { return c.Limits.UploadChunkBytes },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyMaxTrashBytes,
		configured: func(c config.Config) int64 { return c.Limits.MaxTrashBytes },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyPendingUploadBytes,
		configured: func(c config.Config) int64 { return c.Limits.MaxPendingUploadBytes },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyRetainedVersionBytes,
		configured: func(c config.Config) int64 { return c.Limits.MaxRetainedVersionBytes },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyDefaultQuota,
		configured: func(c config.Config) int64 { return c.Limits.DefaultFileQuotaBytes },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyShareMaxExpiry,
		configured: func(c config.Config) int64 { return c.Limits.ShareMaxExpirySeconds },
		validate:   validateNonNegative,
	},
	{
		key:        settingKeyShareDefaultExpiry,
		configured: func(c config.Config) int64 { return c.Limits.ShareDefaultExpirySeconds },
		validate:   validateNonNegative,
	},
	{
		key: settingKeyShareRequirePass,
		configured: func(c config.Config) int64 {
			return c.Limits.ShareRequirePassword
		},
		validate: func(v int64) error {
			if v != 0 && v != 1 {
				return fmt.Errorf("share_require_password is 0 or 1")
			}
			return nil
		},
	},
}

// validateNonNegative rejects a negative size. Zero is allowed and means "unset": for the chunk and
// quota keys that is the documented "no limit"/"schema default" reading, so refusing it would make the
// reset impossible to express.
func validateNonNegative(v int64) error {
	if v < 0 {
		return fmt.Errorf("a size limit cannot be negative")
	}
	return nil
}

// settingsCache holds the overrides for a short interval.
//
// A TTL rather than invalidation-on-write: this process sees its own writes immediately, and a second
// instance sees them within the TTL. Pushing invalidation between instances is a bigger mechanism than
// this product needs, and the TTL is stated here so nobody assumes a change is atomic across a fleet.
type settingsCache struct {
	mu     sync.RWMutex
	values map[string]json.RawMessage
	loaded time.Time
	ttl    time.Duration
}

// NewSettingsCache returns a cache with the default five-second freshness window.
func NewSettingsCache() *settingsCache {
	return &settingsCache{ttl: 5 * time.Second}
}

// Int returns the override for key, or fallback when there is none or it cannot be read.
//
// A read failure falls back to the configured value rather than failing the request: a settings table
// outage must not stop uploads, and the configured value is the operator's stated intent.
func (c *settingsCache) Int(ctx context.Context, meta store.Store, key string, fallback int64) int64 {
	if c == nil {
		return fallback
	}
	c.mu.RLock()
	fresh := time.Since(c.loaded) < c.ttl && c.values != nil
	raw, ok := c.values[key]
	c.mu.RUnlock()
	if !fresh {
		raw, ok = c.refresh(ctx, meta, key)
	}
	if !ok {
		return fallback
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return fallback
	}
	return v
}

func (c *settingsCache) refresh(ctx context.Context, meta store.Store, key string) (json.RawMessage, bool) {
	values := map[string]json.RawMessage{}
	err := meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		got, rerr := tx.ReadSettings(ctx)
		values = got
		return rerr
	})
	if err != nil {
		return nil, false
	}
	c.mu.Lock()
	c.values = values
	c.loaded = time.Now()
	c.mu.Unlock()
	raw, ok := values[key]
	return raw, ok
}

// invalidate drops the cache so the next read sees a change this process just made.
func (c *settingsCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.values = nil
	c.loaded = time.Time{}
	c.mu.Unlock()
}

// findRuntimeSetting returns the allow-list entry for key.
func findRuntimeSetting(key string) (runtimeSetting, bool) {
	for _, s := range runtimeSettings {
		if s.key == key {
			return s, true
		}
	}
	return runtimeSetting{}, false
}

// runtimeSettingKeys lists the accept-ed keys for error messages and the GET response.
func runtimeSettingKeys() []string {
	out := make([]string, 0, len(runtimeSettings))
	for _, s := range runtimeSettings {
		out = append(out, s.key)
	}
	return out
}

// handleAdminSettings lists the effective runtime settings.
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	var overrides map[string]json.RawMessage
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, rerr := tx.ReadSettings(ctx)
		overrides = got
		return rerr
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}

	type entry struct {
		Key        string          `json:"key"`
		Value      json.RawMessage `json:"value"`
		Configured int64           `json:"configured"`
		Source     string          `json:"source"`
	}
	out := make([]entry, 0, len(runtimeSettings))
	for _, spec := range runtimeSettings {
		e := entry{Key: spec.key, Configured: spec.configured(s.deps.Config), Source: "config"}
		if raw, ok := overrides[spec.key]; ok {
			e.Value = raw
			e.Source = "override"
		} else {
			e.Value, _ = json.Marshal(spec.configured(s.deps.Config))
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": out})
}

// handleAdminSetSetting writes one override.
func (s *Server) handleAdminSetSetting(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	key := chiURLParam(r, "key")
	spec, known := findRuntimeSetting(key)
	if !known {
		writeError(w, http.StatusBadRequest, "unknown_setting",
			fmt.Sprintf("%q is not a runtime setting; the keys that take effect without a restart "+
				"are: %s", key, strings.Join(runtimeSettingKeys(), ", ")))
		return
	}

	var value int64
	if !decodeJSON(w, r, &value) {
		return
	}
	if err := spec.validate(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_setting", err.Error())
		return
	}
	body, _ := json.Marshal(value)

	var before json.RawMessage
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, rerr := tx.ReadSettings(ctx)
		if rerr != nil {
			return rerr
		}
		before = got[key]
		if werr := tx.WriteSetting(ctx, key, body, actor.ID()); werr != nil {
			return werr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "admin.setting.change", TargetKind: "setting", TargetID: key,
			Result: "ok",
			Metadata: map[string]any{
				"from":        rawOrConfigured(before, spec.configured(s.deps.Config)),
				"to":          value,
				"change":      "set",
				"was_default": before == nil,
			},
		})
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.deps.Settings.invalidate()
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": value})
}

// handleAdminDeleteSetting removes an override, restoring the configured value.
func (s *Server) handleAdminDeleteSetting(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	key := chiURLParam(r, "key")
	spec, known := findRuntimeSetting(key)
	if !known {
		writeError(w, http.StatusBadRequest, "unknown_setting",
			fmt.Sprintf("%q is not a runtime setting", key))
		return
	}

	var had bool
	if err := s.deps.Meta.WithTx(r.Context(), func(ctx context.Context, tx store.Tx) error {
		got, rerr := tx.ReadSettings(ctx)
		if rerr != nil {
			return rerr
		}
		_, had = got[key]
		if derr := tx.DeleteSetting(ctx, key); derr != nil {
			return derr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: actor.Ptr(), ActorEmail: actor.Email(),
			ActorIP: clientIP(r), UserAgent: r.UserAgent(),
			Action: "admin.setting.change", TargetKind: "setting", TargetID: key,
			Result: "ok",
			Metadata: map[string]any{
				"from":         rawOrConfigured(got[key], spec.configured(s.deps.Config)),
				"to":           spec.configured(s.deps.Config),
				"change":       "reset",
				"had_override": had,
			},
		})
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.deps.Settings.invalidate()
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "configured": spec.configured(s.deps.Config)})
}

// rawOrConfigured renders a previous value for the audit row.
func rawOrConfigured(raw json.RawMessage, configured int64) any {
	if raw == nil {
		return configured
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// maxFileBytes is the effective single-upload cap (§10.1, runtime settings).
func (s *Server) maxFileBytes(ctx context.Context) int64 {
	base := s.deps.Config.Limits.MaxFileBytes
	return s.deps.Settings.Int(ctx, s.deps.Meta, settingKeyMaxFileBytes, base)
}

// maxPendingUploadBytes is the effective cap on unfinished uploads (§4.3).
func (s *Server) maxPendingUploadBytes(ctx context.Context) int64 {
	base := s.deps.Config.Limits.MaxPendingUploadBytes
	return s.deps.Settings.Int(ctx, s.deps.Meta, settingKeyPendingUploadBytes, base)
}

// maxRetainedVersionBytes is the effective cap on retained history (§4.3).
//
// Read by the worker rather than here, but listed with the others so the allow-list stays one place.
func (s *Server) maxRetainedVersionBytes(ctx context.Context) int64 {
	base := s.deps.Config.Limits.MaxRetainedVersionBytes
	return s.deps.Settings.Int(ctx, s.deps.Meta, settingKeyRetainedVersionBytes, base)
}

// maxTrashBytes is the effective retained-usage cap for the trash (§4.3).
func (s *Server) maxTrashBytes(ctx context.Context) int64 {
	base := s.deps.Config.Limits.MaxTrashBytes
	return s.deps.Settings.Int(ctx, s.deps.Meta, settingKeyMaxTrashBytes, base)
}

// chunkBytes is the effective cap on one ranged upload request.
func (s *Server) chunkBytes(ctx context.Context) int64 {
	base := s.deps.Config.Limits.UploadChunkBytes
	return s.deps.Settings.Int(ctx, s.deps.Meta, settingKeyChunkBytes, base)
}

// InitialQuota is the quota a NEW account is provisioned with, including a runtime override.
//
// Exported for the settings test: the provisioning paths are internal, and the property under test
// ("the default changes without a restart, and existing accounts are untouched") is exactly the value
// those paths would store.
func (s *Server) InitialQuota(ctx context.Context) *int64 { return s.initialQuota(ctx) }

// MaxRetainedVersionBytesForTest exposes the effective retained-history cap.
//
// Exported because the reader lives with the worker, and the property under test is that the worker
// resolves the OVERRIDE rather than the configured value.
func (s *Server) MaxRetainedVersionBytesForTest(ctx context.Context) int64 {
	return s.maxRetainedVersionBytes(ctx)
}
