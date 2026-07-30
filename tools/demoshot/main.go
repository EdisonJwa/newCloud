// Screenshots the running demo through headless Chrome.
//
// It talks to the DevTools protocol directly, so it needs no browser-automation
// dependency. The demo already terminates TLS, so unlike tools/uicheck this does not
// start its own proxy — it points Chrome straight at the running instance and accepts
// the generated certificate.
//
//	go run shot_demo.go -base https://localhost:8443 -idp http://127.0.0.1:19200 -out shots
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	base   = flag.String("base", "https://localhost:8443", "demo base URL")
	idpURL = flag.String("idp", "http://127.0.0.1:19200", "fake IdP URL")
	outDir = flag.String("out", "shots", "directory for the PNGs")
	// testDrop drives a real drag-and-drop upload and reports whether it worked, instead of
	// capturing screenshots. It lives here because this tool already knows how to start
	// Chrome and sign in, and the drop path cannot be tested from the DOM: a DataTransfer
	// built in JavaScript carries no OS files.
	testDrop = flag.Bool("test-drop", false, "verify drag-and-drop upload, then exit")
	// eval runs one expression after sign-in and prints the result. Layout bugs are easier to
	// settle by measuring the live DOM than by reasoning about the cascade.
	evalExpr = flag.String("eval", "", "evaluate this JS after sign-in and print the result")
	// startPath navigates to base+path after sign-in and before -eval runs, so a probe can
	// measure a specific view instead of whatever the landing page happens to be.
	startPath = flag.String("path", "", "navigate here (relative) after sign-in, before -eval")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "shot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	chrome, err := findChrome()
	if err != nil {
		return err
	}

	profile, err := os.MkdirTemp("", "newcloud-shot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)

	cmd := exec.Command(chrome,
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile,
		"--no-first-run", "--no-default-browser-check",
		"--disable-gpu", "--disable-extensions",
		"--ignore-certificate-errors",
		"--window-size=1440,1000",
		"about:blank",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	port, err := waitForDevTools(profile)
	if err != nil {
		return err
	}
	wsURL, err := pageWebSocket(port)
	if err != nil {
		return err
	}
	cdp, err := dialWS(wsURL)
	if err != nil {
		return err
	}
	defer cdp.Close()

	for _, d := range []string{"Runtime", "Page", "Log"} {
		if err := cdp.Call(d+".enable", nil); err != nil {
			return err
		}
	}

	// ---- Signed-out: the share landing page ----
	openToken := flag.Arg(0)
	if openToken != "" {
		fmt.Println("capturing the share landing page…")
		if err := navigate(cdp, *base+"/s/"+openToken); err != nil {
			return err
		}
		time.Sleep(1200 * time.Millisecond)
		if err := shoot(cdp, filepath.Join(*outDir, "share-page.png")); err != nil {
			return err
		}
	}

	// ---- Sign in ----
	fmt.Println("signing in…")
	if err := navigate(cdp, *base+"/api/auth/login"); err != nil {
		return err
	}
	time.Sleep(2500 * time.Millisecond)
	// The IdP redirect targets its own address as seen by the server; Chrome reached the
	// app on localhost, so the callback already landed. Verify we are signed in.
	if err := navigate(cdp, *base+"/"); err != nil {
		return err
	}
	time.Sleep(2200 * time.Millisecond)

	// ---- Optional: verify a real drag-and-drop upload ----
	if *testDrop {
		return checkDropUpload(cdp)
	}

	if *startPath != "" {
		if err := navigate(cdp, *base+*startPath); err != nil {
			return err
		}
		time.Sleep(2000 * time.Millisecond)
	}

	if *evalExpr != "" {
		out, err := evalString(cdp, *evalExpr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}

	url, _ := evalString(cdp, "location.href")
	if strings.Contains(url, "auth") {
		return fmt.Errorf("login did not complete, still at %s", url)
	}

	// ---- Home ----
	//
	// The landing surface is the part of the redesign that cannot be judged from a file
	// listing: it is the answer to "what was I doing", built from real data.
	fmt.Println("capturing the home view…")
	if err := shoot(cdp, filepath.Join(*outDir, "home.png")); err != nil {
		return err
	}

	// ---- Files: the column browser ----
	fmt.Println("capturing the column browser…")
	if _, err := goTo(cdp, "Files"); err != nil {
		return err
	}
	time.Sleep(1600 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "files.png")); err != nil {
		return err
	}

	// A selected file turns the trailing column into its detail: preview, properties,
	// versions, links. A screenshot of the list alone would not show that it does anything.
	fmt.Println("capturing the detail column…")
	if msg, err := jsFirst(cdp, ".row:not(.dir)", "click"); err != nil {
		return err
	} else if msg != "ok" {
		return fmt.Errorf("could not select a file: %s", msg)
	}
	time.Sleep(1400 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "files-detail.png")); err != nil {
		return err
	}

	// Descending into a folder is what produces a second column, and a second column is the
	// whole point of the layout: the path becomes space instead of a breadcrumb string.
	fmt.Println("capturing nested columns…")
	if msg, err := jsFirst(cdp, ".row.dir", "dblclick"); err != nil {
		return err
	} else if msg != "ok" {
		fmt.Println("  (no folder in this view; skipping the nested-column frame)")
	} else {
		time.Sleep(1500 * time.Millisecond)
		if err := shoot(cdp, filepath.Join(*outDir, "columns.png")); err != nil {
			return err
		}
	}

	// ---- Immersive preview ----
	fmt.Println("capturing the preview…")
	if msg, err := jsFirst(cdp, ".row:not(.dir)", "dblclick"); err != nil {
		return err
	} else if msg == "ok" {
		time.Sleep(1600 * time.Millisecond)
		if err := shoot(cdp, filepath.Join(*outDir, "preview.png")); err != nil {
			return err
		}
		if _, err := evalString(cdp, "(() => { window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' })); return 'ok'; })()"); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Back to the root: the grid and selection frames need a folder with several entries, and
	// the steps above deliberately descended into a one-file directory.
	if err := navigate(cdp, *base+"/?view=files"); err != nil {
		return err
	}
	time.Sleep(2200 * time.Millisecond)

	// ---- Grid layout ----
	//
	// Only the deepest column can be a grid; the ones to its left are navigation. The frame
	// is captured right after the toggle so it proves the control works, not that a grid can
	// be rendered at all.
	fmt.Println("capturing the grid layout…")
	if msg, err := jsClickSelector(cdp, ".seg button:nth-child(2)"); err != nil {
		return err
	} else if msg != "ok" {
		return fmt.Errorf("could not switch to grid: %s", msg)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "files-grid.png")); err != nil {
		return err
	}
	if _, err := jsClickSelector(cdp, ".seg button:nth-child(1)"); err != nil {
		return err
	}
	time.Sleep(700 * time.Millisecond)

	// ---- Multi-select ----
	fmt.Println("capturing a multi-selection…")
	if msg, err := evalString(cdp, `(() => {
		const rows = [...document.querySelectorAll('.col:last-child .row')];
		if (rows.length < 3) return 'few';
		rows[0].dispatchEvent(new MouseEvent('click', { bubbles: true }));
		rows[2].dispatchEvent(new MouseEvent('click', { bubbles: true, metaKey: true }));
		rows[3].dispatchEvent(new MouseEvent('click', { bubbles: true, metaKey: true }));
		return 'ok';
	})()`); err != nil {
		return err
	} else if msg != "ok" {
		fmt.Println("  (not enough rows in this folder; skipping the selection frame)")
	} else {
		time.Sleep(700 * time.Millisecond)
		if err := shoot(cdp, filepath.Join(*outDir, "files-selection.png")); err != nil {
			return err
		}
		if _, err := evalString(cdp, `(() => {
			const box = document.querySelector('.colbody');
			if (box) box.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
			return 'ok';
		})()`); err != nil {
			return err
		}
		time.Sleep(400 * time.Millisecond)
	}

	// ---- Right to left ----
	//
	// Every rule uses logical properties, so this is the whole of the RTL implementation. The
	// frame exists to prove that claim rather than to assert it.
	fmt.Println("capturing a right-to-left layout…")
	// Capture the hard case: a selected file, so the detail panel is present and the column
	// strip has to share the width with it. Mirroring an empty screen proves very little.
	if _, err := jsFirst(cdp, ".col:last-child .row", "click"); err != nil {
		return err
	}
	time.Sleep(900 * time.Millisecond)
	if _, err := evalString(cdp, "(() => { document.documentElement.dir = 'rtl'; return 'ok'; })()"); err != nil {
		return err
	}
	time.Sleep(600 * time.Millisecond)
	if metrics, err := evalString(cdp, `(() => {
		const r = (e) => e ? { l: Math.round(e.getBoundingClientRect().left), w: Math.round(e.getBoundingClientRect().width) } : null;
		const cols = document.querySelector('.cols');
		return JSON.stringify({
			scrollWidth: cols ? cols.scrollWidth : -1,
			clientWidth: cols ? cols.clientWidth : -1,
			scrollLeft: cols ? cols.scrollLeft : -1,
			collist: r(document.querySelector('.collist')),
			col: r(document.querySelector('.col')),
			detail: r(document.querySelector('.detail')),
		});
	})()`); err != nil {
		return err
	} else {
		fmt.Println("  rtl metrics: " + metrics)
	}
	time.Sleep(700 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "files-rtl.png")); err != nil {
		return err
	}
	if _, err := evalString(cdp, "(() => { document.documentElement.dir = 'ltr'; return 'ok'; })()"); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)

	// ---- Sharing, activity, trash, access ----
	for _, section := range []struct{ name, file string }{
		{"Shared", "shared.png"},
		{"Activity", "activity.png"},
		{"Trash", "trash.png"},
		{"Access", "access.png"},
	} {
		fmt.Printf("capturing %s…\n", strings.ToLower(section.name))
		if _, err := goTo(cdp, section.name); err != nil {
			return err
		}
		time.Sleep(1400 * time.Millisecond)
		if err := shoot(cdp, filepath.Join(*outDir, section.file)); err != nil {
			return err
		}
	}

	// ---- Themes ----
	fmt.Println("capturing the dark and high-contrast palettes…")
	if _, err := goTo(cdp, "Files"); err != nil {
		return err
	}
	time.Sleep(900 * time.Millisecond)
	for _, theme := range []struct{ value, file string }{
		{"dark", "files-dark.png"},
		{"hc", "files-high-contrast.png"},
	} {
		expr := fmt.Sprintf("(() => { document.documentElement.dataset.theme = %q; return 'ok'; })()", theme.value)
		if _, err := evalString(cdp, expr); err != nil {
			return err
		}
		time.Sleep(600 * time.Millisecond)
		if err := shoot(cdp, filepath.Join(*outDir, theme.file)); err != nil {
			return err
		}
	}
	if _, err := evalString(cdp, "(() => { document.documentElement.dataset.theme = 'light'; return 'ok'; })()"); err != nil {
		return err
	}

	// ---- Command palette ----
	fmt.Println("capturing the command palette…")
	if _, err := evalString(cdp, `(() => {
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', metaKey: true, bubbles: true }));
		return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(900 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "palette.png")); err != nil {
		return err
	}
	if _, err := evalString(cdp, "(() => { const s = document.querySelector('.scrim'); if (s) { s.click(); return 'scrim'; } return 'miss'; })()"); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	// ---- Phone ----
	fmt.Println("capturing the phone layout…")
	if err := cdp.Call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true,
	}); err != nil {
		return err
	}
	time.Sleep(1200 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "files-mobile.png")); err != nil {
		return err
	}
	if _, err := goTo(cdp, "Home"); err != nil {
		return err
	}
	time.Sleep(1200 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "home-mobile.png")); err != nil {
		return err
	}
	if err := cdp.Call("Emulation.clearDeviceMetricsOverride", nil); err != nil {
		return err
	}

	fmt.Printf("\nscreenshots written to %s\n", *outDir)
	return nil
}

// goTo clicks a section in the top bar and waits for it to settle.
func goTo(cdp *wsConn, label string) (string, error) {
	return jsClick(cdp, ".nav button", label)
}

// jsClick clicks the first element matching sel whose text contains want.
func jsClick(cdp *wsConn, sel, want string) (string, error) {
	expr := fmt.Sprintf(`(() => {
		const nodes = [...document.querySelectorAll(%q)];
		const el = nodes.find(n => (n.textContent || '').toLowerCase().includes(%q));
		if (!el) return 'miss';
		el.click();
		return 'ok';
	})()`, sel, strings.ToLower(want))
	return evalString(cdp, expr)
}

// jsClickSelector clicks the first element matching a CSS selector.
func jsClickSelector(cdp *wsConn, sel string) (string, error) {
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'miss';
		el.click();
		return 'ok';
	})()`, sel)
	return evalString(cdp, expr)
}

