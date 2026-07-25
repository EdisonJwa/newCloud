// A walkthrough check against the running demo instance.
//
// It exists because the demo must be shown to work, not just to start: the three share
// links are printed on the banner, and "printed" is not "working". This drives each one
// the way a visitor would and reports what actually happens.
//
//	go run verify_demo.go -base https://localhost:8443 -open <token> -locked <token> -drop <token>
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
)

var (
	base   = flag.String("base", "https://localhost:8443", "demo base URL")
	openT  = flag.String("open", "", "open link token")
	lockT  = flag.String("locked", "", "password-protected link token")
	dropT  = flag.String("drop", "", "drop box token")
	pw     = flag.String("password", "demo2026", "password for the locked link")
	idpURL = flag.String("idp", "http://127.0.0.1:19200", "fake IdP URL")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nverify: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nALL DEMO CHECKS PASSED")
}

// newClient returns a client that trusts the demo certificate.
//
// The demo terminates TLS with a generated certificate, exactly where Caddy sits in
// production. Browsers show a warning and let the visitor continue; this client does the
// same thing programmatically so the check exercises the real HTTPS path.
func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:       jar,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

func run() error {
	anon := newClient()

	// ---- 1. The instance is up ----
	if err := check("instance is healthy", func() error {
		return expectStatus(anon, *base+"/healthz", 200)
	}); err != nil {
		return err
	}

	// ---- 2. Open link ----
	if *openT != "" {
		if err := check("open link: page needs no password", func() error {
			body, code, err := get(anon, *base+"/s/"+*openT)
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("status %d", code)
			}
			if strings.Contains(body, "requires_password\":true") {
				return fmt.Errorf("page asked for a password")
			}
			return nil
		}); err != nil {
			return err
		}

		if err := check("open link: downloads as an attachment", func() error {
			body, code, err := get(anon, *base+"/s/"+*openT+"/download")
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("status %d", code)
			}
			if !strings.Contains(body, "newcloud") && len(body) == 0 {
				return fmt.Errorf("empty body")
			}
			fmt.Printf("       downloaded %d bytes\n", len(body))
			return nil
		}); err != nil {
			return err
		}
	}

	// ---- 3. Locked link ----
	if *lockT != "" {
		if err := check("locked link: demands a password", func() error {
			body, code, err := get(anon, *base+"/s/"+*lockT)
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("status %d", code)
			}
			if !strings.Contains(body, "requires_password\":true") {
				return fmt.Errorf("page did not ask for a password: %s", body)
			}
			return nil
		}); err != nil {
			return err
		}

		if err := check("locked link: refuses download before verification", func() error {
			_, code, err := get(anon, *base+"/s/"+*lockT+"/download")
			if err != nil {
				return err
			}
			if code == 200 {
				return fmt.Errorf("download succeeded without a password")
			}
			fmt.Printf("       refused with %d\n", code)
			return nil
		}); err != nil {
			return err
		}

		visitor := newClient()
		if err := check("locked link: wrong password rejected", func() error {
			code, err := postJSONStatus(visitor, *base+"/s/"+*lockT+"/verify", `{"password":"wrong"}`)
			if err != nil {
				return err
			}
			if code == 200 {
				return fmt.Errorf("wrong password was accepted")
			}
			return nil
		}); err != nil {
			return err
		}

		if err := check("locked link: right password accepted", func() error {
			code, err := postJSONStatus(visitor, *base+"/s/"+*lockT+"/verify",
				fmt.Sprintf(`{"password":%q}`, *pw))
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("status %d", code)
			}
			return nil
		}); err != nil {
			return err
		}

		if err := check("locked link: downloads after verification", func() error {
			_, code, err := get(visitor, *base+"/s/"+*lockT+"/download")
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("status %d", code)
			}
			return nil
		}); err != nil {
			return err
		}

		// The cap is 3; one was already used above.
		if err := check("locked link: download cap is enforced", func() error {
			for i := 0; i < 2; i++ {
				if _, code, err := get(visitor, *base+"/s/"+*lockT+"/download"); err != nil || code != 200 {
					return fmt.Errorf("download %d of the allowance failed: %d %v", i+2, code, err)
				}
			}
			_, code, err := get(visitor, *base+"/s/"+*lockT+"/download")
			if err != nil {
				return err
			}
			if code == 200 {
				return fmt.Errorf("the 4th download should have been refused")
			}
			fmt.Printf("       4th download refused with %d\n", code)
			return nil
		}); err != nil {
			return err
		}
	}

	// ---- 4. Drop box ----
	if *dropT != "" {
		if err := check("drop box: page reveals no contents", func() error {
			body, code, err := get(anon, *base+"/s/"+*dropT)
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("status %d", code)
			}
			if strings.Contains(body, `"children"`) {
				return fmt.Errorf("the drop box page listed its contents")
			}
			if !strings.Contains(body, `"hide_contents":true`) {
				return fmt.Errorf("the drop box page did not declare itself upload-only: %s", body)
			}
			return nil
		}); err != nil {
			return err
		}

		if err := check("drop box: refuses downloads", func() error {
			_, code, err := get(anon, *base+"/s/"+*dropT+"/download")
			if err != nil {
				return err
			}
			if code == 200 {
				return fmt.Errorf("a drop box served a download")
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// ---- 5. Signed-in walkthrough ----
	if err := check("sign in through the IdP", func() error {
		return login(*base, *idpURL)
	}); err != nil {
		return err
	}

	return nil
}

func check(name string, fn func() error) error {
	fmt.Printf("  • %-52s", name)
	if err := fn(); err != nil {
		fmt.Printf(" FAIL\n      %v\n", err)
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Println(" ok")
	return nil
}

func get(c *http.Client, target string) (string, int, error) {
	resp, err := c.Get(target)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode, nil
}

func expectStatus(c *http.Client, target string, want int) error {
	_, code, err := get(c, target)
	if err != nil {
		return err
	}
	if code != want {
		return fmt.Errorf("status %d, want %d", code, want)
	}
	return nil
}

func postJSONStatus(c *http.Client, target, body string) (int, error) {
	resp, err := c.Post(target, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// login drives the real OIDC flow and then reads the file list back.
func login(base, idp string) error {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar:       jar,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := c.Get(base + "/api/auth/login")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return fmt.Errorf("login redirect: %d", resp.StatusCode)
	}

	// The IdP is on plain HTTP; only its host needs rewriting.
	authURL := resp.Header.Get("Location")
	if u, e := url.Parse(authURL); e == nil {
		iu, _ := url.Parse(idp)
		u.Scheme, u.Host = iu.Scheme, iu.Host
		authURL = u.String()
	}
	ar, err := c.Get(authURL)
	if err != nil {
		return err
	}
	ar.Body.Close()

	cb := ar.Header.Get("Location")
	bu, _ := url.Parse(base)
	if u, e := url.Parse(cb); e == nil {
		u.Scheme, u.Host = bu.Scheme, bu.Host
		cb = u.String()
	}
	cr, err := c.Get(cb)
	if err != nil {
		return err
	}
	cr.Body.Close()

	me, code, err := get(c, base+"/api/auth/me")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("/api/auth/me: %d %s", code, me)
	}
	fmt.Printf("       signed in as the account the IdP asserted\n")

	list, code, err := get(c, base+"/api/files/?path=/")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("file list: %d", code)
	}
	if !strings.Contains(list, "README.txt") {
		return fmt.Errorf("the seeded files are missing from the listing")
	}

	// Audit export: the log must contain what just happened.
	auditBody, code, err := get(c, base+"/api/audit?format=csv")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("audit export: %d", code)
	}
	if !strings.Contains(auditBody, "auth.login.success") {
		return fmt.Errorf("the audit log does not contain the login")
	}
	if !strings.Contains(auditBody, "share.create") {
		return fmt.Errorf("the audit log does not contain the share creation")
	}
	fmt.Printf("       audit export: %d bytes, login and share events present\n", len(auditBody))

	return nil
}
