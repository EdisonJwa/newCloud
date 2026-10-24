// Package mount is the external-mount service (ADR-0014): it owns mount records, the
// credential cipher, provider dialling, and the path walk that forks a request between
// the metadata store and a remote.
package mount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/storage/layout"
	"github.com/EdisonJwa/newCloud/internal/store"
)

// ErrDisabled, ErrScheme and friends are the service's typed refusals; the HTTP layer maps
// them once.
var (
	ErrDisabled    = errors.New("mounts are disabled on this deployment")
	ErrForbidden   = errors.New("mounts may only be created by an administrator here")
	ErrBadScheme   = errors.New("mount scheme is not in the allowed list")
	ErrBadEndpoint = errors.New("mount endpoint is not a valid URL for its scheme")
	ErrPrivateAddr = errors.New("mount endpoint resolves to a private address; user mounts may only point at public hosts")
	ErrUnreachable = errors.New("mount endpoint did not answer a reachability check")
	ErrNotOwner    = errors.New("mount belongs to another account")
	ErrNoCipher    = errors.New("mount credentials cannot be encrypted: no key configured")
)

// CreateInput is one mount creation request, already authenticated.
type CreateInput struct {
	Name     string
	Scheme   string
	Endpoint string
	RootPath string
	Username string
	Password string
	ReadOnly bool
	// Verify asks for a reachability check before the rows are written. The route
	// defaults it on: a mount that is created broken shows up in listings as a folder
	// that 502s, which is worse than a refusal that names the remote.
	Verify bool
}

// Service owns mount lifecycle and provider access.
type Service struct {
	meta   store.Store
	cfg    config.MountsConfig
	cipher *Cipher
	// dialers maps scheme → constructor. Tests inject fakes here; production registers
	// the real providers (webdav, ftp, sftp) in RegisterProviders.
	dialers map[string]external.DialFunc
	log     *slog.Logger
}

// New builds the service. A nil cipher keeps the service usable for reads (resolution
// and listing) while refusing credential writes — a deployment with mounts.enabled and a
// missing key file fails at startup instead, so this is the test-and-degraded path.
func New(meta store.Store, cfg config.MountsConfig, c *Cipher, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{meta: meta, cfg: cfg, cipher: c, dialers: map[string]external.DialFunc{}, log: log}
}

// RegisterDial makes a scheme diallable. Called at process start for every provider
// compiled in; the map is the single place "what can this build mount" is answered.
func (s *Service) RegisterDial(scheme string, fn external.DialFunc) {
	s.dialers[strings.ToLower(scheme)] = fn
}

// Enabled reports whether the mount surface is on.
func (s *Service) Enabled() bool { return s.cfg.Enabled }

// Timeout is the per-operation budget every provider call runs under.
func (s *Service) Timeout() time.Duration {
	if s.cfg.TimeoutSec <= 0 {
		return 30 * time.Second
	}
	return time.Duration(s.cfg.TimeoutSec) * time.Second
}

// MaxList caps one remote directory listing.
func (s *Service) MaxList() int {
	if s.cfg.MaxListEntries <= 0 {
		return 5000
	}
	return s.cfg.MaxListEntries
}

// MaxWrite caps one file written into a mount. Zero defers to limits.max_file_bytes,
// which the caller supplies as fallback.
func (s *Service) MaxWrite(fallback int64) int64 {
	if s.cfg.MaxFileBytes > 0 {
		return s.cfg.MaxFileBytes
	}
	return fallback
}

