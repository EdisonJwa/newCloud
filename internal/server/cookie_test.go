package server_test

// Cookie prefix rules, as a browser enforces them.
//
// This exists because the server-side tests for the share password gate all passed while every
// browser silently discarded the access cookie: the cookie carried the __Host- prefix and a
// path of /s/<token>, and the prefix requires Path=/. Go's own cookie jar does not implement the
// prefix rules, so the only way to catch this without a browser is to assert the rules here.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/EdisonJwa/newCloud/internal/config"
)

// assertCookiePrefix applies RFC 6265bis §4.1.3 the way a browser does: a cookie that violates
// its prefix's requirements is not stored at all, without an error anywhere.
func assertCookiePrefix(t *testing.T, c *http.Cookie) {
	t.Helper()
	switch {
	case strings.HasPrefix(c.Name, "__Host-"):
		if c.Path != "/" {
			t.Fatalf("cookie %q uses the __Host- prefix with Path=%q: browsers require Path=/, so it will be dropped", c.Name, c.Path)
		}
		if c.Domain != "" {
			t.Fatalf("cookie %q uses the __Host- prefix with a Domain; browsers require none", c.Name)
		}
		if !c.Secure {
			t.Fatalf("cookie %q uses the __Host- prefix without Secure", c.Name)
		}
	case strings.HasPrefix(c.Name, "__Secure-"):
		if !c.Secure {
			t.Fatalf("cookie %q uses the __Secure- prefix without Secure", c.Name)
		}
	}
	// Every cookie this server issues is a credential; none of them may be readable from
	// JavaScript or sent cross-site.
	if strings.Contains(c.Name, "wc_") && !c.HttpOnly {
		t.Fatalf("cookie %q is not HttpOnly", c.Name)
	}
}

// TestShareAccessCookieSurvivesABrowser is the regression test for the password gate: it
// verifies the response that actually gets stored, not just the status code.
func TestShareAccessCookieSurvivesABrowser(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "secret.txt")
	_, _ = io.WriteString(fw, "classified")
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"secret.txt","kind":"link","password":"hunter2"}`),
		map[string]string{"Content-Type": "application/json"})
	token, _ := decode(t, resp)["token"].(string)
	if token == "" {
		t.Fatal("no share token")
	}

	// The password is correct, so this must hand back a usable grant.
	vr := h.do(http.MethodPost, "/s/"+token+"/verify",
		strings.NewReader(`{"password":"hunter2"}`),
		map[string]string{"Content-Type": "application/json"})
	defer vr.Body.Close()
	if vr.StatusCode != http.StatusOK {
		t.Fatalf("verify: %d", vr.StatusCode)
	}

	cookies := vr.Cookies()
	if len(cookies) == 0 {
		t.Fatal("verify returned no cookie: nothing would grant access")
	}
	for _, c := range cookies {
		assertCookiePrefix(t, c)
	}

	// And the grant must actually open the share, sent the way a browser would send it.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/s/"+token, nil)
	req.Header.Set("Accept", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	ar, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Body.Close()
	var info map[string]any
	if err := json.NewDecoder(ar.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info["requires_password"] == true {
		t.Fatal("the share still asks for a password after a correct one: the grant was not accepted")
	}
}

// TestRecipientCodeCookieSurvivesABrowser covers the other path-scoped cookie. It is the same
// mistake in the same place, so it gets the same guard.
func TestRecipientCodeCookieSurvivesABrowser(t *testing.T) {
	h := newHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "offered.txt")
	_, _ = io.WriteString(fw, "for you")
	_ = mw.Close()
	h.do(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.do(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"offered.txt","kind":"email","recipients":["bob@example.test"]}`),
		map[string]string{"Content-Type": "application/json"})
	token, _ := decode(t, resp)["token"].(string)
	if token == "" {
		t.Fatal("no share token")
	}

	// The code is minted when the recipient asks for it, which is also what sends the mail.
	req := h.do(http.MethodPost, "/s/"+token+"/recipient",
		strings.NewReader(`{"email":"bob@example.test"}`),
		map[string]string{"Content-Type": "application/json"})
	req.Body.Close()
	if req.StatusCode != http.StatusAccepted {
		t.Fatalf("recipient request: %d", req.StatusCode)
	}

	code := h.mailer.codeFor("bob@example.test")
	if code == "" {
		t.Fatal("no verification code was generated")
	}
	vr := h.do(http.MethodPost, "/s/"+token+"/recipient/verify",
		strings.NewReader(`{"email":"bob@example.test","code":"`+code+`"}`),
		map[string]string{"Content-Type": "application/json"})
	defer vr.Body.Close()

	for _, c := range vr.Cookies() {
		assertCookiePrefix(t, c)
	}
}

