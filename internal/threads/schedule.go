package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Scheduled triggers (15 §1/§8, round-2 pass 2). The workflow parser validates
// `schedule: - cron: …` (workflow.ParseCron) and this file is the maintainer's
// evaluation of it: one persisted last-fired watermark per (workflow path, cron
// expression), a CAS claim on that watermark, and a run created only by the
// instance that won the claim.
//
// ## The watermark/CAS rule (never double-fire)
//
// `repos/<o>/<r>/threads/schedule.json` records, for each `"<path>|<cron>"`
// entry, the unix second of the last tick this repository fired. A sweep
// computes the most recent tick at or before now and fires only when it is
// strictly greater than the stored value. Firing is a CAS `PutUpdate` of that
// object: two instances sweeping the same minute race on one version, exactly
// one wins, and the loser sees the higher stored tick and creates nothing.
// Because the watermark write happens BEFORE the run is created, a crash
// between the two loses one scheduled run (at-most-once, the fail-safe
// direction); a restart re-reads the watermark and never re-fires a tick.
//
// ## Cost
//
// One exact-key GET (the watermark) plus the file list at the default branch
// tip (one `git ls-tree` + one `git show` per scheduled file). No LIST: the
// sweep walks the file list it already has and skips a file with no
// `schedule:` entries without touching the store again.
const (
	// KindThreadsSchedule is the Seam 5 task kind for cron evaluation.
	KindThreadsSchedule = "threads-schedule"

	// scheduleKeyLimit bounds the watermark document (a repo with thousands of
	// scheduled workflows is a mistake; the cap fails closed with a logged
	// warning rather than an unbounded object).
	scheduleKeyLimit = 512
)

// ScheduleKey returns repos/<o>/<r>/threads/schedule.json.
func ScheduleKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/schedule.json"
}

// ScheduleWatermark is the persisted last-fired state: one LOCAL wall-clock
// slot per (workflow path, cron expression, timezone) entry. A slot is the
// scheduled local minute ("2006-01-02T15:04"), which is what makes the DST
// fall-back hour fire once: both instants name the same slot, and the second
// sees it already claimed. A spring-forward gap has no slot at all.
type ScheduleWatermark struct {
	// Fired maps "<path>|<cron>|<timezone>" → the last fired local slot.
	Fired     map[string]string `json:"fired"`
	UpdatedAt string            `json:"updated_at"`
	Version   int               `json:"version"`
}

// scheduleEntryKey renders one watermark key (the timezone is part of it: the
// same cron under two zones targets two different local minutes).
func scheduleEntryKey(path, cron, timezone string) string {
	return path + "|" + strings.TrimSpace(cron) + "|" + strings.TrimSpace(timezone)
}

// ScheduleSweep is what one pass did (the summary notice).
type ScheduleSweep struct {
	// Evaluated counts the (file, cron) entries examined.
	Evaluated int `json:"evaluated"`
	// Fired counts the ticks this pass claimed and created a run for.
	Fired int `json:"fired"`
	// Skipped lists entries whose cron the scheduler does not support (they
	// cannot exist in a freshly parsed file: the parser rejects them; one can
	// only survive from a record written before validation).
	Skipped []string `json:"skipped,omitempty"`
	// Disabled counts entries an operator disabled (PAR-154): they are reported,
	// never fired.
	Disabled int `json:"disabled,omitempty"`
	// Coalesced counts missed ticks folded into a single fired run: the sweeper
	// fires once per entry per pass, never a backlog (PAR-154).
	Coalesced int `json:"coalesced,omitempty"`
	// Runs are the runs this pass created, in fire order.
	Runs []*Run `json:"runs,omitempty"`
}

// DefaultBranchReader is the optional GitReader extension the scheduler uses to
// name the branch a scheduled run belongs to. A reader without it still works
// (the run resolves HEAD and carries no branch name).
type DefaultBranchReader interface {
	DefaultBranch(ctx context.Context, repo string) (string, error)
}

// TaskLogger narrates one maintainer task (the packages-gc shape; the maintain
// adapter satisfies it, and nil is safe).
type TaskLogger interface {
	Notice(text string)
	Progress(label string, done, total uint64, unit string)
}

