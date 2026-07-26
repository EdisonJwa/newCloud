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

	url, _ := evalString(cdp, "location.href")
	if strings.Contains(url, "auth") {
		return fmt.Errorf("login did not complete, still at %s", url)
	}

	// ---- Files ----
	fmt.Println("capturing the file list…")
	if err := shoot(cdp, filepath.Join(*outDir, "files.png")); err != nil {
		return err
	}

	// ---- Shares tab ----
	fmt.Println("capturing the shares tab…")
	if _, err := evalString(cdp, `(() => {
		const b = [...document.querySelectorAll('nav.primary button')].find(x => x.textContent.trim() === 'Shares');
		if (b) b.click(); return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(1400 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "shares.png")); err != nil {
		return err
	}

	// ---- A folder inside the tree ----
	fmt.Println("capturing a subfolder…")
	if _, err := evalString(cdp, `(() => {
		const b = [...document.querySelectorAll('nav.primary button')].find(x => x.textContent.trim() === 'Files');
		if (b) b.click(); return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(1200 * time.Millisecond)
	// The image folder, so the thumbnails are actually exercised by a frame: a thumbnail
	// that renders in an API test and not in the table is the failure worth catching.
	if _, err := evalString(cdp, `(() => {
		const labels = [...document.querySelectorAll('table.files span.label')];
		const dir = labels.find(b => b.textContent.trim() === '图片');
		if (dir) dir.click(); return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(1600 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "folder.png")); err != nil {
		return err
	}

	// ---- Activity ----
	//
	// The audit timeline is the view a person opens to ask "who downloaded my file", so it
	// is worth a frame of its own rather than being covered by the file list.
	fmt.Println("capturing the activity timeline…")
	if _, err := evalString(cdp, `(() => {
		const b = [...document.querySelectorAll('nav.primary button')].find(x => x.textContent.trim() === 'Activity');
		if (b) b.click(); return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(1600 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "activity.png")); err != nil {
		return err
	}

	// ---- App passwords ----
	fmt.Println("capturing app passwords…")
	if _, err := evalString(cdp, `(() => {
		const b = [...document.querySelectorAll('nav.primary button')].find(x => x.textContent.trim() === 'Access');
		if (b) b.click(); return 'ok';
	})()`); err != nil {
		return err
	}
	time.Sleep(1400 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "apppasswords.png")); err != nil {
		return err
	}

	// ---- Dark palette ----
	//
	// Dark mode here is a separate set of colours rather than an inversion, so it needs its
	// own frame: warming the palette is exactly the kind of change that looks fine in a
	// stylesheet and wrong on screen.
	fmt.Println("capturing the dark palette…")
	if err := cdp.Call("Emulation.setEmulatedMedia", map[string]any{
		"features": []map[string]string{{"name": "prefers-color-scheme", "value": "dark"}},
	}); err != nil {
		return err
	}
	if _, err := evalString(cdp, jsShowFiles); err != nil {
		return err
	}
	time.Sleep(1400 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "files-dark.png")); err != nil {
		return err
	}

	fmt.Printf("\nscreenshots written to %s\n", *outDir)
	return nil
}

func navigate(cdp *wsConn, target string) error {
	return cdp.Call("Page.navigate", map[string]any{"url": target})
}

// jsShowFiles switches the sidebar back to the file list.
const jsShowFiles = `(() => {
	const b = [...document.querySelectorAll('nav.primary button')].find(x => x.textContent.trim() === 'Files');
	if (b) b.click();
	return 'ok';
})()`

func shoot(cdp *wsConn, path string) error {
	raw, err := cdp.CallWithResult("Page.captureScreenshot", map[string]any{
		"format": "png", "captureBeyondViewport": true,
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
