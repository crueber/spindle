package threads

// Email notification tests (PAR-197/D-27). The SMTP server is a tiny in-process
// listener: the notifier is standard library only, so the test needs no mailer
// service and no third-party dependency.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

type smtpMessage struct {
	to   string
	data string
}

func startFakeSMTP(t *testing.T) (string, <-chan smtpMessage, *int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	messages := make(chan smtpMessage, 8)
	var connections int32
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt32(&connections, 1)
			go serveFakeSMTP(conn, messages)
		}
	}()
	return ln.Addr().String(), messages, &connections
}

func serveFakeSMTP(conn net.Conn, out chan<- smtpMessage) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	say := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}
	say("220 fake-smtp ESMTP")
	var to string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			_, _ = w.WriteString("250-fake-smtp\r\n250 8BITMIME\r\n")
			_ = w.Flush()
		case strings.HasPrefix(upper, "MAIL FROM:"):
			say("250 2.1.0 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			to = strings.Trim(strings.TrimSpace(cmd[len("RCPT TO:"):]), "<>")
			say("250 2.1.5 ok")
		case upper == "DATA":
			say("354 end with .")
			var data strings.Builder
			for {
				bodyLine, berr := r.ReadString('\n')
				if berr != nil {
					return
				}
				if bodyLine == ".\r\n" || bodyLine == ".\n" {
					break
				}
				data.WriteString(bodyLine)
			}
			out <- smtpMessage{to: to, data: data.String()}
			say("250 2.0.0 queued")
		case upper == "QUIT":
			say("221 bye")
			return
		case upper == "RSET", upper == "NOOP":
			say("250 ok")
		default:
			say("500 unknown " + cmd)
		}
	}
}

func emailEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	k, err := NewKeyring(strings.Repeat("e", 32), "")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "root", Admin: true}
	if err := e.svc.SetSecret(ctx(), "o/r", "TOKEN", "leaky-secret-value", admin); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, auth.Principal{Name: "reader"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return e
}

func failingRun(t *testing.T, e *testEnv) *Run {
	t.Helper()
	e.git.files = map[string]map[string][]byte{}
	// The workflow name is the seeded secret value, so the email's masking is
	// observable in the subject and body, not just in a synthetic string.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(singleJobWorkflowYAML, "name: Single", "name: leaky-secret-value", 1))
	e.git.refs["refs/heads/main"] = testSHA
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	run := runs[0]
	if _, uerr := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, jerr
		}
		r.State = RunFailure
		return mustJSON(&r), true, nil
	}); uerr != nil {
		t.Fatalf("force failure: %v", uerr)
	}
	return run
}

func TestNotificationEmailSMTP(t *testing.T) {
	e := emailEnv(t)
	addr, messages, connections := startFakeSMTP(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("port: %v", err)
	}
	e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "spindle CI <ci@example.com>", TLS: "none", Timeout: 5 * time.Second}
	e.svc.EmailFor = func(_ context.Context, user string) (string, error) {
		if user != "reader" {
			return "", nil
		}
		return "reader@example.com", nil
	}

	run := failingRun(t, e)
	e.svc.notifyRunFailure(ctx(), "o", "r", run.ID)
	var got smtpMessage
	select {
	case got = <-messages:
	case <-time.After(5 * time.Second):
		t.Fatalf("no email delivered (connections=%d)", atomic.LoadInt32(connections))
	}
	if got.to != "reader@example.com" {
		t.Fatalf("recipient = %q", got.to)
	}
	for _, want := range []string{"Subject: [o/r] *** failed on main", "Repository: o/r", "/o/r/threads/" + run.ID, "***"} {
		if !strings.Contains(got.data, want) {
			t.Fatalf("email misses %q:\n%s", want, got.data)
		}
	}
	if strings.Contains(got.data, "leaky-secret-value") {
		t.Fatalf("email leaked the secret:\n%s", got.data)
	}
	// The inbox record's deterministic key is the dedupe: a second pass sends
	// nothing (and does not even dial).
	e.svc.notifyRunFailure(ctx(), "o", "r", run.ID)
	select {
	case extra := <-messages:
		t.Fatalf("a repeated notification sent a second email: %+v", extra)
	default:
	}
	if atomic.LoadInt32(connections) != 1 {
		t.Fatalf("connections = %d, want exactly one", atomic.LoadInt32(connections))
	}
}