// SweepSchedule evaluates every `schedule:` entry of the repository's default
// branch tip and creates one run per due tick. It is idempotent, safe to run
// from several instances at once (the watermark CAS is the commit point) and
// safe to run twice in the same minute.
func (s *Service) SweepSchedule(ctx context.Context, owner, repo string, log TaskLogger) (*ScheduleSweep, error) {
	res := &ScheduleSweep{}
	if s == nil || s.Store == nil || s.Git == nil {
		return res, nil
	}
	repoPath := repoID(owner, repo)
	// The operator switches and the watermark are one exact-key GET each, read
	// once per pass (PAR-154). A switch read failure fails CLOSED: nothing is
	// auto-fired rather than ignoring an operator's disable.
	overrides, oerr := s.scheduleOverridesForSweep(ctx, owner, repo)
	if oerr != nil {
		return res, oerr
	}
	watermark, werr := s.LoadScheduleWatermark(ctx, owner, repo)
	if werr != nil {
		return res, werr
	}
	commit, err := s.Git.ResolveRef(ctx, repoPath, "HEAD")
	if err != nil {
		if isNotFoundLike(err) {
			return res, nil // an empty repository has nothing to schedule
		}
		return res, err
	}
	files, derr := Discover(ctx, s.Git, repoPath, commit)
	if derr != nil {
		return res, derr
	}
	type due struct {
		path   string
		cron   string
		tz     string
		slot   string
		missed int
	}
	var dues []due
	seen := map[string]bool{}
	now := s.nowUTC()
	for _, f := range files {
		if f.Workflow == nil || !f.Workflow.On.ScheduleSet {
			continue
		}
		for _, entry := range f.Workflow.On.Schedule {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.Evaluated++
			parsed, perr := workflow.ParseCron(entry.Cron)
			if perr != nil {
				// A record written before parse-time validation: report it,
				// never guess an evaluation.
				res.Skipped = append(res.Skipped, f.Path+" ("+perr.Error()+")")
				continue
			}
			loc, lerr := entry.Location()
			if lerr != nil {
				res.Skipped = append(res.Skipped, f.Path+" ("+lerr.Error()+")")
				continue
			}
			key := scheduleEntryKey(f.Path, entry.Cron, entry.Timezone)
			seen[key] = true
			if o, ok := overrides[key]; ok && o.Disabled {
				res.Disabled++
				continue
			}
			tick, ok := parsed.PrevIn(now, loc)
			if !ok {
				continue
			}
			dues = append(dues, due{
				path: f.Path, cron: entry.Cron, tz: entry.Timezone, slot: parsed.Slot(tick),
				missed: coalescedTicks(parsed, loc, watermark.Fired[key], now),
			})
		}
	}
	if len(dues) == 0 {
		return res, nil
	}
	branch, ref := s.scheduleBranch(ctx, repoPath)
	sort.Slice(dues, func(i, j int) bool {
		if dues[i].path != dues[j].path {
			return dues[i].path < dues[j].path
		}
		return dues[i].cron < dues[j].cron
	})
	for i, d := range dues {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if log != nil {
			log.Progress("schedules", uint64(i+1), uint64(len(dues)), "entry")
		}
		key := scheduleEntryKey(d.path, d.cron, d.tz)
		won, cerr := s.claimScheduleTick(ctx, owner, repo, key, d.slot, seen)
		if cerr != nil {
			return res, cerr
		}
		if !won {
			continue // another instance (or an earlier pass) has this tick
		}
		runs, rerr := s.CreateRun(ctx, repoPath, commit, EventSchedule, CreateOpts{
			Branch: branch, Ref: ref, Actor: "schedule", Workflow: d.path,
		})
		if rerr != nil {
			// The tick is already consumed (at-most-once): a failure is logged
			// rather than retried, so a broken workflow can never spin.
			slog.Warn("threads: scheduled run", "repo", repoPath, "workflow", d.path, "cron", d.cron, "err", rerr)
			continue
		}
		res.Fired += len(runs)
		res.Coalesced += d.missed
		res.Runs = append(res.Runs, runs...)
	}
	if log != nil && (res.Fired > 0 || res.Coalesced > 0 || res.Disabled > 0) {
		log.Notice(fmt.Sprintf("threads-schedule %s: %d run(s) from %d entr(ies), %d coalesced tick(s), %d disabled",
			repoPath, res.Fired, res.Evaluated, res.Coalesced, res.Disabled))
	}
	return res, nil
}

