package config

// Validation-gap regressions (2026-10-25 review, fix 9).
//
// Each case here is a value that used to pass Validate while the code downstream treated
// it as broken: a zero mail port, a zero session lifetime, a scheme the browser does not
// have, and a scope set that cannot produce a login. All of them used to be discovered at
// first sign-in (or first send) instead of at startup.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMailPortZeroIsRejected(t *testing.T) {
	c := valid()
	c.Mail = MailConfig{Enabled: true, Host: "smtp.example.test", From: "n@example.test", Port: 0}
	err := c.Validate()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a zero mail port must be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), "mail.port") {
		t.Fatalf("the error should name the field: %v", err)
	}
	// The boundary the message always claimed: 1 is accepted.
	c.Mail.Port = 1
	if err := c.Validate(); err != nil {
		t.Fatalf("mail.port 1 is inside the documented range: %v", err)
	}
}

func TestSessionTTLMustBePositive(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Hour} {
		c := valid()
		c.Auth.SessionTTL = ttl
		err := c.Validate()
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("session_ttl %s must be refused, got %v", ttl, err)
		}
		if !strings.Contains(err.Error(), "session_ttl") {
			t.Fatalf("the error should name the field: %v", err)
		}
	}
	// And the default keeps passing, so an omitted key does not become a refusal.
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatalf("the default session_ttl must stay valid: %v", err)
	}
}

func TestOriginSchemesMustBeHTTPorHTTPS(t *testing.T) {
	cases := map[string]string{
		"public_url":      "gopher://cloud.example.com",
		"usercontent_url": "ftp://usercontent.example.com",
	}
	for field, value := range cases {
		c := valid()
		switch field {
		case "public_url":
			c.Server.PublicURL = value
		case "usercontent_url":
			c.Server.UserContentURL = value
		}
		err := c.Validate()
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("%s=%q must be refused, got %v", field, value, err)
		}
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("the error should name %s: %v", field, err)
		}
	}
}

// TestPlainHTTPOriginWarnsButLoads: dev deployments legitimately run on http, so the
// scheme must not fail validation — but it must not pass in silence either.
func TestPlainHTTPOriginWarnsButLoads(t *testing.T) {
	c := valid()
	c.Server.PublicURL = "http://cloud.local:8080"
	c.Server.UserContentURL = "http://usercontent.local:8081"
	if err := c.Validate(); err != nil {
		t.Fatalf("plain http is legal and must not fail validation: %v", err)
	}
	found := 0
	for _, w := range c.Warnings() {
		if strings.Contains(w, "http") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("expected one warning per http origin, got %d in %v", found, c.Warnings())
	}

	// Warnings are per-Validate-run: fixing the configuration must clear them.
	c.Server.PublicURL = "https://cloud.example.com"
	c.Server.UserContentURL = "https://usercontent.example.com"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings()) != 0 {
		t.Fatalf("warnings survived a re-validate of a fixed configuration: %v", c.Warnings())
	}
}

func TestScopesRequireOpenIDAndEmail(t *testing.T) {
	cases := map[string]string{
		"openid": "login cannot work",
		"email":  "provisioning",
	}
	for missing, want := range cases {
		c := valid()
		scopes := []string{"openid", "profile", "email", "groups"}
		filtered := scopes[:0]
		for _, s := range scopes {
			if s != missing {
				filtered = append(filtered, s)
			}
		}
		c.Auth.Scopes = filtered
		err := c.Validate()
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("scopes without %q must be refused, got %v", missing, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error should name the consequence (%q): %v", want, err)
		}
	}
}
