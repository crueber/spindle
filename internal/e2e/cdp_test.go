package e2e

// cdp_test.go — a minimal Chrome DevTools Protocol client for the rendered
// accessibility smoke (a11y_test.go).
//
// It launches a headless Chromium, reads its DevTools endpoint off stderr, and
// speaks CDP over a WebSocket to the page target: navigate, evaluate a probe,
// emulate a 390 px viewport, and dispatch real key events. Nothing here is a
// browser automation dependency: `golang.org/x/net/websocket` is already in the
// dependency budget (docs/go/15_testing.md §6.6), and Chromium is a CI tool,
// not a Go module.
//
// This file exists only in the e2e test binary (a *_test.go file in package
// e2e): the released binary cannot link it.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// chromiumNames are the executables probed on PATH when SPINDLE_CHROMIUM is not
// set. Debian's package installs `chromium`; the others cover the common macOS
// and Chrome-for-Testing installs a contributor may have.
var chromiumNames = []string{
	"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "chrome-headless-shell",
}

// findChromium locates a browser binary. SPINDLE_CHROMIUM names one explicitly
// (the escape hatch for a non-PATH install, e.g. a downloaded headless shell).
func findChromium() (string, error) {
	if p := os.Getenv("SPINDLE_CHROMIUM"); p != "" {
		st, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("SPINDLE_CHROMIUM=%s: %w", p, err)
		}
		if st.IsDir() {
			return "", fmt.Errorf("SPINDLE_CHROMIUM=%s is a directory", p)
		}
		return p, nil
	}
	for _, name := range chromiumNames {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chromium on PATH (tried %s); set SPINDLE_CHROMIUM", strings.Join(chromiumNames, ", "))
}

// lockedBuffer is the browser's combined stderr, safe to read from the test
// goroutine while the scanner goroutine appends.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// devBrowser is one launched headless Chromium and its page-target connection.
type devBrowser struct {
	t        *testing.T
	cmd      *exec.Cmd
	devtools string
	logs     *lockedBuffer
	conn     *websocket.Conn
	nextID   int64
	done     chan struct{} // closed when the browser process has exited
	// seen counts events read while waiting for a command reply, so a
	// Page.loadEventFired that beats the Page.navigate reply is not lost.
	seen map[string]int
	// console collects page-code errors (console.error, uncaught exceptions,
	// browser log entries at level error). Cleared per page by the test.
	console []string
	// docStatus/docURL are the last Document response the page received, so a
	// test can prove the page really answered 200 instead of a gate or a 404
	// page that happens to carry a skip link and an h1 too.
	docStatus int
	docURL    string
}

// cdpMessage is one CDP frame: a reply (id/result/error) or an event
// (method/params).
type cdpMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// browserSkipOrFail is the local/CI policy in one place: a browser problem is a
// SKIP for a contributor (the a11y smoke is opt-in locally) and a hard FAILURE
// in CI, where SPINDLE_A11Y=1 states that the browser is a required tool.
func browserSkipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SPINDLE_A11Y") == "1" {
		t.Fatalf("SPINDLE_A11Y=1 (CI): %s", fmt.Sprintf(format, args...))
	}
	t.Skipf("%s (set SPINDLE_A11Y=1 to require a browser)", fmt.Sprintf(format, args...))
}

// requireBrowser applies that policy before the expensive setup, so a missing
// browser costs a second instead of a server build and a fixture seed.
func requireBrowser(t *testing.T) {
	t.Helper()
	if cdpEndpoint() != "" {
		return
	}
	if _, err := findChromium(); err != nil {
		browserSkipOrFail(t, "%v", err)
	}
}

// cdpEndpoint is the operator's override: an already-running CDP server
// (e.g. a headless browser served over the DevTools protocol) that the smoke
// should connect to instead of launching Chromium itself. Useful where the
// local Chromium build lacks system libraries but a CDP-speaking browser is
// available. The value is a devtools endpoint ("http://127.0.0.1:9222").
func cdpEndpoint() string { return strings.TrimSpace(os.Getenv("SPINDLE_CDP_URL")) }