// scheduleBranch resolves the default branch name (best effort: a reader
// without DefaultBranchReader, or a detached HEAD, leaves it empty).
func (s *Service) scheduleBranch(ctx context.Context, repo string) (branch, ref string) {
	br, ok := s.Git.(DefaultBranchReader)
	if !ok {
		return "", ""
	}
	name, err := br.DefaultBranch(ctx, repo)
	if err != nil {
		return "", ""
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "refs/heads/")
	if name == "" {
		return "", ""
	}
	return name, "refs/heads/" + name
}

// claimScheduleTick is the commit point: it advances the watermark for one
// entry to tick, returning true for exactly one caller per (entry, tick) — ever,
// across instances and restarts. prune is the set of keys the current sweep saw;
// entries no longer present in any workflow file are dropped from the document
// so a deleted schedule cannot resurrect (and the object cannot grow forever).
func (s *Service) claimScheduleTick(ctx context.Context, owner, repo, key, slot string, prune map[string]bool) (bool, error) {
	keyName := ScheduleKey(owner, repo)
	won := false
	_, err := s.casUpdate(ctx, keyName, 6, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		won = false
		doc := &ScheduleWatermark{Fired: map[string]string{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, doc); jerr != nil {
				return nil, false, fmt.Errorf("%w: schedule watermark: %v", ErrCorrupt, jerr)
			}
		}
		if doc.Fired == nil {
			doc.Fired = map[string]string{}
		}
		if last, ok := doc.Fired[key]; ok && last == slot {
			return nil, false, nil // already fired (another instance won)
		}
		for k := range doc.Fired {
			if prune != nil && !prune[k] {
				delete(doc.Fired, k)
			}
		}
		if len(doc.Fired) >= scheduleKeyLimit {
			return nil, false, fmt.Errorf("%w: more than %d scheduled workflow entries", ErrUnprocessable, scheduleKeyLimit)
		}
		doc.Fired[key] = slot
		doc.UpdatedAt = s.nowUTC().Format(dateTimeFmt)
		doc.Version++
		won = true
		return mustJSON(doc), true, nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// LoadScheduleWatermark reads the persisted watermark (the API/UI surface and
// the tests' observability hook); a missing object is an empty document.
func (s *Service) LoadScheduleWatermark(ctx context.Context, owner, repo string) (*ScheduleWatermark, error) {
	raw, _, err := s.getJSON(ctx, ScheduleKey(owner, repo))
	if err != nil {
		return nil, err
	}
	doc := &ScheduleWatermark{Fired: map[string]string{}}
	if raw == nil {
		return doc, nil
	}
	if jerr := json.Unmarshal(raw, doc); jerr != nil {
		return nil, fmt.Errorf("%w: schedule watermark: %v", ErrCorrupt, jerr)
	}
	if doc.Fired == nil {
		doc.Fired = map[string]string{}
	}
	return doc, nil
}

// ScheduleEntries projects a workflow file's cron entries for the UI (sorted,
// with the parsed validity so a legacy record shows why it is skipped).
func ScheduleEntries(w *workflow.Workflow) []string {
	if w == nil || !w.On.ScheduleSet {
		return []string{}
	}
	out := make([]string, 0, len(w.On.Schedule))
	for _, e := range w.On.Schedule {
		if _, err := workflow.ParseCron(e.Cron); err != nil {
			out = append(out, e.Cron+" (unsupported)")
			continue
		}
		out = append(out, e.Cron)
	}
	sort.Strings(out)
	return out
}

// isNotFoundLike reports the "no such ref/repo" family (a repo with no HEAD is
// an empty repository, not an outage).
func isNotFoundLike(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnprocessable)
}
