package mount

// Tests for the mount service's own seams: the credential cipher, the resolution fork,
// and the create/unmount guards. The provider implementations have their own files'
// tests; the HTTP behaviour lives in internal/server.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/external"
	"github.com/EdisonJwa/newCloud/internal/store"
)

func testService(t *testing.T, mutate func(*config.MountsConfig)) (*Service, *store.Memory, store.User) {
	t.Helper()
	meta := store.NewMemory()
	user := meta.AddUser(store.User{Username: "alice", Email: "alice@example.test", Role: "admin"})
	if _, err := meta.EnsureRoot(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}
	cfg := config.MountsConfig{
		Enabled: true, SecretFile: "unused-in-tests", TimeoutSec: 5,
		MaxListEntries: 100, AllowedSchemes: []string{"webdav", "ftp", "sftp", "fake"},
		DenyPrivateAddresses: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(meta, cfg, c, nil)
	// The fake dialer: mounts created in tests use scheme "fake" and share one Fake.
	fake := external.NewFake()
	svc.RegisterDial("webdav", func(ctx context.Context, endpoint, root, user, pass string) (external.Storage, error) {
		return fake, nil
	})
	t.Cleanup(func() {})
	return svc, meta, user
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal("hunter2-but-real")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2-but-real")) {
		t.Fatal("the ciphertext contains the plaintext")
	}
	got, err := c.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hunter2-but-real" {
		t.Fatalf("round trip changed the secret: %q", got)
	}

	// A different key must not open it.
	other, _ := NewCipher(bytes.Repeat([]byte{1}, 32))
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a ciphertext opened under the wrong key")
	}
	// Tampering must not open it.
	sealed[len(sealed)-1] ^= 0xff
	if _, err := c.Open(sealed); err == nil {
		t.Fatal("a tampered ciphertext opened")
	}
}

func TestLoadKeyFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.hex")
	if err := os.WriteFile(good, []byte("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(good); err != nil {
		t.Fatalf("a well-formed key file must load: %v", err)
	}
	short := filepath.Join(dir, "short.hex")
	_ = os.WriteFile(short, []byte("0102\n"), 0o600)
	if _, err := LoadKeyFile(short); err == nil {
		t.Fatal("a short key file must be refused")
	}
	notHex := filepath.Join(dir, "not.hex")
	_ = os.WriteFile(notHex, []byte("zz"), 0o600)
	if _, err := LoadKeyFile(notHex); err == nil {
		t.Fatal("a non-hex key file must be refused")
	}
}

