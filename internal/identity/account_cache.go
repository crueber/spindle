// account_cache.go — the credential boundary's in-process cache of account
// state (docs/PLAN-ADMIN.md §3.7).
//
// # Why
//
// AuthService.accountGate runs for EVERY authenticated OIDC-derived
// credential — each signed-in page load, each API call, each git request. The
// authoritative read is one exact-key GET of users/<username>/user.json, which
// on an S3-backed instance is a fresh bucket round trip on the hot path (law 6:
// round trips are the cost model). Deactivation is a human-rate administrative
// act, not a per-request fact, so the gate memoizes it the way the block gate
// and access.json already do: bounded, in-process, and never the truth (law 4 —
// memory is a cache, the bucket is).
//
// # Shape
//
// A true LRU (map + container/list), capacity AccountStateCacheCapacity,
// stamped with a monotonic-age TTL of AccountStateCacheTTL. Negative entries
// (an account with no binding) are cached too: the callers of the gate are
// usernames that may not have a user.json yet, and a miss on those is exactly
// as expensive. An ERROR is never cached — a store fault must stay a 503 on
// every attempt (fail closed), never a fabricated "active".
//
// # Freshness
//
// This instance invalidates a username on its own SetUserDisabled, so a local
// deactivation/reactivation takes effect on the very next request. The TTL is
// the cross-instance bound: another instance may still serve a
// just-deactivated account for up to AccountStateCacheTTL. That direction is
// the only one with any slack — a stale DISABLED entry after a reactivation
// merely refuses sign-in until the entry expires or this instance writes the
// transition, and revoked credentials stay revoked regardless because the
// watermark is permanent.
//
// ### Concurrency
//
// Hazard: a cache lock held across a store call serializing the credential
// boundary. Avoidance: the mutex guards the map/list only and is released
// before UserState reads the store; a set after the read is last-writer-wins
// per key, and an interleaved invalidate simply costs one extra read. No lock
// is ever held across I/O, and no goroutine is started here.
package identity

import (
	"container/list"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// Cache bounds. The capacity covers a busy instance's signed-in population
// with headroom at ~100 bytes an entry; the TTL is the cross-instance
// staleness bound a deactivation may take to reach another instance.
const (
	AccountStateCacheCapacity = 4096
	AccountStateCacheTTL      = 10 * time.Second
)

// accountEntry is one cached account state with its insert time.
type accountEntry struct {
	name string
	st   auth.AccountState
	at   time.Time
}

// accountCache is the bounded LRU behind UserState.
type accountCache struct {
	mu  sync.Mutex
	cap int
	ttl time.Duration
	ll  *list.List // front = most recently used; values are *accountEntry
	by  map[string]*list.Element

	hits, misses, evicted uint64
}

func newAccountCache(cap int, ttl time.Duration) *accountCache {
	if cap <= 0 {
		cap = AccountStateCacheCapacity
	}
	if ttl <= 0 {
		ttl = AccountStateCacheTTL
	}
	return &accountCache{cap: cap, ttl: ttl, ll: list.New(), by: map[string]*list.Element{}}
}

// get returns a live entry and marks it most recently used. An expired entry
// is dropped (a miss) so the next read repopulates it.
func (c *accountCache) get(name string, now time.Time) (auth.AccountState, bool) {
	if c == nil {
		return auth.AccountState{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.by[name]
	if !ok {
		c.misses++
		return auth.AccountState{}, false
	}
	e := el.Value.(*accountEntry)
	if now.Sub(e.at) >= c.ttl {
		c.ll.Remove(el)
		delete(c.by, name)
		c.misses++
		return auth.AccountState{}, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return e.st, true
}

// set stores (or refreshes) one entry, evicting the least recently used
// entry when the cache is full.
func (c *accountCache) set(name string, st auth.AccountState, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.by[name]; ok {
		e := el.Value.(*accountEntry)
		e.st, e.at = st, now
		c.ll.MoveToFront(el)
		return
	}
	if c.ll.Len() >= c.cap {
		if back := c.ll.Back(); back != nil {
			c.ll.Remove(back)
			delete(c.by, back.Value.(*accountEntry).name)
			c.evicted++
		}
	}
	c.by[name] = c.ll.PushFront(&accountEntry{name: name, st: st, at: now})
}

// invalidate drops one account (this instance's own write).
func (c *accountCache) invalidate(name string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.by[name]; ok {
		c.ll.Remove(el)
		delete(c.by, name)
	}
}

// stats exposes the cache counters (the budget test and a future evidence
// harness): hits, misses, evictions, size.
func (c *accountCache) stats() (hits, misses, evicted uint64, size int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.evicted, c.ll.Len()
}
