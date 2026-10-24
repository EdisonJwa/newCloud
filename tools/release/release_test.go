package main

// The release pipeline is a Go program so that it runs wherever Go does, which is the point: the
// shell version inside the Makefile failed on Windows with "rm: not found" and nothing caught it,
// because the workflow that exercises it runs on Linux.
//
// This drives one platform end to end — the same code path the six-platform release takes — and
// checks the two failure modes that matter: an artefact missing, and a checksum file that does not
// match the artefacts next to it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseBuildsAnArtefactAnSBOMAndMatchingChecksums(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	dist := t.TempDir()

	if err := run("go", dist, "v0.0.0-test", "linux/amd64", "-s -w"); err != nil {
		t.Fatalf("release: %v", err)
	}

	bin := filepath.Join(dist, "newcloud-v0.0.0-test-linux-amd64")
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("the binary was not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the binary is empty")
	}
	if _, err := os.Stat(filepath.Join(dist, "sbom.cdx.json")); err != nil {
		t.Fatalf("the SBOM was not written: %v", err)
	}

	sums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("the checksum file was not written: %v", err)
	}
	sum, err := fileSHA256(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sums), sum+"  newcloud-v0.0.0-test-linux-amd64") {
		t.Fatalf("the checksum file does not attest to the binary it was written next to:\n%s", sums)
	}

	// The failure this exists to prevent: a rebuild must not inherit the previous run's files. If
	// the directory were not cleared, this second call would leave two SBOMs behind different names
	// and a checksum file covering both.
	stale := filepath.Join(dist, "newcloud-v0.0.0-replaced")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("go", dist, "v0.0.0-test", "linux/amd64", "-s -w"); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a file from the previous release survived: the artefacts are cleared first so " +
			"that an old binary cannot ship under a new name")
	}
}
