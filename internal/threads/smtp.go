package threads

// Failure-notification email (PAR-197/D-27).
//
// The in-app inbox and the repository webhook are the first two channels; this
// file adds the third with the standard library only (net/smtp + crypto/tls),
// so the dependency budget does not move. Email is OFF until an operator sets
// threads.smtp.host. When it is on, a failed run sends ONE message per
// (subscriber, run) to the subscriber's verified address: that is the same
// failure-only event filter as the inbox, and delivery rides the inbox record's
// deterministic key (putCreate), so a re-fold or retry cannot send a second
// message.
//
// The message body is the same fixed facts the inbox carries (repository,
// workflow, branch, run link) and is passed through the shared MaskValues rule
// with the run's own job mask list. If the mask list cannot be resolved, email
// is skipped rather than sent unmasked (fail closed).

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SMTPConfig is the composition-owned mailer configuration (threads.smtp.*).
// Host empty disables the channel.
type SMTPConfig struct {
	Host     string
	Port     int
	From     string
	Username string
	Password string
	TLS      string // starttls | implicit | none
	Timeout  time.Duration
}

func (c SMTPConfig) enabled() bool { return strings.TrimSpace(c.Host) != "" }

func (c SMTPConfig) tlsMode() string {
	switch strings.ToLower(strings.TrimSpace(c.TLS)) {
	case "implicit":
		return "implicit"
	case "none":
		return "none"
	default:
		return "starttls"
	}
}

func (c SMTPConfig) port() int {
	if c.Port > 0 {
		return c.Port
	}
	if c.tlsMode() == "implicit" {
		return 465
	}
	return 587
}

func (c SMTPConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

// sendRunEmail delivers one notification to one address. It is only called
// when SMTP is configured and EmailFor resolved an address.
func (s *Service) sendRunEmail(ctx context.Context, to string, note *Notification, mask []string) error {
	if !s.SMTP.enabled() {
		return nil
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return nil
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("threads: smtp recipient %q: %w", to, err)
	}
	fromAddr, err := mail.ParseAddress(strings.TrimSpace(s.SMTP.From))
	if err != nil {
		return fmt.Errorf("threads: smtp from: %w", err)
	}
	tlsMode := s.SMTP.tlsMode()
	serverName := strings.TrimSpace(s.SMTP.Host)
	addr := net.JoinHostPort(serverName, strconv.Itoa(s.SMTP.port()))
	timeout := s.SMTP.timeout()

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	if tlsMode == "implicit" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("threads: smtp dial %s: %w", addr, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, serverName)
	if err != nil {
		return fmt.Errorf("threads: smtp hello %s: %w", addr, err)
	}
	defer c.Close()
	if tlsMode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("threads: smtp server %s does not advertise STARTTLS (set threads.smtp.tls = implicit or none for a trusted relay)", addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("threads: smtp starttls %s: %w", addr, err)
		}
	}
	if s.SMTP.Username != "" {
		auth := smtp.PlainAuth("", s.SMTP.Username, s.SMTP.Password, serverName)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("threads: smtp auth %s: %w", addr, err)
		}
	}
	if err := c.Mail(fromAddr.Address); err != nil {
		return fmt.Errorf("threads: smtp mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("threads: smtp rcpt %s: %w", to, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("threads: smtp data: %w", err)
	}
	if _, err := w.Write(s.composeRunEmail(note, to, mask)); err != nil {
		_ = w.Close()
		return fmt.Errorf("threads: smtp body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("threads: smtp end of data: %w", err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("threads: smtp quit: %w", err)
	}
	return nil
}

// composeRunEmail renders the fixed notification facts, masked with the run's
// job mask list. Header values are stripped of CR/LF so a workflow or branch
// name can never inject a second header.
func (s *Service) composeRunEmail(note *Notification, to string, mask []string) []byte {
	if note == nil {
		return nil
	}
	workflow := headerSafe(note.Workflow)
	branch := headerSafe(note.Branch)
	subject := fmt.Sprintf("[%s] %s failed on %s", headerSafe(note.Repo), workflow, branch)
	subject = MaskValues(subject, mask)
	link := note.Link
	if base := strings.TrimRight(strings.TrimSpace(s.CheckoutBaseURL), "/"); base != "" && strings.HasPrefix(link, "/") {
		link = base + link
	}
	body := fmt.Sprintf("The workflow %s failed on branch %s.\n\nRepository: %s\nRun: %s\n\n%s\n",
		workflow, branch, note.Repo, note.RunID, link)
	body = MaskValues(body, mask)
	from := strings.TrimSpace(s.SMTP.From)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", headerSafe(from))
	fmt.Fprintf(&b, "To: %s\r\n", headerSafe(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", s.nowUTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@spindle>\r\n", headerSafe(note.RunID))
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

// headerSafe removes CR/LF (and surrounding space) from a value that becomes a
// mail header; a repository, workflow or branch name is untrusted text.
func headerSafe(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

// runMaskValues is the union of every job's mask list for one run: the same
// values the log frames, summaries and annotations are masked with. An error is
// returned rather than swallowed so the caller can skip email instead of
// sending an unmasked message.
func (s *Service) runMaskValues(ctx context.Context, owner, repo, runID string) ([]string, error) {
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, job := range jobs {
		vals, verr := s.jobMaskValues(ctx, owner, repo, job)
		if verr != nil {
			return nil, verr
		}
		for _, v := range vals {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, nil
}

// emailMaskState is one notifyRunFailure pass's lazily resolved mask list. A
// failed resolution is terminal for the pass (fail closed): no subscriber gets
// an unmasked message, and the mask list is not retried per recipient.
type emailMaskState struct {
	loaded bool
	failed bool
	values []string
}

// notifyByEmail sends the inbox notification to every subscriber with a
// verified address. Callers pass only notifications that were just CREATED, so
// the deterministic inbox key is the dedupe: one message per (user, run).
func (s *Service) notifyByEmail(ctx context.Context, owner, repo string, note *Notification, mask *emailMaskState) {
	if !s.SMTP.enabled() || s.EmailFor == nil || note == nil {
		return
	}
	if !mask.loaded {
		vals, err := s.runMaskValues(ctx, owner, repo, note.RunID)
		if err != nil {
			slog.Warn("threads: notification email masks", "run", note.RunID, "err", err)
			mask.loaded, mask.failed = true, true
			return
		}
		mask.values, mask.loaded = vals, true
	}
	if mask.failed {
		return
	}
	to, err := s.EmailFor(ctx, note.User)
	if err != nil {
		slog.Warn("threads: notification email address", "user", note.User, "err", err)
		return
	}
	if strings.TrimSpace(to) == "" {
		return
	}
	if err := s.sendRunEmail(ctx, to, note, mask.values); err != nil {
		slog.Warn("threads: notification email", "user", note.User, "run", note.RunID, "err", err)
	}
}
