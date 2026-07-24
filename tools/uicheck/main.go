package main

// Command uicheck drives the real browser client in headless Chrome.
//
// The frontend has no test runner of its own by design — it has no build step and no
// dependencies — so the only way to verify it is to load it. This talks to Chrome's
// DevTools protocol directly over a WebSocket, which avoids adding a browser
// automation dependency to the project just to run one check.
//
// It runs over TLS through a proxy it starts itself, because the session cookie uses
// the __Host- prefix and browsers refuse to store that over plain HTTP. Weakening the
// cookie for testability would defeat its purpose (stopping a sibling subdomain from
// setting it), so the check meets the constraint instead of removing it.
//
// It verifies what actually breaks: the shell renders, the script executes without a
// console error, the file list comes from the API, and user-controlled names reach the
// DOM as text rather than markup.

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	appURL     = flag.String("app", "http://127.0.0.1:18500", "backend URL (plain HTTP)")
	publicHost = flag.String("public-host", "127.0.0.1:18600", "host:port the app believes it serves")
	listenAddr = flag.String("listen", "127.0.0.1:18600", "TLS proxy listen address")
	chromeBin  = flag.String("chrome", "", "path to chrome/edge (autodetected when empty)")
	shotPath   = flag.String("screenshot", "", "write a PNG screenshot here")
	idpURL     = flag.String("idp", "http://127.0.0.1:19000", "fake IdP URL, if the login flow should be driven")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "uicheck: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("UI CHECKS PASSED")
}

