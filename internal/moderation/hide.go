// hide.go — PLAN-V2 §M.2 (milestone M2): the repo moderation document, the
// hide/unhide verbs for conversation content, and the conversation lock.
//
//	repos/<o>/<r>/meta/moderation.json   CAS'd repo moderation state
//
// The document is the ONE writer for "what content is not shown" and "this
// conversation accepts no new content" (docs/features/16_moderation.md §4).
// Hiding never deletes: the event/review object stays exactly as written
// (it is evidence), and the author's edit history is untouched — the
// projection consults this document on every read.
//
// Lock composition: the existing conversation locks key on thread state
// (internal/issues threadLocked / internal/review threadLocked: closed,
// merged). This lock is ADDITIVE and independent — a closed-and-locked
// thread is both, and unlocking a moderator lock does not reopen a closed
// thread. Neither writer touches the other's object (this document is never
// a thread header), so the P3 header CAS has exactly one writer.
//
// ### Concurrency
//
// Hazard: two moderators hiding different comments at the same time losing
// an entry. Avoidance: one whole-document CAS loop (≤5 attempts → 409), the
// 13_concurrency.md §3 discipline: CAS is the lock, there is no lock object,
// no .lock sidecar and no in-process mutex across a store call. The read
// cache is the same version-stamped conditional-GET pattern as the block
// list (a cached parse, never a freshness shortcut).
package moderation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// docVersion is the on-bucket document version this package knows. A reader
// refuses a newer one (14 §14.12 version rule).
const docVersion = 1

// Hidden-reason set (PLAN-V2 §M.2). Unknown reasons are 400.
var HiddenReasons = map[string]bool{
	"spam": true, "abusive": true, "off-topic": true, "other": true,
}

// DefaultHiddenReason is the reason the page's one-click control sends when
// the moderator does not pick one.
const DefaultHiddenReason = "other"

// Target kinds (the plan's one-form-per-content-family grammar).
const (
	TargetThread       = "thread"
	TargetReview       = "review"
	TargetReviewThread = "review_thread"
)

// Hidden is one entry of Doc.Hidden: who hid the target, when, and why.
// It is evidence and is never rewritten in place; unhide removes the entry.
type Hidden struct {
	By     string `json:"by"`
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// Lock modes (docs/features/16_moderation.md §10.1). LockModeAll is today's
// behavior — the conversation accepts no new content from anybody, host
// admins included. LockModeCollaborators lets write+ (the effective repo
// ladder, where a host admin resolves to admin) keep commenting while read
// and anonymous viewers are refused. The empty string is the legacy spelling
// of LockModeAll (a document written before modes existed keeps its old
// behavior, law 5).
const (
	LockModeAll           = "all"
	LockModeCollaborators = "collaborators"
)

// LockModes is the closed set the verbs accept.
var LockModes = map[string]bool{LockModeAll: true, LockModeCollaborators: true}

// normalizeLockMode maps the legacy empty spelling onto LockModeAll and
// rejects anything outside the closed set.
func normalizeLockMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return LockModeAll, nil
	}
	if !LockModes[m] {
		return "", fmt.Errorf("%w: lock mode must be all|collaborators", ErrInvalid)
	}
	return m, nil
}

// LockState is one conversation lock. The document also carries the plan's
// flat lock fields (the repo-wide envelope); a per-conversation lock uses
// this keyed form because the document is repo-scoped. Mode is additive
// (`omitempty`): a document without it reads as LockModeAll (law 5).
type LockState struct {
	By     string `json:"by"`
	At     string `json:"at"`
	Reason string `json:"reason"`
	Mode   string `json:"mode,omitempty"`
}

