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
