package main

// A smoke test for the artifact, not for the packages.
//
// Everything else in this repository tests code through Go interfaces. This builds the binary an
// operator actually deploys, runs its two database-facing commands against a real PostgreSQL, and
// checks what they exit with and what they leave behind. It found nothing when it was written, but
// the manual version of it did: a stale process from an earlier session answered /healthz on the
// port being probed, so a green result proved nothing about the binary under test. That is why this
// only asserts things a stale process cannot fake — exit codes and the state of the database.
//
//	Skipped unless TEST_DATABASE_URL is set, like every other PostgreSQL test here.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EdisonJwa/newCloud/internal/store"
)

// buildBinary compiles cmd/newcloud into a temp directory and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	name := "newcloud"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, ".")
	// Output goes to a file rather than a pipe: this environment cannot hand a child a named
	// pipe, and a build error that the test cannot print is worse than a slow test.
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, buf.String())
	}
	return out
}

// writeConfig writes a minimal configuration for the smoke test, and returns its path.
func writeConfig(t *testing.T, dir, dsn string) string {
	t.Helper()
	cfg := fmt.Sprintf(`
server:
  addr: "127.0.0.1:0"
  public_url: "http://127.0.0.1:18080"
  usercontent_addr: "127.0.0.1:18081"
  usercontent_url: "http://usercontent.smoke.test:18081"
auth:
  issuer: "http://idp.smoke.test"
  client_id: "newcloud"
  client_secret: "smoke-secret"
  redirect_url: "http://127.0.0.1:18080/auth/callback"
  session_secret: "0123456789abcdef0123456789abcdef"
database:
  url: %q
storage:
  backend: "local"
  layout: "blob"
  local:
    root: %q
    upload_dir: %q
`, dsn, filepath.Join(dir, "blobs"), filepath.Join(dir, "uploads"))

	path := filepath.Join(dir, "smoke.yml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// run executes the binary with the smoke configuration and returns its combined output.
func runBinary(t *testing.T, bin, cfgPath, dsn string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "NEWCLOUD_CONFIG="+cfgPath, "DATABASE_URL="+dsn)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func TestTheShippedBinaryMigratesAndReportsItsVersion(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping the binary smoke test")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	cfgPath := writeConfig(t, dir, dsn)

	// --version must not need a database, a config file or a working directory.
	cmd := exec.Command(bin, "--version")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("--version: %v\n%s", err, out.String())
	}
	if !strings.HasPrefix(out.String(), "newcloud ") {
		t.Fatalf("--version printed %q", out.String())
	}

	// migrate applies the schema, and running it again must be a no-op rather than an error: an
	// operator runs it on every deploy, and a second run that fails is a deploy that fails.
	for i := 0; i < 2; i++ {
		output, err := runBinary(t, bin, cfgPath, dsn, "migrate")
		if err != nil {
			t.Fatalf("migrate (run %d): %v\n%s", i+1, err, output)
		}
	}

	// The database is complete, not partially applied.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	behind, _, err := store.CheckSchema(context.Background(), pool)
	if err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if len(behind) != 0 {
		t.Fatalf("after migrate, %d migration(s) are unapplied: %v", len(behind), behind)
	}

	// A configuration the server would refuse must be refused by the same binary, with a message
	// that names the field. This is the check that catches "it applied the schema with settings
	// that will not boot".
	badCfg := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(badCfg, []byte("server:\n  addr: \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	output, err := runBinary(t, bin, badCfg, dsn, "migrate")
	if err == nil {
		t.Fatalf("migrate accepted an incomplete configuration:\n%s", output)
	}
	if !strings.Contains(output, "invalid configuration") {
		t.Fatalf("the refusal did not say the configuration was invalid:\n%s", output)
	}
}
