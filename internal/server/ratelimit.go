package server

// ratelimit.go — the PLAN-V2 §M.4/M.7 request rate limiter (normative contract:
// docs/features/16_moderation.md §12, wire notes in docs/go/06_server_http.md
// §2.4).
//
// One hand-rolled token bucket per (class, principal-or-address), all classes
// under ONE mutex, refilled continuously from the per-class [rate_limit] rate
// (tokens per window, capacity = the window's count). Four classes, each
// mounted at exactly one place so a page form and its JSON twin share the same
// bucket:
//
//	collab_write  issue/PR comments, review submits and dismissals, reactions,
//	              review-thread opens and comments, review requests
//	issue_open    opening an issue or a pull request
//	report        filing an abuse report
//	auth_fail     failed authentication attempts
//
// Git pushes and reads are deliberately NOT limited: a CI push storm must not
// throttle (PLAN-V2 §M.7), and the classifier only ever matches the shapes
// above. Nothing else on the server consumes a token — a read is free.
//
// ### Concurrency
//
// Hazard: map growth from keys an attacker mints (one per address or per
// principal). Avoidance: a hard rateLimitMaxKeys bound with DETERMINISTIC
// eviction — stale keys are swept first (idle past rateLimitIdle), then the
// oldest `last` timestamps go, ties broken by key, so the surviving set is a
// function of the input sequence and not of map iteration order. The bound is
// asserted by a test, not by timing.
//
// Hazard: the mutex held across a slow operation. Avoidance: Allow/Peek do
// float arithmetic and a map access only — never a store call, never a lock
// while a handler runs. The limiter holds no lock across `next.ServeHTTP`.
//
// Bucket state is instance-local memory BY DESIGN (PLAN-V2 §M.7, L4): a rate
// limiter is a control-plane cache like the render LRU. Durability would
// throttle nobody after a restart anyway, and putting it on the bucket would
// turn every write into a cross-instance CAS. A refused request is a neutral
// 429 with Retry-After: 60 — it discloses no state, not even whether the
// principal exists.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
)

// writeClass names one limited request class (the [rate_limit] key it reads).
type writeClass string

const (
	classNone        writeClass = ""
	classCollabWrite writeClass = "collab_write"
	classIssueOpen   writeClass = "issue_open"
	classReport      writeClass = "report"
	classAuthFail    writeClass = "auth_fail"
)

const (
	// rateLimitMaxKeys is the hard bound on live buckets (PLAN-V2 §M.7: 10k).
	rateLimitMaxKeys = 10000
	// rateLimitIdle is the sweep threshold: a bucket untouched for this long
	// is dropped before the bound eviction runs. Two windows of the slowest
	// configured class would be tighter, but a fixed idle floor keeps the
	// sweep independent of config (and testable).
	rateLimitIdle = 10 * time.Minute
	// rateLimitRetryAfter is the 429 Retry-After value (PLAN-V2 §M.7).
	rateLimitRetryAfter = "60"
	// rateLimitSweepEvery bounds how often the idle sweep runs (the sweep is
	// clock-driven, so a burst of distinct keys cannot trigger it per request).
	rateLimitSweepEvery = time.Minute
)

// rateSpec is one class's bucket: capacity tokens, refilled at rate/second.
type rateSpec struct {
	capacity float64
	perSec   float64
}

// rateToken is one bucket.
type rateToken struct {
	tokens float64
	last   time.Time
}

// rateLimiter is the process-local state (nil = the feature is disabled and
// every request is allowed).
type rateLimiter struct {
	mu        sync.Mutex
	specs     map[writeClass]rateSpec
	buckets   map[string]*rateToken
	now       func() time.Time
	lastSweep time.Time
}

// newRateLimiter builds the limiter from the parsed rates. Nil/empty specs
// yield a limiter that allows everything (the disabled shape), so callers never
// branch on "enabled" twice.
func newRateLimiter(specs map[writeClass]rateSpec, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{specs: specs, buckets: map[string]*rateToken{}, now: now, lastSweep: now()}
}

