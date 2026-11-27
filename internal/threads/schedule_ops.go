package threads

// Schedule operations (PAR-154) and the coalescing rule.
//
// A schedule used to be visible only as "a run may appear". This file is the
// operator surface on top of the persisted watermark (schedule.go):
//
//   - list every cron entry of the default branch's workflows with its next
//     fire, last fire (the watermark) and how many ticks were coalesced;
//   - enable/disable an entry WITHOUT editing the workflow file (the switch is
//     an engine record, like the trigger switches in settings.go), effective on
//     the next sweep and audited;
//   - run an entry now, as a manual trigger (allowed even when disabled, and
//     audited — a disabled schedule is a statement about automatic firing, not
//     a lock on the operator).
//
// ## Missed-run coalescing
//
// The sweeper claims one slot per entry per pass. If the instance was down (or
// the entry was disabled) across several ticks, the next pass fires ONE run and
// reports how many ticks were coalesced into it; it never replays a backlog.
// Replaying could start ten deploys at once after an outage, which is exactly
// the failure the coalescing rule prevents. The count is on the status entry
// (`missed_ticks`) and in the sweep notice.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Audit verbs for the schedule surface.
const (
	AuditScheduleEnable  = "schedule_enable"
	AuditScheduleDisable = "schedule_disable"
	AuditScheduleRunNow  = "schedule_run_now"
)

// ScheduleOpsKey returns repos/<o>/<r>/threads/schedule_ops.json.
func ScheduleOpsKey(owner, repo string) string {
	return "repos/" + owner + "/" + repo + "/threads/schedule_ops.json"
}

// ScheduleOverride is one entry's operator switch.
type ScheduleOverride struct {
	Disabled  bool   `json:"disabled"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ScheduleOpsDoc is the stored switch set, keyed by the same
// "<path>|<cron>|<timezone>" key the watermark uses.
type ScheduleOpsDoc struct {
	Overrides map[string]ScheduleOverride `json:"overrides"`
	UpdatedAt string                      `json:"updated_at"`
	Version   int                         `json:"version"`
}

// ScheduleEntryID identifies one cron entry (the API/CLI/UI spelling).
type ScheduleEntryID struct {
	Path     string `json:"path"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
}

// Key is the watermark/override key.
func (id ScheduleEntryID) Key() string {
	return scheduleEntryKey(strings.TrimSpace(id.Path), id.Cron, id.Timezone)
}

// Validate checks the fields a request must name.
func (id ScheduleEntryID) Validate() error {
	if strings.TrimSpace(id.Path) == "" || strings.TrimSpace(id.Cron) == "" {
		return fmt.Errorf("%w: a schedule entry needs path and cron", ErrInvalid)
	}
	return nil
}