// launchBrowser starts headless Chromium with a throwaway profile and returns
// it once the DevTools endpoint is known. With SPINDLE_CDP_URL set it connects
// to that already-running CDP endpoint and owns no process (so it kills none).
func launchBrowser(t *testing.T) *devBrowser {
	t.Helper()
	if endpoint := cdpEndpoint(); endpoint != "" {
		b := &devBrowser{t: t, devtools: endpoint, logs: &lockedBuffer{}, done: make(chan struct{})}
		close(b.done)
		t.Cleanup(b.close)
		return b
	}
	bin, err := findChromium()
	if err != nil {
		browserSkipOrFail(t, "%v", err)
	}
	profile := t.TempDir()
	args := []string{
		// The smoke drives one page at a time through DevTools and needs no
		// window system; the "new" headless mode is the supported one.
		"--headless=new",
		// Port 0 lets the OS pick, so two concurrent runs never collide; the
		// chosen URL is read from the "DevTools listening on ..." line.
		"--remote-debugging-port=0",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		// The scroll-width assertion is about layout, not scrollbar chrome.
		"--hide-scrollbars",
		// *.localhost is supposed to resolve to loopback already; pinning it
		// removes the resolver from the equation (the server's canonical host
		// redirect points browsers at walgit.localhost, AGENTS field lesson).
		"--host-resolver-rules=MAP walgit.localhost 127.0.0.1",
		// DevTools refuses a WebSocket carrying an Origin header unless that
		// origin is allow-listed (Chrome 111+). The Go client always sends one,
		// so name exactly the origin it sends — never "*".
		"--remote-allow-origins=http://localhost",
		"about:blank",
	}
	if os.Getenv("SPINDLE_CHROMIUM_NO_SANDBOX") != "" {
		// CI containers only: the Chromium sandbox needs user-namespace (or
		// setuid) privileges a job container does not have. Never set this on a
		// developer machine — the browser then runs with the sandbox on.
		args = append(args, "--no-sandbox")
	}
	b := &devBrowser{t: t, logs: &lockedBuffer{}, done: make(chan struct{})}
	b.cmd = exec.Command(bin, args...)
	b.cmd.Stdout = b.logs
	stderr, err := b.cmd.StderrPipe() // the DevTools line is written to stderr
	if err != nil {
		browserSkipOrFail(t, "chromium stderr pipe: %v", err)
	}
	if err := b.cmd.Start(); err != nil {
		browserSkipOrFail(t, "start %s: %v", bin, err)
	}
	t.Cleanup(b.close)
	go func() {
		_ = b.cmd.Wait()
		close(b.done)
	}()

	// The DevTools endpoint exists only once the browser says so; waiting for
	// the line (rather than sleeping) is deterministic and bounded.
	endpoint := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			b.logs.Write([]byte(line + "\n"))
			if u, ok := strings.CutPrefix(line, "DevTools listening on "); ok {
				select {
				case endpoint <- strings.TrimSpace(u):
				default:
				}
			}
		}
	}()
	select {
	case b.devtools = <-endpoint:
	case <-time.After(30 * time.Second):
		browserSkipOrFail(t, "chromium did not announce a DevTools endpoint within 30s\n%s", b.logs.String())
	case <-b.done:
		// Fail fast on an early crash instead of waiting out the deadline.
		browserSkipOrFail(t, "chromium exited before announcing a DevTools endpoint\n%s", b.logs.String())
	}
	return b
}

// close tears the browser down (the profile lives in t.TempDir).
func (b *devBrowser) close() {
	if b.conn != nil {
		_ = b.conn.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		select {
		case <-b.done:
		case <-time.After(10 * time.Second):
		}
	}
}

