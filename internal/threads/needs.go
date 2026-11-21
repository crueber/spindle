package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/crueber/spindle/internal/store"
)

// `needs:` dependency gating (15 §4.1, round 2). The queue owns eligibility:
// a job whose `needs` are not all terminal-success is never offered to a
// runner, and a job whose dependency failed/was cancelled/skipped is
// terminalised as **skipped** (round 3: GitHub reads a dependent that never ran
// as `skipped`, not `cancelled`; a genuinely cancelled job keeps `cancelled`),
// which cascades to its own dependents. The runner knows nothing about ordering.
//
// ### Concurrency
//
// Hazard: two claim probes evaluate one blocked job and both terminalise it.
// Avoidance: the terminal write is a per-job CAS that refuses an already
// terminal state (the second probe's CAS loses or is a no-op), exactly like
// every other job transition. Hazard: a dependency finishes while a dependent
// is being skipped. Avoidance: the gate is evaluated inside the CAS and the
// cascade re-reads the run's jobs between waves, so a "need succeeded" snapshot
// never races a skip decision made on a stale one — the next wave sees the new
// state.

// needsGate is one queue entry's dependency verdict.
type needsGate int

const (
	// needsReady: every dependency is terminal success — the job may start.
	needsReady needsGate = iota
	// needsWaiting: at least one dependency is still queued/running.
	needsWaiting
	// needsBlocked: at least one dependency is terminal but not successful,
	// so the job can never run (it is skipped/cancelled).
	needsBlocked
)

// needsIndex maps a workflow job key (and each matrix group) to the job ids it
// expanded to, so a dependent job can name its prerequisites as ids.
func needsIndex(jobs []*Job) map[string][]string {
	idx := make(map[string][]string, len(jobs))
	for _, j := range jobs {
		key := j.Key
		if j.Spec != nil && j.Spec.MatrixGroup != "" {
			key = j.Spec.MatrixGroup
		}
		idx[key] = append(idx[key], j.ID)
	}
	for k := range idx {
		sort.Strings(idx[k])
	}
	return idx
}