// limiterFromConfig builds the limiter a config asks for: nil when
// rate_limit.enabled is false (the zero-config default — L10: nothing
// changes). A section that fails to parse is a validation error at boot; the
// compiled defaults are the fallback here so a hand-edited file can never boot
// into "enabled but unlimited" (the safe side is the default rate, and the
// config error is reported separately by Validate).
func limiterFromConfig(cfg *config.Config, now func() time.Time) (*rateLimiter, error) {
	if cfg == nil || !cfg.RateLimit.Enabled {
		return nil, nil
	}
	rates, err := cfg.RateLimit.ParseRates()
	if err != nil {
		return nil, err
	}
	specs := map[writeClass]rateSpec{
		classCollabWrite: rateFromConfig(rates.CollabWrite),
		classIssueOpen:   rateFromConfig(rates.IssueOpen),
		classReport:      rateFromConfig(rates.Report),
		classAuthFail:    rateFromConfig(rates.AuthFail),
	}
	return newRateLimiter(specs, now), nil
}

func rateFromConfig(r config.RateSpec) rateSpec {
	return rateSpec{capacity: float64(r.Limit), perSec: r.PerSecond()}
}

// Allow consumes one token for (class, key); false = the bucket is empty.
// Unknown classes always allow (a class without a configured rate is not
// limited).
func (l *rateLimiter) Allow(class writeClass, key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(class, key, l.now())
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Peek reports whether one token is currently available without consuming it
// (the auth_fail pre-check: refuse before another credential verification).
func (l *rateLimiter) Peek(class writeClass, key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bucket(class, key, l.now()).tokens >= 1
}

// bucket refills and returns the bucket for (class, key), evicting under the
// bound. Callers hold l.mu.
func (l *rateLimiter) bucket(class writeClass, key string, now time.Time) *rateToken {
	spec, ok := l.specs[class]
	if !ok {
		// An unlimited class: a bucket that never empties.
		return &rateToken{tokens: 1, last: now}
	}
	if now.Sub(l.lastSweep) >= rateLimitSweepEvery {
		l.sweepLocked(now)
		l.lastSweep = now
	}
	full := string(class) + "\x00" + key
	b := l.buckets[full]
	if b == nil {
		b = &rateToken{tokens: spec.capacity, last: now}
		l.buckets[full] = b
		l.enforceBoundLocked()
		return b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * spec.perSec
		if b.tokens > spec.capacity {
			b.tokens = spec.capacity
		}
	}
	b.last = now
	return b
}

// sweepLocked drops buckets untouched for longer than rateLimitIdle. Callers
// hold l.mu.
func (l *rateLimiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > rateLimitIdle {
			delete(l.buckets, k)
		}
	}
}

// enforceBoundLocked keeps the map at or under rateLimitMaxKeys: the oldest
// `last` timestamps go first, ties broken by key, so the surviving set is a
// function of the input sequence, never of map iteration order. Callers hold
// l.mu.
func (l *rateLimiter) enforceBoundLocked() {
	if len(l.buckets) <= rateLimitMaxKeys {
		return
	}
	over := len(l.buckets) - rateLimitMaxKeys
	if over <= 0 {
		return
	}
	keys := make([]string, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ti, tj := l.buckets[keys[i]].last, l.buckets[keys[j]].last
		if ti.Equal(tj) {
			return keys[i] < keys[j]
		}
		return ti.Before(tj)
	})
	for _, k := range keys[:over] {
		delete(l.buckets, k)
	}
}

// Size reports the live bucket count (tests assert the bound).
func (l *rateLimiter) Size() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// --- classification -----------------------------------------------------------

