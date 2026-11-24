// audit.go — the host-scope moderation audit family (PLAN-V2 §M.2, M1's
// "audit family skeleton"; docs/features/16_moderation.md §3).
//
//	moderation/audit_state.json       CAS'd allocator {"next_seq": N}
//	moderation/audit/<seq:012x>.json  Create-only immutable record
//
// One record per action that changes who owns what or what content is
// visible. M1 writes block/unblock; M2–M4 add hide/unhide/lock/unlock and
// report terminal actions on the SAME family (the transfer feature's T.4
// writes its `transfer` record here too). The record is evidence: it is
// never rewritten, so a terminal state is a NEW record, not a field flip.
//
// No LIST on a hot path: the allocator is one CAS'd counter (the
// collab_state.json shape), and records are read by exact key (an admin
// viewer walks seqs; that page is the only LIST-shaped reader and it is
// explicitly a cold admin surface).

package moderation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// Audit actions. M1 writes block/unblock; M2 adds the hide/lock family;
// M3 adds report terminal actions and M4 the completeness sweep verifies
// every verb writes here.
const (
	ActionBlock   = "block"
	ActionUnblock = "unblock"
	ActionHide    = "hide"
	ActionUnhide  = "unhide"
	ActionLock    = "lock"
	ActionUnlock  = "unlock"
	// M3 report actions: the terminal verb (`report_resolve` for hidden or
	// locked, `report_dismiss` for action none) and the SSE action (not an
	// audit verb) for a new filing.
	ActionReportResolve = "report_resolve"
	ActionReportDismiss = "report_dismiss"
	ActionReportFiled   = "report_filed"
	// ActionConfigSave is the administrative mutation the Administration area
	// writes (M4's audit-completeness sweep): a saved spindle.toml changes who
	// can reach the instance and what it does, so it belongs beside the
	// content actions. The restricted boot has no wired moderation service and
	// no verified principal; the save there is the recovery path and records
	// nothing (16 §14 carries the reason).
	ActionConfigSave = "config_save"
)

// AuditStateKey is the seq allocator object.
func AuditStateKey() string { return "moderation/audit_state.json" }

// AuditKey is one immutable audit record.
func AuditKey(seq int) string { return fmt.Sprintf("moderation/audit/%012x.json", seq) }

