package server

// auth_token_lifecycle_test.go — the static machine token lifecycle
// (06_server_http.md §8.2.1): expiry is a real 401 with the normal challenge,
// rotation overlaps two rows for one principal, and last use is recorded
// asynchronously under a per-token debounce bound without ever failing auth.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

type usageClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *usageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *usageClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newUsageClock() *usageClock {
	return &usageClock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
}

// usageStore counts writes and can fail them, or hold them at a gate, so the
// tests can assert the write bound and the async isolation without sleeping on
// a guess.
type usageStore struct {
	store.ObjectStore
	puts      atomic.Int64
	failPuts  atomic.Bool
	failGets  atomic.Bool
	gate      chan struct{} // non-nil → Put blocks until it is closed
	started   chan struct{} // non-nil → closed when the first Put enters
	startOnce sync.Once
	failFirst atomic.Bool
}

func (u *usageStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if u.failGets.Load() {
		return nil, store.NewRetryable(key, errors.New("read down"))
	}
	return u.ObjectStore.Get(ctx, key, opts)
}

func (u *usageStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	u.puts.Add(1)
	if u.started != nil {
		u.startOnce.Do(func() { close(u.started) })
	}
	if u.gate != nil {
		<-u.gate
	}
	if u.failPuts.Load() {
		return store.ObjectMeta{}, store.NewRetryable(key, errors.New("store down"))
	}
	if u.failFirst.Load() && u.failFirst.CompareAndSwap(true, false) {
		return store.ObjectMeta{}, store.NewPrecondition(key, "other")
	}
	return u.ObjectStore.Put(ctx, key, body, opts)
}

func waitUsageIdle(t *testing.T, r *TokenUsageRecorder) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !r.idle() {
		if time.Now().After(deadline) {
			t.Fatal("token usage writer did not drain")
		}
		time.Sleep(time.Millisecond)
	}
}

func usageRequest(t *testing.T, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/_auth/check", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.RemoteAddr = "203.0.113.7:44321"
	r.Header.Set("User-Agent", "git/2.55.0")
	return r
}

func authServerWithTokens(t *testing.T, clock *usageClock, tokens ...config.StaticToken) (*Server, *usageStore) {
	t.Helper()
	us := &usageStore{ObjectStore: store.NewMemory()}
	s, _ := newTestServer(t, func(o *Options) {
		o.Store = us
		o.Now = clock.Now
		o.Config.Server.Auth.Tokens = tokens
		o.Config.Server.Auth.AnonymousRead = false
	})
	return s, us
}

func TestStaticTokenExpiryIsAReal401(t *testing.T) {
	clock := newUsageClock()
	s, us := authServerWithTokens(t, clock,
		config.StaticToken{Principal: "ci-bot", Token: "expired-token",
			ExpiresAt: clock.Now().Add(-time.Minute).Format(time.RFC3339)},
		config.StaticToken{Principal: "ci-bot", Token: "live-token",
			ExpiresAt: clock.Now().Add(time.Hour).Format(time.RFC3339)},
		config.StaticToken{Principal: "ci-bot", Token: "never-token"},
	)

	rec := httptest.NewRecorder()
	s.authCheck(rec, usageRequest(t, "expired-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") || !strings.Contains(got, "Bearer") {
		t.Fatalf("expired token challenge = %q, want the normal Basic+Bearer challenge", got)
	}
	if body := rec.Body.String(); strings.Contains(strings.ToLower(body), "expire") {
		t.Fatalf("expiry leaked in-band: %q", body)
	}
	// A rejected credential records no use (the recorder fires only after a
	// successful resolution).
	waitUsageIdle(t, s.authSvc.usage)
	if got := us.puts.Load(); got != 0 {
		t.Fatalf("a 401 recorded token usage: %d writes, want 0", got)
	}

	// Live and never-expiring rows authenticate.
	for _, tok := range []string{"live-token", "never-token"} {
		rec := httptest.NewRecorder()
		s.authCheck(rec, usageRequest(t, tok))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", tok, rec.Code)
		}
		if got := rec.Header().Get("X-Walgit-Principal"); got != "ci-bot" {
			t.Fatalf("%s principal = %q", tok, got)
		}
	}

	// The boundary is inclusive: at expires_at the token is already dead.
	at := clock.Now().Add(2 * time.Hour)
	svc := NewAuthService(&config.Auth{Tokens: []config.StaticToken{
		{Principal: "ci-bot", Token: "edge", ExpiresAt: at.Format(time.RFC3339)},
	}}, clock.Now)
	if _, ok := svc.StaticPrincipal("edge"); !ok {
		t.Fatal("token rejected before its expiry")
	}
	clock.Advance(2 * time.Hour)
	if _, ok := svc.StaticPrincipal("edge"); ok {
		t.Fatal("token valid at its expiry instant")
	}
}

