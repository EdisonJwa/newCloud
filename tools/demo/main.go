// Command demo runs a self-contained instance for a walkthrough.
//
// It exists so the product can be shown without asking anyone to configure an IdP,
// install a database, or obtain a certificate. Everything it needs is local:
//
//	PostgreSQL   started in Docker
//	identity     the bundled fake IdP (cmd/devidp's library form)
//	TLS          a reverse proxy with a generated certificate
//	newcloud     the real binary, unmodified
//
// The TLS proxy is not a convenience. The session cookie uses the __Host- prefix,
// which browsers refuse to store over plain HTTP — that restriction is what stops a
// sibling subdomain from overwriting the cookie, so the demo satisfies it rather than
// weakening it. In production Caddy occupies the same position in the stack.
//
// -no-tls turns that proxy off for environments where a self-signed certificate is the
// obstacle rather than the point — a locked-down browser, a container that cannot trust
// the generated CA, or an evaluator who wants to put their own reverse proxy in front.
// The server sees an http:// public_url and issues its cookies without the __Host-/
// __Secure- prefixes and without Secure, which is the same trade a real plain-HTTP or
// proxy-terminated deployment makes (see ServerConfig.PlainHTTP).
//
// Nothing here ships: it is a launcher, and it is the only place in the tree that
// deliberately uses a self-signed certificate.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/EdisonJwa/newCloud/internal/fakeidp"
)