// ScheduleStatus is one row of the schedule page/API.
type ScheduleStatus struct {
	ScheduleEntryID
	// NextFire is the next tick after now (RFC3339, entry timezone), empty
	// when the expression has no future tick.
	NextFire string `json:"next_fire,omitempty"`
	// LastFired is the watermark slot ("2006-01-02T15:04" local) of the last
	// automatic run, empty when the entry has never fired.
	LastFired string `json:"last_fired,omitempty"`
	// MissedTicks counts ticks that elapsed since LastFired and will be
	// coalesced into the next fire (0 when on time or never fired).
	MissedTicks int `json:"missed_ticks,omitempty"`
	// Disabled is the operator switch, UpdatedBy/At who flipped it.
	Disabled  bool   `json:"disabled"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Valid reports whether the stored expression parses; an invalid one is a
	// legacy record the sweeper reports as skipped.
	Valid bool `json:"valid"`
}

// ScheduleList is the read model.
type ScheduleList struct {
	Entries []ScheduleStatus `json:"entries"`
	// WatermarkUpdatedAt is the persisted document's timestamp (the "when did
	// the scheduler last run" fact).
	WatermarkUpdatedAt string `json:"watermark_updated_at,omitempty"`
}

// loadScheduleOps reads the switch document (empty when absent).
func (s *Service) loadScheduleOps(ctx context.Context, owner, repo string) (*ScheduleOpsDoc, error) {
	raw, _, err := s.getJSON(ctx, ScheduleOpsKey(owner, repo))
	if err != nil || raw == nil {
		return &ScheduleOpsDoc{Overrides: map[string]ScheduleOverride{}}, err
	}
	doc := &ScheduleOpsDoc{Overrides: map[string]ScheduleOverride{}}
	if jerr := json.Unmarshal(raw, doc); jerr != nil {
		return nil, fmt.Errorf("%w: schedule overrides: %v", ErrCorrupt, jerr)
	}
	if doc.Overrides == nil {
		doc.Overrides = map[string]ScheduleOverride{}
	}
	return doc, nil
}

// ListSchedules reads the workflows at the default branch tip and joins them
// with the watermark and the operator switches. It is a read (repository read
// role) and costs one discovery plus two exact-key GETs.
func (s *Service) ListSchedules(ctx context.Context, owner, repo string, p auth.Principal) (*ScheduleList, error) {
	if err := s.requireRead(ctx, owner, repo, p); err != nil {
		return nil, err
	}
	out := &ScheduleList{Entries: []ScheduleStatus{}}
	if s.Git == nil {
		return out, nil
	}
	repoPath := repoID(owner, repo)
	commit, err := s.Git.ResolveRef(ctx, repoPath, "HEAD")
	if err != nil {
		if isNotFoundLike(err) {
			return out, nil
		}
		return nil, err
	}
	files, derr := Discover(ctx, s.Git, repoPath, commit)
	if derr != nil {
		return nil, derr
	}
	wm, werr := s.LoadScheduleWatermark(ctx, owner, repo)
	if werr != nil {
		return nil, werr
	}
	ops, oerr := s.loadScheduleOps(ctx, owner, repo)
	if oerr != nil {
		return nil, oerr
	}
	out.WatermarkUpdatedAt = wm.UpdatedAt
	now := s.nowUTC()
	for _, f := range files {
		if f.Workflow == nil || !f.Workflow.On.ScheduleSet {
			continue
		}
		for _, entry := range f.Workflow.On.Schedule {
			row := ScheduleStatus{ScheduleEntryID: ScheduleEntryID{Path: f.Path, Cron: entry.Cron, Timezone: entry.Timezone}}
			key := row.Key()
			if o, ok := ops.Overrides[key]; ok {
				row.Disabled, row.UpdatedBy, row.UpdatedAt = o.Disabled, o.UpdatedBy, o.UpdatedAt
			}
			row.LastFired = wm.Fired[key]
			parsed, perr := workflow.ParseCron(entry.Cron)
			if perr != nil {
				row.Valid = false
				out.Entries = append(out.Entries, row)
				continue
			}
			row.Valid = true
			loc, lerr := entry.Location()
			if lerr != nil {
				row.Valid = false
				out.Entries = append(out.Entries, row)
				continue
			}
			if next, ok := parsed.NextIn(now, loc); ok {
				row.NextFire = next.Format(time.RFC3339)
			}
			row.MissedTicks = coalescedTicks(parsed, loc, row.LastFired, now)
			out.Entries = append(out.Entries, row)
		}
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		if out.Entries[i].Path != out.Entries[j].Path {
			return out.Entries[i].Path < out.Entries[j].Path
		}
		return out.Entries[i].Cron < out.Entries[j].Cron
	})
	return out, nil
}

// coalescedTicks counts ticks strictly after the last fired slot and strictly
// before the most recent tick (the one the next sweep will fire), capped so a
// years-old entry does not walk forever.
func coalescedTicks(c *workflow.Cron, loc *time.Location, lastFired string, now time.Time) int {
	if strings.TrimSpace(lastFired) == "" || c == nil || loc == nil {
		return 0
	}
	// Parse the slot back into a local wall clock. The slot is minute
	// resolution and always a valid local time ("2006-01-02T15:04").
	last, err := time.ParseInLocation("2006-01-02T15:04", lastFired, loc)
	if err != nil {
		return 0
	}
	// Walk the ticks after the stored slot. Stop at the first tick that is not
	// strictly before the most recent due tick; that tick is the one about to
	// fire, so it is not "missed".
	prev, ok := c.PrevIn(now, loc)
	if !ok || !prev.After(last) {
		return 0
	}
	n := 0
	cursor := last
	for n < 1000 {
		next, ok := c.NextIn(cursor, loc)
		if !ok || !next.Before(prev) {
			break
		}
		n++
		cursor = next
	}
	return n
}

// EnsureScheduleEntry confirms the named entry exists in the default branch's
// workflows. It also returns the commit the run-now would build.
func (s *Service) EnsureScheduleEntry(ctx context.Context, owner, repo string, id ScheduleEntryID) (string, error) {
	if err := id.Validate(); err != nil {
		return "", err
	}
	if s.Git == nil {
		return "", fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	repoPath := repoID(owner, repo)
	commit, err := s.Git.ResolveRef(ctx, repoPath, "HEAD")
	if err != nil {
		if isNotFoundLike(err) {
			return "", fmt.Errorf("%w: schedule entry %s", ErrNotFound, id.Path)
		}
		return "", err
	}
	files, derr := Discover(ctx, s.Git, repoPath, commit)
	if derr != nil {
		return "", derr
	}
	for _, f := range files {
		if f.Path != id.Path || f.Workflow == nil || !f.Workflow.On.ScheduleSet {
			continue
		}
		for _, entry := range f.Workflow.On.Schedule {
			if strings.TrimSpace(entry.Cron) == id.Cron && strings.TrimSpace(entry.Timezone) == strings.TrimSpace(id.Timezone) {
				return commit, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s has no schedule entry %q (%s)", ErrNotFound, id.Path, id.Cron, id.Timezone)
}

// SetScheduleEnabled flips one entry's automatic-firing switch (maintain+,
// audited). It does not edit the workflow file and does not touch the
// watermark: a disabled entry keeps its last-fire fact, and re-enabling it
// fires on the next due tick rather than replaying what it missed.
func (s *Service) SetScheduleEnabled(ctx context.Context, repo string, id ScheduleEntryID, enabled bool, p auth.Principal) (*ScheduleStatus, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "maintain"); err != nil {
		return nil, err
	}
	if _, err := s.EnsureScheduleEntry(ctx, owner, name, id); err != nil {
		return nil, err
	}
	now := s.nowUTC().Format(dateTimeFmt)
	key := id.Key()
	doc, derr := s.loadScheduleOps(ctx, owner, name)
	if derr != nil {
		return nil, derr
	}
	_, err = s.casUpdate(ctx, ScheduleOpsKey(owner, name), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		next := &ScheduleOpsDoc{Overrides: map[string]ScheduleOverride{}}
		if cur != nil {
			if jerr := json.Unmarshal(cur, next); jerr != nil {
				return nil, false, fmt.Errorf("%w: schedule overrides: %v", ErrCorrupt, jerr)
			}
		}
		if next.Overrides == nil {
			next.Overrides = map[string]ScheduleOverride{}
		}
		if prev, ok := next.Overrides[key]; ok && prev.Disabled == !enabled {
			doc = next // unchanged
			return nil, false, nil
		}
		next.Overrides[key] = ScheduleOverride{Disabled: !enabled, UpdatedBy: strings.TrimSpace(p.Name), UpdatedAt: now}
		next.UpdatedAt, next.Version = now, next.Version+1
		doc = next
		return mustJSON(next), true, nil
	})
	if err != nil {
		return nil, err
	}
	action := AuditScheduleDisable
	if enabled {
		action = AuditScheduleEnable
	}
	s.auditAdmin(ctx, p.Name, action, "repo:"+repoID(owner, name),
		fmt.Sprintf("enabled=%t;entry=%s", enabled, key))
	row := ScheduleStatus{ScheduleEntryID: id}
	row.Disabled = !enabled
	if o, ok := doc.Overrides[key]; ok {
		row.UpdatedBy, row.UpdatedAt = o.UpdatedBy, o.UpdatedAt
	}
	return s.describeScheduleEntry(ctx, owner, name, row)
}

// RunScheduleNow creates a run for one entry immediately, as a manual trigger
// (write role, audited). It is allowed on a disabled entry: the switch stops
// automatic firing, not the operator's hand.
func (s *Service) RunScheduleNow(ctx context.Context, repo string, id ScheduleEntryID, p auth.Principal) ([]*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "write"); err != nil {
		return nil, err
	}
	commit, err := s.EnsureScheduleEntry(ctx, owner, name, id)
	if err != nil {
		return nil, err
	}
	branch, ref := s.scheduleBranch(ctx, repoID(owner, name))
	runs, rerr := s.CreateRun(ctx, repoID(owner, name), commit, EventSchedule, CreateOpts{
		Branch: branch, Ref: ref, Actor: string(p.Name), Workflow: id.Path,
	})
	if rerr != nil {
		return nil, rerr
	}
	s.auditAdmin(ctx, p.Name, AuditScheduleRunNow, "repo:"+repoID(owner, name),
		fmt.Sprintf("entry=%s;runs=%d", id.Key(), len(runs)))
	return runs, nil
}

// describeScheduleEntry fills the next-fire/last-fire fields for a returned
// row (the page reuses the same read model).
func (s *Service) describeScheduleEntry(ctx context.Context, owner, repo string, row ScheduleStatus) (*ScheduleStatus, error) {
	files, derr := s.discoverAtHead(ctx, owner, repo)
	if derr != nil {
		return nil, derr
	}
	wm, werr := s.LoadScheduleWatermark(ctx, owner, repo)
	if werr != nil {
		return nil, werr
	}
	row.LastFired = wm.Fired[row.Key()]
	for _, f := range files {
		if f.Path != row.Path || f.Workflow == nil || !f.Workflow.On.ScheduleSet {
			continue
		}
		for _, entry := range f.Workflow.On.Schedule {
			if strings.TrimSpace(entry.Cron) != row.Cron || strings.TrimSpace(entry.Timezone) != strings.TrimSpace(row.Timezone) {
				continue
			}
			parsed, perr := workflow.ParseCron(entry.Cron)
			if perr != nil {
				row.Valid = false
				return &row, nil
			}
			loc, lerr := entry.Location()
			if lerr != nil {
				row.Valid = false
				return &row, nil
			}
			row.Valid = true
			now := s.nowUTC()
			if next, ok := parsed.NextIn(now, loc); ok {
				row.NextFire = next.Format(time.RFC3339)
			}
			row.MissedTicks = coalescedTicks(parsed, loc, row.LastFired, now)
			return &row, nil
		}
	}
	return &row, nil
}

// discoverAtHead is the small discovery helper for the single-entry status.
func (s *Service) discoverAtHead(ctx context.Context, owner, repo string) ([]*WorkflowFile, error) {
	if s.Git == nil {
		return nil, nil
	}
	commit, err := s.Git.ResolveRef(ctx, repoID(owner, repo), "HEAD")
	if err != nil {
		if isNotFoundLike(err) {
			return nil, nil
		}
		return nil, err
	}
	return Discover(ctx, s.Git, repoID(owner, repo), commit)
}

// scheduleOverridesForSweep is the sweeper's one read of the switch document.
// A read failure fails CLOSED for firing (no entry is auto-fired) but is
// logged, never a silent empty map that ignores an operator's disable.
func (s *Service) scheduleOverridesForSweep(ctx context.Context, owner, repo string) (map[string]ScheduleOverride, error) {
	doc, err := s.loadScheduleOps(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return doc.Overrides, nil
}