// Doc is repos/<o>/<r>/meta/moderation.json.
//
// Version is a monotonically increasing revision used as the API's
// concurrency token (the plan's `{"version":1}`); the store's own opaque
// version is the CAS token underneath. An absent document reads as
// `{version:0,locked:false,hidden:{},locks:{}}` (the access.json synthesis
// pattern) — never a 404.
type Doc struct {
	Version    int    `json:"version"`
	Locked     bool   `json:"locked"`
	LockedBy   string `json:"locked_by"`
	LockedAt   string `json:"locked_at"`
	LockReason string `json:"lock_reason"`
	// LockMode is the flat (repo-wide) envelope's lock mode; "" reads as
	// LockModeAll. The keyed entries carry their own mode.
	LockMode string `json:"lock_mode,omitempty"`
	// Locks holds per-conversation locks keyed by ThreadLockKey(num). The
	// flat fields above are the plan's repo-wide envelope; ThreadLock is
	// true when EITHER arm is set.
	Locks     map[string]LockState `json:"locks"`
	Hidden    map[string]Hidden    `json:"hidden"`
	UpdatedAt string               `json:"updated_at"`
}

// ModerationKey is the repo's moderation document key.
func ModerationKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/moderation.json"
}

// ThreadLockKey is the per-conversation lock key in Doc.Locks.
func ThreadLockKey(num int) string { return fmt.Sprintf("thread:%06x", num) }

// ThreadTarget is the hide-target key of one issue/PR conversation event
// body (the shared issues family covers both kinds).
func ThreadTarget(num, seq int) string {
	return fmt.Sprintf("thread:%06x:event:%012x", num, seq)
}

// ReviewTarget is the hide-target key of one review body.
func ReviewTarget(num, seq int) string {
	return fmt.Sprintf("review:%06x:%012x", num, seq)
}

// ReviewThreadTarget is the hide-target key of one review-thread comment.
func ReviewThreadTarget(num int, tid string, seq int) string {
	return fmt.Sprintf("review_thread:%06x:%s:%012x", num, tid, seq)
}

// Target is a parsed hide target.
type Target struct {
	Kind string
	Num  int
	Seq  int
	TID  string
}

// ParseTarget validates the plan's target grammar (one form per content
// family, all lowercase, colon-separated, minimum widths growing naturally).
func ParseTarget(s string) (Target, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 3 || len(parts) > 5 {
		return Target{}, fmt.Errorf("%w: unknown target %q", ErrInvalid, s)
	}
	parseHex := func(field, v string, min int) (int, error) {
		if len(v) < min || len(v) > 16 {
			return 0, fmt.Errorf("%w: bad %s in target %q", ErrInvalid, field, s)
		}
		for i := 0; i < len(v); i++ {
			c := v[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return 0, fmt.Errorf("%w: bad %s in target %q", ErrInvalid, field, s)
			}
		}
		n, err := strconv.ParseInt(v, 16, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%w: bad %s in target %q", ErrInvalid, field, s)
		}
		return int(n), nil
	}
	switch parts[0] {
	case TargetThread:
		if len(parts) != 4 || parts[2] != "event" {
			return Target{}, fmt.Errorf("%w: unknown target %q", ErrInvalid, s)
		}
		num, err := parseHex("num", parts[1], 6)
		if err != nil || num < 1 {
			return Target{}, fmt.Errorf("%w: bad num in target %q", ErrInvalid, s)
		}
		seq, err := parseHex("seq", parts[3], 12)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: TargetThread, Num: num, Seq: seq}, nil
	case TargetReview:
		if len(parts) != 3 {
			return Target{}, fmt.Errorf("%w: unknown target %q", ErrInvalid, s)
		}
		num, err := parseHex("num", parts[1], 6)
		if err != nil || num < 1 {
			return Target{}, fmt.Errorf("%w: bad num in target %q", ErrInvalid, s)
		}
		seq, err := parseHex("seq", parts[2], 12)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: TargetReview, Num: num, Seq: seq}, nil
	case TargetReviewThread:
		if len(parts) != 4 {
			return Target{}, fmt.Errorf("%w: unknown target %q", ErrInvalid, s)
		}
		num, err := parseHex("num", parts[1], 6)
		if err != nil || num < 1 {
			return Target{}, fmt.Errorf("%w: bad num in target %q", ErrInvalid, s)
		}
		tid := parts[2]
		if len(tid) != 8 {
			return Target{}, fmt.Errorf("%w: bad thread id in target %q", ErrInvalid, s)
		}
		seq, err := parseHex("seq", parts[3], 12)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: TargetReviewThread, Num: num, Seq: seq, TID: tid}, nil
	}
	return Target{}, fmt.Errorf("%w: unknown target kind %q", ErrInvalid, parts[0])
}