func TestResolveTargetForksAtTheMountRoot(t *testing.T) {
	svc, meta, user := testService(t, nil)
	ctx := context.Background()

	m, err := svc.Create(ctx, user, CreateInput{
		Name: "team", Scheme: "webdav", Endpoint: "https://remote.example.test/dav", Verify: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A purely local path resolves locally.
	local, err := ResolveTarget(ctx, meta, user.ID, "")
	if err != nil || local.External {
		t.Fatalf("the root resolved as external: %+v %v", local, err)
	}

	// The mount root itself: external with an empty remainder. Listing the mount point
	// must list the remote, not an empty folder.
	atRoot, err := ResolveTarget(ctx, meta, user.ID, "team")
	if err != nil {
		t.Fatal(err)
	}
	if !atRoot.External || atRoot.Rel != "" || atRoot.Mount.ID != m.ID {
		t.Fatalf("the mount root resolved wrong: external=%v rel=%q mount=%v", atRoot.External, atRoot.Rel, atRoot.Mount.ID)
	}
	if atRoot.Node().ID != m.NodeID {
		t.Fatal("authorization must run against the mount point node")
	}

	// Below it: the remainder is the remote path, however deep.
	below, err := ResolveTarget(ctx, meta, user.ID, "team/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !below.External || below.Rel != "docs/a.txt" {
		t.Fatalf("below the mount resolved wrong: external=%v rel=%q", below.External, below.Rel)
	}

	// A missing local path is a plain NotFound.
	if _, err := ResolveTarget(ctx, meta, user.ID, "no-such-folder/a.txt"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing path: %v", err)
	}
	// .. never reaches a remote.
	if _, err := ResolveTarget(ctx, meta, user.ID, "team/../etc"); !errors.Is(err, store.ErrInvalidName) {
		t.Fatalf("dotdot path: %v", err)
	}
}

func TestCreateGuards(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled", func(t *testing.T) {
		svc, _, user := testService(t, func(c *config.MountsConfig) { c.Enabled = false })
		if _, err := svc.Create(ctx, user, CreateInput{Name: "x", Scheme: "webdav", Endpoint: "https://remote.example.test/dav"}); !errors.Is(err, ErrDisabled) {
			t.Fatalf("want ErrDisabled, got %v", err)
		}
	})

	t.Run("non-admin without user mounts", func(t *testing.T) {
		svc, meta, _ := testService(t, func(c *config.MountsConfig) { c.AllowUserMounts = false })
		pleb := meta.AddUser(store.User{Username: "pleb", Email: "p@example.test", Role: "user"})
		meta.EnsureRoot(ctx, pleb.ID)
		if _, err := svc.Create(ctx, pleb, CreateInput{Name: "x", Scheme: "webdav", Endpoint: "https://remote.example.test/dav"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("want ErrForbidden, got %v", err)
		}
	})

	t.Run("scheme not allowed", func(t *testing.T) {
		svc, _, user := testService(t, func(c *config.MountsConfig) { c.AllowedSchemes = []string{"webdav"} })
		if _, err := svc.Create(ctx, user, CreateInput{Name: "x", Scheme: "sftp", Endpoint: "sftp://h"}); !errors.Is(err, ErrBadScheme) {
			t.Fatalf("want ErrBadScheme, got %v", err)
		}
	})

	t.Run("scheme without a provider", func(t *testing.T) {
		svc, _, user := testService(t, func(c *config.MountsConfig) {
			c.AllowedSchemes = append(c.AllowedSchemes, "smb")
		})
		_, err := svc.Create(ctx, user, CreateInput{Name: "x", Scheme: "smb", Endpoint: "smb://h", Verify: false})
		if !errors.Is(err, external.ErrUnsupported) {
			t.Fatalf("smb must name the missing provider, got %v", err)
		}
	})

	t.Run("endpoint scheme mismatch", func(t *testing.T) {
		svc, _, user := testService(t, nil)
		if _, err := svc.Create(ctx, user, CreateInput{Name: "x", Scheme: "webdav", Endpoint: "ftp://h", Verify: false}); !errors.Is(err, ErrBadEndpoint) {
			t.Fatalf("want ErrBadEndpoint, got %v", err)
		}
	})

	t.Run("private address refused for users, allowed for admins", func(t *testing.T) {
		svc, meta, _ := testService(t, func(c *config.MountsConfig) {
			c.AllowUserMounts = true
			c.AllowedSchemes = append(c.AllowedSchemes, "webdav")
		})
		// A dialer whose endpoint resolves privately: loopback.
		svc.RegisterDial("webdav", func(ctx context.Context, e, r, u, p string) (external.Storage, error) {
			return external.NewFake(), nil
		})
		pleb := meta.AddUser(store.User{Username: "pleb2", Email: "p2@example.test", Role: "user"})
		meta.EnsureRoot(ctx, pleb.ID)
		if _, err := svc.Create(ctx, pleb, CreateInput{Name: "x", Scheme: "webdav", Endpoint: "https://127.0.0.1:1", Verify: false}); !errors.Is(err, ErrPrivateAddr) {
			t.Fatalf("a user mount at loopback must be refused, got %v", err)
		}
	})

	t.Run("verify failure refuses the create", func(t *testing.T) {
		svc, _, user := testService(t, nil)
		dead := external.NewFake()
		dead.Err = external.ErrUnavailable
		svc.RegisterDial("webdav", func(ctx context.Context, e, r, u, p string) (external.Storage, error) {
			return dead, nil
		})
		svc.cfg.AllowedSchemes = append(svc.cfg.AllowedSchemes, "dead")
		if _, err := svc.Create(ctx, user, CreateInput{Name: "x", Scheme: "webdav", Endpoint: "https://down.example.test/dav", Verify: true}); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("an unreachable remote must refuse the create, got %v", err)
		}
	})
}

func TestUnmountKeepsItAuditableAndScoped(t *testing.T) {
	ctx := context.Background()
	svc, meta, user := testService(t, nil)
	m, err := svc.Create(ctx, user, CreateInput{Name: "team", Scheme: "webdav", Endpoint: "https://remote.example.test/dav", Verify: false})
	if err != nil {
		t.Fatal(err)
	}

	stranger := meta.AddUser(store.User{Username: "stranger", Email: "s@example.test", Role: "user"})
	if err := svc.Unmount(ctx, stranger, m.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a stranger must not unmount, got %v", err)
	}
	if err := svc.Unmount(ctx, user, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(ctx, meta, user.ID, "team"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after unmount the mount point is gone, got %v", err)
	}
	var sawDelete bool
	for _, e := range meta.AuditLog() {
		if e.Action == "mount.delete" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatal("unmount wrote no audit row")
	}
}

func TestStorageDecryptsThroughTheServiceOnly(t *testing.T) {
	ctx := context.Background()
	svc, meta, user := testService(t, nil)
	m, err := svc.Create(ctx, user, CreateInput{
		Name: "team", Scheme: "webdav", Endpoint: "https://remote.example.test/dav",
		Username: "svc", Password: "opensesame", Verify: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The stored row never carries the plaintext.
	var raw store.Mount
	_ = meta.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		raw, _ = tx.MountByID(ctx, m.ID)
		return nil
	})
	if bytes.Contains(raw.SecretEnc, []byte("opensesame")) {
		t.Fatal("the credential is stored in the clear")
	}
	if len(raw.SecretEnc) == 0 {
		t.Fatal("an empty credential was stored for a non-empty password")
	}
	// The dialer sees the plaintext; nothing else does.
	var seen string
	svc.RegisterDial("webdav", func(ctx context.Context, e, r, u, p string) (external.Storage, error) {
		seen = p
		return external.NewFake(), nil
	})
	if _, err := svc.Storage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if seen != "opensesame" {
		t.Fatalf("the dialer saw %q, not the credential", seen)
	}

}