var (
	appPort    = flag.Int("app-port", 8080, "port the newcloud binary listens on (plain HTTP, loopback)")
	tlsPort    = flag.Int("tls-port", 8443, "port the TLS proxy listens on")
	idpPort    = flag.Int("idp-port", 19200, "port the fake IdP listens on")
	pgPort     = flag.Int("pg-port", 15437, "host port for the demo PostgreSQL container")
	pgName     = flag.String("pg-name", "newcloud-demo-pg", "name of the demo PostgreSQL container")
	keepData   = flag.Bool("keep", false, "leave the demo state behind on exit")
	noSeed     = flag.Bool("no-seed", false, "skip creating sample files and shares")
	dataDir    = flag.String("data", "", "state directory (default: a temp directory)")
	skipDocker = flag.Bool("no-docker", false, "use an existing PostgreSQL via -database")
	database   = flag.String("database", "", "DATABASE_URL to use instead of starting a container")
	noTLS      = flag.Bool("no-tls", false, "serve over plain HTTP with no TLS proxy (cookies drop their __Host- prefixes)")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\ndemo: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "newcloud-demo-")
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// ---- 1. Database ----
	dsn := *database
	if dsn == "" {
		if *skipDocker {
			return fmt.Errorf("-no-docker requires -database")
		}
		var err error
		dsn, err = startPostgres()
		if err != nil {
			return err
		}
		defer func() {
			if *keepData {
				fmt.Printf("\n  demo database kept: %s\n", *pgName)
				return
			}
			_ = exec.Command("docker", "rm", "-f", *pgName).Run()
		}()
	}

	// ---- 2. Fake IdP (in this process) ----
	idp, err := fakeidp.NewAt("newcloud-demo", fmt.Sprintf("127.0.0.1:%d", *idpPort), "")
	if err != nil {
		return fmt.Errorf("start fake IdP: %w", err)
	}
	defer idp.Close()

	// ---- 3. TLS proxy ----
	//
	// -no-tls skips it: the app's own loopback listener becomes the public one, and the
	// content origin keeps its separate hostname — *.localhost resolves to the same
	// loopback address, so the single listener serves both origins exactly as the proxy
	// did. The http:// public_url then switches the server to plain-HTTP cookies.
	publicURL := fmt.Sprintf("https://localhost:%d", *tlsPort)
	userContentURL := fmt.Sprintf("https://content.localhost:%d", *tlsPort)
	if !*noTLS {
		proxy, err := startTLSProxy(fmt.Sprintf("localhost:%d", *tlsPort),
			fmt.Sprintf("http://127.0.0.1:%d", *appPort))
		if err != nil {
			return fmt.Errorf("start TLS proxy: %w", err)
		}
		defer proxy.Close()
	} else {
		publicURL = fmt.Sprintf("http://localhost:%d", *appPort)
		userContentURL = fmt.Sprintf("http://content.localhost:%d", *appPort)
	}

	// ---- 4. Configuration ----
	cfgPath := filepath.Join(dir, "config.yml")
	cfg := fmt.Sprintf(`server:
  addr: "127.0.0.1:%d"
  public_url: %q
  usercontent_url: %q
auth:
  issuer: %q
  client_id: "newcloud-demo"
  client_secret: "demo-secret"
  session_secret: "demo-only-session-secret-32-bytes!"
  scopes: ["openid", "profile", "email", "groups"]
  group_role_map:
    "cloud-users": user
    "cloud-admins": admin
storage:
  backend: "local"
  layout: "blob"
  local:
    root: %q
  upload_dir: %q
mail:
  enabled: false
`, *appPort, publicURL, userContentURL,
		idp.URL(), filepath.Join(dir, "blobs"), filepath.Join(dir, "uploads"))

	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}

	// ---- 5. Build and start the real binary ----
	binPath := filepath.Join(dir, "newcloud.exe")
	if err := goBuild(binPath); err != nil {
		return err
	}

	env := append(os.Environ(),
		"NEWCLOUD_CONFIG="+cfgPath,
		"DATABASE_URL="+dsn,
		"NEWCLOUD_LOG_LEVEL=warn",
	)

	if out, err := runCmd(env, binPath, "migrate"); err != nil {
		return fmt.Errorf("migrate: %w\n%s", err, out)
	}

	appLog, err := os.Create(filepath.Join(dir, "app.log"))
	if err != nil {
		return err
	}
	defer appLog.Close()

	app := exec.Command(binPath, "serve")
	app.Env = env
	app.Stdout, app.Stderr = appLog, appLog
	if err := app.Start(); err != nil {
		return fmt.Errorf("start newcloud: %w", err)
	}
	defer func() {
		_ = app.Process.Kill()
		_, _ = app.Process.Wait()
	}()

	if err := waitHealthy(*appPort, 45*time.Second); err != nil {
		tail, _ := os.ReadFile(filepath.Join(dir, "app.log"))
		return fmt.Errorf("server did not become healthy: %w\n%s", err, tail)
	}

	// ---- 6. Sample data ----
	var seeded seedResult
	if !*noSeed {
		seeded, err = seed(publicURL)
		if err != nil {
			// A failed seed is not fatal, but it must be loud: an empty demo looks like a
			// broken product rather than incomplete sample data.
			fmt.Printf("\n  WARNING: sample data was not created: %v\n", err)
			fmt.Printf("  the instance still works; create files through the UI.\n")
		}
	}

	// ---- 7. Report ----
	printBanner(publicURL, idp.URL(), dir, seeded)

	// ---- 8. Wait ----
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("\n  shutting down…")
	return nil
}

// startPostgres runs a throwaway PostgreSQL container and waits for it.
func startPostgres() (string, error) {
	_ = exec.Command("docker", "rm", "-f", *pgName).Run()

	fmt.Println("  starting PostgreSQL…")
	out, err := exec.Command("docker", "run", "-d",
		"--name", *pgName,
		"-e", "POSTGRES_PASSWORD=demo",
		"-e", "POSTGRES_USER=newcloud",
		"-e", "POSTGRES_DB=newcloud",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", *pgPort),
		"postgres:17").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run: %w\n%s", err, out)
	}

	dsn := fmt.Sprintf("postgres://newcloud:demo@127.0.0.1:%d/newcloud?sslmode=disable", *pgPort)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		check := exec.Command("docker", "exec", *pgName, "pg_isready", "-U", "newcloud")
		if err := check.Run(); err == nil {
			return dsn, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("PostgreSQL did not become ready")
}