// TestStaticTokenRotationOverlap: two rows for one principal, the old one with
// an expiry. Both authenticate until the expiry passes; then only the new one
// does (the documented add → roll → cut over → remove procedure).
func TestStaticTokenRotationOverlap(t *testing.T) {
	clock := newUsageClock()
	cutover := clock.Now().Add(24 * time.Hour)
	s, _ := authServerWithTokens(t, clock,
		config.StaticToken{Principal: "ci-bot", Name: "ci-old", Token: "old-token", Write: true,
			ExpiresAt: cutover.Format(time.RFC3339)},
		config.StaticToken{Principal: "ci-bot", Name: "ci-new", Token: "new-token", Write: true,
			ExpiresAt: cutover.Add(30 * 24 * time.Hour).Format(time.RFC3339)},
	)
	for _, tok := range []string{"old-token", "new-token"} {
		rec := httptest.NewRecorder()
		s.authCheck(rec, usageRequest(t, tok))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("overlap: %s = %d, want both rows live", tok, rec.Code)
		}
	}
	clock.Advance(25 * time.Hour)
	rec := httptest.NewRecorder()
	s.authCheck(rec, usageRequest(t, "old-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token after cutover = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.authCheck(rec, usageRequest(t, "new-token"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("new token after cutover = %d, want 204", rec.Code)
	}
}

func TestStaticTokenUsageDebouncedOneWrite(t *testing.T) {
	clock := newUsageClock()
	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ci-bot", Name: "ci-bot", Token: "tok"}}
	svc := NewAuthService(&cfg.Server.Auth, clock.Now)
	us := &usageStore{ObjectStore: store.NewMemory()}
	svc.SetTokenUsage(NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now))

	for i := 0; i < 25; i++ {
		p, aerr := svc.Authenticate(usageRequest(t, "tok"), cfg)
		if aerr != nil || p.Name != "ci-bot" {
			t.Fatalf("request %d = %+v, %v", i, p, aerr)
		}
	}
	waitUsageIdle(t, svc.usage)
	if got := us.puts.Load(); got != 1 {
		t.Fatalf("N requests → %d writes, want 1 (5-minute debounce)", got)
	}
	doc, err := auth.LoadTokenUsage(context.Background(), us.ObjectStore)
	if err != nil || doc == nil {
		t.Fatalf("usage doc = %+v, %v", doc, err)
	}
	e, ok := doc.Entry("ci-bot")
	if !ok {
		t.Fatal("named token has no usage entry")
	}
	if e.Principal != "ci-bot" || e.Source != "203.0.113.7" || e.UserAgent != "git/2.55.0" {
		t.Fatalf("usage entry = %+v", e)
	}
	if e.LastUsed != "2026-11-23T12:00:00Z" {
		t.Fatalf("last_used = %q", e.LastUsed)
	}

	// After the debounce window the next use is recorded (second write).
	clock.Advance(tokenUsageDebounce)
	if _, aerr := svc.Authenticate(usageRequest(t, "tok"), cfg); aerr != nil {
		t.Fatal(aerr)
	}
	waitUsageIdle(t, svc.usage)
	if got := us.puts.Load(); got != 2 {
		t.Fatalf("after debounce → %d writes, want 2", got)
	}
	doc, _ = auth.LoadTokenUsage(context.Background(), us.ObjectStore)
	if e, _ := doc.Entry("ci-bot"); e.Count != 2 {
		t.Fatalf("usage count = %d, want 2 update windows", e.Count)
	}
}

