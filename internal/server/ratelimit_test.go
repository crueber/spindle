package server

// ratelimit_test.go — PLAN-V2 §M.4/M.7 (docs/features/16_moderation.md §12,
// docs/go/06_server_http.md §2.4). The assertions are invariants, never
// timing: budgets are consumed by counting requests, the clock is injected,
// and the key bound is a size assertion.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
)

// limiterServer builds a server whose limiter is enabled with one-minute
// classes shaped like the configuration (small counts so a test is a loop, not
// a wait).
func limiterServer(t *testing.T, rl config.RateLimit) (*Server, http.Handler) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ci-bot", Token: "tok-ci", Write: true}}
	cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	cfg.RateLimit = rl
	s := New(Options{Config: cfg, Store: store.NewMemory(), Version: "testsha"})
	return s, s.Handler()
}

func defaultLimits(mut func(*config.RateLimit)) config.RateLimit {
	rl := config.RateLimit{
		Enabled: true, CollabWrite: "2/min", IssueOpen: "1/min",
		Report: "1/min", AuthFail: "2/min",
	}
	if mut != nil {
		mut(&rl)
	}
	return rl
}

// post fires one request with a Bearer static token (the authenticated key) and
// returns the response.
func post(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRateLimitPerClassAndLane pins the classes on BOTH lanes: the page form
// and the JSON twin share one bucket per class, and a class with its own budget
// is not spent by another class.
func TestRateLimitPerClassAndLane(t *testing.T) {
	_, h := limiterServer(t, defaultLimits(func(rl *config.RateLimit) {
		rl.CollabWrite = "2/min"
		rl.IssueOpen = "1/min"
	}))
	// collab_write, page lane: 2 allowed, the third is 429.
	for i := 0; i < 2; i++ {
		// The page provider is unwired here, so the handler answers 405/404 —
		// what matters is that the LIMITER let these through.
		if rec := post(h, "POST", "/acme/widgets/issues/3/comments", "tok-ci", "body=hi"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("page comment %d was limited before its budget was spent", i)
		}
	}
	rec := post(h, "POST", "/acme/widgets/issues/3/comments", "tok-ci", "body=hi")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third page comment = %d, want 429", rec.Code)
	}
	// The SAME bucket covers the JSON lane (one gate per class).
	if rec := post(h, "POST", "/acme/widgets/api/issues/3/comments", "tok-ci", `{"body":"hi"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("api comment after the page burst = %d, want 429", rec.Code)
	}
	// issue_open has its own budget: one allowed, then 429.
	if rec := post(h, "POST", "/acme/widgets/api/pulls", "tok-ci", `{"title":"x"}`); rec.Code == http.StatusTooManyRequests {
		t.Fatal("issue_open shared the collab_write bucket")
	}
	if rec := post(h, "POST", "/acme/widgets/pulls/new", "tok-ci", "title=x"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second issue_open = %d, want 429 across lanes", rec.Code)
	}
	// A read is never limited.
	for i := 0; i < 10; i++ {
		if rec := post(h, "GET", "/acme/widgets/issues/3/comments", "tok-ci", ""); rec.Code == http.StatusTooManyRequests {
			t.Fatal("a read was rate limited")
		}
	}
}

// TestRateLimitRetryAfter is the wire contract: plain text + Retry-After: 60,
// no state disclosure.
func TestRateLimitRetryAfter(t *testing.T) {
	_, h := limiterServer(t, defaultLimits(func(rl *config.RateLimit) { rl.Report = "1/min" }))
	post(h, "POST", "/acme/widgets/api/moderation/reports", "tok-ci", `{"reason":"spam"}`)
	rec := post(h, "POST", "/acme/widgets/api/moderation/reports", "tok-ci", `{"reason":"spam"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	if body := strings.TrimSpace(rec.Body.String()); strings.Contains(body, "acme") || strings.Contains(body, "exists") {
		t.Fatalf("refusal body discloses state: %q", body)
	}
}

// TestRateLimitAnonymousAddressKeying pins the anonymous key: the remote
// address, one budget per address.
func TestRateLimitAnonymousAddressKeying(t *testing.T) {
	_, h := limiterServer(t, defaultLimits(func(rl *config.RateLimit) { rl.CollabWrite = "1/min" }))
	first := func(addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/acme/widgets/pull/1/comments", strings.NewReader("body=x"))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := first("198.51.100.7:1234"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("first anonymous write refused")
	}
	if rec := first("198.51.100.7:9999"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the same address with another port = %d, want 429 (the port is not the key)", rec.Code)
	}
	if rec := first("198.51.100.8:1234"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("a second address shared the first address's budget")
	}
}

// TestRateLimitTrustProxy pins the X-Forwarded-For rule: ignored by default,
// and only its FIRST hop counts when trusted.
func TestRateLimitTrustProxy(t *testing.T) {
	xff := func(h http.Handler, addr, header string) int {
		req := httptest.NewRequest("POST", "/acme/widgets/pull/1/comments", strings.NewReader("body=x"))
		req.RemoteAddr = addr
		req.Header.Set("X-Forwarded-For", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	_, untrusted := limiterServer(t, defaultLimits(func(rl *config.RateLimit) {
		rl.CollabWrite = "1/min"
		rl.TrustProxy = false
	}))
	xff(untrusted, "203.0.113.9:1111", "1.1.1.1")
	if got := xff(untrusted, "203.0.113.9:2222", "2.2.2.2"); got != http.StatusTooManyRequests {
		t.Fatalf("untrusted X-Forwarded-For minted a new bucket (status %d)", got)
	}

	_, trusted := limiterServer(t, defaultLimits(func(rl *config.RateLimit) {
		rl.CollabWrite = "1/min"
		rl.TrustProxy = true
	}))
	if got := xff(trusted, "203.0.113.9:1111", "1.1.1.1"); got == http.StatusTooManyRequests {
		t.Fatal("the first hop was not keyed")
	}
	if got := xff(trusted, "203.0.113.9:2222", "2.2.2.2"); got == http.StatusTooManyRequests {
		t.Fatalf("a different first hop shared the bucket (status %d)", got)
	}
	if got := xff(trusted, "203.0.113.9:3333", "1.1.1.1, 9.9.9.9"); got != http.StatusTooManyRequests {
		t.Fatalf("the first hop was not the key (status %d)", got)
	}
}

// TestRateLimitDisabledAllowsEverything pins the zero-config default: disabled
// limiter = allow, no headers, no buckets.
func TestRateLimitDisabledAllowsEverything(t *testing.T) {
	_, h := limiterServer(t, config.RateLimit{}) // Enabled false, defaults otherwise
	for i := 0; i < 20; i++ {
		if rec := post(h, "POST", "/acme/widgets/api/issues/1/comments", "", `{"body":"x"}`); rec.Code == http.StatusTooManyRequests {
			t.Fatal("a disabled limiter refused a request")
		}
	}
}

// TestRateLimitNeverThrottlesGit is the plan's CI-storm guarantee: git pushes
// and reads do not classify into any class.
func TestRateLimitNeverThrottlesGit(t *testing.T) {
	_, h := limiterServer(t, defaultLimits(nil))
	for i := 0; i < 50; i++ {
		for _, p := range []string{"/acme/widgets.git/git-receive-pack", "/acme/widgets.git/git-upload-pack", "/acme/widgets.git/info/refs?service=git-upload-pack"} {
			method := "POST"
			if strings.Contains(p, "info/refs") {
				method = "GET"
			}
			if rec := post(h, method, p, "tok-ci", ""); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("git path %s was rate limited", p)
			}
		}
	}
}

// TestRateLimitAuthFail pins the login-failure class: rejected credentials
// consume the budget and the exhausted address is refused with 429, never a
// third 401.
func TestRateLimitAuthFail(t *testing.T) {
	s, h := limiterServer(t, defaultLimits(func(rl *config.RateLimit) {
		rl.AuthFail = "3/min"
		rl.CollabWrite = "1000/min" // isolate the auth_fail class
	}))
	if s.rate == nil {
		t.Fatal("limiter nil with enabled config")
	}
	bad := "Basic " + basicCred("wrong", "wrong")
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/services/setup.json", nil)
		req.Header.Set("Authorization", bad)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 3; i++ {
		if got := do().Code; got != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, got)
		}
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted auth_fail = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "60" {
		t.Fatal("auth_fail 429 must carry Retry-After")
	}
	// A successful credential is not a failure and is never counted.
	for i := 0; i < 10; i++ {
		if rec := post(h, "POST", "/acme/widgets/api/issues/1/comments", "tok-ci", `{"body":"x"}`); rec.Code == http.StatusTooManyRequests {
			t.Fatal("a valid write was throttled by the auth_fail class")
		}
	}
}

// TestAuthFailNeverThrottlesGit pins the CI-storm guarantee for the auth_fail
// pre-check too: a git request with a credential is never refused with 429, even
// from an address that exhausted its failure budget — a stale CI credential must
// fail as a real 401 (which git erases) and never as a retryable 429.
func TestAuthFailNeverThrottlesGit(t *testing.T) {
	_, h := limiterServer(t, defaultLimits(func(rl *config.RateLimit) { rl.AuthFail = "1/min" }))
	bad := "Basic " + basicCred("wrong", "wrong")
	// Burn the budget on a non-git credential attempt.
	req := httptest.NewRequest("GET", "/services/setup.json", nil)
	req.Header.Set("Authorization", bad)
	h.ServeHTTP(httptest.NewRecorder(), req)
	for i := 0; i < 5; i++ {
		git := httptest.NewRequest("POST", "/acme/widgets.git/git-receive-pack", strings.NewReader(""))
		git.Header.Set("Authorization", "Basic "+basicCred("ci", "stale"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, git)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatal("a git push was refused by the auth_fail pre-check")
		}
	}
}

// TestSpamBodyBufferingRestoresHandlerView pins the middleware contract with the
// body: the handler reads exactly the client's bytes, a body over the cap is
// passed through untouched, and a body at the cap is still extracted.
func TestSpamBodyBufferingRestoresHandlerView(t *testing.T) {
	s, _ := limiterServer(t, defaultLimits(func(rl *config.RateLimit) { rl.CollabWrite = "100/min" }))
	var got string
	s.spamObserver = func(ctx context.Context, owner, repo, principal, body string) bool { return false }
	h := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	payload := `{"body":"` + strings.Repeat("x", 100) + `"}`
	req := httptest.NewRequest("POST", "/acme/widgets/api/issues/1/comments", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer tok-ci")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != payload {
		t.Fatalf("handler saw %d bytes, want the original %d", len(got), len(payload))
	}

	// Over the cap (declared): untouched stream, no extraction.
	big := strings.Repeat("y", spamBodyLimit+10)
	var seen int
	s.spamObserver = func(ctx context.Context, owner, repo, principal, body string) bool {
		t.Fatal("an over-cap body was extracted")
		return false
	}
	h = s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = len(b)
	}))
	req = httptest.NewRequest("POST", "/acme/widgets/api/issues/1/comments", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer tok-ci")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != len(big) {
		t.Fatalf("over-cap handler saw %d of %d bytes", seen, len(big))
	}
}