// classifyWrite maps a request onto its rate-limit class plus the repo it
// writes to (the spam scorer's key). classNone means "not limited".
func classifyWrite(r *http.Request) (class writeClass, owner, repo string) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		return classNone, "", ""
	}
	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		return classNone, "", ""
	}
	segs := strings.Split(path, "/")
	// Repo lanes: /{owner}/{repo}/api[-browser]/…
	if len(segs) >= 4 && (segs[2] == "api" || segs[2] == "api-browser") {
		return classifyRepoLane(segs[0], segs[1], segs[3:], r.Method)
	}
	// Page lane: /{owner}/{repo}/… (the top-level API lives under /api/v1 and
	// never carries these shapes).
	if len(segs) >= 4 && segs[0] != "api" && segs[0] != "api-browser" && segs[0] != "services" {
		return classifyPageLane(segs[0], segs[1], segs[2:], r.Method)
	}
	return classNone, "", ""
}

// classifyRepoLane classifies /{o}/{r}/api[-browser]/<sub…> (07 §2 lanes).
func classifyRepoLane(owner, repo string, sub []string, method string) (writeClass, string, string) {
	switch sub[0] {
	case "issues":
		switch {
		case method == http.MethodPost && len(sub) == 1:
			return classIssueOpen, owner, repo
		case method == http.MethodPost && len(sub) >= 3 && sub[2] == "comments":
			return classCollabWrite, owner, repo
		case method == http.MethodPost && len(sub) >= 3 && sub[2] == "reactions":
			return classCollabWrite, owner, repo
		}
	case "pulls":
		switch {
		case method == http.MethodPost && len(sub) == 1:
			return classIssueOpen, owner, repo
		case len(sub) >= 3 && sub[2] == "comments" && method == http.MethodPost:
			return classCollabWrite, owner, repo
		case len(sub) >= 3 && sub[2] == "reviews" && method == http.MethodPost:
			return classCollabWrite, owner, repo
		case len(sub) >= 3 && sub[2] == "threads" && method == http.MethodPost:
			return classCollabWrite, owner, repo
		case len(sub) >= 3 && sub[2] == "review-requests" &&
			(method == http.MethodPost || method == http.MethodDelete):
			return classCollabWrite, owner, repo
		}
	case "moderation":
		if method == http.MethodPost && len(sub) == 2 && sub[1] == "reports" {
			return classReport, owner, repo
		}
	}
	return classNone, "", ""
}

// classifyPageLane classifies /{o}/{r}/<sub…> page forms (the same verbs as
// their JSON twins, so one bucket covers both lanes).
func classifyPageLane(owner, repo string, sub []string, method string) (writeClass, string, string) {
	if method != http.MethodPost {
		return classNone, "", ""
	}
	switch sub[0] {
	case "issues":
		switch {
		case len(sub) == 2 && sub[1] == "new":
			return classIssueOpen, owner, repo
		case len(sub) >= 3 && sub[2] == "comments":
			return classCollabWrite, owner, repo
		case len(sub) >= 3 && sub[2] == "reactions":
			return classCollabWrite, owner, repo
		case len(sub) == 3 && sub[2] == "report":
			return classReport, owner, repo
		}
	case "pulls":
		if len(sub) == 2 && sub[1] == "new" {
			return classIssueOpen, owner, repo
		}
	case "pull":
		if len(sub) >= 3 {
			switch sub[2] {
			case "comments", "reviews", "reviewers", "threads":
				return classCollabWrite, owner, repo
			case "report":
				return classReport, owner, repo
			}
		}
	}
	return classNone, "", ""
}

// --- middleware -----------------------------------------------------------------

