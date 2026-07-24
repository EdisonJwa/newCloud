package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// writeFile keeps the layout tests readable without importing os at each site.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// TestLayoutIsRequired: an empty layout used to silently mean "blob". Requiring
// it makes the deployment decision explicit, because the two layouts differ in
// backup and migration behaviour and a wrong default is only discovered later.
func TestLayoutIsRequired(t *testing.T) {
	c := valid()
	c.Storage.Layout = ""
	err := c.Validate()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	if !strings.Contains(err.Error(), "storage.layout") {
		t.Fatalf("error should name the field: %v", err)
	}
}

func TestLayoutDefaultsToBlob(t *testing.T) {
	c := Default()
	if c.Storage.Layout != LayoutBlob {
		t.Fatalf("default layout should be blob, got %q", c.Storage.Layout)
	}
	if c.Storage.Layout.Valid() != true {
		t.Fatal("default layout must be valid")
	}
}

func TestLayoutValidation(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		layout  Layout
		wantErr bool
	}{
		{"blob on local", "local", LayoutBlob, false},
		{"blob on s3", "s3", LayoutBlob, false},
		{"tree on local", "local", LayoutTree, false},
		// An object store has no directory hierarchy, so a tree layout cannot be
		// mirrored; rejecting at startup beats failing on the first upload.
		{"tree on s3", "s3", LayoutTree, true},
		{"unknown layout", "local", Layout("flat"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := valid()
			cfg.Storage.Backend = c.backend
			cfg.Storage.Layout = c.layout
			cfg.Storage.Local.Root = "/tmp/blobs"
			cfg.Storage.S3.Bucket = "bucket"

			err := cfg.Validate()
			if c.wantErr {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("want rejection, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("should be accepted: %v", err)
			}
		})
	}
}

func TestLayoutParsedFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yml"
	yaml := `
server:
  public_url: https://cloud.example.com
  usercontent_url: https://usercontent.example.com
auth:
  issuer: https://id.example.com
  client_id: newcloud
  client_secret: s3cret
  session_secret: kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk
storage:
  backend: local
  layout: tree
  local:
    root: /var/lib/newcloud/files
`
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.Layout != LayoutTree {
		t.Fatalf("layout not parsed: %q", c.Storage.Layout)
	}
}

func TestLayoutEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yml"
	yaml := `
server:
  public_url: https://cloud.example.com
  usercontent_url: https://usercontent.example.com
auth:
  issuer: https://id.example.com
  client_id: newcloud
  client_secret: s3cret
  session_secret: kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk
storage:
  backend: local
  layout: blob
`
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEWCLOUD_STORAGE_LAYOUT", "tree")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Storage.Layout != LayoutTree {
		t.Fatalf("env override ignored: %q", c.Storage.Layout)
	}
}