func TestNotificationEmailOffByDefault(t *testing.T) {
	e := emailEnv(t)
	_, messages, connections := startFakeSMTP(t)
	// SMTP is zero (host empty): the channel is off even with an address.
	e.svc.EmailFor = func(context.Context, string) (string, error) { return "reader@example.com", nil }
	run := failingRun(t, e)
	e.svc.notifyRunFailure(ctx(), "o", "r", run.ID)
	if atomic.LoadInt32(connections) != 0 {
		t.Fatalf("email dialled while off")
	}
	select {
	case msg := <-messages:
		t.Fatalf("email sent while off: %+v", msg)
	default:
	}
}

func TestNotificationEmailOnlyOnFailure(t *testing.T) {
	e := emailEnv(t)
	addr, messages, connections := startFakeSMTP(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "ci@example.com", TLS: "none"}
	e.svc.EmailFor = func(context.Context, string) (string, error) { return "reader@example.com", nil }
	e.git.refs["refs/heads/main"] = testSHA
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The run is still queued (not a failure): the same event filter that keeps
	// the inbox quiet keeps email quiet.
	e.svc.notifyRunFailure(ctx(), "o", "r", runs[0].ID)
	if atomic.LoadInt32(connections) != 0 {
		t.Fatalf("email dialled for a non-failure run")
	}
	select {
	case msg := <-messages:
		t.Fatalf("email sent for a non-failure run: %+v", msg)
	default:
	}
}