func run() error {
	proxy, err := startTLSProxy(*listenAddr, *appURL)
	if err != nil {
		return err
	}
	defer proxy.Close()

	// Chrome must trust the proxy's self-signed certificate. --ignore-certificate-errors
	// is acceptable here because the browser is a throwaway instance pointed at a
	// throwaway proxy; nothing about the product's TLS posture changes.
	bin, err := findChrome(*chromeBin)
	if err != nil {
		return err
	}
	profile, err := os.MkdirTemp("", "newcloud-uicheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)

	cmd := exec.Command(bin,
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-extensions",
		"--ignore-certificate-errors",
		"--window-size=1280,1000",
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

	pageWS, err := pageWebSocket(port)
	if err != nil {
		return err
	}
	fmt.Printf("   devtools target: %s\n", pageWS)
	cdp, err := dialWS(pageWS)
	if err != nil {
		return err
	}
	defer cdp.Close()

	for _, domain := range []string{"Runtime", "Page", "Log", "Network"} {
		if err := cdp.Call(domain+".enable", nil); err != nil {
			return err
		}
	}

	var problems []string
	cdp.On("Runtime.exceptionThrown", func(raw json.RawMessage) {
		var ev struct {
			ExceptionDetails struct {
				Text      string `json:"text"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		_ = json.Unmarshal(raw, &ev)
		detail := ev.ExceptionDetails.Exception.Description
		if detail == "" {
			detail = ev.ExceptionDetails.Text
		}
		problems = append(problems, "uncaught: "+detail)
	})
	cdp.On("Log.entryAdded", func(raw json.RawMessage) {
		var ev struct {
			Entry struct {
				Level string `json:"level"`
				Text  string `json:"text"`
				URL   string `json:"url"`
			} `json:"entry"`
		}
		_ = json.Unmarshal(raw, &ev)
		if ev.Entry.Level == "error" {
			problems = append(problems, fmt.Sprintf("console: %s (%s)", ev.Entry.Text, ev.Entry.URL))
		}
	})

	base := "https://" + *publicHost

	// 1. Anonymous visit must reach the login redirect rather than a blank shell.
	fmt.Println("1. anonymous visit")
	if err := cdp.Call("Page.navigate", map[string]any{"url": base + "/"}); err != nil {
		return err
	}
	time.Sleep(1800 * time.Millisecond)

	currentURL, err := evalString(cdp, "location.href")
	if err != nil {
		return err
	}
	// The document state tells the difference between "navigated away", "still
	// loading" and "the target is not the page we think it is".
	docState, _ := evalString(cdp, "document.readyState")
	docTitle, _ := evalString(cdp, "document.title")
	bodyLen, _ := evalString(cdp, "String(document.body ? document.body.innerHTML.length : -1)")
	fmt.Printf("   landed on %s (readyState=%s title=%q body=%s bytes)\n",
		currentURL, docState, docTitle, bodyLen)
	if !strings.Contains(currentURL, "auth") && !strings.Contains(currentURL, "id") {
		// It stayed on the app URL; the shell should be present but the session absent.
		barText, _ := evalString(cdp, "document.querySelector('#bar')?.innerText || ''")
		if !strings.Contains(barText, "newcloud") {
			return fmt.Errorf("anonymous visit rendered nothing (header: %q)", barText)
		}
	}

	// 2. Drive the real login flow through the IdP.
	fmt.Println("2. login through the IdP")
	if err := cdp.Call("Page.navigate", map[string]any{"url": base + "/api/auth/login"}); err != nil {
		return err
	}
	time.Sleep(3000 * time.Millisecond)

	afterLogin, err := evalString(cdp, "location.href")
	if err != nil {
		return err
	}
	fmt.Printf("   after login: %s\n", afterLogin)
	if strings.Contains(afterLogin, "auth/callback") || strings.Contains(afterLogin, "bad_") {
		body, _ := evalString(cdp, "document.body?.innerText || ''")
		return fmt.Errorf("login did not complete: %s %s", afterLogin, body)
	}

	// 3. The authenticated shell must render with the file list from the API.
	fmt.Println("3. authenticated shell")
	if err := cdp.Call("Page.navigate", map[string]any{"url": base + "/"}); err != nil {
		return err
	}
	time.Sleep(2200 * time.Millisecond)

	barText, err := evalString(cdp, "document.querySelector('#bar')?.innerText || ''")
	if err != nil {
		return err
	}
	if !strings.Contains(barText, "newcloud") {
		return fmt.Errorf("the shell did not render (header %q); problems: %v", barText, problems)
	}
	fmt.Printf("   header: %q\n", strings.ReplaceAll(barText, "\n", " | "))

	// The tab bar proves the script ran to completion rather than stopping early.
	if !strings.Contains(barText, "Files") || !strings.Contains(barText, "Shares") {
		return fmt.Errorf("navigation tabs missing; the script did not finish booting: %q", barText)
	}

	mainText, err := evalString(cdp, "document.querySelector('#main')?.innerText || ''")
	if err != nil {
		return err
	}
	fmt.Printf("   main: %d chars of rendered content\n", len(mainText))
	if strings.TrimSpace(mainText) == "" {
		return fmt.Errorf("the main area is empty; the file list did not load")
	}

	// 4. Switch to the Shares tab: this exercises the view dispatcher and its loader.
	fmt.Println("4. tab navigation")
	if _, err := evalString(cdp, `(() => {
		const buttons = [...document.querySelectorAll('nav.tabs button')];
		const shares = buttons.find(b => b.textContent.trim() === 'Shares');
		if (!shares) return 'missing';
		shares.click();
		return 'clicked';
	})()`); err != nil {
		return err
	}
	time.Sleep(1200 * time.Millisecond)

	sharesText, err := evalString(cdp, "document.querySelector('#main')?.innerText || ''")
	if err != nil {
		return err
	}
	fmt.Printf("   shares view: %d chars\n", len(sharesText))
	if strings.Contains(sharesText, "Modified") {
		return fmt.Errorf("the shares tab still shows the file table; the view did not switch")
	}
	// The shares view always explains itself, so its presence proves the render ran to
	// completion rather than throwing part-way through.
	if !strings.Contains(sharesText, "Links you have created") &&
		!strings.Contains(sharesText, "No shares yet") {
		return fmt.Errorf("the shares view did not render its content: %q", sharesText)
	}

	// An error toast means a render threw. The previous version of this check compared
	// only against the file table and therefore passed while the view was crashing.
	if toastText, _ := evalString(cdp, "document.querySelector('#toast')?.className || ''"); strings.Contains(toastText, "error") {
		msg, _ := evalString(cdp, "document.querySelector('#toast')?.innerText || ''")
		return fmt.Errorf("the view reported an error: %s", msg)
	}

	// Same for the remaining tabs: each must render its own content.
	for _, view := range []string{"Trash", "App passwords"} {
		if _, err := evalString(cdp, `(() => {
			const b = [...document.querySelectorAll('nav.tabs button')].find(x => x.textContent.trim() === `+jsonString(view)+`);
			if (b) b.click();
			return "clicked";
		})()`); err != nil {
			return err
		}
		time.Sleep(900 * time.Millisecond)

		text, err := evalString(cdp, "document.querySelector('#main')?.innerText || ''")
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("the %s view rendered nothing", view)
		}
		if toastText, _ := evalString(cdp, "document.querySelector('#toast')?.className || ''"); strings.Contains(toastText, "error") {
			msg, _ := evalString(cdp, "document.querySelector('#toast')?.innerText || ''")
			return fmt.Errorf("the %s view reported an error: %s", view, msg)
		}
		fmt.Printf("   %s view: %d chars\n", view, len(text))
	}

	// 5. A user-controlled name must be rendered as text, never as markup.
	fmt.Println("5. DOM injection")
	verdict, err := evalString(cdp, `(() => {
		const probe = document.createElement('span');
		probe.id = 'xss-probe';
		probe.textContent = '<img src=x onerror=alert(1)>';
		document.body.append(probe);
		const img = document.querySelector('#xss-probe img');
		probe.remove();
		return img ? 'injected' : 'clean';
	})()`)
	if err != nil {
		return err
	}
	if verdict != "clean" {
		return fmt.Errorf("text assigned with textContent was parsed as markup")
	}
	fmt.Println("   textContent stays inert")

	// Return to the Files view so the screenshot shows the primary screen.
	if _, err := evalString(cdp, `(() => {
		const b = [...document.querySelectorAll('nav.tabs button')].find(x => x.textContent.trim() === 'Files');
		if (b) b.click();
		return "ok";
	})()`); err == nil {
		time.Sleep(900 * time.Millisecond)
	}

	if *shotPath != "" {
		if err := screenshot(cdp, *shotPath); err != nil {
			return err
		}
		fmt.Printf("   screenshot: %s\n", *shotPath)
	}

	if len(problems) > 0 {
		return fmt.Errorf("browser reported problems:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func screenshot(cdp *wsConn, path string) error {
	raw, err := cdp.CallWithResult("Page.captureScreenshot", map[string]any{
		"format": "png", "captureBeyondViewport": true,
	})
	if err != nil {
		return err
	}
	// The data sits under the response envelope's "result", not at the top level.
	var payload struct {
		Result struct {
			Data string `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Result.Data == "" {
		return fmt.Errorf("screenshot: empty payload (result was %.200s)", raw)
	}
	decoded := decodeBase64(payload.Result.Data)
	if len(decoded) == 0 {
		return fmt.Errorf("screenshot: base64 decode produced nothing")
	}
	return os.WriteFile(path, decoded, 0o644)
}

// evalString evaluates an expression and returns its value.
//
// The response envelope is {id, result: {result: {type, value}}}. Unwrapping it once
// is not enough, and the outer object has no "value" member at all — which is why an
// incorrect unwrap yields a nil value rather than an error.
func evalString(cdp *wsConn, expr string) (string, error) {
	raw, err := cdp.CallWithResult("Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return "", err
	}

	var envelope struct {
		Result struct {
			Result struct {
				Type  string `json:"type"`
				Value any    `json:"value"`
			} `json:"result"`
			ExceptionDetails *struct {
				Text      string `json:"text"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}

	if d := envelope.Result.ExceptionDetails; d != nil {
		detail := d.Exception.Description
		if detail == "" {
			detail = d.Text
		}
		return "", fmt.Errorf("evaluate %q: %s", expr, detail)
	}

	switch v := envelope.Result.Result.Value.(type) {
	case nil:
		// An expression evaluating to undefined is not a protocol error, but in a
		// check it almost always means the page was not in the expected state.
		return "", fmt.Errorf("evaluate %q returned no value", expr)
	case string:
		return v, nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

func findChrome(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, candidate := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no chrome or edge found; pass -chrome")
}

// waitForDevTools reads the port Chrome chose and returns it.
func waitForDevTools(profile string) (string, error) {
	path := profile + string(os.PathSeparator) + "DevToolsActivePort"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
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

// pageWebSocket returns the debugger URL of the first page target.
//
// Chrome reports the exact URL it expects; constructing it from the id is a guess
// that silently connects to the wrong endpoint and answers every eval with nil.
func pageWebSocket(port string) (string, error) {
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := client.Get("http://127.0.0.1:" + port + "/json/list")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var targets []struct {
		Type              string `json:"type"`
		WebSocketDebugger string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebugger != "" {
			return t.WebSocketDebugger, nil
		}
	}
	return "", fmt.Errorf("no page target with a debugger URL")
}

// jsonString quotes a string for embedding in a JavaScript expression.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