// rateLimit is the §2.2 chain's last middleware: one gate per class, above
// every handler, so no verb can skip it (PLAN-V2 §M.6.4).
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rate == nil {
			next.ServeHTTP(w, r)
			return
		}
		class, owner, repo := classifyWrite(r)
		if class == classNone {
			// auth_fail pre-check: a credential-bearing request (or a login
			// attempt) from an address that has burned its failure budget is
			// refused before another verification runs. Reads and git are
			// never refused here (they carry no credential header).
			if authAttempt(r) && !isGitTransportPath(r.URL.Path) &&
				!s.rate.Peek(classAuthFail, s.clientAddr(r)) {
				s.rateLimited(w)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// AuthenticateForwarded: a brokered request keys on the forwardED
		// principal, never on the broker (the §8.6 rule every Seam-1 surface
		// follows — otherwise all of a broker's users would share one bucket).
		p, aerr := s.authSvc.AuthenticateForwarded(r, s.cfg)
		if aerr != nil {
			// The handler owns the failure mapping (and the auth_fail
			// consumption); the limiter is not a second authenticator.
			next.ServeHTTP(w, r)
			return
		}
		if !s.rate.Allow(class, rateKeyOf(p, s.clientAddr(r))) {
			s.rateLimited(w)
			return
		}
		if class != classCollabWrite || s.spamObserver == nil || p.Anonymous {
			next.ServeHTTP(w, r)
			return
		}
		// Advisory spam signal (PLAN-V2 §M.7): observe the body of a landed
		// collaboration write. The body is buffered and restored so the
		// handler reads exactly what the client sent; oversized bodies are
		// skipped (they cannot be a repeated spam body).
		body, ok := bufferCollabBody(r)
		if !ok || body == "" {
			next.ServeHTTP(w, r)
			return
		}
		spy := &statusSpy{ResponseWriter: w}
		next.ServeHTTP(spy, r)
		if spy.status != 0 && (spy.status < 200 || spy.status >= 400) {
			return // a rejected write is not content
		}
		// The write already landed; a client that disconnected must not
		// cancel the signal (the report is a store write, not a response).
		if s.spamObserver(context.WithoutCancel(r.Context()), owner, repo, p.Name, body) {
			s.metrics.Counter(spamFlagMetric, spamFlagHelp).Inc("repo", owner+"/"+repo)
		}
	})
}

// spamFlagMetric is the pre-filed-report counter (PLAN-V2 §M.3/M.7): one
// increment per spam signal that produced a report, labelled by repository.
const (
	spamFlagMetric = "spindle_spam_flag_total"
	spamFlagHelp   = "spam signals that pre-filed a moderation report"
)

// rateLimited writes the neutral 429 (PLAN-V2 §M.7: plain text + Retry-After,
// no state disclosure).
func (s *Server) rateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", rateLimitRetryAfter)
	plainStatus(w, http.StatusTooManyRequests, "too many requests")
}

// authAttempt reports whether the request is a credential verification worth
// the auth_fail pre-check: a machine credential header (what brute force uses)
// or a login-flow attempt. Session cookies are deliberately excluded: a stale
// cookie resolves to anonymous (no failure counted), and refusing browsers
// over another client's failures on a shared address is the one lockout a
// limiter must not create.
func authAttempt(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" ||
		strings.TrimSpace(r.Header.Get("X-Walgit-Authorization")) != "" {
		return true
	}
	switch r.URL.Path {
	case "/_auth/callback", "/_auth/tokens":
		return true
	}
	return false
}

// clientAddr is the limiter's anonymous key: the remote address host, or the
// FIRST X-Forwarded-For hop when rate_limit.trust_proxy is true.
//
// Trust assumption (PLAN-V2 §M.7, docs/features/16 §12.2): X-Forwarded-For is
// client-controlled. Enabling trust_proxy is a deployment statement that a
// reverse proxy in front of this process OVERWRITES the header (never appends
// to a client-supplied one). Only the first hop is read; later hops are the
// client's own text. An unparseable value falls back to the socket address.
func (s *Server) clientAddr(r *http.Request) string {
	if s.cfg != nil && s.cfg.RateLimit.TrustProxy {
		if xff, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); strings.TrimSpace(xff) != "" {
			if host, ok := addrHost(strings.TrimSpace(xff)); ok {
				return "x:" + host
			}
		}
	}
	host := r.RemoteAddr
	if h, ok := addrHost(host); ok {
		host = h
	}
	return "a:" + host
}

// addrHost strips a port from "host:port" (bare hosts pass through).
func addrHost(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h, h != ""
	}
	return s, true
}