// entryNeeds resolves one job's `needs` keys to the job ids it waits on
// (deduplicated, sorted). A key with no matching job yields nothing: the parser
// rejects an unknown `needs` name, and a legacy run with one must not block
// forever.
func entryNeeds(idx map[string][]string, job *Job) []string {
	if len(job.Needs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(job.Needs))
	for _, key := range job.Needs {
		for _, id := range idx[key] {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// anyNeeds reports whether any job of a run waits on another (the cascade's
// fast path: no `needs` anywhere means nothing to skip).
func anyNeeds(jobs []*Job) bool {
	for _, j := range jobs {
		if len(j.Needs) > 0 {
			return true
		}
	}
	return false
}

// jobCache is one claim probe's job-record cache (a dependency is usually
// shared by several dependents, and a probe must not re-GET it per entry).
type jobCache map[string]*Job

// loadJobCached reads one job record through the per-probe cache.
func (s *Service) loadJobCached(ctx context.Context, owner, repo, runID, jobID string, cache jobCache) (*Job, error) {
	if cache != nil {
		if j, ok := cache[jobID]; ok {
			return j, nil
		}
	}
	raw, _, err := s.getJSON(ctx, JobKey(owner, repo, runID, jobID))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		if cache != nil {
			cache[jobID] = nil
		}
		return nil, nil
	}
	var job Job
	if jerr := json.Unmarshal(raw, &job); jerr != nil {
		return nil, fmt.Errorf("%w: job %s: %v", ErrCorrupt, jobID, jerr)
	}
	if cache != nil {
		cache[jobID] = &job
	}
	return &job, nil
}

// needsGate evaluates one queue entry's dependencies. Missing dependency
// records are treated as blocked (a job whose prerequisite cannot be read may
// never run silently beside it).
func (s *Service) needsGate(ctx context.Context, owner, repo string, entry *QueueEntry, cache jobCache) (needsGate, error) {
	verdict := needsReady
	for _, id := range entry.Needs {
		dep, err := s.loadJobCached(ctx, owner, repo, entry.RunID, id, cache)
		if err != nil {
			return needsWaiting, err
		}
		if dep == nil {
			return needsBlocked, nil
		}
		switch dep.State {
		case JobSuccess:
			// satisfied
		case JobFailure, JobCancelled, JobSkipped:
			return needsBlocked, nil
		default:
			verdict = needsWaiting
		}
	}
	return verdict, nil
}

// skipJob terminalises one queued job whose dependencies can never succeed. It
// goes through UpdateJob, so the checks mirror, the run fold and the transitive
// cascade are the same code every other terminal transition uses.
func (s *Service) skipJob(ctx context.Context, owner, repo string, entry *QueueEntry) (*Job, error) {
	return s.UpdateJob(ctx, repoID(owner, repo), entry.RunID, entry.JobID, JobUpdate{
		State: JobSkipped, CancelRequested: boolPtr(false),
	})
}

// skipBlockedDependents terminalises every still-queued job of a run whose
// `needs` can never be satisfied, wave by wave (so a skipped job's dependents
// are skipped too). It runs after any terminal job transition — no runner poll
// is needed for a doomed dependent to conclude — and finishes with one fold.
//
// It never touches a RUNNING job: that job already has a container, and only
// the operator's cancel path may stop it.
func (s *Service) skipBlockedDependents(ctx context.Context, owner, repo, runID string, snapshot []*Job) error {
	// Fast path: a run whose jobs declare no `needs` has nothing to cascade
	// (the common case), so a terminal transition costs no extra read.
	if snapshot != nil && !anyNeeds(snapshot) {
		return nil
	}
	run, err := s.loadRun(ctx, owner, repo, runID)
	if err != nil || run == nil {
		return err
	}
	// The loop terminates: a wave that cancels nothing ends it, and a victim
	// whose CAS was lost to a concurrent claim is remembered as stuck so the
	// next wave does not try it again (that job belongs to its own transition).
	stuck := map[string]bool{}
	for wave := 0; ; wave++ {
		jobs := snapshot
		snapshot = nil // only the first wave may reuse the caller's snapshot
		if jobs == nil {
			jobs, err = s.loadJobs(ctx, owner, repo, runID)
			if err != nil {
				return err
			}
		}
		byID := make(map[string]*Job, len(jobs))
		idx := needsIndex(jobs)
		for _, j := range jobs {
			byID[j.ID] = j
		}
		var victims []*Job
		for _, j := range jobs {
			if j.State != JobQueued || len(j.Needs) == 0 || stuck[j.ID] {
				continue
			}
			blocked := false
			for _, needID := range entryNeeds(idx, j) {
				dep := byID[needID]
				switch {
				case dep == nil, dep.State == JobFailure, dep.State == JobCancelled, dep.State == JobSkipped:
					blocked = true
				}
			}
			if blocked {
				victims = append(victims, j)
			}
		}
		if len(victims) == 0 {
			if wave > 0 {
				// The run header is a projection of its jobs: a skip that
				// terminalised the last live job must be folded, or the run
				// page would keep saying "running" forever.
				_, ferr := s.foldRun(ctx, owner, repo, runID)
				return ferr
			}
			return nil
		}
		for _, v := range victims {
			updated, cerr := s.skipQueuedJob(ctx, owner, repo, run, runID, v.ID)
			if cerr != nil {
				return cerr
			}
			if updated == nil {
				stuck[v.ID] = true // a concurrent claim/cancel owns it now
			}
		}
	}
}

// skipQueuedJob CASes one queued job to `skipped` without re-entering the
// cascade (the caller drives the waves), and mirrors its check. A job that moved
// on (a claim won the race) is left to its own transition.
func (s *Service) skipQueuedJob(ctx context.Context, owner, repo string, run *Run, runID, jobID string) (*Job, error) {
	now := s.nowUTC().Format(dateTimeFmt)
	var updated *Job
	_, err := s.casUpdate(ctx, JobKey(owner, repo, runID, jobID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
		}
		var job Job
		if jerr := json.Unmarshal(cur, &job); jerr != nil {
			return nil, false, fmt.Errorf("%w: job %s: %v", ErrCorrupt, jobID, jerr)
		}
		if job.State != JobQueued {
			return nil, false, nil // claimed/cancelled meanwhile: its own path owns it
		}
		job.State = JobSkipped
		job.CancelRequested = false
		job.UpdatedAt = now
		job.CompletedAt = &now
		job.Version++
		body, _ := json.Marshal(&job) // cannot fail: the record is pure JSON types
		updated = &job
		return body, true, nil
	})
	if err != nil || updated == nil {
		return nil, err
	}
	if cerr := s.reportCheck(ctx, run, updated); cerr != nil {
		return nil, cerr
	}
	return updated, nil
}