// TargetProbe checks that a hide target names an object that exists (the
// plan's 400 "target that does not exist"). Composition implements it over
// each content family's exported key helper — the moderation package never
// learns another feature's key layout. Nil skips the probe (grammar only;
// tests that do not own a store use it).
type TargetProbe interface {
	Exists(ctx context.Context, owner, repo string, t Target) (bool, error)
}

// threadLockOf reports the conversation lock from one doc: the repo-wide
// envelope or the per-conversation entry, plus the lock's mode (normalized;
// an absent mode on a stored entry reads as LockModeAll).
func threadLockOf(doc Doc, num int) (locked bool, mode, reason string) {
	if e, ok := doc.Locks[ThreadLockKey(num)]; ok && e.By != "" {
		m, err := normalizeLockMode(e.Mode)
		if err != nil {
			m = LockModeAll
		}
		return true, m, e.Reason
	}
	if doc.Locked {
		m, err := normalizeLockMode(doc.LockMode)
		if err != nil {
			m = LockModeAll
		}
		return true, m, doc.LockReason
	}
	return false, "", ""
}

// Snapshot visits every hidden target in the repo and reports the lock of
// conversation num, in ONE conditional GET — the read every projection
// consults (the consumer-owned gate returns primitives; each projection
// builds its own wire type). fn may be nil when only the lock is wanted.
// Errors fail closed at the caller (a read verb refuses rather than serving
// content it cannot prove is visible).
func (s *Service) Snapshot(ctx context.Context, owner, repo string, num int, fn func(target, by, at, reason string)) (locked bool, lockMode, lockReason string, revision int, err error) {
	doc, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return false, "", "", 0, err
	}
	if fn != nil {
		keys := make([]string, 0, len(doc.Hidden))
		for k := range doc.Hidden {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h := doc.Hidden[k]
			fn(k, h.By, h.At, h.Reason)
		}
	}
	locked, mode, reason := threadLockOf(doc, num)
	return locked, mode, reason, doc.Version, nil
}

// State returns the whole document for the read API (the caller has already
// applied its read gate). Absent reads as the zero document, never an error.
func (s *Service) State(ctx context.Context, owner, repo string) (Doc, error) {
	doc, _, err := s.loadDoc(ctx, owner, repo)
	return doc, err
}

// DocFor is the API GET: read-gated (the page gate mirrors it through
// identity's require_read hook) and never 404s for an absent document.
func (s *Service) DocFor(ctx context.Context, owner, repo string, p auth.Principal) (Doc, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return Doc{}, err
	}
	return s.State(ctx, owner, repo)
}

// Hide marks one target hidden. changed=false means it was already hidden:
// idempotent, no write and no audit record. Floor: triage.
func (s *Service) Hide(ctx context.Context, owner, repo string, actor auth.Principal, target, reason string) (int, bool, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleTriage); err != nil {
		return 0, false, err
	}
	t, err := ParseTarget(target)
	if err != nil {
		return 0, false, err
	}
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		reason = DefaultHiddenReason
	}
	if !HiddenReasons[reason] {
		return 0, false, fmt.Errorf("%w: unknown reason %q", ErrInvalid, reason)
	}
	if s.Targets != nil {
		ok, perr := s.Targets.Exists(ctx, owner, repo, t)
		if perr != nil {
			return 0, false, perr
		}
		if !ok {
			return 0, false, fmt.Errorf("%w: target %s does not exist", ErrInvalid, target)
		}
	}
	by := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	doc, changed, err := s.mutateDoc(ctx, owner, repo, func(d *Doc) (bool, error) {
		if _, ok := d.Hidden[target]; ok {
			return false, nil
		}
		d.Hidden[target] = Hidden{By: by, At: now, Reason: reason}
		return true, nil
	})
	if err != nil {
		return 0, false, err
	}
	if !changed {
		return doc.Version, false, nil
	}
	s.audit(ctx, by, ActionHide, owner, repo, reason, "target="+target+";prev=visible")
	s.stream(ctx, owner, repo, StreamEvent{Num: t.Num, Seq: t.Seq, TID: t.TID, Action: ActionHide, By: by, At: now})
	return doc.Version, true, nil
}

