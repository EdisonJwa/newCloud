package main

// The workflow files are build configuration that nothing compiles, so a syntax error in one is
// invisible until the moment it matters: a tag push that fails to publish, or a CI job that never
// starts. Parsing them in a test turns that into a failing build.
//
// This exists because the release workflow was added by hand and could not be run from this
// environment — no GitHub Actions runner, no `act` — so the least that could be verified locally is
// that it is YAML and that it declares the two things every workflow must have.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowFilesAreValidYAML(t *testing.T) {
	root := repoRootForWorkflows(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		var doc map[string]any
		if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
			t.Fatalf("%s is not valid YAML: %v", e.Name(), uerr)
		}
		if len(doc) == 0 {
			t.Fatalf("%s parsed as an empty document", e.Name())
		}
		// Every workflow needs a trigger and at least one job. The trigger is checked against the
		// source text rather than the decoded map, because YAML 1.1 folds the bare key `on` into a
		// boolean and the decoded map's key then depends on the library's coercion rules — a check
		// that is about the parser, not about the workflow.
		if !strings.Contains(string(raw), "\non:") && !strings.HasPrefix(string(raw), "on:") {
			t.Fatalf("%s has no `on:` trigger", e.Name())
		}
		if jobs, ok := doc["jobs"].(map[string]any); !ok || len(jobs) == 0 {
			t.Fatalf("%s declares no jobs", e.Name())
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no workflow files found in %s; this check would pass vacuously", dir)
	}
}

// repoRootForWorkflows walks up to the module root.
func repoRootForWorkflows(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the module root")
	return ""
}