// connect opens the CDP connection to the browser's first page target and
// enables the domains the smoke uses.
func (b *devBrowser) connect() {
	b.t.Helper()
	u, err := url.Parse(b.devtools)
	if err != nil {
		b.t.Fatalf("devtools endpoint %q: %v", b.devtools, err)
	}
	listURL := "http://" + u.Host + "/json/list"
	var targets []struct {
		Type                 string `json:"type"`
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := httpClient.Get(listURL)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(resp.Body).Decode(&targets)
			}
			resp.Body.Close()
		}
		if err == nil && len(targets) > 0 {
			break
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("no page target from %s: %v\n%s", listURL, err, b.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	pageURL := ""
	for _, target := range targets {
		if target.Type == "page" {
			pageURL = target.WebSocketDebuggerURL
			break
		}
	}
	if pageURL == "" {
		b.t.Fatalf("no page target in %s", listURL)
	}
	loc, err := url.Parse(pageURL)
	if err != nil {
		b.t.Fatalf("page endpoint %q: %v", pageURL, err)
	}
	origin, _ := url.Parse("http://localhost")
	conn, err := websocket.DialConfig(&websocket.Config{
		Location: loc,
		Origin:   origin,
		Version:  websocket.ProtocolVersionHybi13,
	})
	if err != nil {
		b.t.Fatalf("dial page target: %v", err)
	}
	conn.MaxPayloadBytes = 16 << 20
	b.conn = conn
	for _, domain := range []string{"Page.enable", "Runtime.enable", "Log.enable", "Network.enable"} {
		if _, err := b.call(domain, map[string]any{}); err != nil {
			b.t.Fatalf("%s: %v", domain, err)
		}
	}
}

// receive reads one CDP frame under the given deadline.
func (b *devBrowser) receive(deadline time.Time) (cdpMessage, error) {
	if err := b.conn.SetReadDeadline(deadline); err != nil {
		return cdpMessage{}, err
	}
	var data []byte
	if err := websocket.Message.Receive(b.conn, &data); err != nil {
		return cdpMessage{}, err
	}
	var msg cdpMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return cdpMessage{}, fmt.Errorf("decode CDP frame: %w", err)
	}
	return msg, nil
}

// call issues one command and returns its result, recording events (console
// errors) that arrive before the reply.
func (b *devBrowser) call(method string, params any) (json.RawMessage, error) {
	b.nextID++
	id := b.nextID
	frame, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := b.conn.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return nil, err
	}
	if err := websocket.Message.Send(b.conn, string(frame)); err != nil {
		return nil, fmt.Errorf("send %s: %w", method, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		msg, err := b.receive(deadline)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		if msg.ID == id {
			if msg.Error != nil {
				return nil, fmt.Errorf("%s: %s (%d)", method, msg.Error.Message, msg.Error.Code)
			}
			return msg.Result, nil
		}
		b.record(msg)
	}
}

// waitEvent reads until the named event arrives, recording events meanwhile.
func (b *devBrowser) waitEvent(method string, timeout time.Duration) error {
	if b.seen[method] > 0 {
		b.seen[method]--
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		msg, err := b.receive(deadline)
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", method, err)
		}
		if msg.Method == method {
			return nil
		}
		b.record(msg)
	}
}

