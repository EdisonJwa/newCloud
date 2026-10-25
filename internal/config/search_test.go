package config

// The `search` and `limits.comment_rate_per_hour` sections (core-scope §7.2, §6).
//
// These are validated rather than merely parsed, and the reason is the same one the configuration
// ratchet exists for: a key that is accepted, documented and silently ignored is worse than a key
// that does not exist, because an operator configures it and then believes it is in effect.
//
// The two claims worth pinning hardest:
//
//   * An engine named without URLs is a STARTUP ERROR. A deployment that says
//     `engine: elasticsearch` and can never reach anything would otherwise run forever with content
//     search silently absent — and the user-facing symptom is "my document is not there".
//   * A content type with no extractor is REFUSED, not ignored. An operator who adds "docx"
//     expects Office documents to be searchable; accepting the key and ignoring it is the
//     "silently worse than promised" outcome §7.2 names by hand.

import (
	"errors"
	"strings"
	"testing"
)

// TestSearchDefaultsToNoEngine is the product decision, expressed as a configuration default.
//
// `none` is not a degraded mode of the product; it is a smaller version of it. A default of
// "elasticsearch" would make every fresh deployment fail validation until the operator found a
// cluster they did not know they needed.
func TestSearchDefaultsToNoEngine(t *testing.T) {
	c := Default()
	if c.Search.Engine != "none" {
		t.Fatalf("the default engine is %q, want none", c.Search.Engine)
	}
	if c.SearchEnabled() {
		t.Fatal("a default configuration reports content search as enabled")
	}
	if c.Search.IndexBytes <= 0 {
		t.Fatalf("index_bytes defaults to %d; a zero cap would index no content at all",
			c.Search.IndexBytes)
	}
	// The types an extractor exists for. The set is small on purpose and the next test pins that
	// anything outside it is refused.
	got := strings.Join(c.Search.ContentTypes, ",")
	if got != "text,markdown,pdf" {
		t.Fatalf("the default content types are %q, want text,markdown,pdf", got)
	}
}

// TestElasticsearchWithoutURLsIsRefused is the "cannot reach anything" case.
func TestElasticsearchWithoutURLsIsRefused(t *testing.T) {
	c := valid()
	c.Search.Engine = "elasticsearch"
	err := c.Validate()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("an engine with no URLs validated cleanly; a deployment that names an engine it "+
			"can never reach would run forever with content search silently absent: %v", err)
	}
	if !strings.Contains(err.Error(), "search.elasticsearch.urls") {
		t.Fatalf("the error does not name the missing field: %v", err)
	}
}

// TestAnUnknownEngineIsRefused: a typo must not silently mean "none".
func TestAnUnknownEngineIsRefused(t *testing.T) {
	c := valid()
	c.Search.Engine = "opensearch"
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown engine validated cleanly; a typo that silently disables content search " +
			"is indistinguishable from an operator who chose not to have it")
	}
}

// TestAValidEnginePassesAndNormalises checks the accepting path, including the case-fold.
func TestAValidEnginePassesAndNormalises(t *testing.T) {
	c := valid()
	c.Search.Engine = "ElasticSearch"
	c.Search.Elasticsearch.URLs = []string{"https://es.internal:9200"}
	if err := c.Validate(); err != nil {
		t.Fatalf("a valid engine configuration was refused: %v", err)
	}
	if c.Search.Engine != "elasticsearch" {
		t.Fatalf("the engine was stored as %q, want the normalised elasticsearch", c.Search.Engine)
	}
	if !c.SearchEnabled() {
		t.Fatal("SearchEnabled reports false for a configured engine")
	}
	if c.Search.Elasticsearch.IndexPrefix != "newcloud" {
		t.Fatalf("the index prefix defaulted to %q, want newcloud", c.Search.Elasticsearch.IndexPrefix)
	}
}

// TestEngineURLsAreValidated: a bare host or a wrong scheme is an operator mistake with a confusing
// symptom (every write reported unavailable), so it is caught at startup.
func TestEngineURLsAreValidated(t *testing.T) {
	for _, u := range []string{
		"not a url",
		"//missing-scheme",
		"ftp://es.internal:9200",
		"es.internal:9200",
	} {
		t.Run(u, func(t *testing.T) {
			c := valid()
			c.Search.Engine = "elasticsearch"
			c.Search.Elasticsearch.URLs = []string{u}
			if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("%q was accepted as an engine URL: %v", u, err)
			}
		})
	}
}