// Create validates, verifies and records one mount, with its mount-point node.
func (s *Service) Create(ctx context.Context, actor store.User, in CreateInput) (store.Mount, error) {
	if !s.cfg.Enabled {
		return store.Mount{}, ErrDisabled
	}
	if actor.Role != string(config.RoleAdmin) && !s.cfg.AllowUserMounts {
		return store.Mount{}, ErrForbidden
	}
	scheme := strings.ToLower(in.Scheme)
	if !s.schemeAllowed(scheme) {
		return store.Mount{}, ErrBadScheme
	}
	if err := layout.ValidateSegment(in.Name); err != nil {
		return store.Mount{}, fmt.Errorf("mount name: %w", err)
	}
	endpoint, err := validateEndpoint(scheme, in.Endpoint)
	if err != nil {
		return store.Mount{}, err
	}
	// The SSRF guard (ADR-0014): a non-admin creating a mount is pointing the server at
	// a host of their choosing. Private and link-local space is where the services live
	// that this must never read on a user's behalf.
	if actor.Role != string(config.RoleAdmin) && s.cfg.DenyPrivateAddresses {
		if err := refusePrivateAddresses(endpoint.Host); err != nil {
			return store.Mount{}, err
		}
	}
	if _, ok := s.dialers[scheme]; !ok {
		// Registered nowhere: smb today. Refusing here rather than at first use means
		// an operator learns at creation, with a message naming the scheme.
		return store.Mount{}, fmt.Errorf("%w: scheme %q has no provider in this build", external.ErrUnsupported, scheme)
	}
	if s.cipher == nil {
		return store.Mount{}, ErrNoCipher
	}

	var storage external.Storage
	if in.Verify {
		storage, err = s.dialers[scheme](ctx, in.Endpoint, strings.Trim(in.RootPath, "/"), in.Username, in.Password)
		if err != nil {
			return store.Mount{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		vctx, cancel := context.WithTimeout(ctx, s.Timeout())
		defer cancel()
		if err := storage.Ping(vctx); err != nil {
			return store.Mount{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
	}

	secret, err := s.cipher.Seal(in.Password)
	if err != nil {
		return store.Mount{}, err
	}

	root, err := s.meta.Root(ctx, actor.ID)
	if err != nil {
		return store.Mount{}, err
	}

	// Mounts are top-level by design (ADR-0014): the mount point is a folder beside the
	// user's own folders, not a folder nested somewhere in them, which is what keeps the
	// resolution fork to one level deep and the mount discoverable at a glance.
	mountID := uuid.New()
	var created store.Mount
	err = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		node, nerr := tx.CreateDir(ctx, store.NewNode{
			OwnerID: actor.ID, ParentID: root.ID, Name: in.Name,
		})
		if nerr != nil {
			return nerr
		}
		created, nerr = tx.CreateMount(ctx, store.NewMount{
			ID: mountID, NodeID: node.ID, OwnerID: actor.ID, Name: in.Name,
			Scheme: scheme, Endpoint: in.Endpoint, RootPath: strings.Trim(in.RootPath, "/"),
			Username: in.Username, SecretEnc: secret, ReadOnly: in.ReadOnly,
			CreatedBy: actor.ID,
		})
		if nerr != nil {
			return nerr
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &actor.ID, ActorEmail: actor.Email,
			Action: "mount.create", TargetKind: "mount", TargetID: mountID.String(),
			Result: "ok",
			// The endpoint is recorded because it is the security-relevant fact (where
			// does this server send credentials); the credential itself never appears.
			Metadata: map[string]any{
				"scheme": scheme, "endpoint": in.Endpoint, "read_only": in.ReadOnly,
				"name": in.Name,
			},
		})

	})
	if err != nil {
		return store.Mount{}, err
	}
	if storage != nil {
		_ = s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.SetMountHealth(ctx, mountID, "ok", "", time.Now())
		})
	}
	return created, nil
}

// Unmount removes a mount and its mount-point folder. The remote's data is untouched —
// that is the whole meaning of the operation, and it is audited as such.
func (s *Service) Unmount(ctx context.Context, actor store.User, id uuid.UUID) error {
	if !s.cfg.Enabled {
		return ErrDisabled
	}
	var m store.Mount
	err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		m, err = tx.MountByID(ctx, id)
		return err
	})
	if err != nil {
		return err
	}
	if actor.Role != string(config.RoleAdmin) && m.OwnerID != actor.ID {
		return ErrNotOwner
	}
	return s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.DeleteMount(ctx, id); err != nil {
			return err
		}
		return tx.Audit(ctx, store.AuditEntry{
			ActorKind: "user", ActorUserID: &actor.ID, ActorEmail: actor.Email,
			Action: "mount.delete", TargetKind: "mount", TargetID: id.String(),
			Result: "ok",
			Metadata: map[string]any{
				"scheme": m.Scheme, "endpoint": m.Endpoint,
				"remote_data": "untouched",
			},
		})
	})
}