// Unhide removes a hidden target. changed=false means it was not hidden
// (no write, no audit). Floor: triage.
func (s *Service) Unhide(ctx context.Context, owner, repo string, actor auth.Principal, target string) (int, bool, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleTriage); err != nil {
		return 0, false, err
	}
	t, err := ParseTarget(target)
	if err != nil {
		return 0, false, err
	}
	by := normPrincipal(actor.Name)
	doc, changed, err := s.mutateDoc(ctx, owner, repo, func(d *Doc) (bool, error) {
		if _, ok := d.Hidden[target]; !ok {
			return false, nil
		}
		delete(d.Hidden, target)
		return true, nil
	})
	if err != nil {
		return 0, false, err
	}
	if !changed {
		return doc.Version, false, nil
	}
	s.audit(ctx, by, ActionUnhide, owner, repo, "", "target="+target+";prev=hidden")
	s.stream(ctx, owner, repo, StreamEvent{Num: t.Num, Seq: t.Seq, TID: t.TID, Action: ActionUnhide, By: by, At: s.nowUTC().Format(dateTimeFmt)})
	return doc.Version, true, nil
}

// LockThread locks one conversation in mode ("" → LockModeAll). changed=false
// means it was already locked in that mode (idempotent: the reason and the
// moderator of record are not rewritten). Re-locking an already-locked thread
// in the OTHER mode switches the mode and keeps the first moderator's words —
// the way a moderator widens or narrows an existing lock without dropping it.
// Floor: maintain.
func (s *Service) LockThread(ctx context.Context, owner, repo string, num int, actor auth.Principal, reason, mode string) (int, bool, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleMaintain); err != nil {
		return 0, false, err
	}
	if num < 1 {
		return 0, false, fmt.Errorf("%w: unknown conversation", ErrInvalid)
	}
	mode, merr := normalizeLockMode(mode)
	if merr != nil {
		return 0, false, merr
	}
	reason = strings.TrimSpace(reason)
	by := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	prevMode := ""
	modeChanged := false
	doc, changed, err := s.mutateDoc(ctx, owner, repo, func(d *Doc) (bool, error) {
		key := ThreadLockKey(num)
		if e, ok := d.Locks[key]; ok && e.By != "" {
			cur, cerr := normalizeLockMode(e.Mode)
			if cerr != nil {
				cur = LockModeAll
			}
			if cur == mode {
				return false, nil
			}
			prevMode, modeChanged = cur, true
			e.Mode = mode
			d.Locks[key] = e // By/At/Reason stay: the evidence is not rewritten
			return true, nil
		}
		d.Locks[key] = LockState{By: by, At: now, Reason: reason, Mode: mode}
		return true, nil
	})
	if err != nil {
		return 0, false, err
	}
	if !changed {
		return doc.Version, false, nil
	}
	if modeChanged {
		s.audit(ctx, by, ActionLock, owner, repo, doc.Locks[ThreadLockKey(num)].Reason,
			fmt.Sprintf("thread=%06x;mode=%s;prev_mode=%s;prev=locked", num, mode, prevMode))
		return doc.Version, true, nil
	}
	s.audit(ctx, by, ActionLock, owner, repo, reason, fmt.Sprintf("thread=%06x;mode=%s;prev=unlocked", num, mode))
	s.stream(ctx, owner, repo, StreamEvent{Num: num, Action: ActionLock, By: by, At: now})
	return doc.Version, true, nil
}

