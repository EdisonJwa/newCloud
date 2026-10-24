// Command sbom writes a CycloneDX inventory of everything this binary is built from.
//
// Why this exists: a self-hosted product is deployed into other people's networks, and "what is in
// the binary you are running" is a question their security review asks before the deployment, and
// their incident response asks after a CVE lands. Answering it from a lockfile by hand is the kind
// of work that gets skipped, so it is generated from the module graph the build itself uses.
//
// The output is CycloneDX 1.5 JSON — the format dependency scanners and SBOM repositories consume —
// with one component per module and a purl that a scanner can match against advisory databases.
//
//	go run ./tools/sbom -version "$(git describe --tags --always)" > sbom.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

// module is the subset of `go list -m -json` this tool reads.
type module struct {
	Path     string
	Version  string
	Main     bool
	Indirect bool
	Replace  *module
}

// bom is the CycloneDX document. Only the fields a scanner needs are emitted; the specification has
// many more, and inventing values for them would make the file look more authoritative than it is.
type bom struct {
	BOMFormat   string      `json:"bomFormat"`
	SpecVersion string      `json:"specVersion"`
	Version     int         `json:"version"`
	Metadata    bomMetadata `json:"metadata"`
	Components  []component `json:"components"`
}

type bomMetadata struct {
	Timestamp string     `json:"timestamp"`
	Tools     []tool     `json:"tools"`
	Component *component `json:"component,omitempty"`
}

type tool struct {
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type component struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	PURL       string `json:"purl,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Properties []prop `json:"properties,omitempty"`
}

type prop struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func main() {
	version := flag.String("version", "dev", "version of the application component")
	out := flag.String("o", "-", "output file, or - for stdout")
	pkg := flag.String("pkg", "./cmd/newcloud", "package to inventory: the binary whose dependencies matter")
	flag.Parse()

	mods, err := listModules(*pkg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbom: %v\n", err)
		os.Exit(1)
	}

	doc := bom{
		BOMFormat:   "CycloneDX",
		SpecVersion: "1.5",
		Version:     1,
		Metadata: bomMetadata{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tools:     []tool{{Vendor: "newcloud", Name: "sbom", Version: "1"}},
			Component: &component{
				Type: "application", Name: "newcloud", Version: *version,
				PURL: "pkg:golang/newcloud@" + *version,
			},
		},
	}

	for _, m := range mods {
		if m.Main {
			// The application itself is metadata.component; listing it twice makes a scanner
			// report the product as one of its own dependencies.
			continue
		}
		c := component{
			Type:    "library",
			Name:    m.Path,
			Version: m.Version,
			PURL:    purl(m),
		}
		if m.Indirect {
			c.Scope = "optional"
		}
		if m.Replace != nil {
			// A replacement changes what is actually compiled, so the scanner has to see it. The
			// purl names the replacement; the original path stays in a property so a report that
			// mentions either name can be matched.
			c.Properties = []prop{{
				Name:  "newcloud:replaces",
				Value: m.Path + "@" + m.Version,
			}}
		}
		doc.Components = append(doc.Components, c)
	}

	// Stable order: an SBOM that reorders itself between two builds of the same commit is noise in
	// every diff that touches it.
	sort.Slice(doc.Components, func(i, j int) bool {
		return doc.Components[i].Name < doc.Components[j].Name
	})

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbom: encode: %v\n", err)
		os.Exit(1)
	}
	body = append(body, '\n')

	if *out == "-" {
		if _, err := os.Stdout.Write(body); err != nil {
			fmt.Fprintf(os.Stderr, "sbom: write: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sbom: write %s: %v\n", *out, err)
		os.Exit(1)
	}
}

// listModules returns the modules that actually contribute code to the binary.
//
// Deliberately NOT `go list -m all`: that is the whole module graph, including the linter plugins
// this repository pins for its own tooling. An SBOM listing those is not an inventory of the binary
// — it is an inventory of the build environment — and a security review that scans it will raise
// findings against code that is not shipped. `go list -deps` walks what the target package imports,
// which is the question the SBOM is supposed to answer.
func listModules(pkg string) ([]module, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", pkg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m: %v: %s", err, stderr.String())
	}

	// One entry per package; several packages usually share a module, and the standard library
	// packages have no module at all.
	seen := map[string]module{}
	type listed struct {
		Module   *module
		Standard bool
		DepOnly  bool
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var p listed
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if p.Standard || p.Module == nil || p.Module.Main || p.Module.Version == "" {
			continue
		}
		seen[p.Module.Path] = *p.Module
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("the package imports no external modules; refusing to publish an " +
			"SBOM that would say the binary has no dependencies")
	}
	mods := make([]module, 0, len(seen))
	for _, m := range seen {
		mods = append(mods, m)
	}
	return mods, nil
}

// purl renders a package URL for a Go module: pkg:golang/<path>@<version>, which is what advisory
// databases index.
func purl(m module) string {
	target := m
	if m.Replace != nil {
		target = *m.Replace
	}
	if target.Version == "" {
		return ""
	}
	return "pkg:golang/" + target.Path + "@" + target.Version
}
