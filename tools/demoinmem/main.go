// Command demo-inmem runs a complete, self-contained newCloud instance for a browser demo.
//
// It exists because the shipped `tools/demo` needs Docker (it starts PostgreSQL in a container) and
// this host has no usable container runtime. The server itself supports an in-memory metadata store
// — `openMetadata` falls back to it when DATABASE_URL is unset, with a warning — so a fully working
// instance needs no database at all. The only components that must be real are the ones a browser
// talks to: an OIDC identity provider, and a TLS listener whose certificate covers both hostnames.
//
// So this tool wires up four things in one process:
//
//  1. the project's own fake IdP, in-process, on a loopback port;
//  2. a TLS reverse proxy on https://localhost:<port>, whose certificate covers localhost and
//     content.localhost (two HOSTNAMES, because that is what server.usercontent_url now requires —
//     a different port is a different browser origin but the same cookie jar);
//  3. the real newcloud binary, built from this tree and pointed at the fake IdP with an in-memory
//     store;
//  4. seeded content and shares, created by logging in through the real OIDC flow.
//
// What this is NOT: a production configuration. The store is in memory (nothing survives a restart),
// the IdP accepts any login, and the certificate is self-signed. It is a demonstration.
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
	appPort = flag.Int("app-port", 18080, "port the newcloud binary listens on (plain HTTP, loopback)")
	tlsPort = flag.Int("tls-port", 18443, "port the TLS proxy listens on")
	idpPort = flag.Int("idp-port", 19201, "port the fake IdP listens on")
	dataDir = flag.String("data", "", "state directory (default: ./.demo-inmem)")
	noSeed  = flag.Bool("no-seed", false, "skip creating sample files and shares")
	binPath = flag.String("bin", "", "use this newcloud binary instead of building one")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\ndemo failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir := *dataDir
	if dir == "" {
		dir = ".demo-inmem"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	fmt.Println("newCloud demo (in-memory metadata, no Docker)")
	fmt.Printf("  state directory: %s\n", abs)

	// ---- 1. Fake IdP, in this process ----
	idp, err := fakeidp.NewAt("newcloud-demo", fmt.Sprintf("127.0.0.1:%d", *idpPort), "")
	if err != nil {
		return fmt.Errorf("start fake IdP: %w", err)
	}
	defer idp.Close()
	// The provider runs in THIS process, so seeding can call SetUser on it directly. It is held in a
	// package variable only because seed() is a free function; threading it through as a parameter
	// would be cleaner and is the obvious refactor if this tool grows.
	demoIDP = idp
	fmt.Printf("  identity provider: %s\n", idp.URL())

	// ---- 2. TLS proxy ----
	publicURL := fmt.Sprintf("https://localhost:%d", *tlsPort)
	contentURL := fmt.Sprintf("https://content.localhost:%d", *tlsPort)
	proxy, err := startTLSProxy(fmt.Sprintf("localhost:%d", *tlsPort),
		fmt.Sprintf("http://127.0.0.1:%d", *appPort), contentURL)
	if err != nil {
		return fmt.Errorf("start TLS proxy: %w", err)
	}
	defer proxy.Close()
	fmt.Printf("  public URL:        %s\n", publicURL)
	fmt.Printf("  content URL:       %s\n", contentURL)

	// ---- 3. Configuration ----
	cfgPath := filepath.Join(abs, "config.yml")
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
  # Bounded revalidation (ID-06). 12 hours: long enough that a demo session never expires mid-tour.
  revalidate_minutes: 720
storage:
  backend: "local"
  layout: "blob"
  local:
    root: %q
  upload_dir: %q
limits:
  max_file_bytes: 1073741824
  default_file_quota_bytes: 10737418240
  max_propfind_children: 10000
  # Decode thumbnails in a child process (WC-GAP-004), the production default.
  thumbnail_isolation: "process"
  trash_retention_days: 30
  version_retention_days: 365
mail:
  enabled: false
audit:
  retention_days: 365
`, *appPort, publicURL, contentURL, idp.URL(),
		filepath.Join(abs, "blobs"), filepath.Join(abs, "uploads"))

	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}

	// ---- 4. The binary ----
	bin := *binPath
	if bin == "" {
		bin = filepath.Join(abs, "newcloud.exe")
		fmt.Println("  building the server binary…")
		if err := goBuild(bin); err != nil {
			return err
		}
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("server binary %q: %w", bin, err)
	}

	// DATABASE_URL is deliberately NOT set: the server then uses its in-memory store. `migrate` is
	// skipped for the same reason — there is no schema to apply to a store that has no schema.
	env := append(os.Environ(),
		"NEWCLOUD_CONFIG="+cfgPath,
		"NEWCLOUD_LOG_LEVEL=info",
	)
	env = removeEnv(env, "DATABASE_URL")

	logPath := filepath.Join(abs, "app.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "serve")
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	if err := waitForHTTP(fmt.Sprintf("http://127.0.0.1:%d/healthz", *appPort), 30*time.Second); err != nil {
		tail, _ := os.ReadFile(logPath)
		return fmt.Errorf("server did not become healthy: %w\n--- log ---\n%s", err, tail)
	}
	fmt.Println("  server: healthy")

	// ---- 5. Seed ----
	var seeded seedResult
	if !*noSeed {
		// Seed through the TLS PROXY, not the loopback app port.
		//
		// This is not laziness: the app checks that the OIDC callback arrives on public_url's host
		// (`hostMatches`), so a client talking to 127.0.0.1:18080 gets "callback host mismatch" —
		// rewriting the redirect's host to the loopback address is exactly what that check exists to
		// refuse. Going through the proxy makes the Host header, the scheme and the cookie domain
		// match what a browser would send, so the flow is exercised as a browser exercises it.
		seeded, err = seed(publicURL, publicURL)
		if err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}

	printBanner(publicURL, seeded)

	fmt.Println("\n  Press Ctrl+C to stop.")
	// Wait on a signal rather than `select {}`: with no other runnable goroutine Go's runtime panics
	// with "all goroutines are asleep - deadlock!" and exits, which would kill the demo the moment it
	// finished seeding.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\n  stopping.")
	return nil
}

func removeEnv(env []string, key string) []string {
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// goBuild compiles the server into path.
func goBuild(path string) error {
	cmd := exec.Command("go", "build", "-o", path, "./cmd/newcloud")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}
	return nil
}

// waitForHTTP polls a URL until it answers 200 or the deadline passes.
func waitForHTTP(rawURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(rawURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("timed out")
	}
	return last
}

// ---- TLS proxy ----

type tlsProxy struct{ srv *http.Server }

func (p *tlsProxy) Close() { _ = p.srv.Close() }

// startTLSProxy terminates TLS for both the app and the content hostname.
//
// One listener, one certificate, two DNS names. The certificate must cover content.localhost as well
// as localhost: browsers resolve *.localhost to loopback on every platform that matters, and the
// server refuses to start unless the content origin has a different HOSTNAME from the app origin.
func startTLSProxy(listen, target, contentURL string) (*tlsProxy, error) {
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
		// Preserve the original Host: the server compares it against public_url, and rewriting it to
		// the upstream address would defeat the callback-host check and produce a confusing 404 at
		// the OIDC redirect.
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

// selfSignedCert builds a certificate covering localhost and content.localhost.
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
		DNSNames:     []string{"localhost", "content.localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ---- seeding ----

type seedResult struct {
	Files   int
	Link    string
	Locked  string
	Dropbox string
	Pass    string
	Expired string
}

// seed logs in through the real OIDC flow and creates sample content.
func seed(appBase, publicURL string) (seedResult, error) {
	var out seedResult

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			// The certificate is self-signed; the demo is loopback-only.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Configure who the fake IdP will assert, then walk the login flow.
	idpURL := fmt.Sprintf("http://127.0.0.1:%d", *idpPort)
	if err := setFakeUser(idpURL, fakeidp.User{
		Subject:  "demo-admin",
		Username: "demo",
		Email:    "demo@example.test",
		Name:     "Demo User",
		Groups:   []string{"cloud-users", "cloud-admins"},
	}); err != nil {
		return out, err
	}

	if err := login(client, appBase, publicURL); err != nil {
		return out, err
	}

	// Sample files. Names chosen so the grid view shows several preview kinds.
	files := map[string]string{
		"Welcome.txt": "newCloud demo.\n\nFiles, WebDAV, sharing and an audit trail,\nin one binary and one database.\n",
		"notes.md":    "# Notes\n\n- Thumbnails decode in a child process\n- Uploads resume after a restart\n- Shares can expire\n",
		"data.csv":    "name,size,kind\nalpha,120,file\nbeta,340,dir\n",
	}
	for name, body := range files {
		if err := upload(client, appBase, name, body); err != nil {
			return out, fmt.Errorf("upload %s: %w", name, err)
		}
		out.Files++
	}

	// A folder with a file in it, so the tree view has depth.
	if err := mkdir(client, appBase, "Projects"); err != nil {
		return out, err
	}
	if err := upload(client, appBase, "Projects/plan.txt", "project plan\n"); err != nil {
		return out, err
	}
	out.Files++

	// Shares: an open link, a password-protected one, and a drop box.
	open, err := createShare(client, appBase, "Welcome.txt", map[string]any{"kind": "link"})
	if err != nil {
		return out, err
	}
	out.Link = publicURL + "/s/" + open

	out.Pass = "demo2026"
	locked, err := createShare(client, appBase, "notes.md", map[string]any{
		"kind": "link", "password": out.Pass, "expires_in_seconds": 86400,
	})
	if err != nil {
		return out, err
	}
	out.Locked = publicURL + "/s/" + locked

	box, err := createShare(client, appBase, "Projects", map[string]any{
		"kind": "request", "label": "Drop files here",
	})
	if err != nil {
		return out, err
	}
	out.Dropbox = publicURL + "/s/" + box

	return out, nil
}

// setFakeUser points the fake IdP at a subject. The demo IdP accepts any login and asserts whatever
// user was registered last, which is what makes a single-browser demo work.
func setFakeUser(idpURL string, u fakeidp.User) error {
	// The fake IdP exposes SetUser on the Provider; this tool runs it IN-PROCESS, so the caller
	// already holds the pointer. See run(): the provider is created before seeding and is passed
	// through the package-level variable below.
	if demoIDP == nil {
		return fmt.Errorf("fake IdP not initialised")
	}
	demoIDP.SetUser(u)
	return nil
}

// demoIDP is the in-process fake IdP, set in run().
var demoIDP *fakeidp.Provider

// login walks the OIDC authorization-code flow with PKCE.
func login(client *http.Client, appBase, publicURL string) error {
	// Ask the app for the authorization URL; it answers with a redirect to the IdP.
	req, _ := http.NewRequest(http.MethodGet, appBase+"/api/auth/login", nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("login start: %w", err)
	}
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return fmt.Errorf("login start: no redirect (status %d)", resp.StatusCode)
	}

	// Follow the IdP leg: it redirects straight back to the callback with a code.
	idpResp, err := client.Get(loc)
	if err != nil {
		return fmt.Errorf("idp: %w", err)
	}
	idpResp.Body.Close()
	callback := idpResp.Header.Get("Location")
	if callback == "" {
		return fmt.Errorf("idp: no callback redirect (status %d)", idpResp.StatusCode)
	}

	// The callback host must be the public HTTPS origin, and it already is: this client talks to the
	// same https://localhost:<port> the redirect points at, through the proxy.
	cb := callback

	cbResp, err := client.Get(cb)
	if err != nil {
		return fmt.Errorf("callback: %w", err)
	}
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK && cbResp.StatusCode != http.StatusFound &&
		cbResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(cbResp.Body, 400))
		return fmt.Errorf("callback: status %d: %s", cbResp.StatusCode, body)
	}

	// Confirm the session actually took.
	me, err := client.Get(appBase + "/api/auth/me")
	if err != nil {
		return fmt.Errorf("me: %w", err)
	}
	defer me.Body.Close()
	if me.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(me.Body, 400))
		return fmt.Errorf("session not established (status %d): %s", me.StatusCode, body)
	}
	return nil
}

func upload(client *http.Client, appBase, name, body string) error {
	// `path` is the DIRECTORY the file goes into, relative to the caller's root — an empty string for
	// the root itself. Sending "/" is rejected as an absolute path component, which is what the
	// first run of this tool did.
	dir := filepath.Dir(name)
	if dir == "." || dir == "/" {
		dir = ""
	}
	base := filepath.Base(name)

	var buf strings.Builder
	boundary := "----demoBoundary"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"path\"\r\n\r\n" + dir + "\r\n")
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"" + base + "\"\r\n")
	buf.WriteString("Content-Type: application/octet-stream\r\n\r\n")
	buf.WriteString(body)
	buf.WriteString("\r\n--" + boundary + "--\r\n")

	req, _ := http.NewRequest(http.MethodPost, appBase+"/api/files/upload", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	return nil
}

func mkdir(client *http.Client, appBase, name string) error {
	// The endpoint takes the FULL new path ("Projects"), not a parent plus a name; the handler splits
	// it itself with splitParent.
	body, _ := json.Marshal(map[string]string{"path": name})
	req, _ := http.NewRequest(http.MethodPost, appBase+"/api/files/mkdir", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("mkdir %s: status %d: %s", name, resp.StatusCode, b)
	}
	return nil
}

func createShare(client *http.Client, appBase, path string, extra map[string]any) (string, error) {
	payload := map[string]any{"path": path}
	for k, v := range extra {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, appBase+"/api/shares/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("share %s: status %d: %s", path, resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Token != "" {
		return out.Token, nil
	}
	if out.URL != "" {
		parts := strings.Split(out.URL, "/s/")
		return parts[len(parts)-1], nil
	}
	return "", fmt.Errorf("share %s: no token in response", path)
}

func printBanner(publicURL string, s seedResult) {
	fmt.Printf(`
──────────────────────────────────────────────────────────────
  open in a browser:  %s

  The certificate is self-signed: choose "Advanced" and continue.

  sign in as:         demo@example.test  (admin, any password)

  seeded content:
    files:            %d
`, publicURL, s.Files)

	if s.Link != "" {
		fmt.Printf("    open link:        %s\n", s.Link)
	}
	if s.Locked != "" {
		fmt.Printf("    password link:    %s   (password: %s)\n", s.Locked, s.Pass)
	}
	if s.Dropbox != "" {
		fmt.Printf("    drop box:         %s\n", s.Dropbox)
	}
	fmt.Println("──────────────────────────────────────────────────────────────")
}