// TestNotificationEmailErrorPaths pins the mailer's named failures and the
// fail-closed mask rule. The point is that a misconfiguration or an
// undeliverable relay is visible/logged, never a silent success and never an
// unmasked message.
func TestNotificationEmailErrorPaths(t *testing.T) {
	e := emailEnv(t)
	note := &Notification{
		ID: "r1", User: "reader", Repo: "o/r", RunID: "r1",
		Workflow: "CI", Branch: "main", Link: "/o/r/threads/r1",
	}
	// Off: no host, no attempt.
	if err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil); err != nil {
		t.Fatalf("off sendRunEmail = %v", err)
	}
	e.svc.SMTP = SMTPConfig{Host: "127.0.0.1", From: "ci@example.com", TLS: "none"}
	if err := e.svc.sendRunEmail(ctx(), "  ", note, nil); err != nil {
		t.Fatalf("empty recipient = %v", err)
	}
	if err := e.svc.sendRunEmail(ctx(), "not-an-address", note, nil); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	e.svc.SMTP.From = "not-an-address"
	if err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil); err == nil {
		t.Fatal("invalid From accepted")
	}
	// A closed port is a named dial error.
	e.svc.SMTP = SMTPConfig{Host: "127.0.0.1", Port: 1, From: "ci@example.com", TLS: "none", Timeout: 200 * time.Millisecond}
	if err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil); err == nil || !strings.Contains(err.Error(), "smtp dial") {
		t.Fatalf("dial error = %v", err)
	}
	// The implicit-TLS path dials TLS first; a closed port is the same named
	// failure, and proves the branch is exercised.
	e.svc.SMTP = SMTPConfig{Host: "127.0.0.1", Port: 1, From: "ci@example.com", TLS: "implicit", Timeout: 200 * time.Millisecond}
	if err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil); err == nil || !strings.Contains(err.Error(), "smtp dial") {
		t.Fatalf("implicit dial error = %v", err)
	}
	// A server without STARTTLS is refused rather than sent in clear text.
	addr, _, _ := startFakeSMTP(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "ci@example.com", TLS: "starttls", Timeout: 2 * time.Second}
	if err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("starttls refusal = %v", err)
	}
	// A rejected AUTH surfaces as an error (the fake answers 500 to AUTH).
	e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "ci@example.com", TLS: "none", Username: "u", Password: "p", Timeout: 2 * time.Second}
	if err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil); err == nil || !strings.Contains(err.Error(), "smtp auth") {
		t.Fatalf("auth error = %v", err)
	}

	run := failingRun(t, e)
	note.RunID, note.ID = run.ID, run.ID
	note.Link = "/o/r/threads/" + run.ID
	// notifyByEmail's lookup failures are logged, not fatal, and never send.
	e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "ci@example.com", TLS: "none"}
	e.svc.EmailFor = nil
	e.svc.notifyByEmail(ctx(), "o", "r", note, &emailMaskState{})
	e.svc.EmailFor = func(context.Context, string) (string, error) { return "", fmt.Errorf("lookup down") }
	e.svc.notifyByEmail(ctx(), "o", "r", note, &emailMaskState{})
	e.svc.EmailFor = func(context.Context, string) (string, error) { return "", nil }
	e.svc.notifyByEmail(ctx(), "o", "r", note, &emailMaskState{})
	// A corrupt job record makes the mask list unresolvable: email is skipped.
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("jobs = %d, %v", len(jobs), err)
	}
	if _, err := store.PutBytes(ctx(), e.store, JobKey("o", "r", run.ID, jobs[0].ID), []byte("{not json"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("corrupt job: %v", err)
	}
	if _, lerr := e.svc.loadJobs(ctx(), "o", "r", run.ID); lerr == nil {
		t.Fatalf("loadJobs did not see the corrupt record (key %s)", JobKey("o", "r", run.ID, jobs[0].ID))
	}
	e.svc.EmailFor = func(context.Context, string) (string, error) { return "reader@example.com", nil }
	e.svc.notifyByEmail(ctx(), "o", "r", note, &emailMaskState{})
	// The mask failure is remembered for the pass.
	state := emailMaskState{}
	e.svc.notifyByEmail(ctx(), "o", "r", note, &state)
	if !state.failed || !state.loaded {
		t.Fatalf("mask state = %+v", state)
	}
}

// TestSMTPConfigHelpers pins the small defaults the config layer and the mailer
// share, plus the absolute-link rendering. It is deliberately independent of a
// listener: these are pure decisions.
func TestSMTPConfigHelpers(t *testing.T) {
	if (SMTPConfig{}).enabled() {
		t.Fatal("zero config is enabled")
	}
	if !(SMTPConfig{Host: "smtp.example.com"}).enabled() {
		t.Fatal("configured host is disabled")
	}
	if got := (SMTPConfig{}).tlsMode(); got != "starttls" {
		t.Fatalf("default tls = %q", got)
	}
	if got := (SMTPConfig{TLS: "implicit"}).tlsMode(); got != "implicit" {
		t.Fatalf("implicit tls = %q", got)
	}
	if got := (SMTPConfig{TLS: "none"}).tlsMode(); got != "none" {
		t.Fatalf("none tls = %q", got)
	}
	if got := (SMTPConfig{TLS: "bogus"}).tlsMode(); got != "starttls" {
		t.Fatalf("unknown tls = %q, want starttls", got)
	}
	if got := (SMTPConfig{}).port(); got != 587 {
		t.Fatalf("starttls port = %d", got)
	}
	if got := (SMTPConfig{TLS: "implicit"}).port(); got != 465 {
		t.Fatalf("implicit port = %d", got)
	}
	if got := (SMTPConfig{Port: 2525}).port(); got != 2525 {
		t.Fatalf("explicit port = %d", got)
	}
	if got := (SMTPConfig{}).timeout(); got != 10*time.Second {
		t.Fatalf("default timeout = %s", got)
	}
	if got := (SMTPConfig{Timeout: 2 * time.Second}).timeout(); got != 2*time.Second {
		t.Fatalf("explicit timeout = %s", got)
	}
	s := &Service{SMTP: SMTPConfig{From: "ci@example.com"}, CheckoutBaseURL: "https://ci.example/"}
	if got := s.composeRunEmail(nil, "to", nil); got != nil {
		t.Fatalf("nil note = %q", got)
	}
	note := &Notification{Repo: "o/r", Workflow: "CI", Branch: "main", RunID: "r1", Link: "/o/r/threads/r1"}
	data := string(s.composeRunEmail(note, "reader@example.com", nil))
	if !strings.Contains(data, "https://ci.example/o/r/threads/r1") {
		t.Fatalf("absolute link missing:\n%s", data)
	}
}

