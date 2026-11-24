package moderation

// spam.go — PLAN-V2 §M.7's advisory spam signal (docs/features/16_moderation.md
// §13).
//
// The scorer keeps an in-memory ring of the last SpamRingLimit bodies each
// principal wrote (one SHA-256 per body, never the body itself), and when the
// SAME body appears SpamRepeats times inside SpamWindow it pre-files exactly
// ONE report through the M3 filing tail. It is ADVISORY: it never blocks a
// write, never changes a role floor, never hides or locks anything, and a
// machine verdict never removes content (PLAN-V2 §M.1, L9's spirit). The only
// observable effects are the report Create and the
// spindle_spam_flag_total{repo} counter the server increments on the returned
// flag.
//
// Anonymous callers are never scored (there is no principal to score); a fork
// run never reaches this path with a non-anonymous collaboration principal, so
// the rule holds for it too.
//
// ### Concurrency
//
// Hazard: two concurrent writes by one principal tripping the threshold at the
// same instant and filing twice. Avoidance: the ring append, the threshold
// check and the cooldown stamp all happen under the scorer's one mutex; the
// report write happens AFTER the unlock (never a store call under a lock), and
// the second racer sees the stamped cooldown and files nothing. A duplicate
// open report (the M3 reporter+target rule) is a second, store-side guard:
// ErrConflict is swallowed, never surfaced.
//
// Hazard: unbounded memory from principals. Avoidance: the per-principal ring
// is a fixed 256 entries (drop-oldest), and the principal map is bounded at
// SpamMaxPrincipals with deterministic eviction (oldest activity first, ties
// by name) — the rate limiter's rule, for the same reason.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SpamWindow is the repeat window (PLAN-V2 §M.7: 24h).
	SpamWindow = 24 * time.Hour
	// SpamRepeats is the threshold: the same body this many times in the
	// window pre-files one report (PLAN-V2 §M.7: ≥3).
	SpamRepeats = 3
	// SpamRingLimit bounds one principal's recent-body ring (PLAN-V2 §M.7).
	SpamRingLimit = 256
	// SpamMaxPrincipals bounds the scored-principal map.
	SpamMaxPrincipals = 10000
	// SpamMinBody is the shortest body the scorer considers. A body below it
	// carries no spam evidence — "+1", "thanks" and "LGTM" are what a busy
	// reviewer writes three times a day, and flagging them would fill the
	// moderation queue with noise. (Decided in the feature doc: a scorer that
	// cries wolf is worse than no scorer.)
	SpamMinBody = 40
	// SpamReporter is the reserved reporter name on a pre-filed report. It is
	// not a principal: it never authenticates, and the M3 duplicate rule keys
	// on it, so one user cannot have two open spam reports per repo.
	SpamReporter = "spam-signal"
)

// spamEntry is one hashed body observation.
type spamEntry struct {
	hash [32]byte
	at   time.Time
}

// spamRing is one principal's recent bodies plus the per-hash cooldowns (a
// body that already filed cannot file again inside the window).
type spamRing struct {
	entries  []spamEntry
	filed    map[[32]byte]time.Time
	lastSeen time.Time
}

// scorer is the in-process state; Service owns one.
type scorer struct {
	mu   sync.Mutex
	ring map[string]*spamRing
}

// NoteBody observes one collaboration write and pre-files a report when the
// same body has repeated SpamRepeats times inside SpamWindow. It returns true
// exactly when this call created the report — the counter's increment.
//
// The call is synchronous and best-effort: a store failure is logged and
// returned as false, never propagated to the write it was observing (an
// advisory signal cannot fail a user's comment).
func (s *Service) NoteBody(ctx context.Context, owner, repo, principal, body string) bool {
	principal = normPrincipal(principal)
	body = strings.TrimSpace(body)
	if principal == "" || len(body) < SpamMinBody || len(body) > 2000 {
		return false
	}
	hash := sha256.Sum256([]byte(body))
	now := s.nowUTC()

	s.spam.mu.Lock()
	r := s.spam.ring[principal]
	if r == nil {
		r = &spamRing{filed: map[[32]byte]time.Time{}}
		s.spam.ring[principal] = r
		s.evictScoredPrincipalsLocked(now)
	}
	r.lastSeen = now
	r.entries = append(r.entries, spamEntry{hash: hash, at: now})
	if len(r.entries) > SpamRingLimit {
		r.entries = append([]spamEntry(nil), r.entries[len(r.entries)-SpamRingLimit:]...)
	}
	repeats := 0
	for _, e := range r.entries {
		if e.hash == hash && now.Sub(e.at) <= SpamWindow {
			repeats++
		}
	}
	tripped := false
	if repeats >= SpamRepeats {
		if last, ok := r.filed[hash]; !ok || now.Sub(last) > SpamWindow {
			// One report per burst: the cooldown AND the cleared occurrences
			// mean the same body must re-accumulate a fresh window before it
			// can file again.
			r.filed[hash] = now
			r.entries = dropHash(r.entries, hash)
			tripped = true
		}
	}
	s.spam.mu.Unlock()

	if !tripped {
		return false
	}
	note := fmt.Sprintf("repeated identical body %d times in %s (advisory spam signal)", repeats, SpamWindow)
	if _, err := s.appendReport(ctx, owner, repo, SpamReporter, ReportKindUser, "user:"+principal, "spam", note); err != nil {
		if errors.Is(err, ErrConflict) {
			return false // an open spam report already exists for this user+repo
		}
		s.warn(ctx, "moderation: spam report filing failed",
			"repo", owner+"/"+repo, "principal", principal, "err", err)
		return false
	}
	return true
}

// dropHash removes every observation of one body (a filed burst is consumed).
func dropHash(entries []spamEntry, hash [32]byte) []spamEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.hash != hash {
			out = append(out, e)
		}
	}
	return out
}

// evictScoredPrincipalsLocked keeps the principal map bounded: the oldest
// lastSeen goes first, ties by name (deterministic — the limiter's rule).
// Callers hold s.spam.mu.
func (s *Service) evictScoredPrincipalsLocked(now time.Time) {
	if len(s.spam.ring) <= SpamMaxPrincipals {
		return
	}
	names := make([]string, 0, len(s.spam.ring))
	for name := range s.spam.ring {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := s.spam.ring[names[i]].lastSeen, s.spam.ring[names[j]].lastSeen
		if a.Equal(b) {
			return names[i] < names[j]
		}
		return a.Before(b)
	})
	for _, name := range names[:len(s.spam.ring)-SpamMaxPrincipals] {
		delete(s.spam.ring, name)
	}
}

// ScoredPrincipals reports how many principals the scorer is tracking (tests
// assert the bound; nothing in production reads it).
func (s *Service) ScoredPrincipals() int {
	s.spam.mu.Lock()
	defer s.spam.mu.Unlock()
	return len(s.spam.ring)
}

// BodyObservations reports one principal's live ring size (tests).
func (s *Service) BodyObservations(principal string) int {
	s.spam.mu.Lock()
	defer s.spam.mu.Unlock()
	r := s.spam.ring[normPrincipal(principal)]
	if r == nil {
		return 0
	}
	return len(r.entries)
}