// TestPlainHTTPToAPublicEngineWarns: on a private network plain HTTP is a normal choice, and saying
// so is not. The distinction is the host, not the port.
func TestPlainHTTPToAPublicEngineWarns(t *testing.T) {
	cases := map[string]bool{
		"http://es.example.com:9200":  true,  // public: warns
		"http://10.0.0.5:9200":        false, // private space: quiet
		"http://127.0.0.1:9200":       false, // loopback: quiet
		"http://localhost:9200":       false, // loopback by name: quiet
		"https://es.example.com:9200": false, // encrypted: quiet
	}
	for raw, wantWarning := range cases {
		t.Run(raw, func(t *testing.T) {
			c := valid()
			c.Search.Engine = "elasticsearch"
			c.Search.Elasticsearch.URLs = []string{raw}
			if err := c.Validate(); err != nil {
				t.Fatalf("%q was refused: %v", raw, err)
			}
			warned := false
			for _, w := range c.Warnings() {
				if strings.Contains(w, "plain http") {
					warned = true
				}
			}
			if warned != wantWarning {
				t.Fatalf("%q: warning=%v, want %v (warnings: %v)",
					raw, warned, wantWarning, c.Warnings())
			}
		})
	}
}

// TestInsecureSkipVerifyIsSaidOutLoud: it removes the only protection against an active attacker on
// the path to the engine, so a startup that does not mention it makes the deployment look encrypted.
func TestInsecureSkipVerifyIsSaidOutLoud(t *testing.T) {
	c := valid()
	c.Search.Engine = "elasticsearch"
	c.Search.Elasticsearch.URLs = []string{"https://es.internal:9200"}
	c.Search.Elasticsearch.InsecureSkipVerify = true
	if err := c.Validate(); err != nil {
		t.Fatalf("insecure_skip_verify was refused rather than warned about: %v", err)
	}
	warned := false
	for _, w := range c.Warnings() {
		if strings.Contains(w, "insecure_skip_verify") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("insecure_skip_verify produced no warning (warnings: %v)", c.Warnings())
	}
}

// TestTheIndexPrefixIsRestricted: it becomes part of a URL path, so a slash would address a
// different endpoint entirely and an uppercase letter would address a different index.
func TestTheIndexPrefixIsRestricted(t *testing.T) {
	for _, prefix := range []string{"newcloud/other", "NewCloud", "new cloud", "new$cloud"} {
		t.Run(prefix, func(t *testing.T) {
			c := valid()
			c.Search.Engine = "elasticsearch"
			c.Search.Elasticsearch.URLs = []string{"https://es.internal:9200"}
			c.Search.Elasticsearch.IndexPrefix = prefix
			if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("the prefix %q was accepted: %v", prefix, err)
			}
		})
	}
	for _, prefix := range []string{"newcloud", "acme-2", "a_b.c"} {
		t.Run("ok_"+prefix, func(t *testing.T) {
			c := valid()
			c.Search.Engine = "elasticsearch"
			c.Search.Elasticsearch.URLs = []string{"https://es.internal:9200"}
			c.Search.Elasticsearch.IndexPrefix = prefix
			if err := c.Validate(); err != nil {
				t.Fatalf("the prefix %q was refused: %v", prefix, err)
			}
		})
	}
}

// TestAContentTypeWithNoExtractorIsRefused is the "silently worse than promised" case, and it is
// checked with the engine OFF as well: the refusal is about the extractor, not about the engine.
func TestAContentTypeWithNoExtractorIsRefused(t *testing.T) {
	for _, ct := range []string{"docx", "xlsx", "pptx", "odt", "rtf", "html"} {
		t.Run(ct, func(t *testing.T) {
			c := valid()
			// No engine configured: the key still must not be accepted, because an operator who
			// adds it here is configuring the deployment for a type nothing will ever extract.
			c.Search.ContentTypes = append(c.Search.ContentTypes, ct)
			err := c.Validate()
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("%q was accepted as a content type; this build has no extractor for it, "+
					"so accepting the key makes an operator believe it is searchable: %v", ct, err)
			}
			if !strings.Contains(err.Error(), "search.content_types") {
				t.Fatalf("the error does not name the field: %v", err)
			}
			if !strings.Contains(err.Error(), ct) {
				t.Fatalf("the error does not name the offending type: %v", err)
			}
		})
	}
}

// TestTheSupportedContentTypesAreAccepted is the other half: a refusal list that refuses everything
// is a feature nobody can configure.
func TestTheSupportedContentTypesAreAccepted(t *testing.T) {
	for _, ct := range []string{"text", "markdown", "pdf", "Text", " PDF "} {
		t.Run(ct, func(t *testing.T) {
			c := valid()
			c.Search.ContentTypes = []string{ct}
			if err := c.Validate(); err != nil {
				t.Fatalf("%q was refused: %v", ct, err)
			}
		})
	}
}

// TestANegativeIndexCapIsRefused: a negative cap would make every document exceed it, so the index
// would hold metadata and never a body — content search would be "configured" and useless.
func TestANegativeIndexCapIsRefused(t *testing.T) {
	c := valid()
	c.Search.IndexBytes = -1
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a negative index_bytes was accepted: %v", err)
	}
}