// List answers the mounts the actor may see: their own, or every mount for an admin.
func (s *Service) List(ctx context.Context, actor store.User) ([]store.Mount, error) {
	var out []store.Mount
	err := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		if actor.Role == string(config.RoleAdmin) {
			out, err = tx.AllMounts(ctx)
		} else {
			out, err = tx.MountsForOwner(ctx, actor.ID)
		}
		return err
	})
	return out, err
}

// Storage dials a mount's remote, decrypting the credential here and nowhere else. The
// returned Storage is for one request: providers hold connections, and pooling remote
// sessions is a per-protocol optimization that must not leak across requests' contexts.
func (s *Service) Storage(ctx context.Context, m store.Mount) (external.Storage, error) {
	dial, ok := s.dialers[strings.ToLower(m.Scheme)]
	if !ok {
		return nil, fmt.Errorf("%w: scheme %q has no provider in this build", external.ErrUnsupported, m.Scheme)
	}
	password := ""
	if s.cipher != nil && len(m.SecretEnc) > 0 {
		var err error
		password, err = s.cipher.Open(m.SecretEnc)
		if err != nil {
			return nil, err
		}
	}
	dctx, cancel := context.WithTimeout(ctx, s.Timeout())
	defer cancel()
	return dial(dctx, m.Endpoint, m.RootPath, m.Username, password)
}

// RecordHealth writes a reachability outcome. Failure to record is logged, not returned:
// health is advisory and must not turn a successful file operation into an error.
func (s *Service) RecordHealth(ctx context.Context, id uuid.UUID, err error) {
	status, detail := "ok", ""
	if err != nil {
		status, detail = "unreachable", err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
	}
	if werr := s.meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.SetMountHealth(ctx, id, status, detail, time.Now())
	}); werr != nil {
		s.log.Warn("mount health not recorded", "mount", id, "error", werr)
	}
}

func (s *Service) schemeAllowed(scheme string) bool {
	for _, allowed := range s.cfg.AllowedSchemes {
		if strings.EqualFold(allowed, scheme) {
			return true
		}
	}
	return false
}

// endpointSchemes maps a mount scheme to the URL schemes its endpoint may use.
var endpointSchemes = map[string]map[string]bool{
	"webdav": {"http": true, "https": true},
	"ftp":    {"ftp": true, "ftps": true},
	"sftp":   {"sftp": true},
	"smb":    {"smb": true},
}

func validateEndpoint(scheme, endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEndpoint, err)
	}
	allowed := endpointSchemes[scheme]
	if allowed == nil || !allowed[strings.ToLower(u.Scheme)] {
		return nil, fmt.Errorf("%w: scheme %q wants a %s URL, got %q",
			ErrBadEndpoint, scheme, schemeLabel(allowed), u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: endpoint needs a host", ErrBadEndpoint)
	}
	return u, nil
}

func schemeLabel(allowed map[string]bool) string {
	var out []string
	for s := range allowed {
		out = append(out, s)
	}
	// Sorted for a stable message; two entries max today.
	if len(out) == 2 && out[0] > out[1] {
		out[0], out[1] = out[1], out[0]
	}
	return strings.Join(out, "/")
}

// refusePrivateAddresses resolves host and refuses anything that is not public. An
// unresolvable host is refused too: "cannot check" and "safe" are different answers, and
// this guard runs at creation where a false negative is cheap and a false positive is an
// incident.
func refusePrivateAddresses(host string) error {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPrivateAddr, err)
	}
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return fmt.Errorf("%w: %q is not an address", ErrPrivateAddr, a)
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("%w (%s)", ErrPrivateAddr, a)
		}
	}
	return nil
}
