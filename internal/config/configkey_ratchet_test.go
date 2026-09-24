package config_test

// Every configuration key must actually be read.
//
// Three keys found in this work were declared, documented and validated, and read by nothing:
// server.trust_proxy (whose absence let X-Forwarded-For bypass the break-glass CIDR restriction),
// limits.default_file_quota_bytes (documented as "per-user quota", never applied to any account),
// and auth.scim_token (inert until a SCIM endpoint exists — legitimately).
//
// The failure mode is quiet in a specific way: the key appears in the example config, an operator
// sets it, nothing happens, and no error is produced. A key that is not implemented should be
// either removed or listed here with the reason, so the next person reading configuration.md knows
// which is which.
//
// The check is deliberately conservative about accessors. A field is "read" when it appears in a
// package other than internal/config, OR when a method in this package that mentions it is itself
// called from outside. That is what a config accessor is: TrashRetention() is read by the worker,
// so TrashRetentionDays counts as consumed — and a field whose only mention is its own declaration,
// its default value, or a validation branch does not.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// knownUnread pins the keys that are deliberately not read, with the reason. Removing one from this
// list is part of implementing it, and the test fails if a pinned key starts being read, so the list
// cannot rot in the permissive direction.
var knownUnread = map[string]string{
	"UserContentURL": "VALIDATED BUT UNUSED: the config requires an absolute URL that differs from " +
		"public_url, and the server never reads it. Serving user content from a separate origin is " +
		"not implemented, so the isolation this key implies does not exist. Kept pinned rather than " +
		"deleted because removing it changes every deployment's required configuration for a feature " +
		"that is still intended.",
	"SCIMToken":        "no SCIM endpoint exists; the key is accepted so a future one can use it",
	"RetentionDays":    "audit retention is not implemented; the page says so rather than pretending",
	"UploadChunkBytes": "validated and advertised to clients as advice, never enforced",
}

func TestEveryConfigKeyIsReadOrPinned(t *testing.T) {
	root := repoRoot(t)

	cfgPath := filepath.Join(root, "internal", "config", "config.go")
	src, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	// Every yaml-tagged field, by Go name.
	fieldRe := regexp.MustCompile("(?m)^\\t([A-Z]\\w*)\\s+[\\w\\*\\[\\]\\.]+\\s+`yaml:\"([a-z0-9_]+)\"")
	fields := map[string]string{} // Go name -> yaml key
	for _, m := range fieldRe.FindAllStringSubmatch(text, -1) {
		fields[m[1]] = m[2]
	}

	// Chunks run from one `func` to the next, so a struct declared BETWEEN two functions lands
	// inside the earlier function's chunk and every field it declares looks read. Removing the
	// declaration lines first is what stops a struct declaration from counting as a use.
	text = fieldRe.ReplaceAllString(text, "")
	if len(fields) < 40 {
		t.Fatalf("found only %d config fields; the parse is probably broken and this check is not "+
			"testing anything", len(fields))
	}

	// Method bodies in this package, so a field read only by an accessor can be traced.
	//
	// Split at every top-level `func ` rather than at every method signature: a signature-only
	// split leaves each body running to the next MATCHING signature, so a field mentioned by a
	// later function appeared inside an earlier one and several dead keys looked consumed.
	// Both methods and plain functions count: client_secret_file is consumed by Load(), which
	// has no receiver, and an earlier version of this check called that key dead because it only
	// looked at methods.
	methodNameRe := regexp.MustCompile(`^func \([a-z] \*?\w+\) ([A-Z]\w*)\(`)
	funcNameRe := regexp.MustCompile(`^func ([A-Z]\w*)\(`)
	methods := map[string]string{} // name -> body
	var current func(string)
	current = func(chunk string) {
		if m := methodNameRe.FindStringSubmatch(chunk); m != nil {
			methods[m[1]] = chunk
			return
		}
		if m := funcNameRe.FindStringSubmatch(chunk); m != nil {
			methods[m[1]] = chunk
		}
	}
	start := 0
	for _, loc := range regexp.MustCompile(`(?m)^func `).FindAllStringIndex(text, -1) {
		if loc[0] > start {
			current(text[start:loc[0]])
		}
		start = loc[0]
	}
	current(text[start:])

	// Everything outside internal/config.
	var outside strings.Builder
	// BOTH internal/ and cmd/: the config accessors are called from cmd/newcloud, and a walk that
	// only covered internal/ reported five live keys as unread. The instrument has to see every
	// place a key can be consumed, or it reports false positives and gets switched off.
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Only internal/ and cmd/ can consume configuration.
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			if rel == "." {
				return nil
			}
			top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
			if top != "internal" && top != "cmd" {
				return filepath.SkipDir
			}
			if rel == filepath.Join("internal", "config") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		outside.Write(body)
		outside.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rest := outside.String()
	if len(rest) == 0 {
		t.Fatal("read no files outside internal/config; the walk is broken")
	}

	// A method counts as used when it appears anywhere outside internal/config.
	methodUsed := func(name string) bool {
		return strings.Contains(rest, "."+name+"(") || strings.Contains(rest, name+"(")
	}

	var unread []string
	// Methods that mention a key without consuming it: Default() assigns the defaults,
	// applyEnv() copies the environment over them, Validate() checks them. All three touch every
	// key, so treating them as readers would make this check pass for keys nothing uses — which is
	// exactly the failure it exists to catch.
	setters := map[string]bool{"Validate": true, "Default": true, "applyEnv": true, "ApplyEnv": true}

	for goName, yamlKey := range fields {
		if _, pinned := knownUnread[goName]; pinned {
			continue
		}
		// Read directly outside this package?
		if strings.Contains(rest, "."+goName) {
			continue
		}
		// Read through an accessor that is itself used elsewhere?
		consumed := false
		for mName, body := range methods {
			if setters[mName] {
				continue
			}
			if mentionsIdent(body, goName) && methodUsed(mName) {
				consumed = true
				break
			}
		}
		if !consumed {
			unread = append(unread, goName+" (yaml: "+yamlKey+")")
		}
	}
	sort.Strings(unread)
	if len(unread) > 0 {
		t.Fatalf("config key(s) that nothing reads:\n  %s\n\n"+
			"An operator sets these in the config file, sees no effect and gets no error. Either "+
			"read them, remove them, or add them to knownUnread in this file with the reason.",
			strings.Join(unread, "\n  "))
	}

	// And the pinned ones must still be unread, or the list is stale in the permissive direction.
	var stale []string
	for goName := range knownUnread {
		if !strings.Contains(rest, "."+goName) {
			readSomewhere := false
			for mName, body := range methods {
				if !setters[mName] && mentionsIdent(body, goName) && methodUsed(mName) {
					readSomewhere = true
					break
				}
			}
			if !readSomewhere {
				continue
			}
		}
		stale = append(stale, goName)
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("knownUnread is out of date for: %s\n\nRemove them from the list so the ratchet "+
			"records the improvement and can catch a regression.", strings.Join(stale, ", "))
	}
}

// mentionsIdent reports whether text mentions the identifier name, as a whole word.
//
// A plain substring check is wrong here and was the first version: "RetentionDays" is a suffix of
// "VersionRetentionDays", so every method that read the version-retention key appeared to read the
// audit-retention key and two dead keys were reported as implemented.
func mentionsIdent(text, name string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(text)
}

// repoRoot walks up until it finds go.mod.
func repoRoot(t *testing.T) string {
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
	t.Fatal("could not find the repository root")
	return ""
}