// TestTheSearchPasswordFileIsReadOnlyWhenEnabled pins the condition, because an unconditional read
// makes a deployment with `engine: none` fail to start when a stale path is left in the file.
//
// It goes through Load rather than Validate: the read happens during Load, so a test that called
// Validate would pass whether or not the file was ever opened, and would be asserting the wrong
// thing while looking like it asserted this one.
func TestTheSearchPasswordFileIsReadOnlyWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	secret := dir + "/es.pass"
	if err := writeFile(secret, "hunter2\n"); err != nil {
		t.Fatal(err)
	}
	missing := dir + "/absent.pass"

	// yaml renders a configuration with the search section, substituting the file paths.
	//
	// The paths are single-quoted, because YAML single quotes are literal: a Windows temp path in a
	// double-quoted scalar is read as escapes (`\U`, `\a`, `\L`) and fails to parse, which would make
	// this test fail for a reason that has nothing to do with the read being conditional.
	yaml := func(t *testing.T, engine, urls, password, passwordFile string) string {
		t.Helper()
		var b strings.Builder
		b.WriteString("server:\n")
		b.WriteString("  public_url: https://cloud.example.com\n")
		b.WriteString("  usercontent_url: https://usercontent.example.com\n")
		b.WriteString("auth:\n")
		b.WriteString("  issuer: https://id.example.com\n")
		b.WriteString("  client_id: newcloud\n")
		b.WriteString("  client_secret: s3cret\n")
		b.WriteString("  session_secret: " + strings.Repeat("k", 32) + "\n")
		b.WriteString("search:\n")
		b.WriteString("  engine: " + engine + "\n")
		b.WriteString("  elasticsearch:\n")
		if urls != "" {
			b.WriteString("    urls:\n      - '" + urls + "'\n")
		}
		if password != "" {
			b.WriteString("    password: '" + password + "'\n")
		}
		if passwordFile != "" {
			b.WriteString("    password_file: '" + passwordFile + "'\n")
		}
		path := dir + "/config.yml"
		if err := writeFile(path, b.String()); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("a disabled engine never reads the file", func(t *testing.T) {
		// The path does not exist. Nothing is going to read it, and requiring it to exist would make
		// an example configuration that mentions a secret path fail to start.
		cfg, err := Load(yaml(t, "none", "", "", missing))
		if err != nil {
			t.Fatalf("a deployment with no engine failed over an unused password_file: %v", err)
		}
		if cfg.Search.Elasticsearch.Password != "" {
			t.Fatal("a disabled engine loaded a password anyway")
		}
	})

	t.Run("an enabled engine reads it", func(t *testing.T) {
		cfg, err := Load(yaml(t, "elasticsearch", "https://es.internal:9200", "", secret))
		if err != nil {
			t.Fatalf("a readable password_file was refused: %v", err)
		}
		// Trailing whitespace stripped: a secret written by `echo` or `openssl rand -hex` carries a
		// newline, and sending it to the engine would authenticate as nobody.
		if cfg.Search.Elasticsearch.Password != "hunter2" {
			t.Fatalf("the password loaded as %q, want the file's contents with the newline "+
				"stripped", cfg.Search.Elasticsearch.Password)
		}
	})

	t.Run("an enabled engine with an unreadable file fails", func(t *testing.T) {
		// Fail loudly: an engine that is configured and cannot authenticate looks exactly like one
		// that is reachable but has no documents.
		if _, err := Load(yaml(t, "elasticsearch", "https://es.internal:9200", "", missing)); err == nil {
			t.Fatal("an enabled engine with an unreadable password_file loaded cleanly")
		}
	})

	t.Run("an explicit password wins over the file", func(t *testing.T) {
		cfg, err := Load(yaml(t, "elasticsearch", "https://es.internal:9200", "from-config", missing))
		if err != nil {
			t.Fatalf("an explicit password with a missing file failed: %v", err)
		}
		if cfg.Search.Elasticsearch.Password != "from-config" {
			t.Fatalf("the password is %q, want the configured value: an inline password must not "+
				"be overridden, and its presence must not make the file required",
				cfg.Search.Elasticsearch.Password)
		}
	})
}

// TestTheCommentRateLimitDefaultsToASaneNumber: 0 disables the limit, so a zero default would ship
// the feature with its protection off.
func TestTheCommentRateLimitParameter(t *testing.T) {
	c := Default()
	if c.Limits.CommentRatePerHour != 60 {
		t.Fatalf("comment_rate_per_hour defaults to %d, want 60: a comment a minute is generous "+
			"for a human and far below what a script needs", c.Limits.CommentRatePerHour)
	}
	// Zero is legal and means "no limit"; negative is a configuration mistake, because it reads as
	// a limit and behaves as one that refuses every post.
	c.Limits.CommentRatePerHour = -1
	if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a negative comment rate was accepted: %v", err)
	}
}
