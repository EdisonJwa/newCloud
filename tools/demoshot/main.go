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
	// testChunk drops a file large enough to take the resumable path and then checks the
	// published size: the only end-to-end evidence that the browser's slicing and the
	// server's reassembly agree.
	testChunk = flag.Bool("test-chunk", false, "verify a large chunked upload through the UI, then exit")
	chunkMiB  = flag.Int("chunk-mib", 20, "size of the file used by -test-chunk")
	// testExpansion lengthens every visible string and checks the layout still fits. English is
	// the shipped baseline and Chinese is shorter, so nothing in the product currently
	// exercises the direction that actually breaks layouts: strings getting longer.
	testExpansion = flag.Bool("test-expansion", false, "verify the layout survives longer text, then exit")
	// testShare drives the password gate on a share page. Nothing else in the tooling ever
	// submitted that form, which is how a broken access cookie shipped.
	testShare = flag.Bool("test-share", false, "verify the share password gate, then exit")
	// testFresh opens / with no session, which is what a visitor actually does. Every other
	// mode signs in through /api/auth/login first, so a broken sign-in redirect — the kind
	// that reloads forever — was invisible to this tool.
	testFresh = flag.Bool("test-fresh", false, "open / with no session and verify it settles, then exit")
	sharePass = flag.String("share-password", "", "password for -test-share")
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

	// ---- Cold start: / with no session ----
	if *testFresh {
		return checkColdStart(cdp)
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
		return checkDropUpload(cdp, 0)
	}
	if *testChunk {
		return checkDropUpload(cdp, *chunkMiB)
	}
	if *testExpansion {
		return checkTextExpansion(cdp)
	}
	if *testShare {
		return checkSharePassword(cdp, flag.Arg(0), *sharePass)
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

	// Type into the palette, so the frame shows server search results rather than the list of
	// recently touched files. A React-controlled input ignores a plain value assignment: the
	// native setter has to be used and an input event dispatched, or nothing happens.
	fmt.Println("capturing a search…")
	if msg, err := evalString(cdp, `(() => {
		const input = document.querySelector('.palette input');
		if (!input) return 'miss';
		const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		setter.call(input, '合同');
		input.dispatchEvent(new Event('input', { bubbles: true }));
		return 'ok';
	})()`); err != nil {
		return err
	} else if msg != "ok" {
		return fmt.Errorf("could not type into the palette: %s", msg)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := shoot(cdp, filepath.Join(*outDir, "palette-search.png")); err != nil {
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

// checkColdStart opens the app the way a person does — by typing the address — and verifies
// that the page settles instead of reloading.
//
// A sign-in redirect pointed at a path the SPA answers turns authentication into a reload loop
// with no error message: every load reports "not signed in", every redirect returns the shell,
// forever. The signal is the number of distinct documents: performance.timeOrigin changes on
// every navigation, so a loop shows up as many origins behind one address.
func checkColdStart(cdp *wsConn) error {
	if err := navigate(cdp, *base+"/"); err != nil {
		return err
	}

	probe := `(() => {
		const key = 'wc_origins';
		const seen = JSON.parse(sessionStorage.getItem(key) || '[]');
		const cur = String(window.performance.timeOrigin);
		if (!seen.includes(cur)) {
			seen.push(cur);
			sessionStorage.setItem(key, JSON.stringify(seen));
		}
		return JSON.stringify({
			origins: seen.length,
			path: location.pathname,
			shell: !!document.querySelector('.nav'),
			prompt: !!document.querySelector('input[type="password"], .signin'),
		});
	})()`

	// Sample fast: a reload loop cycles in a few hundred milliseconds, so a one-second poll
	// would sample the same document every time and report a healthy page.
	deadline := time.Now().Add(20 * time.Second)
	saw := 0
	var settled bool
	for time.Now().Before(deadline) {
		time.Sleep(120 * time.Millisecond)
		raw, err := evalString(cdp, probe)
		if err != nil {
			// Navigating tears down the execution context; that is expected mid-flight.
			continue
		}
		var state struct {
			Origins int    `json:"origins"`
			Path    string `json:"path"`
			Shell   bool   `json:"shell"`
			Prompt  bool   `json:"prompt"`
		}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			continue
		}
		if state.Origins > saw {
			saw = state.Origins
		}
		if state.Origins > 6 && !state.Prompt {
			return fmt.Errorf("the app cycled through %d documents without settling: the sign-in redirect loops", state.Origins)
		}
		if state.Shell && state.Path == "/" {
			settled = true
			// Keep sampling briefly after it looks settled: a loop can pass through a
			// momentary good frame.
			if state.Origins <= 3 {
				break
			}
		}
	}

	if !settled {
		return fmt.Errorf("the app never settled on a signed-in shell (documents seen: %d)", saw)
	}
	if saw > 3 {
		return fmt.Errorf("the app settled but only after %d documents: the redirects are cycling", saw)
	}
	fmt.Printf("OK: / settled in %d document load(s), signed in\n", saw)
	return shoot(cdp, filepath.Join(*outDir, "cold-start.png"))
}

// checkSharePassword drives the password gate the way a visitor does.
//
// It exists because a browser rejects an access cookie that the Go tests happily accept, so
// every server-side test of this flow passed while the page kept asking for the password.
func checkSharePassword(cdp *wsConn, token, password string) error {
	if token == "" || password == "" {
		return fmt.Errorf("-test-share needs a share token argument and -share-password")
	}

	if err := navigate(cdp, *base+"/s/"+token); err != nil {
		return err
	}
	time.Sleep(2200 * time.Millisecond)

	// It has to start gated, or the test proves nothing.
	state, err := evalString(cdp, `(() => {
		const pw = document.querySelector('input[type="password"]');
		return pw ? 'gated' : 'open';
	})()`)
	if err != nil {
		return err
	}
	if state != "gated" {
		return fmt.Errorf("the share page did not ask for a password (state %q)", state)
	}

	// Fill the field the way a user would: React ignores a plain assignment, so the native
	// setter and an input event are required.
	msg, err := evalString(cdp, fmt.Sprintf(`(() => {
		const input = document.querySelector('input[type="password"]');
		if (!input) return 'no-input';
		const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		setter.call(input, %q);
		input.dispatchEvent(new Event('input', { bubbles: true }));
		const form = input.closest('form');
		if (!form) return 'no-form';
		form.requestSubmit ? form.requestSubmit() : form.submit();
		return 'ok';
	})()`, password))
	if err != nil {
		return err
	}
	if msg != "ok" {
		return fmt.Errorf("could not submit the password form: %s", msg)
	}

	// After a correct password the page must offer the content, not the form again.
	deadline := time.Now().Add(12 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		last, err = evalString(cdp, `(() => {
			const pw = document.querySelector('input[type="password"]');
			const dl = document.querySelector('a[href*="/download"]');
			if (dl) return 'download';
			return pw ? 'gated' : 'other';
		})()`)
		if err != nil {
			return err
		}
		if last == "download" {
			fmt.Println("OK: the password gate let the visitor through")
			return shoot(cdp, filepath.Join(*outDir, "share-unlocked.png"))
		}
	}
	return fmt.Errorf("the page still asks for the password after a correct one (state %q); "+
		"the access cookie was probably rejected by the browser", last)
}

// checkTextExpansion rewrites every text node to be about 60% longer and then checks that
// nothing overflows the viewport or its container.
//
// No pseudo-locale ships in the product for this: the check belongs in the tool, so the
// verification is real and production carries no debug code.
func checkTextExpansion(cdp *wsConn) error {
	// A German-ish expansion: longer words, not just more characters.
	expr := `(() => {
		const extra = ' Verlängerungszuschlag';
		const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
		const nodes = [];
		while (walker.nextNode()) nodes.push(walker.currentNode);
		for (const node of nodes) {
			const text = node.nodeValue;
			if (!text || !text.trim()) continue;
			node.nodeValue = text + extra;
		}
		return 'ok';
	})()`
	if msg, err := evalString(cdp, expr); err != nil {
		return err
	} else if msg != "ok" {
		return fmt.Errorf("could not expand the text: %s", msg)
	}
	time.Sleep(600 * time.Millisecond)

	report, err := evalString(cdp, `(() => {
		const overflowing = [];
		const doc = document.documentElement;
		if (doc.scrollWidth > window.innerWidth + 1) {
			overflowing.push('document');
		}
		for (const el of document.querySelectorAll('.topbar, .viewhead, .colhead, .detail, .palette, .landcard')) {
			if (el.scrollWidth > el.clientWidth + 1) {
				overflowing.push(el.className.split(' ')[0]);
			}
		}
		return JSON.stringify({ overflowing, width: window.innerWidth });
	})()`)
	if err != nil {
		return err
	}
	fmt.Println("  expansion report: " + report)

	if err := shoot(cdp, filepath.Join(*outDir, "text-expanded.png")); err != nil {
		return err
	}
	if strings.Contains(report, `"overflowing":["`) {
		return fmt.Errorf("the layout overflows when text gets longer: %s", report)
	}
	fmt.Println("OK: the layout survives a 60% text expansion")
	return nil
}

// fetchJSON runs a same-origin GET inside the page and hands back the parsed body.
//
// It goes through the page rather than the tool's own HTTP client so the request carries the
// session cookie the browser already has.
func fetchJSON(cdp *wsConn, path string, dst any) error {
	out, err := evalString(cdp, fmt.Sprintf(`(() => {
		return fetch(%q, { credentials: 'same-origin' }).then((r) => r.text());
	})()`, path))
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), dst)
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

// jsShowFiles switches back to the file list.
//
// This used to click a button in the old left rail. After the redesign the navigation lives in
// the top bar, so the selector had to move with it — a stale selector here does not fail, it
// quietly leaves the tool on the Home view and every later assertion measures the wrong page.
const jsShowFiles = `(() => {
	const b = [...document.querySelectorAll('.nav button')].find((x) => (x.textContent || '').trim() === 'Files');
	if (!b) return 'miss';
	b.click();
	return 'ok';
})()`

// checkDropUpload drives a real drag-and-drop upload and reports whether it worked.
//
// This cannot be done from the DOM: a DataTransfer constructed in JavaScript carries no OS
// files, so dispatching a synthetic `drop` proves only that the listener runs. The DevTools
// protocol can dispatch a drag naming a real path, which Chrome turns into a File the page
// can read — the closest thing available to a user dragging from Explorer.
func checkDropUpload(cdp *wsConn, bigMiB int) error {
	dir, err := os.MkdirTemp("", "newcloud-drop-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	payload := []byte("uploaded by dragging\n")
	name := "dropped-file.txt"
	if bigMiB > 0 {
		// Large enough to cross the chunk threshold, and patterned so nothing silently
		// deduplicates it against an existing blob.
		payload = make([]byte, bigMiB<<20)
		for i := range payload {
			payload[i] = byte((i*31 + 7) % 251)
		}
		name = fmt.Sprintf("big-%dMiB.bin", bigMiB)
	}
	source := filepath.Join(dir, name)
	if err := os.WriteFile(source, payload, 0o644); err != nil {
		return err
	}
	wantSize := int64(len(payload))

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
	if msg, err := evalString(cdp, jsShowFiles); err != nil {
		return err
	} else if msg != "ok" {
		return fmt.Errorf("could not switch to the File view (%s)", msg)
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

	// For the large file the interesting assertion is the published size: a mis-assembled
	// upload still produces a row, just a short one.
	if bigMiB > 0 {
		name := filepath.Base(source)
		var files struct {
			Children []struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			} `json:"children"`
		}
		if err := fetchJSON(cdp, "/api/files/?path=", &files); err != nil {
			return err
		}
		found := false
		for _, child := range files.Children {
			if child.Name != name {
				continue
			}
			found = true
			if child.Size != wantSize {
				return fmt.Errorf("published size %d, want %d", child.Size, wantSize)
			}
		}
		if !found {
			return fmt.Errorf("%s missing from the API listing", name)
		}
		fmt.Printf("OK: %d bytes uploaded in chunks and reassembled exactly\n", wantSize)
	} else {
		fmt.Println("OK: drag and drop uploaded the file")
	}
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