// newPlainHTTPHarness builds a deployment whose public_url is http:// — TLS terminated by an
// upstream reverse proxy, or the demo's -no-tls mode. The cookie contract there is the mirror
// image of the https one: every prefix and the Secure flag must be dropped, because a browser
// refuses both on a non-secure origin and the login silently dies.
func newPlainHTTPHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, func(c *config.Config) {
		c.Server.PublicURL = "http://cloud.example.test"
		c.Server.UserContentURL = "http://content.example.test"
	})
}

// doPlain is h.do for a plain-HTTP harness: the session cookie travels under its unprefixed
// name, which the standard helper hardcodes away for the https case.
func (h *harness) doPlain(method, path string, body io.Reader, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "wc_session", Value: h.session})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// TestPlainHTTPLoginCookiesDropPrefixesAndSecure pins the naming decision for the login flow.
//
// /login issues all four one-shot cookies before it contacts the identity provider, so one
// request that refuses the redirect observes every name this mode produces.
func TestPlainHTTPLoginCookiesDropPrefixesAndSecure(t *testing.T) {
	h := newPlainHTTPHarness(t)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(h.srv.URL + "/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := map[string]*http.Cookie{}
	for _, c := range resp.Cookies() {
		assertCookiePrefix(t, c)
		got[c.Name] = c
	}
	for _, name := range []string{"wc_state", "wc_pkce", "wc_nonce", "wc_next"} {
		c, ok := got[name]
		if !ok {
			t.Fatalf("login did not issue %q; got %v", name, cookieNames(resp.Cookies()))
		}
		if c.Secure {
			t.Fatalf("cookie %q is Secure on a plain-HTTP deployment: a browser will drop it", name)
		}
		if !c.HttpOnly {
			t.Fatalf("cookie %q is not HttpOnly", name)
		}
	}
}

// TestPlainHTTPShareCookieDropsPrefix covers the path-scoped half: the password gate's grant
// must also lose its __Secure- prefix, or a correct password keeps looking like a wrong one.
func TestPlainHTTPShareCookieDropsPrefix(t *testing.T) {
	h := newPlainHTTPHarness(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("path", "")
	fw, _ := mw.CreateFormFile("file", "secret.txt")
	_, _ = io.WriteString(fw, "classified")
	_ = mw.Close()
	h.doPlain(http.MethodPost, "/api/files/upload", body,
		map[string]string{"Content-Type": mw.FormDataContentType()}).Body.Close()

	resp := h.doPlain(http.MethodPost, "/api/shares/",
		strings.NewReader(`{"path":"secret.txt","kind":"link","password":"hunter2"}`),
		map[string]string{"Content-Type": "application/json"})
	token, _ := decode(t, resp)["token"].(string)
	if token == "" {
		t.Fatal("no share token")
	}

	vr := h.doPlain(http.MethodPost, "/s/"+token+"/verify",
		strings.NewReader(`{"password":"hunter2"}`),
		map[string]string{"Content-Type": "application/json"})
	defer vr.Body.Close()
	if vr.StatusCode != http.StatusOK {
		t.Fatalf("verify: %d", vr.StatusCode)
	}
	for _, c := range vr.Cookies() {
		assertCookiePrefix(t, c)
		if strings.HasPrefix(c.Name, "__") {
			t.Fatalf("share cookie %q keeps a prefix on a plain-HTTP deployment", c.Name)
		}
		if c.Secure {
			t.Fatalf("share cookie %q is Secure on a plain-HTTP deployment", c.Name)
		}
		if c.Path != "/s/"+token {
			t.Fatalf("share cookie path = %q, want the per-share scope", c.Path)
		}
	}
}

// TestHTTPSLoginCookiesKeepHostPrefix is the regression guard for the other direction: the
// default https deployment must keep the __Host- prefix and the Secure flag it exists for.
func TestHTTPSLoginCookiesKeepHostPrefix(t *testing.T) {
	h := newHarness(t)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(h.srv.URL + "/api/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := map[string]*http.Cookie{}
	for _, c := range resp.Cookies() {
		assertCookiePrefix(t, c)
		got[c.Name] = c
	}
	for _, name := range []string{"__Host-wc_state", "__Host-wc_pkce", "__Host-wc_nonce", "__Host-wc_next"} {
		c, ok := got[name]
		if !ok {
			t.Fatalf("https login did not issue %q; got %v", name, cookieNames(resp.Cookies()))
		}
		if !c.Secure {
			t.Fatalf("cookie %q lost the Secure flag on an https deployment", name)
		}
	}
}

func cookieNames(cs []*http.Cookie) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}
