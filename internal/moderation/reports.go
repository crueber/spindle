// reports.go — PLAN-V2 §M.2/M.3 (milestone M3): abuse reports, their two hot
// windows, the resolve/dismiss verbs, and the compaction task.
//
//	repos/<o>/<r>/meta/moderation/reports/<id:012x>.json   Create-only record
//	repos/<o>/<r>/meta/moderation/reports/index.json       CAS'd hot window
//	moderation/reports/index.json                          CAS'd global queue
//
// The per-repo record is **evidence**: it is created once and never
// rewritten. Terminal state (resolved/dismissed, by whom, with which action)
// is a compensating field on the *index card*, so a resolution is a new
// document revision rather than an edit to the report (the invitations
// discipline, docs/features/16_moderation.md §11).
//
// The per-repo index is the request-path truth for one repo; the global index
// is a hot-window projection of the same rows (plan §M.2: "rows are (repo,id)
// pointers; full records stay per-repo"). A failed global append is logged and
// never reverts the repo report — the `moderation-reports-compact` task
// rebuilds the global window from the per-repo index objects (the ONLY place
// that LISTs; a request path never does).
//
// ### Concurrency
//
// Hazard: two reports filed at the same instant allocating the same id, or a
// resolve racing a resolve on the same report. Avoidance: one CAS loop per
// document (≤5 attempts → 409), the 13_concurrency.md §3 discipline — CAS is
// the lock, there is no lock object and no in-process mutex across a store
// call. The duplicate-open check and the terminal check run INSIDE the
// mutator, so a retry re-evaluates them against the freshly read document: a
// lost CAS can never turn into a duplicate card or a double resolution.
//
// Hazard: the index growing past the ~256 KiB hot-window cap. Avoidance: the
// same mutation marshals the document and evicts the oldest resolved cards
// (never an open one) until it fits, advancing `compacted_through`; the record
// itself is untouched. The global rebuild is a background task
// (`moderation-reports-compact`, (moderation, kind) single-flight) so no
// request path pays for a LIST.
//
// Hazard: the background compact goroutine racing another one. Avoidance: the
// wal.TaskTable's (repo, kind) single-flight is the only arbiter — a second
// start joins or is skipped by the Running() pre-check, the goroutine reads no
// Service field the request path mutates, and the table owns its context
// (context.WithoutCancel only detaches the builder's cancellation, never the
// table's). No new lock is introduced.
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
	"github.com/crueber/spindle/internal/wal"
)

// ReportIndexLimit is the hot-window cap (P4, the issues/checks precedent):
// a mutation that writes more bytes evicts the oldest resolved cards inline.
const ReportIndexLimit = 256 << 10

// TaskKindReportsCompact is the Seam 5 task kind that rebuilds the global
// report index from the per-repo indexes (the only LIST of the family).
const TaskKindReportsCompact = "moderation-reports-compact"

// reportsTaskRepo is the pseudo-repo key the instance-scope task runs under:
// the task table is keyed (repo, kind) and the global queue belongs to no
// single repository.
const reportsTaskRepo = "moderation"

// Report kinds (plan §M.3 request grammar).
const (
	ReportKindThread       = "thread"
	ReportKindReview       = "review"
	ReportKindReviewThread = "review_thread"
	ReportKindUser         = "user"
)

// Report states on the index card.
const (
	ReportOpen      = "open"
	ReportResolved  = "resolved"
	ReportDismissed = "dismissed"
)

// Resolve actions.
const (
	ResolveHidden = "hidden"
	ResolveLocked = "locked"
	ResolveNone   = "none"
)