// startRejectingSMTP answers 220/EHLO and then rejects one command, so the
// mailer's MAIL/RCPT/DATA error paths are named (not swallowed).
func startRejectingSMTP(t *testing.T, reject string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				w := bufio.NewWriter(conn)
				say := func(line string) { _, _ = w.WriteString(line + "\r\n"); _ = w.Flush() }
				say("220 rejecting-smtp")
				for {
					line, rerr := r.ReadString('\n')
					if rerr != nil {
						return
					}
					upper := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
						say("250 rejecting-smtp")
					case strings.HasPrefix(upper, "MAIL FROM:"):
						if reject == "mail" {
							say("550 mail rejected")
							continue
						}
						say("250 ok")
					case strings.HasPrefix(upper, "RCPT TO:"):
						if reject == "rcpt" {
							say("550 rcpt rejected")
							continue
						}
						say("250 ok")
					case upper == "DATA":
						if reject == "data" {
							say("451 data rejected")
							continue
						}
						say("354 go")
						for {
							body, berr := r.ReadString('\n')
							if berr != nil || body == ".\r\n" {
								break
							}
						}
						say("250 queued")
					case upper == "QUIT":
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}(conn)
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func TestNotificationEmailCommandErrors(t *testing.T) {
	e := emailEnv(t)
	note := &Notification{Repo: "o/r", Workflow: "CI", Branch: "main", RunID: "r1", Link: "/o/r/threads/r1"}
	for _, tc := range []struct {
		reject string
		want   string
	}{
		{"mail", "smtp mail from"},
		{"rcpt", "smtp rcpt"},
		{"data", "smtp data"},
	} {
		host, port := startRejectingSMTP(t, tc.reject)
		e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "ci@example.com", TLS: "none", Timeout: 2 * time.Second}
		err := e.svc.sendRunEmail(ctx(), "reader@example.com", note, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s rejection = %v, want %q", tc.reject, err, tc.want)
		}
	}
}

func TestNotificationEmailDeliveryFailureAndEmptyMask(t *testing.T) {
	e := emailEnv(t)
	// A run with no jobs is a valid, empty mask list (nothing to mask).
	if vals, err := e.svc.runMaskValues(ctx(), "o", "r", "no-such-run"); err != nil || len(vals) != 0 {
		t.Fatalf("empty mask = %v, %v", vals, err)
	}
	run := failingRun(t, e)
	host, port := startRejectingSMTP(t, "mail")
	e.svc.SMTP = SMTPConfig{Host: host, Port: port, From: "ci@example.com", TLS: "none", Timeout: 2 * time.Second}
	e.svc.EmailFor = func(context.Context, string) (string, error) { return "reader@example.com", nil }
	note := &Notification{ID: run.ID, User: "reader", Repo: "o/r", RunID: run.ID,
		Workflow: run.Workflow, Branch: run.Branch, Link: "/o/r/threads/" + run.ID}
	// The SMTP failure is logged and swallowed: notifications never fail a run.
	e.svc.notifyByEmail(ctx(), "o", "r", note, &emailMaskState{})
}