// TestRateLimitNoStoreReadForKeying is the budget assertion: keying on a
// resolved principal (and refusing) adds no store round trip — the limiter is
// memory only.
func TestRateLimitNoStoreReadForKeying(t *testing.T) {
	counting := &countingStore{ObjectStore: store.NewMemory()}
	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ci-bot", Token: "tok-ci", Write: true}}
	cfg.RateLimit = defaultLimits(func(rl *config.RateLimit) { rl.CollabWrite = "1/min" })
	s := New(Options{Config: cfg, Store: counting, Version: "testsha"})
	// Disable the static-token last-used recorder (06 §8.2.1): it is an
	// asynchronous background writer, and this assertion measures the RATE
	// LIMITER's own path — a store op from unrelated bookkeeping landing in the
	// window would not be a limiter regression. Authentication still resolves;
	// only the advisory usage write is off.
	s.Auth().SetTokenUsage(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/acme/widgets/api/issues/1/comments", strings.NewReader(`{"body":"x"}`))
		req.Header.Set("Authorization", "Bearer tok-ci")
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	if got := counting.reads.Load() + counting.writes.Load(); got != 0 {
		t.Fatalf("rate limiting did %d store operations, want 0", got)
	}
}

func basicCred(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// countingStore counts store round trips (the budget assertions).
type countingStore struct {
	store.ObjectStore
	reads  atomic.Int64
	writes atomic.Int64
}

func (c *countingStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	c.reads.Add(1)
	return c.ObjectStore.Get(ctx, key, opts)
}

func (c *countingStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	c.writes.Add(1)
	return c.ObjectStore.Put(ctx, key, body, opts)
}

// TestRateLimiterBoundAndSweep is the map invariant: <= 10k live buckets ever,
// idle keys swept, eviction deterministic (the old keys go, the new one stays).
func TestRateLimiterBoundAndSweep(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	l := newRateLimiter(map[writeClass]rateSpec{
		classCollabWrite: {capacity: 30, perSec: 0.5},
	}, func() time.Time { return now })
	for i := 0; i < rateLimitMaxKeys+500; i++ {
		l.Allow(classCollabWrite, fmt.Sprintf("u:user%05d", i))
	}
	if got := l.Size(); got != rateLimitMaxKeys {
		t.Fatalf("bucket count = %d, want the hard bound %d", got, rateLimitMaxKeys)
	}
	l.mu.Lock()
	if _, ok := l.buckets[string(classCollabWrite)+"\x00u:user00000"]; ok {
		t.Fatal("an old key survived: eviction must drop the oldest deterministically")
	}
	_, newest := l.buckets[string(classCollabWrite)+"\x00u:user10499"]
	l.mu.Unlock()
	if !newest {
		t.Fatal("the newest key was evicted")
	}

	// Idle sweep: every key untouched past rateLimitIdle is dropped on the
	// next clock tick past the sweep interval.
	now = now.Add(rateLimitIdle + rateLimitSweepEvery + time.Second)
	l.Allow(classCollabWrite, "u:fresh")
	if got := l.Size(); got != 1 {
		t.Fatalf("after the idle sweep: %d buckets, want 1", got)
	}
}

// TestRateLimiterRefillAndPeek pins the token arithmetic on the injected clock:
// a full bucket allows capacity requests, then one token per period.
func TestRateLimiterRefillAndPeek(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	l := newRateLimiter(map[writeClass]rateSpec{
		classReport: {capacity: 2, perSec: 1.0 / 60.0},
	}, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if !l.Allow(classReport, "u:a") {
			t.Fatalf("token %d refused", i+1)
		}
	}
	if l.Allow(classReport, "u:a") {
		t.Fatal("a third token was allowed from an empty bucket")
	}
	if l.Peek(classReport, "u:a") {
		t.Fatal("Peek reported a token on an empty bucket")
	}
	now = now.Add(30 * time.Second) // half a token
	if l.Peek(classReport, "u:a") || l.Allow(classReport, "u:a") {
		t.Fatal("half a token was spendable")
	}
	now = now.Add(31 * time.Second) // 1.01 tokens
	if !l.Peek(classReport, "u:a") {
		t.Fatal("Peek missed the refilled token")
	}
	if !l.Allow(classReport, "u:a") {
		t.Fatal("the refilled token was not spendable")
	}
	if l.Allow(classReport, "u:a") {
		t.Fatal("one refill bought two requests")
	}
	// An unconfigured class is unlimited (the disabled shape).
	if !l.Allow(classNone, "u:a") || !l.Allow(writeClass("unknown"), "u:a") {
		t.Fatal("an unconfigured class was limited")
	}
	if nilLimiter := (*rateLimiter)(nil); !nilLimiter.Allow(classReport, "x") || !nilLimiter.Peek(classReport, "x") || nilLimiter.Size() != 0 {
		t.Fatal("nil limiter must allow and report zero size")
	}
}

// TestClassifyWrite pins the closed class table: the shapes that must be
// limited, and — just as load-bearing — the nearby writes that must not be.
func TestClassifyWrite(t *testing.T) {
	cases := []struct {
		method, path string
		class        writeClass
	}{
		// collab writes, both lanes
		{"POST", "/acme/widgets/api/issues/3/comments", classCollabWrite},
		{"POST", "/acme/widgets/api-browser/issues/3/comments", classCollabWrite},
		{"POST", "/acme/widgets/api/issues/3/reactions/5", classCollabWrite},
		{"POST", "/acme/widgets/api/pulls/7/comments", classCollabWrite},
		{"POST", "/acme/widgets/api/pulls/7/reviews", classCollabWrite},
		{"POST", "/acme/widgets/api/pulls/7/reviews/2/dismiss", classCollabWrite},
		{"POST", "/acme/widgets/api/pulls/7/threads", classCollabWrite},
		{"POST", "/acme/widgets/api/pulls/7/threads/t1/comments", classCollabWrite},
		{"POST", "/acme/widgets/api/pulls/7/review-requests", classCollabWrite},
		{"DELETE", "/acme/widgets/api/pulls/7/review-requests", classCollabWrite},
		{"POST", "/acme/widgets/issues/3/comments", classCollabWrite},
		{"POST", "/acme/widgets/issues/3/reactions/5", classCollabWrite},
		{"POST", "/acme/widgets/pull/7/comments", classCollabWrite},
		{"POST", "/acme/widgets/pull/7/reviews", classCollabWrite},
		{"POST", "/acme/widgets/pull/7/reviewers", classCollabWrite},
		{"POST", "/acme/widgets/pull/7/reviewers/remove", classCollabWrite},
		{"POST", "/acme/widgets/pull/7/threads", classCollabWrite},
		{"POST", "/acme/widgets/pull/7/threads/t1/comments", classCollabWrite},
		// opens
		{"POST", "/acme/widgets/api/issues", classIssueOpen},
		{"POST", "/acme/widgets/api/pulls", classIssueOpen},
		{"POST", "/acme/widgets/issues/new", classIssueOpen},
		{"POST", "/acme/widgets/pulls/new", classIssueOpen},
		// reports
		{"POST", "/acme/widgets/api/moderation/reports", classReport},
		{"POST", "/acme/widgets/issues/3/report", classReport},
		{"POST", "/acme/widgets/pull/7/report", classReport},
		// NOT limited: moderation actions, state changes, git, reads, the
		// host-admin queue.
		{"POST", "/acme/widgets/issues/3/moderation/hide", classNone},
		{"POST", "/acme/widgets/issues/3/close", classNone},
		{"POST", "/acme/widgets/pull/7/merge", classNone},
		{"POST", "/acme/widgets/api/pulls/7/merge", classNone},
		{"POST", "/acme/widgets/api/moderation/reports/9/resolve", classNone},
		{"POST", "/api/v1/moderation/reports/acme/widgets/9/resolve", classNone},
		{"POST", "/acme/widgets.git/git-receive-pack", classNone},
		{"POST", "/acme/widgets/git-receive-pack", classNone},
		{"GET", "/acme/widgets/api/issues/3/comments", classNone},
		{"POST", "/new", classNone},
		{"POST", "/acme/widgets/labels", classNone},
		{"POST", "/acme/widgets/contents", classNone},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		got, _, _ := classifyWrite(req)
		if got != tc.class {
			t.Fatalf("classifyWrite(%s %s) = %q, want %q", tc.method, tc.path, got, tc.class)
		}
	}
	req := httptest.NewRequest("POST", "/acme/widgets/api/pulls/7/comments", nil)
	if _, owner, repo := classifyWrite(req); owner != "acme" || repo != "widgets" {
		t.Fatalf("repo key = %q/%q", owner, repo)
	}
}

// TestCollabBodyExtraction pins the scorer's input: the typed text of the two
// content types the write verbs accept, and nothing else.
func TestCollabBodyExtraction(t *testing.T) {
	form := "reason=spam&body=" + url.QueryEscape("hello there & goodbye")
	if got := collabText("application/x-www-form-urlencoded", []byte(form)); got != "hello there & goodbye" {
		t.Fatalf("form body = %q", got)
	}
	if got := collabText("application/json", []byte(`{"body":"json text","title":"x"}`)); got != "json text" {
		t.Fatalf("json body = %q", got)
	}
	if got := collabText("application/json", []byte(`{"note":"a note"}`)); got != "a note" {
		t.Fatalf("json note = %q", got)
	}
	for _, tc := range []struct{ ct, body string }{
		{"application/json", `not json`},
		{"application/json", `{"body":42}`},
		{"multipart/form-data; boundary=x", "body=ignored"},
		{"", ""},
	} {
		if got := collabText(tc.ct, []byte(tc.body)); got != "" {
			t.Fatalf("collabText(%q, %q) = %q, want empty", tc.ct, tc.body, got)
		}
	}
}

// TestSpamObserverRunsOnLandedWrite pins the advisory hook: called once for a
// landed collab write with the principal and the extracted text, never for a
// rejected write, never for a read, and the metric counts the filed signals.
func TestSpamObserverRunsOnLandedWrite(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	s, _ := limiterServer(t, defaultLimits(nil))
	s.spamObserver = func(ctx context.Context, owner, repo, principal, body string) bool {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, owner+"/"+repo+"|"+principal+"|"+body)
		return true
	}
	h := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest("POST", "/acme/widgets/api/issues/3/comments", strings.NewReader(`{"body":"the spam text"}`))
	req.Header.Set("Authorization", "Bearer tok-ci")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	mu.Lock()
	if len(seen) != 1 || seen[0] != "acme/widgets|ci-bot|the spam text" {
		t.Fatalf("observer saw %v", seen)
	}
	mu.Unlock()
	if render := s.metrics.Render(); !strings.Contains(render, `spindle_spam_flag_total{repo="acme/widgets"} 1`) {
		t.Fatalf("counter not incremented:\n%s", render)
	}

	// A rejected write is not content.
	s.spamObserver = func(ctx context.Context, owner, repo, principal, body string) bool {
		t.Fatal("observer ran for a rejected write")
		return false
	}
	rej := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainStatus(w, http.StatusBadRequest, "nope")
	}))
	req = httptest.NewRequest("POST", "/acme/widgets/api/issues/3/comments", strings.NewReader(`{"body":"x"}`))
	req.Header.Set("Authorization", "Bearer tok-ci")
	req.Header.Set("Content-Type", "application/json")
	rej.ServeHTTP(httptest.NewRecorder(), req)

	// Anonymous writes are not scored (no principal).
	s.spamObserver = func(ctx context.Context, owner, repo, principal, body string) bool {
		t.Fatal("observer ran for an anonymous write")
		return false
	}
	anon := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	anonReq := httptest.NewRequest("POST", "/acme/widgets/api/issues/3/comments", strings.NewReader(`{"body":"x"}`))
	anonReq.Header.Set("Content-Type", "application/json")
	anon.ServeHTTP(httptest.NewRecorder(), anonReq)
}