// UnlockThread removes one conversation lock. changed=false means it was not
// locked. Floor: maintain.
func (s *Service) UnlockThread(ctx context.Context, owner, repo string, num int, actor auth.Principal) (int, bool, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleMaintain); err != nil {
		return 0, false, err
	}
	if num < 1 {
		return 0, false, fmt.Errorf("%w: unknown conversation", ErrInvalid)
	}
	by := normPrincipal(actor.Name)
	doc, changed, err := s.mutateDoc(ctx, owner, repo, func(d *Doc) (bool, error) {
		key := ThreadLockKey(num)
		if _, ok := d.Locks[key]; !ok {
			return false, nil
		}
		delete(d.Locks, key)
		return true, nil
	})
	if err != nil {
		return 0, false, err
	}
	if !changed {
		return doc.Version, false, nil
	}
	s.audit(ctx, by, ActionUnlock, owner, repo, "", fmt.Sprintf("thread=%06x;prev=locked", num))
	s.stream(ctx, owner, repo, StreamEvent{Num: num, Action: ActionUnlock, By: by, At: s.nowUTC().Format(dateTimeFmt)})
	return doc.Version, true, nil
}

// Apply is the PUT one-writer verb (PLAN-V2 §M.3): the posted full document
// is diffed against the current one and the stricter floor is applied per
// changed field — hide/unhide deltas need triage, lock deltas need maintain.
// A stale `version` is 409; an unknown/absent target is 400. Hidden-entry
// values are evidence: a kept entry keeps its original by/at/reason (change
// it by unhide→hide, never by an in-place rewrite).
func (s *Service) Apply(ctx context.Context, owner, repo string, actor auth.Principal, in Doc) (int, error) {
	if in.Version < 0 {
		return 0, fmt.Errorf("%w: bad version", ErrInvalid)
	}
	cur, _, err := s.loadDoc(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	if in.Version != cur.Version {
		return 0, fmt.Errorf("%w: stale document (version %d, current %d)", ErrConflict, in.Version, cur.Version)
	}
	addedHidden, removedHidden := diffHidden(cur.Hidden, in.Hidden)
	if len(addedHidden) > 0 || len(removedHidden) > 0 {
		if err := s.requireRole(ctx, owner, repo, actor, identity.RoleTriage); err != nil {
			return 0, err
		}
	}
	envelopeModeChanged := false
	if in.Locked {
		inMode, merr := normalizeLockMode(in.LockMode)
		if merr != nil {
			return 0, merr
		}
		curMode, cerr := normalizeLockMode(cur.LockMode)
		if cerr != nil {
			curMode = LockModeAll
		}
		if !cur.Locked {
			in.LockMode = inMode // stamp the explicit spelling on a fresh lock
		} else if inMode != curMode {
			envelopeModeChanged = true
			in.LockMode = inMode
		}
	}
	lockChanged := in.Locked != cur.Locked || in.LockReason != cur.LockReason || envelopeModeChanged
	addedLocks, removedLocks := diffLocks(cur.Locks, in.Locks)
	modeChangedLocks := changedLockModes(cur.Locks, in.Locks)
	if lockChanged || len(addedLocks) > 0 || len(removedLocks) > 0 || len(modeChangedLocks) > 0 {
		if err := s.requireRole(ctx, owner, repo, actor, identity.RoleMaintain); err != nil {
			return 0, err
		}
	}
	// Validate every added entry before the write (grammar + existence).
	for _, target := range addedHidden {
		t, perr := ParseTarget(target)
		if perr != nil {
			return 0, perr
		}
		if s.Targets != nil {
			ok, qerr := s.Targets.Exists(ctx, owner, repo, t)
			if qerr != nil {
				return 0, qerr
			}
			if !ok {
				return 0, fmt.Errorf("%w: target %s does not exist", ErrInvalid, target)
			}
		}
	}
	for _, key := range addedLocks {
		num, ok := parseThreadLockKey(key)
		if !ok {
			return 0, fmt.Errorf("%w: bad lock key %q", ErrInvalid, key)
		}
		_ = num
	}
	// Every added or re-moded entry must carry a valid mode (the empty
	// spelling is the legacy "all").
	for _, key := range append(append([]string{}, addedLocks...), modeChangedLocks...) {
		if _, merr := normalizeLockMode(in.Locks[key].Mode); merr != nil {
			return 0, merr
		}
	}
	by := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	doc, changed, err := s.mutateDoc(ctx, owner, repo, func(d *Doc) (bool, error) {
		if d.Version != in.Version {
			return false, fmt.Errorf("%w: stale document", ErrConflict)
		}
		for k, h := range in.Hidden {
			if _, ok := d.Hidden[k]; !ok {
				d.Hidden[k] = Hidden{By: by, At: now, Reason: h.Reason}
			}
		}
		for _, k := range removedHidden {
			delete(d.Hidden, k)
		}
		for k, l := range in.Locks {
			if _, ok := d.Locks[k]; !ok {
				mode, _ := normalizeLockMode(l.Mode)
				d.Locks[k] = LockState{By: by, At: now, Reason: l.Reason, Mode: mode}
			}
		}
		for _, k := range modeChangedLocks {
			e := d.Locks[k]
			e.Mode, _ = normalizeLockMode(in.Locks[k].Mode) // evidence (by/at/reason) kept
			d.Locks[k] = e
		}
		for _, k := range removedLocks {
			delete(d.Locks, k)
		}
		if d.Locked != in.Locked || d.LockReason != in.LockReason || envelopeModeChanged {
			d.Locked = in.Locked
			d.LockReason = in.LockReason
			if in.Locked {
				d.LockedBy, d.LockedAt = by, now
				d.LockMode, _ = normalizeLockMode(in.LockMode)
			} else {
				d.LockedBy, d.LockedAt = "", ""
				d.LockMode = ""
			}
		}
		// A PUT that changes nothing writes nothing (no empty revision
		// churn, no phantom audit record).
		return len(addedHidden)+len(removedHidden)+len(addedLocks)+len(removedLocks)+len(modeChangedLocks) > 0 || lockChanged, nil
	})
	if err != nil {
		return 0, err
	}
	if !changed {
		return doc.Version, nil
	}
	// One audit record per changed family, naming the actor, the repo and
	// the previous state.
	for _, target := range addedHidden {
		s.audit(ctx, by, ActionHide, owner, repo, doc.Hidden[target].Reason, "target="+target+";prev=visible")
	}
	for _, target := range removedHidden {
		s.audit(ctx, by, ActionUnhide, owner, repo, "", "target="+target+";prev=hidden")
	}
	for _, key := range addedLocks {
		s.audit(ctx, by, ActionLock, owner, repo, doc.Locks[key].Reason,
			fmt.Sprintf("lock=%s;mode=%s;prev=unlocked", key, doc.Locks[key].Mode))
	}
	for _, key := range modeChangedLocks {
		s.audit(ctx, by, ActionLock, owner, repo, doc.Locks[key].Reason,
			fmt.Sprintf("lock=%s;mode=%s;prev=locked", key, doc.Locks[key].Mode))
	}
	for _, key := range removedLocks {
		s.audit(ctx, by, ActionUnlock, owner, repo, "", "lock="+key+";prev=locked")
	}
	if lockChanged {
		action, prev := ActionLock, "unlocked"
		if !in.Locked {
			action, prev = ActionUnlock, "locked"
		}
		s.audit(ctx, by, action, owner, repo, in.LockReason, "scope=repo;mode="+doc.LockMode+";prev="+prev)
	}
	s.stream(ctx, owner, repo, StreamEvent{Action: "apply", By: by, At: now})
	return doc.Version, nil
}

// diffHidden returns the keys added to and removed from the hidden map.
// Kept keys are not compared by value: the record is evidence.
func diffHidden(cur, next map[string]Hidden) (added, removed []string) {
	for k := range next {
		if _, ok := cur[k]; !ok {
			added = append(added, k)
		}
	}
	for k := range cur {
		if _, ok := next[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// diffLocks is diffHidden's lock-map twin.
func diffLocks(cur, next map[string]LockState) (added, removed []string) {
	for k := range next {
		if _, ok := cur[k]; !ok {
			added = append(added, k)
		}
	}
	for k := range cur {
		if _, ok := next[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// changedLockModes returns the kept lock keys whose MODE differs (an entry
// present on both sides; add/remove are diffLocks's job). A stored or posted
// empty mode normalizes to LockModeAll, so a legacy document round-trips
// unchanged.
func changedLockModes(cur, next map[string]LockState) []string {
	var out []string
	for k, n := range next {
		c, ok := cur[k]
		if !ok {
			continue
		}
		cm, cerr := normalizeLockMode(c.Mode)
		nm, nerr := normalizeLockMode(n.Mode)
		if cerr != nil || nerr != nil {
			continue
		}
		if cm != nm {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// parseThreadLockKey validates a Doc.Locks key.
func parseThreadLockKey(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, "thread:")
	if !ok || len(rest) < 6 || len(rest) > 16 {
		return 0, false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(rest, 16, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return int(n), true
}

// --- loading + CAS ------------------------------------------------------------

type cachedDoc struct {
	version store.Version
	doc     Doc
}

// loadDoc reads the document through the version-stamped cache: every call
// is a conditional GET (freshness is the store's, never a TTL) and a cached
// entry skips only the parse.
func (s *Service) loadDoc(ctx context.Context, owner, repo string) (Doc, store.Version, error) {
	key := ModerationKey(owner, repo)
	s.mu.Lock()
	cached, have := s.docCache[key]
	s.mu.Unlock()
	opts := store.GetOptions{}
	if have {
		opts.IfNoneMatch = cached.version
	}
	raw, meta, err := store.GetBytes(ctx, s.Store, key, opts)
	if err != nil {
		if store.IsNotFound(err) {
			s.mu.Lock()
			delete(s.docCache, key)
			s.mu.Unlock()
			return emptyDoc(), "", nil
		}
		return Doc{}, "", err
	}
	if raw == nil {
		// Clone: the returned document is handed to mutators and to the
		// read API, and neither may mutate the cached maps in place.
		return cloneDoc(cached.doc), cached.version, nil
	}
	var doc Doc
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		// Corrupt state fails closed (a projection must never serve content
		// it cannot prove visible).
		return Doc{}, "", fmt.Errorf("corrupt moderation doc %s: %v", key, uerr)
	}
	if doc.Version > docVersion {
		return Doc{}, "", fmt.Errorf("unsupported moderation doc version %d at %s", doc.Version, key)
	}
	normalizeDoc(&doc)
	s.mu.Lock()
	s.docCache[key] = cachedDoc{version: meta.Version, doc: cloneDoc(doc)}
	s.mu.Unlock()
	return doc, meta.Version, nil
}

// cloneDoc deep-copies the two maps so a caller (or a mutator retrying a CAS)
// can never observe, or leak, a mutation of the cached document.
func cloneDoc(doc Doc) Doc {
	out := doc
	if doc.Hidden != nil {
		out.Hidden = make(map[string]Hidden, len(doc.Hidden))
		for k, v := range doc.Hidden {
			out.Hidden[k] = v
		}
	}
	if doc.Locks != nil {
		out.Locks = make(map[string]LockState, len(doc.Locks))
		for k, v := range doc.Locks {
			out.Locks[k] = v
		}
	}
	return out
}

// emptyDoc is the absent-document read shape (access.json synthesis).
func emptyDoc() Doc {
	return Doc{Hidden: map[string]Hidden{}, Locks: map[string]LockState{}}
}

func normalizeDoc(doc *Doc) {
	if doc.Hidden == nil {
		doc.Hidden = map[string]Hidden{}
	}
	if doc.Locks == nil {
		doc.Locks = map[string]LockState{}
	}
}

// mutateDoc runs the bounded whole-document CAS loop. The mutator returns
// changed=false to stop without a write (idempotent duplicate). The
// document revision increments on every committed write.
func (s *Service) mutateDoc(ctx context.Context, owner, repo string, f func(d *Doc) (bool, error)) (Doc, bool, error) {
	const attempts = 5
	key := ModerationKey(owner, repo)
	for i := 0; i < attempts; i++ {
		doc, ver, err := s.loadDoc(ctx, owner, repo)
		if err != nil {
			return Doc{}, false, err
		}
		normalizeDoc(&doc)
		changed, ferr := f(&doc)
		if ferr != nil {
			return Doc{}, false, ferr
		}
		if !changed {
			return doc, false, nil
		}
		doc.Version++
		doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		body, merr := json.Marshal(doc)
		if merr != nil {
			return Doc{}, false, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		meta, perr := store.PutBytes(ctx, s.Store, key, body, opts)
		if perr == nil {
			s.mu.Lock()
			s.docCache[key] = cachedDoc{version: meta.Version, doc: doc}
			s.mu.Unlock()
			return doc, true, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return Doc{}, false, perr
		}
		s.mu.Lock()
		delete(s.docCache, key)
		s.mu.Unlock()
	}
	return Doc{}, false, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

// requireRole enforces the M2 floors inside the verb (never in a handler),
// so the API handler and the page form cannot disagree. Host admins always
// pass. With no Roles seam wired the principal flags are the fallback (the
// issues/review house rule); anonymous never passes.
func (s *Service) requireRole(ctx context.Context, owner, repo string, p auth.Principal, want identity.Role) error {
	if p.Admin {
		return nil
	}
	got := identity.Role("")
	if s.Roles != nil {
		got, _ = s.Roles.Resolve(ctx, owner, repo, p)
	} else {
		switch {
		case p.Write:
			got = identity.RoleWrite
		case p.Anonymous:
			got = ""
		default:
			got = identity.RoleRead
		}
	}
	if roleRank(got) >= roleRank(want) {
		return nil
	}
	if p.Anonymous {
		return fmt.Errorf("%w", ErrUnauthorized)
	}
	return fmt.Errorf("%w: need %s", ErrForbidden, want)
}

// requireRead is the require_read gate (identity's hook), failing closed.
func (s *Service) requireRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if p.Admin || p.Write {
		return nil
	}
	if s.Roles == nil {
		if p.Anonymous {
			return fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil
	}
	if aerr := s.Roles.CheckRead(ctx, owner, repo, p); aerr != nil {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, aerr.Why)
		case auth.ErrUnavailable:
			return fmt.Errorf("identity unavailable: %s", aerr.Why)
		default:
			return fmt.Errorf("%w: %s", ErrUnauthorized, aerr.Why)
		}
	}
	return nil
}

// roleRank orders the P6 ladder.
func roleRank(role identity.Role) int {
	switch identity.Role(strings.ToLower(string(role))) {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

// audit writes one record, logging (never failing the action) on error: the
// state change is committed and must not be reverted by an audit outage. The
// target names the repo AND the exact content target (the plan's
// "repo:<owner>/<name>"); detail carries the previous state.
func (s *Service) audit(ctx context.Context, actor, action, owner, repo, reason, detail string) {
	if aerr := s.appendAudit(ctx, actor, action, "repo:"+owner+"/"+repo, reason, detail); aerr != nil {
		s.warn(ctx, "moderation: audit write failed", "actor", actor, "action", action, "err", aerr)
	}
}

// stream publishes the moderation SSE frame through the composition seam.
func (s *Service) stream(ctx context.Context, owner, repo string, ev StreamEvent) {
	if s.Stream == nil {
		return
	}
	ev.Repo = owner + "/" + repo
	if ev.At == "" {
		ev.At = s.nowUTC().Format(dateTimeFmt)
	}
	s.Stream(ctx, ev)
}

// StreamEvent is the moderation SSE frame (PLAN-V2 §M.2/M.3): the "moderation"
// collab kind, carrying the action and the resulting state so a page can
// revalidate and a hidden row can announce itself (aria-live).
type StreamEvent struct {
	Repo   string
	Num    int
	Seq    int
	TID    string
	Action string // hidden|unhidden|locked|unlocked|apply
	By     string
	At     string
}

// State maps the action onto the frame's state word ("hidden"/"visible"/…).
// A filed report carries no state (the frame is the pointer, not the row), so
// it answers "" and the frame omits the field.
func (e StreamEvent) State() string {
	switch e.Action {
	case ActionHide:
		return "hidden"
	case ActionUnhide:
		return "visible"
	case ActionLock:
		return "locked"
	case ActionUnlock:
		return "unlocked"
	case ActionReportFiled:
		return ""
	}
	return e.Action
}