// TestStaticTokenUsageFailureNeverFailsAuth: the writer's store is down; every
// request still authenticates and the failure is only logged.
func TestStaticTokenUsageFailureNeverFailsAuth(t *testing.T) {
	clock := newUsageClock()
	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ci-bot", Token: "tok"}}
	svc := NewAuthService(&cfg.Server.Auth, clock.Now)
	us := &usageStore{ObjectStore: store.NewMemory()}
	us.failPuts.Store(true)
	svc.SetTokenUsage(NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now))

	for i := 0; i < 3; i++ {
		p, aerr := svc.Authenticate(usageRequest(t, "tok"), cfg)
		if aerr != nil || p.Name != "ci-bot" {
			t.Fatalf("auth failed while usage write failed: %+v, %v", p, aerr)
		}
	}
	waitUsageIdle(t, svc.usage)
	if us.puts.Load() == 0 {
		t.Fatal("the recorder never attempted a write")
	}
}

// TestStaticTokenUsageAnonymousHashKey: an unnamed token is keyed by a hash
// prefix, and the recorded entry never carries the secret.
func TestStaticTokenUsageAnonymousHashKey(t *testing.T) {
	clock := newUsageClock()
	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ci-bot", Token: "super-secret-value"}}
	svc := NewAuthService(&cfg.Server.Auth, clock.Now)
	us := &usageStore{ObjectStore: store.NewMemory()}
	svc.SetTokenUsage(NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now))
	if _, aerr := svc.Authenticate(usageRequest(t, "super-secret-value"), cfg); aerr != nil {
		t.Fatal(aerr)
	}
	waitUsageIdle(t, svc.usage)
	doc, err := auth.LoadTokenUsage(context.Background(), us.ObjectStore)
	if err != nil || doc == nil {
		t.Fatalf("doc = %+v, %v", doc, err)
	}
	key := auth.TokenUsageIdentity("", "super-secret-value")
	if _, ok := doc.Entry(key); !ok {
		t.Fatalf("entries = %+v, want key %q", doc.Tokens, key)
	}
	raw := doc.Encode()
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatal("the usage object contains the token secret")
	}
}

// TestStaticTokenUsageCASRetryAndCorruptRecovery: a lost CAS race retries on a
// fresh read, and an unreadable object is replaced instead of wedging the
// writer.
func TestStaticTokenUsageCASRetryAndCorruptRecovery(t *testing.T) {
	clock := newUsageClock()
	us := &usageStore{ObjectStore: store.NewMemory()}
	us.failFirst.Store(true)
	rec := NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now)
	rec.note("ci", auth.TokenUsageEntry{Principal: "ci"}, "203.0.113.9:1", "git/1")
	waitUsageIdle(t, rec)
	if got := us.puts.Load(); got != 2 {
		t.Fatalf("CAS loss → %d puts, want 2 (one retry)", got)
	}

	// Corrupt object: the next write replaces it with a valid record.
	if _, err := store.PutBytes(context.Background(), us.ObjectStore, auth.TokenUsageKey,
		[]byte("{not json"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	rec2 := NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now)
	rec2.note("ci", auth.TokenUsageEntry{Principal: "ci"}, "203.0.113.9:1", "git/1")
	waitUsageIdle(t, rec2)
	doc, err := auth.LoadTokenUsage(context.Background(), us.ObjectStore)
	if err != nil || doc == nil {
		t.Fatalf("corrupt object not replaced: %+v, %v", doc, err)
	}
	if _, ok := doc.Entry("ci"); !ok {
		t.Fatalf("replacement doc = %+v", doc.Tokens)
	}
}

// TestStaticTokenUsageQueueIsBounded: with the writer blocked, the queue fills
// and further observations are dropped (never blocking the request), and the
// drain completes after release.
func TestStaticTokenUsageQueueIsBounded(t *testing.T) {
	clock := newUsageClock()
	release := make(chan struct{})
	us := &usageStore{ObjectStore: store.NewMemory(), gate: release}
	rec := NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	for i := 0; i < 200; i++ {
		rec.note("tok-"+strconv.Itoa(i), auth.TokenUsageEntry{Principal: "ci"}, "127.0.0.1:1", "git/1")
	}
	close(release)
	waitUsageIdle(t, rec)
	doc, err := auth.LoadTokenUsage(context.Background(), us.ObjectStore)
	if err != nil || doc == nil {
		t.Fatalf("doc = %+v, %v", doc, err)
	}
	if len(doc.Tokens) == 0 || len(doc.Tokens) > tokenUsageQueueDepth+1 {
		t.Fatalf("recorded %d entries, want the bounded window (1..%d)", len(doc.Tokens), tokenUsageQueueDepth+1)
	}
}