// goBuild compiles the binary into the demo directory.
func goBuild(out string) error {
	fmt.Println("  building newcloud…")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/newcloud")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// runCmd runs the binary once and returns combined output.
func runCmd(env []string, bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitHealthy polls the health endpoint.
func waitHealthy(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", port)
	var last error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return last
}

// ---- TLS proxy ----

type tlsProxy struct{ srv *http.Server }

func (p *tlsProxy) Close() { _ = p.srv.Close() }

// startTLSProxy terminates TLS in front of the plain-HTTP binary.
//
// It keeps the incoming Host header, because the application's OIDC callback verifies it
// against the configured public URL. Rewriting Host would make the check test something
// other than what production does.
func startTLSProxy(listen, target string) (*tlsProxy, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, err
	}
	upstream, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	rp := httputil.NewSingleHostReverseProxy(upstream)
	inner := rp.Director
	rp.Director = func(r *http.Request) {
		host := r.Host
		inner(r)
		r.Host = host
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:   rp,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return &tlsProxy{srv: srv}, nil
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// content.localhost is listed because the demo serves user content from a separate
		// HOSTNAME, which is what server.usercontent_url requires: a different port alone is a
		// different browser origin but the same cookie jar. Without this name the demo would be
		// refused at startup, or serve content through a certificate error.
		DNSNames:    []string{"localhost", "content.localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		IsCA:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ---- seeding ----

type seedResult struct {
	Files    int
	Link     string // link share, open
	Locked   string // link share with a password
	Dropbox  string // request share
	Password string
	Expired  string // an expired link, to show the failure mode
}

// seed logs in through the real OIDC flow and creates sample content.
func seed(base string) (seedResult, error) {
	var out seedResult

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	noRedirect := &http.Client{Jar: jar, Transport: client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	fmt.Println("  creating sample data…")

	// Login.
	resp, err := noRedirect.Get(base + "/api/auth/login")
	if err != nil {
		return out, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return out, fmt.Errorf("login: %d", resp.StatusCode)
	}
	authURL := resp.Header.Get("Location")
	// The IdP is reached at its own address; retarget only the host.
	if u, e := url.Parse(authURL); e == nil {
		u.Host = strings.TrimPrefix(strings.TrimPrefix(idpAddress(), "http://"), "https://")
		authURL = u.String()
	}
	idpResp, err := noRedirect.Get(authURL)
	if err != nil {
		return out, err
	}
	idpResp.Body.Close()
	cb := idpResp.Header.Get("Location")
	if u, e := url.Parse(cb); e == nil {
		u.Host = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
		cb = u.String()
	}
	cbResp, err := noRedirect.Get(cb)
	if err != nil {
		return out, err
	}
	cbResp.Body.Close()

	// Directories and files.
	for _, d := range []string{"合同", "设计稿", "图片", "archive"} {
		_, _ = postJSON(client, base+"/api/files/mkdir", map[string]any{"path": d})
	}

	// Sample files that actually render.
	//
	// A placeholder "PDF" that is not a PDF makes the preview look broken rather than the
	// data look fake: the browser shows its cannot-display icon and the visitor concludes
	// the product is at fault. Every entry below is a real, small file of its type.
	files := []struct{ dir, name, body string }{
		{"", "README.txt", demoReadme},
		{"", "报告.md", demoMarkdown},
		{"", "数据.csv", demoCSV},
		{"图片", "示例图.png", demoPNG()},
		{"合同", "采购合同-2026.pdf", demoPDF()},
		{"设计稿", "首页-v3.fig", "figma-binary-placeholder\n"},
		{"archive", "2025-年度总结.txt", "占位内容\n"},
	}
	for _, f := range files {
		if err := upload(client, base, f.dir, f.name, f.body); err != nil {
			return out, err
		}
		out.Files++
	}

	// An open link share.
	if v, err := postJSON(client, base+"/api/shares/", map[string]any{
		"path": "README.txt", "kind": "link", "label": "公开说明",
	}); err == nil {
		out.Link = shareURL(base, tokenOf(v))
	}

	// A password-protected link with a download cap and an expiry.
	out.Password = "demo2026"
	if v, err := postJSON(client, base+"/api/shares/", map[string]any{
		"path": "合同/采购合同-2026.pdf", "kind": "link", "label": "合同（限 3 次）",
		"password": out.Password, "max_downloads": 3, "expires_in_seconds": 86400,
	}); err == nil {
		out.Locked = shareURL(base, tokenOf(v))
	}

	// A drop box: accepts uploads, reveals nothing.
	if v, err := postJSON(client, base+"/api/shares/", map[string]any{
		"path": "archive", "kind": "request", "label": "收件箱（只能上传）",
		"hide_contents": true,
	}); err == nil {
		out.Dropbox = shareURL(base, tokenOf(v))
	}

	return out, nil
}

func idpAddress() string {
	return fmt.Sprintf("http://127.0.0.1:%d", *idpPort)
}

func postJSON(c *http.Client, target string, payload any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	resp, err := c.Post(target, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, raw)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func upload(c *http.Client, base, dir, name, content string) error {
	body := &strings.Builder{}
	mw := newMultipart(body)
	if err := mw.writeField("path", dir); err != nil {
		return err
	}
	if err := mw.writeFile("file", name, content); err != nil {
		return err
	}
	if err := mw.close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, base+"/api/files/upload", strings.NewReader(body.String()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.contentType())

	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload %s: %d: %s", name, resp.StatusCode, raw)
	}
	return nil
}

func printBanner(publicURL, idpURL, dir string, s seedResult) {
	line := strings.Repeat("─", 74)
	fmt.Printf("\n%s\n  newcloud demo is running\n%s\n\n", line, line)
	fmt.Printf("  Open in a browser  %s\n\n", publicURL)
	fmt.Printf("  The certificate is self-signed, so the browser will warn.\n")
	fmt.Printf("  Choose \"Advanced\" and continue to localhost.\n\n")
	fmt.Printf("  Sign-in            OIDC (fake IdP; signs in as alice automatically)\n")
	fmt.Printf("  IdP console        %s\n\n", idpURL)

	if s.Files > 0 {
		fmt.Printf("%s\n  Links you can try right now\n%s\n\n", line, line)
		if s.Link != "" {
			fmt.Printf("  Public link (no password)\n    %s\n\n", s.Link)
		}
		if s.Locked != "" {
			fmt.Printf("  Password + 3-download limit\n    %s\n    Password %s\n\n", s.Locked, s.Password)
		}
		if s.Dropbox != "" {
			fmt.Printf("  Drop box (upload only; the contents are not listed)\n    %s\n\n", s.Dropbox)
		}
	}

	fmt.Printf("%s\n  Suggested walkthrough\n%s\n\n", line, line)
	fmt.Printf(`  1. Sign in on the home page: 4 sample files and 3 folders.
  2. Files -> Share: create a link and try password, download limit, and expiry.
  3. The Shares tab revokes a link you have already handed out.
  4. The Trash tab: delete a file, then restore it.
  5. App passwords: create a WebDAV credential.
  6. Connect a WebDAV client to %s/dav/alice/
     (user alice, password from step 5).
  7. Open the drop-box link: upload only, no directory listing.
  8. Open the 3-download link: the fourth attempt is refused.
  9. Audit: GET %s/api/audit?format=csv shows every step.

`, publicURL, publicURL)

	fmt.Printf("%s\n  State directory %s\n  Press Ctrl+C to stop (the demo database is removed)\n%s\n\n", line, dir, line)
}

// tokenOf extracts the share token from a create response.
func tokenOf(v map[string]any) string {
	tok, _ := v["token"].(string)
	return tok
}

// shareURL builds the visitor-facing link.
func shareURL(base, token string) string {
	if token == "" {
		return ""
	}
	return base + "/s/" + token
}
