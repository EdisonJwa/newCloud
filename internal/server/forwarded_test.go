package server_test

// Forwarded headers must not decide who the client is (§9.4, T-01/T-13).
//
// §9.4: 反向代理转发协议与可信源列表必须有显式配置 — the forwarded-protocol handling and the list of
// trusted sources must be explicit.
//
// The server applied chi's RealIP middleware unconditionally, which rewrites r.RemoteAddr from
// X-Forwarded-For for ANY caller. Every address-derived control then trusted a header the client
// chooses: the break-glass CIDR restriction, the per-IP share password and recipient throttles, the
// upload rate limit, and the ActorIP recorded in the audit log.
//
// The break-glass case is a direct authorization bypass: an operator restricts break-glass to an
// admin network, and the restriction is defeated by adding one header.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
	"github.com/EdisonJwa/newCloud/internal/password"
)

// TestForwardedHeaderCannotForgeTheClientAddress is the bypass itself.
//
// A request arriving from an address OUTSIDE the allowed set must be refused even when it claims a
// forwarded address inside it. Both halves matter: the refusal proves the header is not trusted
// from an arbitrary peer, and the second case proves the restriction still works for a genuine
// caller, so the fix is not simply "disable the check".
func TestForwardedHeaderCannotForgeTheClientAddress(t *testing.T) {
	// Allowed: an internal range the test client is NOT on. The test client connects from
	// 127.0.0.1, so it must be refused.
	h, _ := breakglassHarness(t, []string{"10.0.0.0/8"})

	body, _ := json.Marshal(map[string]string{
		"username": "breakglass-admin", "password": "correct horse battery",
	})
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/auth/breakglass",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The attacker's claim. 10.1.2.3 is inside the allowed range.
	req.Header.Set("X-Forwarded-For", "10.1.2.3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	status := resp.StatusCode
	payload := readBody(resp)
	resp.Body.Close()

	if status == http.StatusOK {
		t.Fatalf("break-glass login SUCCEEDED from 127.0.0.1, an address outside the allowed "+
			"range, because the request claimed X-Forwarded-For: 10.1.2.3 (%d %s). The CIDR "+
			"restriction is then not a restriction: any client on the internet can bypass it with "+
			"one header, and the same header also forges the ActorIP in the audit log and the key "+
			"for every per-IP throttle.", status, payload)
	}

	// The restriction must still work in the direction it was written for: a real caller inside
	// the allowed range is accepted.
	h2, _ := breakglassHarness(t, []string{"127.0.0.1/32"})
	req2, err := http.NewRequest(http.MethodPost, h2.srv.URL+"/api/auth/breakglass",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	status2 := resp2.StatusCode
	payload2 := readBody(resp2)
	resp2.Body.Close()
	if status2 != http.StatusOK {
		t.Fatalf("a genuine caller inside the allowed range was refused (%d %s); the network "+
			"check must not have been disabled, only made unspoofable", status2, payload2)
	}
}

// TestTrustedProxyMaySetTheClientAddress is the other half of §9.4: the list must actually work.
//
// A fix that simply ignored X-Forwarded-For would pass the test above and break every deployment
// behind a reverse proxy — the client address would become the proxy's address, collapsing all
// per-IP limits onto one key and recording the proxy as the actor in every audit event. The
// requirement asks for a trusted SOURCE LIST, not for the header to be unusable.
func TestTrustedProxyMaySetTheClientAddress(t *testing.T) {
	// The allowed range names the FORWARDED address, not the peer: the peer is 127.0.0.1, which is
	// trusted as a proxy but is not itself a permitted break-glass origin. That is what makes this
	// a real test of the header — the same configuration refuses the request when the proxy is not
	// trusted, and accepts it when it is.
	trusted := trustedProxyHarness(t, []string{"10.0.0.0/8"})
	body, _ := json.Marshal(map[string]string{
		"username": "breakglass-admin", "password": "correct horse battery",
	})
	req, err := http.NewRequest(http.MethodPost, trusted.srv.URL+"/api/auth/breakglass",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The peer is 127.0.0.1, which IS trusted, so the header is honoured and the effective client
	// address is 10.1.2.3 — inside the allowed range.
	req.Header.Set("X-Forwarded-For", "10.1.2.3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	status := resp.StatusCode
	payload := readBody(resp)
	resp.Body.Close()

	if status != http.StatusOK {
		t.Fatalf("a trusted proxy's X-Forwarded-For was not honoured (%d %s). Behind a reverse "+
			"proxy every request would then appear to come from the proxy itself, which collapses "+
			"the per-IP throttles onto one key and records the proxy as the actor in the audit log.",
			status, payload)
	}
}

// trustedProxyHarness builds a break-glass server whose trusted_proxies includes the test peer.
func trustedProxyHarness(t *testing.T, allowedCIDRs []string) *harness {
	t.Helper()
	hash, err := password.Hash("correct horse battery", password.Params{
		Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass.hex")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return newHarnessTuned(t, func(c *config.Config) {
		c.Auth.AllowLocalAdmin = true
		c.Auth.LocalAdmin = config.LocalAdminConfig{
			Username: "breakglass-admin", PasswordFile: path, AllowedCIDRs: allowedCIDRs,
		}
		c.Server.TrustedProxies = []string{"127.0.0.1/32"}
	}, nil)
}