// TestUsageSourceAndUserAgentAreSanitized: the stored source has no port, and
// control characters/oversized user agents are cut before they reach the page.
func TestUsageSourceAndUserAgentAreSanitized(t *testing.T) {
	if got := remoteAddrHost("203.0.113.7:1234"); got != "203.0.113.7" {
		t.Fatalf("source = %q", got)
	}
	if got := remoteAddrHost("[::1]:80"); got != "::1" {
		t.Fatalf("v6 source = %q", got)
	}
	if got := remoteAddrHost("no-port"); got != "no-port" {
		t.Fatalf("bare source = %q", got)
	}
	if got := remoteAddrHost(""); got != "" {
		t.Fatalf("empty source = %q", got)
	}
	ua := strings.Repeat("a", 400) + "\r\nattack"
	got := sanitizeUsageText(ua, tokenUsageUserAgentMax)
	if len(got) > tokenUsageUserAgentMax || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("sanitized UA = %q (%d bytes)", got, len(got))
	}
	if got := sanitizeUsageText("  ", 10); got != "" {
		t.Fatalf("blank text = %q", got)
	}
	// Truncation never leaves an invalid UTF-8 tail.
	cut := sanitizeUsageText(strings.Repeat("é", 200), tokenUsageUserAgentMax)
	if !utf8.ValidString(cut) {
		t.Fatalf("invalid UTF-8 after truncation: %q", cut)
	}
}

// TestTokenUsageRecorderNilAndReadFailure covers the defensive branches: an
// unwired recorder, an unwired service, a nil clock/logger, and a store whose
// read fails (logged, no write, no panic).
func TestTokenUsageRecorderNilAndReadFailure(t *testing.T) {
	var nilRec *TokenUsageRecorder
	nilRec.note("x", auth.TokenUsageEntry{}, "", "")
	if !nilRec.idle() {
		t.Fatal("nil recorder is not idle")
	}
	(&AuthService{}).SetTokenUsage(nil)
	var nilSvc *AuthService
	nilSvc.SetTokenUsage(NewTokenUsageRecorder(store.NewMemory(), nil, nil))

	clock := newUsageClock()
	rec := NewTokenUsageRecorder(store.NewMemory(), nil, nil) // default logger + clock
	if !rec.idle() {
		t.Fatal("fresh recorder is not idle")
	}
	us := &usageStore{ObjectStore: store.NewMemory()}
	us.failGets.Store(true)
	rec = NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now)
	rec.note("ci", auth.TokenUsageEntry{Principal: "ci"}, "127.0.0.1:1", "git/1")
	waitUsageIdle(t, rec)
	if us.puts.Load() != 0 {
		t.Fatalf("a failed read still wrote: %d", us.puts.Load())
	}
	if _, err := auth.LoadTokenUsage(context.Background(), us); err == nil {
		t.Fatal("load error not surfaced")
	}
}

// TestStaticTokenUsageNeverBlocksAuth: while the async writer is stuck inside a
// store PUT, a successful authentication still returns. The observation is
// offered to a queue (non-blocking) after the credential resolves.
func TestStaticTokenUsageNeverBlocksAuth(t *testing.T) {
	clock := newUsageClock()
	cfg := config.Defaults()
	cfg.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "ci-bot", Token: "tok-a"},
		{Principal: "ci-bot", Token: "tok-b"},
	}
	svc := NewAuthService(&cfg.Server.Auth, clock.Now)
	gate := make(chan struct{})
	started := make(chan struct{})
	us := &usageStore{ObjectStore: store.NewMemory(), gate: gate, started: started}
	svc.SetTokenUsage(NewTokenUsageRecorder(us, slog.New(slog.DiscardHandler), clock.Now))
	defer close(gate)

	if _, aerr := svc.Authenticate(usageRequest(t, "tok-a"), cfg); aerr != nil {
		t.Fatal(aerr)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the usage writer never reached the store")
	}
	// The writer is now blocked in Put; authentication must not wait for it.
	done := make(chan *auth.AuthError, 1)
	go func() {
		_, aerr := svc.Authenticate(usageRequest(t, "tok-b"), cfg)
		done <- aerr
	}()
	select {
	case aerr := <-done:
		if aerr != nil {
			t.Fatal(aerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authentication blocked on the usage writer")
	}
}