// AuditEntry is one moderation action record (PLAN-V2 §M.2 wire shape).
type AuditEntry struct {
	Seq     int    `json:"seq"`
	At      string `json:"at"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Target  string `json:"target"` // "user:<principal>" | "repo:<owner>/<name>"
	Reason  string `json:"reason"`
	Context string `json:"context"`
}

// auditState is the allocator document.
type auditState struct {
	NextSeq int `json:"next_seq"`
}

// RecordAdmin writes one audit entry for an administrative mutation whose
// verb lives outside this package (the Administration area's config save
// today, ownership transfer's T.4 next). It is the one exported entry point to
// the family: callers pass the already-resolved actor, so an unauthenticated
// path can never fabricate an entry. A write failure is returned to the
// caller — the callers log it and keep the committed change (the
// audit-after-commit rule).
func (s *Service) RecordAdmin(ctx context.Context, actor, action, target, detail string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(action) == "" {
		return fmt.Errorf("%w: an audit record needs an actor and an action", ErrInvalid)
	}
	return s.appendAudit(ctx, actor, action, target, "", detail)
}

// appendAudit reserves a seq and Creates one immutable record. The seq gap
// left by a failed record create is honest (the notify activity-log
// convention: a gap is allowed, a rewrite is not).
func (s *Service) appendAudit(ctx context.Context, actor, action, target, reason, detail string) error {
	seq, err := s.reserveAuditSeq(ctx)
	if err != nil {
		return err
	}
	entry := AuditEntry{
		Seq: seq, At: s.nowUTC().Format(dateTimeFmt), Actor: normPrincipal(actor),
		Action: action, Target: target, Reason: reason, Context: detail,
	}
	body, merr := json.Marshal(entry)
	if merr != nil {
		return merr
	}
	if _, perr := store.PutBytes(ctx, s.Store, AuditKey(seq), body,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json", Immutable: true}); perr != nil {
		return perr
	}
	return nil
}

// reserveAuditSeq is the CAS'd allocator (one object, human rate; the
// collab_state.json reasoning — contention is what concurrent moderation
// looks like, and five attempts is generous for humans).
func (s *Service) reserveAuditSeq(ctx context.Context) (int, error) {
	const attempts = 5
	key := AuditStateKey()
	for i := 0; i < attempts; i++ {
		raw, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			return 0, err
		}
		st := auditState{NextSeq: 1}
		if raw != nil {
			if uerr := json.Unmarshal(raw, &st); uerr != nil {
				return 0, fmt.Errorf("corrupt audit allocator %s: %v", key, uerr)
			}
			if st.NextSeq < 1 {
				st.NextSeq = 1
			}
		}
		seq := st.NextSeq
		st.NextSeq = seq + 1
		body, merr := json.Marshal(st)
		if merr != nil {
			return 0, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		if meta.Version == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, key, body, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return 0, perr
		}
		return seq, nil
	}
	return 0, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

// AuditEntryAt reads one record by seq; nil when absent.
func (s *Service) AuditEntryAt(ctx context.Context, seq int) (*AuditEntry, error) {
	if seq < 1 {
		return nil, fmt.Errorf("%w: seq must be >= 1", ErrInvalid)
	}
	raw, _, err := store.GetBytes(ctx, s.Store, AuditKey(seq), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var e AuditEntry
	if uerr := json.Unmarshal(raw, &e); uerr != nil {
		return nil, fmt.Errorf("corrupt audit record %d: %v", seq, uerr)
	}
	return &e, nil
}

// AuditSince reads up to n records after a seq cursor, stopping at the first
// gap (the allocator may have a gap; a gap ends this page and the caller
// resumes from the last returned seq). This is the admin viewer's cold read
// — no hot path calls it.
func (s *Service) AuditSince(ctx context.Context, after, n int) ([]AuditEntry, bool, error) {
	page, err := s.AuditQuery(ctx, after, n, AuditFilter{})
	if err != nil {
		return nil, false, err
	}
	return page.Entries, page.More, nil
}

// AuditFilter narrows the audit viewer (PLAN-ADMIN §8.6: verb/action, actor,
// repo (or "-" for instance-level) and a since bound). The zero value matches
// every record.
type AuditFilter struct {
	Action string    // exact verb, case-insensitive
	Actor  string    // exact actor principal, case-insensitive
	Repo   string    // "owner/name" matches repo:<owner>/<name>; "-" matches non-repo targets
	Since  time.Time // zero = unbounded; records at/after this instant
}

// Match reports whether one record passes the filter.
func (f AuditFilter) Match(e AuditEntry) bool {
	if f.Action != "" && !strings.EqualFold(e.Action, f.Action) {
		return false
	}
	if f.Actor != "" && !strings.EqualFold(e.Actor, f.Actor) {
		return false
	}
	if f.Repo != "" {
		switch {
		case f.Repo == "-":
			if strings.HasPrefix(e.Target, "repo:") {
				return false
			}
		default:
			if !strings.EqualFold(e.Target, "repo:"+f.Repo) {
				return false
			}
		}
	}
	if !f.Since.IsZero() {
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil || at.Before(f.Since) {
			return false
		}
	}
	return true
}

// auditScanMax bounds one filtered page's walk. A filter that matches nothing
// must not turn one page request into an unbounded seq walk, so the walk stops
// here and the page reports "scanned N, no match yet" with a resume cursor —
// honest and bounded, the same discipline as the seq cursor itself.
const auditScanMax = 1000

// AuditPage is one filtered page of the audit viewer.
type AuditPage struct {
	Entries []AuditEntry
	// Next is the resume cursor (the last seq examined), never past the log.
	Next int
	// More reports that records may remain after Next.
	More bool
	// Scanned is how many seqs this page examined (the honest "no match in
	// this range" reading when Entries is short).
	Scanned int
}

// AuditQuery reads one filtered page. It walks exact keys only — the audit
// allocator and records — so there is no LIST anywhere on the page path; a
// seq gap ends the page exactly as AuditSince documents, and the caller
// resumes from Next.
func (s *Service) AuditQuery(ctx context.Context, after, n int, f AuditFilter) (AuditPage, error) {
	if after < 0 {
		after = 0
	}
	if n <= 0 || n > 200 {
		n = 50
	}
	page := AuditPage{Entries: make([]AuditEntry, 0, n), Next: after}
	seq := after + 1
	for page.Scanned < auditScanMax {
		e, err := s.AuditEntryAt(ctx, seq)
		if err != nil {
			return page, err
		}
		if e == nil {
			// A gap (a burned seq) or the end: this page ends here, exactly
			// like AuditSince. The gap may fill later; the next reload sees it.
			page.Next = seq - 1
			return page, nil
		}
		page.Scanned++
		if f.Match(*e) {
			if len(page.Entries) == n {
				// A further matching record exists: the page stops BEFORE it
				// (Next = the last seq this page examined) so paging never
				// skips it, and More means "more MATCHES", not "more rows".
				page.Next = seq - 1
				page.More = true
				return page, nil
			}
			page.Entries = append(page.Entries, *e)
		}
		page.Next = seq
		seq++
	}
	// Scan budget exhausted: probe one past so the resume link is honest.
	more, err := s.AuditEntryAt(ctx, seq)
	if err != nil {
		return page, err
	}
	page.More = more != nil
	return page, nil
}