// record extracts the page-code errors the smoke forbids. Console output and
// browser log entries are kept with their text; the test allow-lists the one
// engine message that is not page code (see a11yAllowList).
func (b *devBrowser) record(msg cdpMessage) {
	if b.seen == nil {
		b.seen = map[string]int{}
	}
	b.seen[msg.Method]++
	switch msg.Method {
	case "Runtime.consoleAPICalled":
		var p struct {
			Type string            `json:"type"`
			Args []json.RawMessage `json:"args"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.Type != "error" {
			return
		}
		parts := make([]string, 0, len(p.Args))
		for _, raw := range p.Args {
			var arg struct {
				Value       any    `json:"value"`
				Description string `json:"description"`
			}
			if json.Unmarshal(raw, &arg) != nil {
				continue
			}
			if arg.Description != "" {
				parts = append(parts, arg.Description)
			} else if arg.Value != nil {
				parts = append(parts, fmt.Sprint(arg.Value))
			}
		}
		b.console = append(b.console, "console.error: "+strings.Join(parts, " "))
	case "Runtime.exceptionThrown":
		var p struct {
			ExceptionDetails struct {
				Text      string `json:"text"`
				Exception struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		text := p.ExceptionDetails.Text
		if d := p.ExceptionDetails.Exception.Description; d != "" {
			text += " " + d
		}
		b.console = append(b.console, "uncaught: "+text)
	case "Log.entryAdded":
		var p struct {
			Entry struct {
				Source string `json:"source"`
				Level  string `json:"level"`
				Text   string `json:"text"`
				URL    string `json:"url"`
			} `json:"entry"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.Entry.Level != "error" {
			return
		}
		b.console = append(b.console, fmt.Sprintf("log[%s] %s (%s)", p.Entry.Source, p.Entry.Text, p.Entry.URL))
	case "Network.responseReceived":
		var p struct {
			Type     string `json:"type"`
			Response struct {
				URL    string `json:"url"`
				Status int    `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.Type != "Document" {
			return
		}
		b.docStatus = p.Response.Status
		b.docURL = p.Response.URL
	}
}

// evaluate runs expression in the page's main world and decodes its value into
// out.
func (b *devBrowser) evaluate(expression string, out any) error {
	res, err := b.call("Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
		"userGesture":   true,
	})
	if err != nil {
		return err
	}
	var reply struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &reply); err != nil {
		return fmt.Errorf("decode evaluate result: %w", err)
	}
	if reply.ExceptionDetails != nil {
		text := reply.ExceptionDetails.Text
		if d := reply.ExceptionDetails.Exception.Description; d != "" {
			text += ": " + d
		}
		return fmt.Errorf("page exception: %s", text)
	}
	if out == nil || len(reply.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(reply.Result.Value, out)
}

// navigate loads url and waits for the load event, failing on a net error.
func (b *devBrowser) navigate(target string) {
	b.t.Helper()
	b.seen = map[string]int{} // a load event from an earlier navigation must not satisfy this one
	b.docStatus, b.docURL = 0, ""
	res, err := b.call("Page.navigate", map[string]any{"url": target})
	if err != nil {
		b.t.Fatalf("navigate %s: %v", target, err)
	}
	var reply struct {
		ErrorText string `json:"errorText"`
	}
	_ = json.Unmarshal(res, &reply)
	if reply.ErrorText != "" {
		b.t.Fatalf("navigate %s: %s", target, reply.ErrorText)
	}
	if err := b.waitEvent("Page.loadEventFired", 30*time.Second); err != nil {
		b.t.Fatalf("navigate %s: %v", target, err)
	}
}

// waitFor polls expression until it is truthy or the deadline passes.
func (b *devBrowser) waitFor(timeout time.Duration, what, expression string) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ok bool
		if err := b.evaluate(expression, &ok); err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			var state map[string]any
			_ = b.evaluate(`({url: location.href, ready: document.readyState})`, &state)
			b.t.Fatalf("timed out waiting for %s (page state %v)\n%s", what, state, b.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// setViewport emulates a viewport (CDP Emulation.setDeviceMetricsOverride).
func (b *devBrowser) setViewport(width, height int, mobile bool) {
	b.t.Helper()
	if _, err := b.call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": height, "deviceScaleFactor": 1, "mobile": mobile,
	}); err != nil {
		b.t.Fatalf("emulate %dx%d viewport: %v", width, height, err)
	}
}

// key dispatches one real key press (rawKeyDown + keyUp) — the same event a
// keyboard sends, so focus navigation and :focus-visible are the browser's own.
func (b *devBrowser) key(keyName, code string, vk int) {
	b.t.Helper()
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		if _, err := b.call("Input.dispatchKeyEvent", map[string]any{
			"type": typ, "key": keyName, "code": code,
			"windowsVirtualKeyCode": vk, "nativeVirtualKeyCode": vk,
		}); err != nil {
			b.t.Fatalf("dispatch %s %s: %v", typ, keyName, err)
		}
	}
}