// Report is one immutable per-repo evidence record. It never carries terminal
// state: a resolution is a new index revision, not a record edit.
type Report struct {
	ID        int    `json:"id"`
	Repo      string `json:"repo"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Reporter  string `json:"reporter"`
	Reason    string `json:"reason"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ReportCard is one row of the per-repo hot window. The terminal fields are
// empty while State is "open"; they are stamped by the resolving verb.
type ReportCard struct {
	ID          int    `json:"id"`
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Reporter    string `json:"reporter"`
	Reason      string `json:"reason"`
	Note        string `json:"note,omitempty"`
	CreatedAt   string `json:"created_at"`
	State       string `json:"state"`
	ResolvedBy  string `json:"resolved_by,omitempty"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
	Action      string `json:"action,omitempty"`
	ResolveNote string `json:"resolve_note,omitempty"`
}

// GlobalCard is one global-queue row: the per-repo card plus its repo pointer
// (the full record and the card stay in the repo family).
type GlobalCard struct {
	ReportCard
	Repo string `json:"repo"`
}

// ReportsIndex is repos/<o>/<r>/meta/moderation/reports/index.json (P4 hot
// window). `version` is the document revision (the CAS token is the store's
// own); `next_id` is the per-repo allocator; `compacted_through` names the
// newest card evicted from resolved_recent (lowercase hex id).
type ReportsIndex struct {
	Version          int          `json:"version"`
	Open             []ReportCard `json:"open"`
	ResolvedRecent   []ReportCard `json:"resolved_recent"`
	CompactedThrough string       `json:"compacted_through"`
	NextID           int          `json:"next_id"`
	UpdatedAt        string       `json:"updated_at"`
}

// GlobalReportsIndex is moderation/reports/index.json.
type GlobalReportsIndex struct {
	Version          int          `json:"version"`
	Open             []GlobalCard `json:"open"`
	ResolvedRecent   []GlobalCard `json:"resolved_recent"`
	CompactedThrough string       `json:"compacted_through"`
	UpdatedAt        string       `json:"updated_at"`
}

// ReportTargetRef is the structured target of a filing request (plan §M.3:
// `target:{num,seq?,tid?,principal?}`). The service, not the client, builds
// the canonical key.
type ReportTargetRef struct {
	Num       int    `json:"num,omitempty"`
	Seq       int    `json:"seq,omitempty"`
	TID       string `json:"tid,omitempty"`
	Principal string `json:"principal,omitempty"`
}

// ReportInput is one filing request.
type ReportInput struct {
	Kind   string          `json:"kind"`
	Target ReportTargetRef `json:"target"`
	Reason string          `json:"reason"`
	Note   string          `json:"note,omitempty"`
}

// ReportResolve is one resolve/dismiss request. Action none records the
// dismissal only (the audit verb is report_dismiss); hidden/locked call the
// M2 writers before the report is marked terminal.
type ReportResolve struct {
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

// TaskRunner is the Seam 5 surface the compaction task drives (satisfied by
// *wal.TaskTable at composition). Nil = the global rebuild waits for the next
// wired host; inline trimming still keeps both windows under the cap.
type TaskRunner interface {
	Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, task *wal.Task) error) (*wal.TaskRecord, error)
	Running(repo, kind string) *wal.TaskRecord
}

// ReportNotifier publishes the report_filed notification (composition binds
// it: repo admins resolved through identity, then notify.EmitModeration). The
// callback receives NO report content beyond the conversation pointer, so a
// notification can never carry a reported body. Nil = no notification.
type ReportNotifier func(ctx context.Context, owner, repo string, num int, actor, at string)

// ReportsKey is the per-repo hot window.
func ReportsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/meta/moderation/reports/index.json"
}

// ReportRecordKey is one immutable report record.
func ReportRecordKey(owner, repo string, id int) string {
	return fmt.Sprintf("repos/%s/%s/meta/moderation/reports/%012x.json", owner, repo, id)
}

// GlobalReportsKey is the global queue hot window.
func GlobalReportsKey() string { return "moderation/reports/index.json" }

// --- target grammar ----------------------------------------------------------

// reportTargetKey validates a filing request and returns the canonical target
// key. Content targets go through the M2 grammar (one form per family);
// a user-profile report is `user:<principal>` and is report-only.
func (s *Service) reportTargetKey(ctx context.Context, owner, repo string, in ReportInput) (string, Target, error) {
	switch in.Kind {
	case ReportKindThread:
		key := ThreadTarget(in.Target.Num, in.Target.Seq)
		t, err := ParseTarget(key)
		if err != nil {
			return "", Target{}, err
		}
		if err := s.probeTarget(ctx, owner, repo, t); err != nil {
			return "", Target{}, err
		}
		return key, t, nil
	case ReportKindReview:
		key := ReviewTarget(in.Target.Num, in.Target.Seq)
		t, err := ParseTarget(key)
		if err != nil {
			return "", Target{}, err
		}
		if err := s.probeTarget(ctx, owner, repo, t); err != nil {
			return "", Target{}, err
		}
		return key, t, nil
	case ReportKindReviewThread:
		key := ReviewThreadTarget(in.Target.Num, in.Target.TID, in.Target.Seq)
		t, err := ParseTarget(key)
		if err != nil {
			return "", Target{}, err
		}
		if err := s.probeTarget(ctx, owner, repo, t); err != nil {
			return "", Target{}, err
		}
		return key, t, nil
	case ReportKindUser:
		principal := normPrincipal(in.Target.Principal)
		if !identity.ValidPrincipal(principal) {
			return "", Target{}, fmt.Errorf("%w: bad target principal %q", ErrInvalid, in.Target.Principal)
		}
		if err := s.requireKnownUser(ctx, principal); err != nil {
			return "", Target{}, err
		}
		return "user:" + principal, Target{}, nil
	}
	return "", Target{}, fmt.Errorf("%w: unknown report kind %q", ErrInvalid, in.Kind)
}

// probeTarget checks the content object exists in its owning family (the
// plan's 400 "bad target key"). A nil probe is grammar-only (tests).
func (s *Service) probeTarget(ctx context.Context, owner, repo string, t Target) error {
	if s.Targets == nil {
		return nil
	}
	ok, err := s.Targets.Exists(ctx, owner, repo, t)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: target %s does not exist", ErrInvalid, reportTargetOf(t))
	}
	return nil
}

func reportTargetOf(t Target) string {
	switch t.Kind {
	case TargetThread:
		return ThreadTarget(t.Num, t.Seq)
	case TargetReview:
		return ReviewTarget(t.Num, t.Seq)
	case TargetReviewThread:
		return ReviewThreadTarget(t.Num, t.TID, t.Seq)
	}
	return ""
}

// requireKnownUser is the profile-existence probe for a user report.
func (s *Service) requireKnownUser(ctx context.Context, principal string) error {
	if s.Namespaces == nil {
		return nil
	}
	exists, err := s.Namespaces.UserExists(ctx, principal)
	if err != nil {
		return fmt.Errorf("identity unavailable: %v", err)
	}
	if !exists {
		return fmt.Errorf("%w: unknown principal %q", ErrInvalid, principal)
	}
	return nil
}

// --- filing ------------------------------------------------------------------

// FileReport files one report. Floor: any authenticated principal with read
// (anonymous is a real 401, a private-repo outsider a 403). A second OPEN
// report by the same reporter for the same target is a 409 (the plan's
// duplicate rule); a resolved/dismissed report does not block a new filing.
func (s *Service) FileReport(ctx context.Context, owner, repo string, actor auth.Principal, in ReportInput) (ReportCard, error) {
	if actor.Anonymous || normPrincipal(actor.Name) == "" {
		return ReportCard{}, fmt.Errorf("%w", ErrUnauthorized)
	}
	if err := s.requireRead(ctx, owner, repo, actor); err != nil {
		return ReportCard{}, err
	}
	reason := strings.ToLower(strings.TrimSpace(in.Reason))
	if reason == "" || !HiddenReasons[reason] {
		return ReportCard{}, fmt.Errorf("%w: unknown reason %q", ErrInvalid, in.Reason)
	}
	target, _, err := s.reportTargetKey(ctx, owner, repo, in)
	if err != nil {
		return ReportCard{}, err
	}
	note := strings.TrimSpace(in.Note)
	if len(note) > 2000 {
		return ReportCard{}, fmt.Errorf("%w: note is too long (max 2000 bytes)", ErrInvalid)
	}
	return s.appendReport(ctx, owner, repo, normPrincipal(actor.Name), in.Kind, target, reason, note)
}

// appendReport is the one filing tail both the human verb (FileReport, above)
// and the advisory spam signal (spam.go) use: the duplicate-open check and the
// id allocation run INSIDE the index mutator, the immutable evidence record is
// Created, the global hot window is appended best-effort, and the SSE frame +
// admin notification follow. Floors and target grammar stay with the caller —
// this function cannot be reached from a request without them.
func (s *Service) appendReport(ctx context.Context, owner, repo, reporter, kind, target, reason, note string) (ReportCard, error) {
	now := s.nowUTC().Format(dateTimeFmt)
	var card ReportCard
	_, err := s.mutateReportsIndexDoc(ctx, owner, repo, func(ix *ReportsIndex) (bool, error) {
		for _, c := range ix.Open {
			if c.Reporter == reporter && c.Target == target {
				return false, fmt.Errorf("%w: you already have an open report for this target", ErrConflict)
			}
		}
		id := ix.NextID
		if id < 1 {
			id = 1
		}
		ix.NextID = id + 1
		card = ReportCard{
			ID: id, Kind: kind, Target: target, Reporter: reporter,
			Reason: reason, Note: note, CreatedAt: now, State: ReportOpen,
		}
		ix.Open = append(ix.Open, card)
		return true, nil
	})
	if err != nil {
		return ReportCard{}, err
	}
	rec := Report{
		ID: card.ID, Repo: owner + "/" + repo, Kind: card.Kind, Target: card.Target,
		Reporter: card.Reporter, Reason: card.Reason, Note: card.Note, CreatedAt: card.CreatedAt,
	}
	body, merr := json.Marshal(rec)
	if merr != nil {
		return ReportCard{}, merr
	}
	if _, perr := store.PutBytes(ctx, s.Store, ReportRecordKey(owner, repo, card.ID), body,
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json", Immutable: true}); perr != nil {
		// The record is the evidence; the card must not outlive it. Compensate
		// (remove the just-added open card) so a retry does not see a 409 for a
		// report the caller was told failed.
		s.removeOpenCard(ctx, owner, repo, card.ID)
		return ReportCard{}, perr
	}
	// Global hot window: appended best-effort (a failed append is a logged
	// gap; the compact task rebuilds it from the per-repo indexes).
	if gerr := s.appendGlobalCard(ctx, GlobalCard{ReportCard: card, Repo: owner + "/" + repo}); gerr != nil {
		s.warn(ctx, "moderation: global report index append failed", "repo", owner+"/"+repo, "id", card.ID, "err", gerr)
	}
	// The frame carries the conversation pointer only, never the note.
	num, seq, tid := reportPointers(card.Kind, card.Target)
	s.stream(ctx, owner, repo, StreamEvent{Num: num, Seq: seq, TID: tid, Action: ActionReportFiled, By: reporter, At: now})
	if s.ReportNotify != nil {
		s.ReportNotify(ctx, owner, repo, num, reporter, now)
	}
	return card, nil
}

// removeOpenCard is the compensation for a failed evidence Create: one CAS
// removes the card (and leaves next_id consumed — ids are never reused).
func (s *Service) removeOpenCard(ctx context.Context, owner, repo string, id int) {
	_, cerr := s.mutateReportsIndexDoc(ctx, owner, repo, func(ix *ReportsIndex) (bool, error) {
		for i, c := range ix.Open {
			if c.ID == id {
				ix.Open = append(ix.Open[:i], ix.Open[i+1:]...)
				return true, nil
			}
		}
		return false, nil
	})
	if cerr != nil {
		s.warn(ctx, "moderation: report card compensation failed", "repo", owner+"/"+repo, "id", id, "err", cerr)
	}
}

// reportPointers extracts the SSE payload pointers from a target key.
func reportPointers(kind, target string) (num, seq int, tid string) {
	if kind == ReportKindUser {
		return 0, 0, ""
	}
	t, err := ParseTarget(target)
	if err != nil {
		return 0, 0, ""
	}
	return t.Num, t.Seq, t.TID
}

// appendGlobalCard CAS-appends one row to the global hot window.
func (s *Service) appendGlobalCard(ctx context.Context, row GlobalCard) error {
	_, err := s.mutateGlobalIndex(ctx, func(ix *GlobalReportsIndex) (bool, error) {
		for _, c := range ix.Open {
			if c.Repo == row.Repo && c.ID == row.ID {
				return false, nil
			}
		}
		ix.Open = append(ix.Open, row)
		return true, nil
	})
	return err
}

// --- listing -----------------------------------------------------------------

// ReportStateFilter validates a state query value ("" = open).
func ReportStateFilter(state string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "open":
		return ReportOpen, nil
	case "resolved", "dismissed":
		return ReportResolved, nil
	case "all":
		return "all", nil
	}
	return "", fmt.Errorf("%w: unknown state %q", ErrInvalid, state)
}

// ListReports returns the repo hot window. Floor: repo admin (host admins
// pass). `after` is the last id of the previous page; rows are ordered by id
// ascending. No LIST: the hot window is the request-path truth.
func (s *Service) ListReports(ctx context.Context, owner, repo string, actor auth.Principal, state string, after, n int) ([]ReportCard, bool, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleAdmin); err != nil {
		return nil, false, err
	}
	filter, err := ReportStateFilter(state)
	if err != nil {
		return nil, false, err
	}
	ix, err := s.loadReportsIndex(ctx, owner, repo)
	if err != nil {
		return nil, false, err
	}
	var rows []ReportCard
	switch filter {
	case "all":
		rows = append(rows, ix.Open...)
		rows = append(rows, ix.ResolvedRecent...)
	case ReportResolved:
		rows = append(rows, ix.ResolvedRecent...)
	default:
		rows = append(rows, ix.Open...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return pageCards(rows, after, n)
}

// ReportQueues returns the open queue and the resolved tail in ONE index read
// (the Settings → Reports tab renders both). Floor: repo admin. Open rows are
// ascending by id; the resolved tail is newest-first. limit caps each slice
// (0 = 200).
func (s *Service) ReportQueues(ctx context.Context, owner, repo string, actor auth.Principal, limit int) ([]ReportCard, []ReportCard, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleAdmin); err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	ix, err := s.loadReportsIndex(ctx, owner, repo)
	if err != nil {
		return nil, nil, err
	}
	open := append([]ReportCard{}, ix.Open...)
	sort.SliceStable(open, func(i, j int) bool { return open[i].ID < open[j].ID })
	if len(open) > limit {
		open = open[:limit]
	}
	terminal := append([]ReportCard{}, ix.ResolvedRecent...)
	sort.SliceStable(terminal, func(i, j int) bool { return terminal[i].ID > terminal[j].ID })
	if len(terminal) > limit {
		terminal = terminal[:limit]
	}
	return open, terminal, nil
}

// ListGlobalReports returns the global queue. Floor: host admin (the top-level
// twin; there is no repo-scoped spelling). Ordered newest-first by
// (created_at, repo, id); `after` is the opaque cursor of the last row of the
// previous page.
func (s *Service) ListGlobalReports(ctx context.Context, actor auth.Principal, state, after string, n int) ([]GlobalCard, bool, error) {
	if !actor.Admin || actor.Anonymous {
		if actor.Anonymous {
			return nil, false, fmt.Errorf("%w", ErrUnauthorized)
		}
		return nil, false, fmt.Errorf("%w: host admin required", ErrForbidden)
	}
	filter, err := ReportStateFilter(state)
	if err != nil {
		return nil, false, err
	}
	ix, _, err := s.loadGlobalIndex(ctx)
	if err != nil {
		return nil, false, err
	}
	var rows []GlobalCard
	switch filter {
	case "all":
		rows = append(rows, ix.Open...)
		rows = append(rows, ix.ResolvedRecent...)
	case ReportResolved:
		rows = append(rows, ix.ResolvedRecent...)
	default:
		rows = append(rows, ix.Open...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return globalBefore(rows[i], rows[j]) })
	return pageGlobal(rows, after, n)
}

// globalBefore is the global queue order: newest first, then repo/id desc.
func globalBefore(a, b GlobalCard) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	if a.Repo != b.Repo {
		return a.Repo > b.Repo
	}
	return a.ID > b.ID
}

// GlobalCursor is the opaque page cursor of one global row.
func GlobalCursor(row GlobalCard) string {
	return row.CreatedAt + "|" + row.Repo + "|" + fmt.Sprintf("%012x", row.ID)
}

func parseGlobalCursor(s string) (createdAt, repo string, id int, err error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	id64, perr := strconv.ParseInt(parts[2], 16, 64)
	if perr != nil || id64 < 1 {
		return "", "", 0, fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	return parts[0], parts[1], int(id64), nil
}

func pageGlobal(rows []GlobalCard, after string, n int) ([]GlobalCard, bool, error) {
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	start := 0
	if after != "" {
		createdAt, repo, id, err := parseGlobalCursor(after)
		if err != nil {
			return nil, false, err
		}
		for start < len(rows) {
			r := rows[start]
			if r.CreatedAt < createdAt ||
				(r.CreatedAt == createdAt && r.Repo < repo) ||
				(r.CreatedAt == createdAt && r.Repo == repo && r.ID < id) {
				break
			}
			start++
		}
	}
	if start >= len(rows) {
		return []GlobalCard{}, false, nil
	}
	end := start + n
	more := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return append([]GlobalCard{}, rows[start:end]...), more, nil
}

func pageCards(rows []ReportCard, after, n int) ([]ReportCard, bool, error) {
	if after < 0 {
		return nil, false, fmt.Errorf("%w: bad cursor", ErrInvalid)
	}
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	start := 0
	for start < len(rows) && rows[start].ID <= after {
		start++
	}
	if start >= len(rows) {
		return []ReportCard{}, false, nil
	}
	end := start + n
	more := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return append([]ReportCard{}, rows[start:end]...), more, nil
}

// --- resolving ---------------------------------------------------------------

// ResolveReport marks one report terminal and applies the requested action.
// Floor: repo admin (the host-admin twin delegates here with p.Admin).
//
// `hidden` calls the M2 Hide verb for the reported target, `locked` calls
// LockThread for the conversation the target lives in, and `none` records the
// dismissal only. The effect runs BEFORE the index CAS: both M2 writers are
// idempotent, so a lost CAS leaves the report open for a clean retry instead of
// showing "resolved" for content that was never hidden. Every path writes one
// audit record (`report_resolve` or `report_dismiss`). A terminal report is a
// 409.
func (s *Service) ResolveReport(ctx context.Context, owner, repo string, id int, actor auth.Principal, action, note string) (ReportCard, error) {
	if err := s.requireRole(ctx, owner, repo, actor, identity.RoleAdmin); err != nil {
		return ReportCard{}, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case ResolveHidden, ResolveLocked, ResolveNone:
	default:
		return ReportCard{}, fmt.Errorf("%w: action must be hidden, locked or none", ErrInvalid)
	}
	if id < 1 {
		return ReportCard{}, fmt.Errorf("%w: bad report id", ErrInvalid)
	}
	ix, err := s.loadReportsIndex(ctx, owner, repo)
	if err != nil {
		return ReportCard{}, err
	}
	card, ok := findOpen(ix, id)
	if !ok {
		if findTerminal(ix, id) {
			return ReportCard{}, fmt.Errorf("%w: report is already resolved", ErrConflict)
		}
		return ReportCard{}, fmt.Errorf("%w: unknown report", ErrNotFound)
	}
	note = strings.TrimSpace(note)
	if len(note) > 2000 {
		return ReportCard{}, fmt.Errorf("%w: note is too long (max 2000 bytes)", ErrInvalid)
	}
	if action == ResolveHidden || action == ResolveLocked {
		t, perr := ParseTarget(card.Target)
		if perr != nil {
			return ReportCard{}, fmt.Errorf("%w: a %s report has no content to %s", ErrInvalid, card.Kind, action)
		}
		if action == ResolveHidden {
			if _, _, herr := s.Hide(ctx, owner, repo, actor, card.Target, card.Reason); herr != nil {
				return ReportCard{}, herr
			}
		} else if _, _, lerr := s.LockThread(ctx, owner, repo, t.Num, actor, "report "+fmt.Sprintf("%012x", card.ID), LockModeAll); lerr != nil {
			return ReportCard{}, lerr
		}
	}
	by := normPrincipal(actor.Name)
	now := s.nowUTC().Format(dateTimeFmt)
	state := ReportResolved
	if action == ResolveNone {
		state = ReportDismissed
	}
	var resolved ReportCard
	_, err = s.mutateReportsIndexDoc(ctx, owner, repo, func(ix *ReportsIndex) (bool, error) {
		for i, c := range ix.Open {
			if c.ID != id {
				continue
			}
			c.State = state
			c.ResolvedBy = by
			c.ResolvedAt = now
			c.Action = action
			c.ResolveNote = note
			ix.Open = append(ix.Open[:i], ix.Open[i+1:]...)
			ix.ResolvedRecent = append(ix.ResolvedRecent, c)
			resolved = c
			return true, nil
		}
		if findTerminal(*ix, id) {
			return false, fmt.Errorf("%w: report is already resolved", ErrConflict)
		}
		return false, fmt.Errorf("%w: unknown report", ErrNotFound)
	})
	if err != nil {
		return ReportCard{}, err
	}
	if gerr := s.resolveGlobalCard(ctx, owner, repo, resolved); gerr != nil {
		s.warn(ctx, "moderation: global report index resolve failed", "repo", owner+"/"+repo, "id", id, "err", gerr)
	}
	auditAction := ActionReportResolve
	if action == ResolveNone {
		auditAction = ActionReportDismiss
	}
	s.audit(ctx, by, auditAction, owner, repo, resolved.Reason,
		fmt.Sprintf("report=%012x;action=%s;target=%s", resolved.ID, action, resolved.Target))
	return resolved, nil
}

func findOpen(ix ReportsIndex, id int) (ReportCard, bool) {
	for _, c := range ix.Open {
		if c.ID == id {
			return c, true
		}
	}
	return ReportCard{}, false
}

func findTerminal(ix ReportsIndex, id int) bool {
	for _, c := range ix.ResolvedRecent {
		if c.ID == id {
			return true
		}
	}
	return false
}

// resolveGlobalCard moves the global twin row from open to resolved_recent.
func (s *Service) resolveGlobalCard(ctx context.Context, owner, repo string, card ReportCard) error {
	full := owner + "/" + repo
	_, err := s.mutateGlobalIndex(ctx, func(ix *GlobalReportsIndex) (bool, error) {
		for i, c := range ix.Open {
			if c.Repo != full || c.ID != card.ID {
				continue
			}
			ix.Open = append(ix.Open[:i], ix.Open[i+1:]...)
			ix.ResolvedRecent = append(ix.ResolvedRecent, GlobalCard{ReportCard: card, Repo: full})
			return true, nil
		}
		for _, c := range ix.ResolvedRecent {
			if c.Repo == full && c.ID == card.ID {
				return false, nil // already projected
			}
		}
		return false, nil
	})
	return err
}

// --- compaction --------------------------------------------------------------

// CompactReports rebuilds the global hot window from the per-repo index
// objects: the ONE LIST of the family, run as the moderation-reports-compact
// task (never on a request path). Open rows are the union of every repo index;
// terminal rows keep a bounded recent tail. Returns the number of open rows.
func (s *Service) CompactReports(ctx context.Context) (int, error) {
	indexes, err := s.listRepoIndexes(ctx)
	if err != nil {
		return 0, err
	}
	open := make([]GlobalCard, 0)
	terminal := make([]GlobalCard, 0)
	for _, entry := range indexes {
		for _, c := range entry.ix.Open {
			open = append(open, GlobalCard{ReportCard: c, Repo: entry.repo})
		}
		for _, c := range entry.ix.ResolvedRecent {
			terminal = append(terminal, GlobalCard{ReportCard: c, Repo: entry.repo})
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return globalBefore(open[i], open[j]) })
	sort.SliceStable(terminal, func(i, j int) bool { return globalBefore(terminal[i], terminal[j]) })
	_, err = s.mutateGlobalIndex(ctx, func(ix *GlobalReportsIndex) (bool, error) {
		ix.Open = open
		ix.ResolvedRecent = terminal
		ix.CompactedThrough = compactedTail(terminal, ix.CompactedThrough)
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	return len(open), nil
}

// startCompactTask runs the rebuild on the (moderation, kind) single-flight
// table. Best-effort: the inline trim already kept the window under the cap.
func (s *Service) startCompactTask(ctx context.Context) {
	if s.Tasks == nil || s.Tasks.Running(reportsTaskRepo, TaskKindReportsCompact) != nil {
		return
	}
	go func() {
		// Detach from the request: the task outlives this response (the
		// identity transfer precedent); the table's ctx bounds it.
		if _, err := s.Tasks.Run(context.WithoutCancel(ctx), reportsTaskRepo, TaskKindReportsCompact,
			map[string]string{"reason": "global report index over cap"}, func(ctx context.Context, task *wal.Task) error {
				n, cerr := s.CompactReports(ctx)
				if cerr != nil {
					return cerr
				}
				task.Notice(fmt.Sprintf("global report index rebuilt: %d open", n))
				return nil
			}); err != nil {
			s.warn(ctx, "moderation: report compaction task failed", "err", err)
		}
	}()
}

type repoIndexEntry struct {
	repo string
	ix   ReportsIndex
}

// listRepoIndexes walks repos/<owner>/<repo>/ prefixes and reads each report
// index. Compaction-task-only (the plan's LIST fallback).
func (s *Service) listRepoIndexes(ctx context.Context) ([]repoIndexEntry, error) {
	var out []repoIndexEntry
	var owners []string
	if err := s.Store.ListPrefixes(ctx, "repos/", func(p string) error {
		owners = append(owners, p)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, ownerPrefix := range owners {
		owner := strings.TrimSuffix(strings.TrimPrefix(ownerPrefix, "repos/"), "/")
		if owner == "" {
			continue
		}
		var repoNames []string
		if err := s.Store.ListPrefixes(ctx, ownerPrefix, func(p string) error {
			repoNames = append(repoNames, p)
			return nil
		}); err != nil {
			return nil, err
		}
		for _, prefix := range repoNames {
			repo := strings.TrimSuffix(strings.TrimPrefix(prefix, ownerPrefix), "/")
			if repo == "" {
				continue
			}
			ix, verr, err := s.readReportsIndexAt(ctx, ReportsKey(owner, repo))
			if err != nil {
				return nil, err
			}
			if verr == "" {
				continue
			}
			out = append(out, repoIndexEntry{repo: owner + "/" + repo, ix: ix})
		}
	}
	return out, nil
}

func compactedTail(terminal []GlobalCard, prev string) string {
	if len(terminal) == 0 {
		return prev
	}
	return fmt.Sprintf("%012x", terminal[len(terminal)-1].ID)
}

// --- document load / CAS -----------------------------------------------------

// loadReportsIndex reads one repo index; absent reads as the empty document
// (version 0, next_id 1) and corrupt/unknown versions fail closed.
func (s *Service) loadReportsIndex(ctx context.Context, owner, repo string) (ReportsIndex, error) {
	ix, _, err := s.readReportsIndexAt(ctx, ReportsKey(owner, repo))
	return ix, err
}

func (s *Service) readReportsIndexAt(ctx context.Context, key string) (ReportsIndex, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return emptyReportsIndex(), "", nil
		}
		return ReportsIndex{}, "", err
	}
	if raw == nil {
		return emptyReportsIndex(), meta.Version, nil
	}
	var ix ReportsIndex
	if uerr := json.Unmarshal(raw, &ix); uerr != nil {
		// Corrupt state fails closed: a queue must never invent a row.
		return ReportsIndex{}, "", fmt.Errorf("corrupt report index %s: %v", key, uerr)
	}
	normalizeReportsIndex(&ix)
	return ix, meta.Version, nil
}

func emptyReportsIndex() ReportsIndex {
	return ReportsIndex{Open: []ReportCard{}, ResolvedRecent: []ReportCard{}, NextID: 1}
}

func normalizeReportsIndex(ix *ReportsIndex) {
	if ix.Open == nil {
		ix.Open = []ReportCard{}
	}
	if ix.ResolvedRecent == nil {
		ix.ResolvedRecent = []ReportCard{}
	}
	if ix.NextID < 1 {
		ix.NextID = 1
	}
}

// mutateReportsIndexDoc is the bounded whole-document CAS loop. The mutator
// returns changed=false to stop without a write. A committed write increments
// the revision and trims the hot window when it crossed the cap.
func (s *Service) mutateReportsIndexDoc(ctx context.Context, owner, repo string, f func(ix *ReportsIndex) (bool, error)) (ReportsIndex, error) {
	const attempts = 5
	key := ReportsKey(owner, repo)
	for i := 0; i < attempts; i++ {
		ix, ver, err := s.readReportsIndexAt(ctx, key)
		if err != nil {
			return ReportsIndex{}, err
		}
		changed, ferr := f(&ix)
		if ferr != nil {
			return ReportsIndex{}, ferr
		}
		if !changed {
			return ix, nil
		}
		ix.Version++
		ix.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		trimRepoIndex(&ix)
		body, merr := json.Marshal(ix)
		if merr != nil {
			return ReportsIndex{}, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		_, perr := store.PutBytes(ctx, s.Store, key, body, opts)
		if perr == nil {
			return ix, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return ReportsIndex{}, perr
		}
	}
	return ReportsIndex{}, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

// trimRepoIndex evicts the oldest resolved cards until the marshaled document
// fits ReportIndexLimit. Open cards are never evicted.
func trimRepoIndex(ix *ReportsIndex) {
	for len(ix.ResolvedRecent) > 0 && reportsIndexSize(ix) > ReportIndexLimit {
		evicted := ix.ResolvedRecent[0]
		ix.ResolvedRecent = ix.ResolvedRecent[1:]
		ix.CompactedThrough = fmt.Sprintf("%012x", evicted.ID)
	}
}

func reportsIndexSize(ix *ReportsIndex) int {
	body, err := json.Marshal(ix)
	if err != nil {
		return 0
	}
	return len(body)
}

// loadGlobalIndex reads the global hot window (absent = empty). The returned
// store version is the CAS token; it is read with the document so a mutation
// never pairs a body with another writer's version.
func (s *Service) loadGlobalIndex(ctx context.Context) (GlobalReportsIndex, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, GlobalReportsKey(), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return emptyGlobalIndex(), "", nil
		}
		return GlobalReportsIndex{}, "", err
	}
	if raw == nil {
		return emptyGlobalIndex(), meta.Version, nil
	}
	var ix GlobalReportsIndex
	if uerr := json.Unmarshal(raw, &ix); uerr != nil {
		return GlobalReportsIndex{}, "", fmt.Errorf("corrupt global report index: %v", uerr)
	}
	normalizeGlobalIndex(&ix)
	return ix, meta.Version, nil
}

func emptyGlobalIndex() GlobalReportsIndex {
	return GlobalReportsIndex{Open: []GlobalCard{}, ResolvedRecent: []GlobalCard{}}
}

func normalizeGlobalIndex(ix *GlobalReportsIndex) {
	if ix.Open == nil {
		ix.Open = []GlobalCard{}
	}
	if ix.ResolvedRecent == nil {
		ix.ResolvedRecent = []GlobalCard{}
	}
}

// mutateGlobalIndex is the CAS loop for moderation/reports/index.json (one
// bucket-root object written by every repo's filing and resolution — the
// meta/repos.pb precedent, cross-repo contention at human rate).
func (s *Service) mutateGlobalIndex(ctx context.Context, f func(ix *GlobalReportsIndex) (bool, error)) (GlobalReportsIndex, error) {
	const attempts = 5
	key := GlobalReportsKey()
	for i := 0; i < attempts; i++ {
		ix, ver, err := s.loadGlobalIndex(ctx)
		if err != nil {
			return GlobalReportsIndex{}, err
		}
		changed, ferr := f(&ix)
		if ferr != nil {
			return GlobalReportsIndex{}, ferr
		}
		if !changed {
			return ix, nil
		}
		ix.Version++
		ix.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		trimGlobalIndex(&ix)
		body, merr := json.Marshal(ix)
		if merr != nil {
			return GlobalReportsIndex{}, merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
		if ver == "" {
			opts.Mode = store.PutCreate
		}
		_, perr := store.PutBytes(ctx, s.Store, key, body, opts)
		if perr == nil {
			if reportsGlobalSize(&ix) > ReportIndexLimit {
				s.startCompactTask(ctx)
			}
			return ix, nil
		}
		if !store.IsPreconditionFailed(perr) {
			return GlobalReportsIndex{}, perr
		}
	}
	return GlobalReportsIndex{}, fmt.Errorf("%w: %s changed concurrently; reload and retry", ErrConflict, key)
}

func trimGlobalIndex(ix *GlobalReportsIndex) {
	for len(ix.ResolvedRecent) > 0 && reportsGlobalSize(ix) > ReportIndexLimit {
		evicted := ix.ResolvedRecent[0]
		ix.ResolvedRecent = ix.ResolvedRecent[1:]
		ix.CompactedThrough = fmt.Sprintf("%012x", evicted.ID)
	}
}

func reportsGlobalSize(ix *GlobalReportsIndex) int {
	body, err := json.Marshal(ix)
	if err != nil {
		return 0
	}
	return len(body)
}