// jsFirst dispatches an event on the first element matching sel.
func jsFirst(cdp *wsConn, sel, event string) (string, error) {
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return 'miss';
		el.dispatchEvent(new MouseEvent(%q, { bubbles: true }));
		return 'ok';
	})()`, sel, event)
	return evalString(cdp, expr)
}

func navigate(cdp *wsConn, target string) error {
	return cdp.Call("Page.navigate", map[string]any{"url": target})
}

// jsShowFiles switches the sidebar back to the file list.
const jsShowFiles = `(() => {
	const b = [...document.querySelectorAll('.rail-nav button')].find(x => x.textContent.trim() === 'Files');
	if (b) b.click();
	return 'ok';
})()`

// checkDropUpload drives a real drag-and-drop upload and reports whether it worked.
//
// This cannot be done from the DOM: a DataTransfer constructed in JavaScript carries no OS
// files, so dispatching a synthetic `drop` proves only that the listener runs. The DevTools
// protocol can dispatch a drag naming a real path, which Chrome turns into a File the page
// can read — the closest thing available to a user dragging from Explorer.
func checkDropUpload(cdp *wsConn) error {
	dir, err := os.MkdirTemp("", "newcloud-drop-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	source := filepath.Join(dir, "dropped-file.txt")
	if err := os.WriteFile(source, []byte("uploaded by dragging\n"), 0o644); err != nil {
		return err
	}

	countRows := func() int {
		out, cerr := evalString(cdp, `(() => String(document.querySelectorAll('.row').length))()`)
		if cerr != nil {
			return -1
		}
		n := 0
		fmt.Sscanf(out, "%d", &n)
		return n
	}

	// Start from the file list, so the count is about the root folder.
	if _, err := evalString(cdp, jsShowFiles); err != nil {
		return err
	}
	time.Sleep(1400 * time.Millisecond)

	before := countRows()
	if before < 0 {
		return fmt.Errorf("could not read the file list")
	}
	fmt.Printf("rows before the drop: %d\n", before)

	data := map[string]any{
		"items":              []map[string]any{},
		"files":              []string{source},
		"dragOperationsMask": 1, // copy
	}

	for _, kind := range []string{"dragEnter", "dragOver", "drop"} {
		if err := cdp.Call("Input.dispatchDragEvent", map[string]any{
			"type": kind, "x": 420, "y": 320, "data": data,
		}); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		time.Sleep(260 * time.Millisecond)
	}

	// The upload is an ordinary POST; poll the listing until it appears.
	deadline := time.Now().Add(15 * time.Second)
	after := before
	for time.Now().Before(deadline) {
		time.Sleep(600 * time.Millisecond)
		after = countRows()
		if after > before {
			break
		}
	}

	if after <= before {
		return fmt.Errorf("the drop added no file (rows stayed at %d)", before)
	}
	fmt.Printf("rows after the drop:  %d\n", after)
	fmt.Println("OK: drag and drop uploaded the file")
	return nil
}

// checkInteractions asserts the behaviours a screenshot cannot show.
//
// The layout exists to support select-then-inspect and keyboard navigation. If either is
// broken the design is decorative, so both are checked against the live DOM.
func checkInteractions(cdp *wsConn) error {
	// Single click selects and fills the inspector.
	if _, err := evalString(cdp, `(() => {
		const row = [...document.querySelectorAll('.row')].find(r => r.textContent.includes('README'));
		if (!row) return 'no-row';
		row.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(1400 * time.Millisecond)

	sel, err := evalString(cdp, `(() => String(document.querySelectorAll('.row[aria-selected="true"]').length))()`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(sel) != "1" {
		return fmt.Errorf("single click did not select exactly one row (got %q)", sel)
	}

	// The inspector must show that file's details, not a placeholder.
	title, err := evalString(cdp, `(() => {
		const n = document.querySelector('.inspector-file .name');
		return n ? n.textContent.trim() : '';
	})()`)
	if err != nil {
		return err
	}
	if !strings.Contains(title, "README") {
		return fmt.Errorf("the inspector did not follow the selection (title %q)", title)
	}
	fmt.Println("  selection fills the inspector")

	// Arrow keys move the selection without a mouse.
	//
	// The event is dispatched on the focused element, because the keys are handled by the
	// listbox rather than by the document. Dispatching on document would test nothing: it
	// does not propagate downward, and a handler that never ran would look like a pass.
	if _, err := evalString(cdp, `(() => {
		const target = document.activeElement && document.activeElement.matches('[role="listbox"]')
			? document.activeElement
			: document.querySelector('[role="listbox"]');
		if (!target) return 'no-listbox';
		target.focus();
		target.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
		return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(900 * time.Millisecond)

	moved, err := evalString(cdp, `(() => {
		const n = document.querySelector('.inspector-file .name');
		return n ? n.textContent.trim() : '';
	})()`)
	if err != nil {
		return err
	}
	if moved == title {
		return fmt.Errorf("ArrowDown did not move the selection (still %q)", moved)
	}
	fmt.Printf("  keyboard moves the selection (%q -> %q)\n", title, moved)

	return nil
}

func shoot(cdp *wsConn, path string) error {
	raw, err := cdp.CallWithResult("Page.captureScreenshot", map[string]any{
		"format": "png", "captureBeyondViewport": false,
	})
	if err != nil {
		return err
	}
	var payload struct {
		Result struct {
			Data string `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	blob, err := base64.StdEncoding.DecodeString(payload.Result.Data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0o644)
}

func evalString(cdp *wsConn, expr string) (string, error) {
	raw, err := cdp.CallWithResult("Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return "", err
	}
	var env struct {
		Result struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
			ExceptionDetails *struct {
				Text string `json:"text"`
			} `json:"exceptionDetails"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	if d := env.Result.ExceptionDetails; d != nil {
		return "", fmt.Errorf("evaluate %q: %s", expr, d.Text)
	}
	if s, ok := env.Result.Result.Value.(string); ok {
		return s, nil
	}
	return "", nil
}

func findChrome() (string, error) {
	for _, c := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no chrome or edge found")
}

func waitForDevTools(profile string) (string, error) {
	p := filepath.Join(profile, "DevToolsActivePort")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(p); err == nil {
			if line, _, ok := strings.Cut(string(data), "\n"); ok {
				if port := strings.TrimSpace(line); port != "" {
					return port, nil
				}
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	return "", fmt.Errorf("chrome did not expose a debugging port")
}

func pageWebSocket(port string) (string, error) {
	cl := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := cl.Get("http://127.0.0.1:" + port + "/json/list")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var targets []struct {
		Type string `json:"type"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Type == "page" && t.WS != "" {
			return t.WS, nil
		}
	}
	return "", fmt.Errorf("no page target")
}

// ---- minimal CDP WebSocket client ----

type wsConn struct {
	conn     net.Conn
	br       *bufio.Reader
	writeMu  sync.Mutex
	nextID   int64
	mu       sync.Mutex
	pending  map[int64]chan json.RawMessage
	closed   chan struct{}
	closeOne sync.Once
}

func dialWS(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, err
	}

	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("handshake: %d", resp.StatusCode)
	}

	w := &wsConn{
		conn: conn, br: br,
		pending: map[int64]chan json.RawMessage{},
		closed:  make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

func (w *wsConn) Close() error {
	var err error
	w.closeOne.Do(func() {
		close(w.closed)
		err = w.conn.Close()
	})
	return err
}

func (w *wsConn) Call(method string, params any) error {
	_, err := w.CallWithResult(method, params)
	return err
}

func (w *wsConn) CallWithResult(method string, params any) (json.RawMessage, error) {
	w.mu.Lock()
	w.nextID++
	id := w.nextID
	ch := make(chan json.RawMessage, 1)
	w.pending[id] = ch
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	payload, _ := json.Marshal(msg)
	if err := w.writeText(payload); err != nil {
		return nil, err
	}

	select {
	case raw := <-ch:
		var probe struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, probe.Error.Message)
		}
		return raw, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("%s: timeout", method)
	}
}

func (w *wsConn) writeText(payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	header := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n)|0x80)
	case n <= 0xFFFF:
		header = append(header, 126|0x80)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 127|0x80)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	header = append(header, 0, 0, 0, 0) // zero mask: permitted, simplest

	_, err := w.conn.Write(append(header, payload...))
	return err
}

func (w *wsConn) readLoop() {
	defer w.Close()
	for {
		opcode, payload, err := w.readFrame()
		if err != nil {
			return
		}
		if opcode != 0x1 {
			continue
		}
		var env struct {
			ID *int64 `json:"id"`
		}
		if err := json.Unmarshal(payload, &env); err != nil || env.ID == nil {
			continue
		}
		w.mu.Lock()
		ch, ok := w.pending[*env.ID]
		w.mu.Unlock()
		if ok {
			select {
			case ch <- payload:
			default:
			}
		}
	}
}

func (w *wsConn) readFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(w.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}

	if length < 0 || length > 64<<20 {
		return 0, nil, fmt.Errorf("frame too large")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(w.br, buf); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range buf {
			buf[i] ^= mask[i%4]
		}
	}
	return opcode, buf, nil
}
