// auth_usage.go — the asynchronous, debounced last-used recorder for static
// machine tokens (06_server_http.md §8.2.1).
//
// A successful static-token authentication is a hot path (a git fetch per
// client poll), so it never touches the object store. Instead it offers one
// observation to a fixed-size queue; a single drainer goroutine coalesces the
// queue into ONE conditional PUT of auth/token-usage.json. The bound is per
// token and per process:
//
//   - at most one observation per token per tokenUsageDebounce (5 minutes),
//   - at most tokenUsageQueueDepth observations waiting at once (a full queue
//     drops the observation; the next debounce window re-offers it),
//   - at most one writer goroutine (it exits when the queue is empty),
//   - at most 1 GET + 1 PUT per write, retried once on a CAS loss, under a
//     10 s budget; a failure is logged and NEVER fails the request.
//
// The record is a cache of observations, not authority: authentication reads
// none of it, and an absent or corrupt object only makes the Access page say
// "not recorded yet".
package server

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

const (
	// tokenUsageDebounce is the minimum interval between recorded observations
	// of one token. It is the write bound stated in the docs.
	tokenUsageDebounce = 5 * time.Minute
	// tokenUsageQueueDepth bounds pending observations. There is one identity
	// per configured token, so a healthy instance never approaches it; a
	// saturated queue drops observations instead of slowing authentication.
	tokenUsageQueueDepth = 64
	// tokenUsageWriteTimeout bounds one background read-modify-write.
	tokenUsageWriteTimeout = 10 * time.Second
	// tokenUsageUserAgentMax / tokenUsageSourceMax bound the stored text so a
	// hostile User-Agent cannot bloat the object.
	tokenUsageUserAgentMax = 160
	tokenUsageSourceMax    = 100
)

// tokenUsageWrite is one queued observation.
type tokenUsageWrite struct {
	key   string
	entry auth.TokenUsageEntry
}

// TokenUsageRecorder debounces and asynchronously persists static-token use.
// It is an injectable component of AuthService: nil means disabled, which is
// what a harness measuring another subsystem wants (no background store work
// in its window).
type TokenUsageRecorder struct {
	st  store.ObjectStore
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	last    map[string]time.Time // usage key → last observation offered
	running bool                 // one drainer is active
	queue   chan tokenUsageWrite
}

func NewTokenUsageRecorder(st store.ObjectStore, log *slog.Logger, now func() time.Time) *TokenUsageRecorder {
	if st == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &TokenUsageRecorder{
		st:    st,
		log:   log,
		now:   now,
		last:  map[string]time.Time{},
		queue: make(chan tokenUsageWrite, tokenUsageQueueDepth),
	}
}

// SetTokenUsage injects the last-used recorder (nil = disabled: authentication
// still works and records nothing). Composition enables it by default from the
// store; callers must set it before the service starts serving.
func (s *AuthService) SetTokenUsage(r *TokenUsageRecorder) {
	if s == nil {
		return
	}
	s.usage = r
}

// note offers one successful authentication. It is non-blocking and never
// returns an error: the request has already been authenticated and must not
// pay for bookkeeping.
func (r *TokenUsageRecorder) note(key string, entry auth.TokenUsageEntry, remoteAddr, userAgent string) {
	if r == nil || r.st == nil || key == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	if last, ok := r.last[key]; ok && now.Sub(last) < tokenUsageDebounce {
		r.mu.Unlock()
		return
	}
	r.last[key] = now
	entry.LastUsed = auth.NowRFC3339(now)
	entry.Source = sanitizeUsageText(remoteAddrHost(remoteAddr), tokenUsageSourceMax)
	entry.UserAgent = sanitizeUsageText(userAgent, tokenUsageUserAgentMax)
	select {
	case r.queue <- tokenUsageWrite{key: key, entry: entry}:
	default:
		// Bounded queue: drop rather than block the request path. The next
		// observation after the debounce window is offered again.
		r.log.Debug("static token usage: queue full, observation dropped")
		r.mu.Unlock()
		return
	}
	start := !r.running
	r.running = true
	r.mu.Unlock()
	if start {
		go r.drain()
	}
}

// drain writes queued observations serially, then exits when the queue is
// empty. The emptiness check and the running flag reset share the mutex with
// note's enqueue, so an observation can never be left behind without a worker.
func (r *TokenUsageRecorder) drain() {
	for {
		select {
		case w := <-r.queue:
			r.write(w)
			continue
		default:
		}
		r.mu.Lock()
		select {
		case w := <-r.queue:
			r.mu.Unlock()
			r.write(w)
			continue
		default:
			r.running = false
			r.mu.Unlock()
			return
		}
	}
}

// write merges one observation into the single CAS object. A lost CAS race is
// retried once on a fresh read; any other failure is logged and dropped (the
// next debounce window records the next use).
func (r *TokenUsageRecorder) write(w tokenUsageWrite) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenUsageWriteTimeout)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		doc, ver, err := r.load(ctx)
		if err != nil {
			r.log.Warn("static token usage: read failed", "key", auth.TokenUsageKey, "err", err)
			return
		}
		if doc == nil {
			doc = auth.NewTokenUsageDoc()
		}
		doc.Set(w.key, w.entry)
		raw := doc.Encode()
		opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}
		if ver != "" {
			opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		}
		if _, err := store.PutBytes(ctx, r.st, auth.TokenUsageKey, raw, opts); err != nil {
			if store.IsPreconditionFailed(err) {
				continue
			}
			r.log.Warn("static token usage: write failed", "key", auth.TokenUsageKey, "err", err)
			return
		}
		return
	}
	r.log.Warn("static token usage: CAS lost twice, observation dropped", "key", auth.TokenUsageKey)
}

// load returns the object and the version the write must be conditioned on
// ("" when absent).
func (r *TokenUsageRecorder) load(ctx context.Context) (*auth.TokenUsageDoc, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, r.st, auth.TokenUsageKey, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	doc, derr := auth.DecodeTokenUsage(raw)
	if derr != nil {
		// An unreadable object must not wedge the writer forever: replace it
		// with a fresh record (usage is advisory, never authority).
		return nil, meta.Version, nil
	}
	return doc, meta.Version, nil
}

// idle reports whether the drainer has exited (tests wait on it).
func (r *TokenUsageRecorder) idle() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.running
}

// remoteAddrHost keeps the address without the ephemeral port.
func remoteAddrHost(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(strings.TrimSpace(addr), "[]")
}

// sanitizeUsageText bounds stored text and removes control characters so an
// attacker-controlled User-Agent cannot break the table or smuggle newlines.
func sanitizeUsageText(s string, max int) string {
	if s == "" {
		return ""
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	t := strings.TrimSpace(string(out))
	if len(t) > max {
		t = t[:max]
		for len(t) > 0 && !utf8.ValidString(t) {
			t = t[:len(t)-1]
		}
	}
	return t
}