// rateKeyOf is the bucket key: the principal name when authenticated (a shared
// address must not let one signed-in client spend another's budget), else the
// client address.
func rateKeyOf(p auth.Principal, addr string) string {
	if p.Anonymous || strings.TrimSpace(p.Name) == "" {
		return addr
	}
	return "u:" + strings.ToLower(strings.TrimSpace(p.Name))
}

// --- spam body extraction ---------------------------------------------------------

// spamBodyLimit bounds how much of a collaboration write the scorer reads. A
// comment body larger than this is not a repeated spam body worth hashing; a
// larger request is passed through untouched.
const spamBodyLimit = 1 << 20

// bufferCollabBody reads up to spamBodyLimit bytes of the request body and
// restores r.Body so the handler sees exactly the bytes the client sent (the
// unread tail is chained back on, never truncated). ok=false when the body is
// over the cap: nothing is extracted, and the handler still reads the whole
// stream.
func bufferCollabBody(r *http.Request) (string, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return "", true
	}
	if r.ContentLength > spamBodyLimit {
		return "", false // declared over the cap: never buffered
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, spamBodyLimit+1))
	// Restore the stream: what we read plus whatever the limit left unread.
	// The original body stays open for net/http to close after the handler.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
	if err != nil {
		// The client aborted mid-body; the handler sees what arrived and
		// nothing is scored (an incomplete body is not a spam signal).
		return "", false
	}
	if len(buf) > spamBodyLimit {
		return "", false // unbounded chunked body: the read bytes are restored
	}
	return collabText(r.Header.Get("Content-Type"), buf), true
}

// isGitTransportPath reports the git/LFS byte families. They are never
// rate-limited, including the auth_fail pre-check: a CI push storm with a valid
// credential must not be throttled, and a stale one must fail as a real 401
// (which is what makes git erase the credential), never as a 429 it would retry
// forever.
func isGitTransportPath(path string) bool {
	return strings.Contains(path, "git-upload-pack") ||
		strings.Contains(path, "git-receive-pack") ||
		strings.Contains(path, "/info/refs") ||
		strings.Contains(path, "/info/lfs")
}

// collabText extracts the human text of a collaboration write from its body:
// the `body`/`note` form fields for page and form-encoded API calls, or the
// `body`/`note`/`text` string members of a JSON object (07 §2). Anything else
// (an empty body, a multipart upload) yields "" — the scorer only ever sees
// text a human typed.
func collabText(contentType string, buf []byte) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "application/json"):
		return jsonCollabText(buf)
	case strings.Contains(ct, "application/x-www-form-urlencoded") || ct == "":
		return formCollabText(string(buf))
	}
	return ""
}

func formCollabText(body string) string {
	// url.ParseQuery is deliberately avoided: a malformed percent escape in
	// one field must not discard the whole body's text.
	var best string
	for _, pair := range strings.Split(body, "&") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		switch unescapeCollab(k) {
		case "body", "note":
			if t := strings.TrimSpace(unescapeCollab(v)); len(t) > len(best) {
				best = t
			}
		}
	}
	return best
}

func unescapeCollab(s string) string {
	if !strings.Contains(s, "%") && !strings.Contains(s, "+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '+':
			b.WriteByte(' ')
		case s[i] == '%' && i+2 < len(s):
			if hi, ok := unhex(s[i+1]); ok {
				if lo, ok := unhex(s[i+2]); ok {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// jsonCollabText walks a top-level JSON object for the string members the
// write verbs carry. Deliberately tiny: a body that is not a flat object with
// one of these members is not scored (never a parse error page, never a
// second JSON decoder with different semantics).
func jsonCollabText(buf []byte) string {
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return ""
	}
	var best string
	for _, key := range []string{"body", "body_text", "note", "text"} {
		if v, ok := m[key].(string); ok {
			if t := strings.TrimSpace(v); len(t) > len(best) {
				best = t
			}
		}
	}
	return best
}

// statusSpy records the status a handler wrote (0 = nothing written, which the
// net/http layer treats as 200).
type statusSpy struct {
	http.ResponseWriter
	status int
}

func (w *statusSpy) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusSpy) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusSpy) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
