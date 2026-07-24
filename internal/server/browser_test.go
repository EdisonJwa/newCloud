package server_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
)

// browser is an HTTP client that behaves like a browser pinned to the public host.
//
// The indirection exists because of one hard constraint: the session cookie uses the
// __Host- prefix, so it is bound to the exact host that set it. A test that reaches
// the handler on 127.0.0.1 while the application believes it serves
// cloud.example.test cannot carry that cookie — the jar stores it under one name
// and looks it up under another, and the failure looks like a broken login.
//
// The fix is to keep the URL on the public hostname and redirect only the dial. That
// is also what makes the test exercise the real host check and the real cookie
// rules instead of a relaxed variant.
type browser struct {
	t          *testing.T
	client     *http.Client
	publicHost string
	dialAddr   string
}

// newBrowserFor starts an HTTPS test server and wraps it in a browser pinned to
// publicURL.
func newBrowserFor(t *testing.T, h http.Handler, publicURL string) (*httptest.Server, *browser) {
	t.Helper()

	srv := httptest.NewTLSServer(h)

	pub, err := url.Parse(publicURL)
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, pub.Host, srv.Listener.Addr().String())
	return srv, b
}

// newBrowser builds a browser that resolves publicHost to dialAddr.
func newBrowser(t *testing.T, publicHost, dialAddr string) *browser {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			// The httptest server has a self-signed certificate and the connection
			// is pinned to this process, so there is nothing to intercept. Every
			// request is forced through dialAddr below regardless of the URL host.
			InsecureSkipVerify: true,
			ServerName:         publicHost,
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Only the application host is redirected; the fake IdP is a real server
			// on 127.0.0.1 and must be reached at its own address, otherwise its
			// plain-HTTP endpoint would be dialled as TLS.
			if addr == publicHost+":443" {
				var d net.Dialer
				return d.DialContext(ctx, network, dialAddr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}

	return &browser{
		t:          t,
		publicHost: publicHost,
		dialAddr:   dialAddr,
		client:     &http.Client{Jar: jar, Transport: transport},
	}
}

// URL builds a URL on the public host.
func (b *browser) URL(path string) string {
	return "https://" + b.publicHost + path
}

// followRedirects returns a client sharing the jar that follows redirects.
func (b *browser) followRedirects() *http.Client {
	c := *b.client
	c.CheckRedirect = nil
	return &c
}

// noRedirect returns a client sharing the jar that stops at the first redirect.
func (b *browser) noRedirect() *http.Client {
	c := *b.client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// get issues a GET following redirects.
func (b *browser) get(path string) (*http.Response, error) {
	return b.client.Get(b.URL(path))
}

// getRaw issues a GET without following redirects.
func (b *browser) getRaw(path string) (*http.Response, error) {
	return b.noRedirect().Get(b.URL(path))
}

// post issues a POST with no body, following redirects.
func (b *browser) post(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, b.URL(path), nil)
	if err != nil {
		return nil, err
	}
	return b.client.Do(req)
}

// me fetches the current account.
func (b *browser) me() (int, map[string]any) {
	b.t.Helper()
	resp, err := b.get("/api/auth/me")
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			b.t.Fatal(err)
		}
	}
	return resp.StatusCode, body
}

// sessionCookie returns the session token the jar holds for a URL.
func (b *browser) sessionCookie(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == "__Host-wc_session" {
			return c.Value
		}
	}
	return ""
}

// resetJar clears cookies, simulating a second browser.
func (b *browser) resetJar(t *testing.T) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	b.client.Jar = jar
}