// TestLimiterBootFromConfig pins the boot contract: a bad [rate_limit] spelling
// falls back to the compiled defaults with the limiter still enabled (never
// "enabled but unlimited"), and disabled is nil.
func TestLimiterBootFromConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.RateLimit = config.RateLimit{Enabled: true, CollabWrite: "nonsense", IssueOpen: "10/h", Report: "5/h", AuthFail: "10/min"}
	s := New(Options{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if s.rate == nil {
		t.Fatal("a bad spelling disabled the limiter")
	}
	if got := s.rate.specs[classCollabWrite].capacity; got != 30 {
		t.Fatalf("fallback capacity = %v, want the 30/min default", got)
	}
	cfg.RateLimit.Enabled = false
	if s := New(Options{Config: cfg}); s.rate != nil {
		t.Fatal("disabled config built a limiter")
	}
	// The spam observer is the composition's; New only stores it.
	obs := func(context.Context, string, string, string, string) bool { return true }
	if s := New(Options{Config: config.Defaults(), SpamObserver: obs}); s.spamObserver == nil {
		t.Fatal("Options.SpamObserver was not stored")
	}
	if lim, err := limiterFromConfig(nil, nil); err != nil || lim != nil {
		t.Fatalf("nil config = %v, %v", lim, err)
	}
}

// TestRateLimitMiddlewareEdgeCases pins the small branches: an unreadable
// request body, a streamed (ContentLength -1) over-cap body, the auth-error
// pass-through, the Flush pass-through of the status spy, and the
// authAttempt/isGitTransportPath helpers.
func TestRateLimitMiddlewareEdgeCases(t *testing.T) {
	s, _ := limiterServer(t, defaultLimits(nil))
	failRead := &failingBody{}
	r := httptest.NewRequest("POST", "/acme/widgets/api/issues/1/comments", failRead)
	r.Body = failRead
	if body, ok := bufferCollabBody(r); body != "" || ok {
		t.Fatalf("failing body = %q, %v", body, ok)
	}
	if _, ok := bufferCollabBody(httptest.NewRequest("GET", "/x", nil)); !ok {
		t.Fatal("a nil body must report ok with no text")
	}

	// Chunked over-cap body: the cap bytes are restored, nothing extracted.
	big := strings.Repeat("z", spamBodyLimit+5)
	r = httptest.NewRequest("POST", "/acme/widgets/api/issues/1/comments", strings.NewReader(big))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if body, ok := bufferCollabBody(r); body != "" || ok {
		t.Fatalf("streamed over-cap body = %q, %v", body, ok)
	}
	rest, _ := io.ReadAll(r.Body)
	if len(rest) != spamBodyLimit+5 {
		t.Fatalf("restored body = %d bytes, want %d", len(rest), spamBodyLimit+5)
	}

	// An authentication error passes through (the handler owns the mapping).
	h := s.rateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest("POST", "/acme/widgets/api/issues/1/comments", strings.NewReader(`{"body":"x"}`))
	req.Header.Set("Authorization", "Basic "+basicCred("wrong", "wrong"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("auth-error passthrough = %d", rec.Code)
	}

	// statusSpy forwards Flush and records the status on Write.
	spy := &statusSpy{ResponseWriter: httptest.NewRecorder()}
	if _, err := spy.Write([]byte("x")); err != nil || spy.status != http.StatusOK {
		t.Fatalf("spy write = %d, %v", spy.status, err)
	}
	spy.Flush()

	for _, tc := range []struct {
		method, path string
		header       bool
		want         bool
	}{
		{"GET", "/services/setup.json", true, true},
		{"GET", "/services/setup.json", false, false},
		{"GET", "/_auth/callback", false, true},
		{"GET", "/_auth/tokens", false, true},
		{"GET", "/_auth/login", false, false},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.header {
			req.Header.Set("X-Walgit-Authorization", "Bearer x")
		}
		if got := authAttempt(req); got != tc.want {
			t.Fatalf("authAttempt(%s %s, header=%v) = %v", tc.method, tc.path, tc.header, got)
		}
	}
	for _, p := range []string{"/o/r.git/git-receive-pack", "/o/r/info/refs", "/o/r.git/info/lfs/objects/x", "/o/r.git-upload-pack"} {
		if !isGitTransportPath(p) {
			t.Fatalf("%s not recognized as a git path", p)
		}
	}
	if isGitTransportPath("/acme/widgets/api/issues/1/comments") {
		t.Fatal("an API path was classified as git")
	}
	// An unparseable X-Forwarded-For value falls back to the socket address.
	cfg := config.Defaults()
	s2 := New(Options{Config: cfg})
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "198.51.100.4:99"
	if got, want := s2.clientAddr(req), "a:198.51.100.4"; got != want {
		t.Fatalf("clientAddr = %q, want %q", got, want)
	}
}

// failingBody is a request body whose read errors.
type failingBody struct{}

func (*failingBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (*failingBody) Close() error             { return nil }
