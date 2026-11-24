package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/crueber/spindle/internal/store"
)

// `needs:` dependency gating (15 §4.1, round 2) and the job-level `if:` that
// reads it (round 4, 2026-11-21). The queue owns eligibility: a job whose
// `needs` are not all terminal is never offered to a runner, and once they are
// terminal the job's `if:` decides whether it runs:
//
//   - no `if:`  → the default: every dependency must be `success`; otherwise
//     the job is terminalised **skipped** (GitHub reads a dependent that never
//     ran as `skipped`, not `cancelled`; a genuinely cancelled job keeps
//     `cancelled`), which cascades to its own dependents;
//   - an `if:` that evaluates true → the job runs even when a dependency failed
//     (`if: always()`, `if: needs.build.result == 'failure'`), which is what the
//     context is for;
//   - an `if:` that evaluates false → skipped, with the condition on the record;
//   - an `if:` outside the documented subset → the job fails with the error (a
//     condition spindle cannot read is an author error, never a silent skip).
//
// The runner knows nothing about ordering or job conditions.
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
// state. Hazard: the cascade recursing through UpdateJob (which runs it).
// Avoidance: the cascade writes terminal jobs with its own CAS, never through
// UpdateJob; the caller's fold runs once per wave.

// needsGate is one queue entry's dependency verdict.
type needsGate int

const (
	// needsReady: every dependency is terminal — the job's `if:` may decide.
	needsReady needsGate = iota
	// needsWaiting: at least one dependency is still queued/running.
	needsWaiting
	// needsBlocked: a dependency record cannot be read, so the job can never
	// be evaluated (it is skipped rather than left queued forever).
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

// needIDs flattens a key → ids map into the deduplicated, sorted id list the
// queue entry carries and the claim gate walks.
func needIDs(needs map[string][]string) []string {
	if len(needs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(needs))
	for _, ids := range needs {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
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

// runCache is one claim probe's run-header cache (the dependent gate reads the
// header for the github.* context of a job `if:`).
type runCache map[string]*Run

// loadRunCached reads one run header through the per-probe cache.
func (s *Service) loadRunCached(ctx context.Context, owner, repo, runID string, cache runCache) (*Run, error) {
	if cache != nil {
		if r, ok := cache[runID]; ok {
			return r, nil
		}
	}
	run, err := s.loadRun(ctx, owner, repo, runID)
	if err != nil {
		return nil, err
	}
	if cache != nil {
		cache[runID] = run
	}
	return run, nil
}

// needsGate evaluates one queue entry's dependencies: ready when every record
// is terminal (whatever the results — the job's `if:` decides), waiting while
// one is live, blocked when a record is missing (the dependent is skipped: a
// prerequisite that cannot be read may never run silently beside it).
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
		if !terminal(dep.State) {
			verdict = needsWaiting
		}
	}
	return verdict, nil
}

// needFacts is the folded view of one job's `needs:`: each dependency key's
// aggregate result, its merged outputs, and whether the fold is still waiting
// on (or missing) a dependency record.
type needFacts struct {
	// Results maps a `needs:` job key to the aggregate result (the worst cell
	// for a matrix dependency: failure > cancelled > skipped > success).
	Results map[string]string
	// Outputs maps a `needs:` job key to the dependency's outputs. Only a
	// dependency that succeeded contributes: a failed, cancelled or skipped job
	// exposes no outputs (docs/threads.md §3.9).
	Outputs map[string]map[string]string
	// Waiting reports a dependency that is still queued/running; Missing one
	// whose record could not be read.
	Waiting bool
	Missing bool
}

// foldNeeds folds the dependency records of one job. For a matrix dependency
// every cell is folded: `needs.<job>.result` is the worst cell's state, and
// `needs.<job>.outputs.<name>` is the union of the cells' outputs with the LAST
// cell in job-id order winning a name collision — deterministic, unlike
// GitHub's run-order rule, and only when the whole dependency succeeded.
func foldNeeds(needs map[string][]string, byID map[string]*Job) needFacts {
	facts := needFacts{Results: make(map[string]string, len(needs))}
	for key, ids := range needs {
		result := JobSuccess
		merged := map[string]string{}
		for _, id := range ids {
			dep := byID[id]
			if dep == nil {
				facts.Missing = true
				return needFacts{Missing: true}
			}
			if !terminal(dep.State) {
				facts.Waiting = true
				continue
			}
			result = worseResult(result, dep.State)
			if dep.State == JobSuccess {
				for name, val := range dep.Outputs {
					merged[name] = val
				}
			}
		}
		// A dependency that did not wholly succeed exposes no outputs at all:
		// discard what successful siblings contributed.
		if result != JobSuccess {
			merged = map[string]string{}
		}
		facts.Results[key] = result
		// Keep the key even when it contributes nothing (a failed dependency,
		// or a successful one with no outputs): a dependent's fail-closed
		// message then says "did not publish" for a real dependency rather than
		// "no needs dependency". No VALUE from a non-successful dependency is
		// ever merged above.
		if facts.Outputs == nil {
			facts.Outputs = make(map[string]map[string]string, len(needs))
		}
		facts.Outputs[key] = merged
	}
	return facts
}

// worseResult is the precedence order of a folded dependency result.
func worseResult(current, next string) string {
	rank := func(state string) int {
		switch state {
		case JobSuccess:
			return 0
		case JobSkipped:
			return 1
		case JobCancelled:
			return 2
		case JobFailure:
			return 3
		default:
			return 4
		}
	}
	if rank(next) > rank(current) {
		return next
	}
	return current
}

// dependentVerdict decides a dependent job's `if:` once its `needs:` are
// terminal (the claim probe's own copy of the decision the cascade normally
// makes, so a cascade that did not run cannot leak a job).
func (s *Service) dependentVerdict(ctx context.Context, owner, repo string, entry *QueueEntry, cache jobCache, runs runCache) (jobIfVerdict, string, error) {
	job, err := s.loadJobCached(ctx, owner, repo, entry.RunID, entry.JobID, cache)
	if err != nil {
		return jobIfRun, "", err
	}
	if job == nil {
		// The claim path retires a missing job's entry; nothing to decide.
		return jobIfRun, "", nil
	}
	run, err := s.loadRunCached(ctx, owner, repo, entry.RunID, runs)
	if err != nil {
		return jobIfRun, "", err
	}
	facts := needFacts{Results: map[string]string{}}
	if len(entry.NeedKeys) > 0 {
		byID := make(map[string]*Job, len(entry.NeedKeys))
		for _, ids := range entry.NeedKeys {
			for _, id := range ids {
				dep, derr := s.loadJobCached(ctx, owner, repo, entry.RunID, id, cache)
				if derr != nil {
					return jobIfRun, "", derr
				}
				if dep != nil {
					byID[id] = dep
				}
			}
		}
		facts = foldNeeds(entry.NeedKeys, byID)
	} else {
		// An entry written before NeedKeys existed still gates correctly: the
		// results are keyed by job id (the default condition only needs the
		// states; a `needs.<key>.result` condition reads empty and fails).
		for _, id := range entry.Needs {
			dep, derr := s.loadJobCached(ctx, owner, repo, entry.RunID, id, cache)
			if derr != nil {
				return jobIfRun, "", derr
			}
			if dep != nil {
				facts.Results[id] = dep.State
			}
		}
	}
	verdict, reason, verr := evaluateJobIf(job, run, facts)
	if verr != nil {
		return jobIfError, reason, nil
	}
	return verdict, reason, nil
}

// settleQueuedJob terminalises one queued job through UpdateJob (so the checks
// mirror, the run fold and the transitive cascade are the same code every other
// terminal transition uses) with the reason the record and page show.
func (s *Service) settleQueuedJob(ctx context.Context, owner, repo string, entry *QueueEntry, state, reason string) (*Job, error) {
	return s.UpdateJob(ctx, repoID(owner, repo), entry.RunID, entry.JobID, JobUpdate{
		State: state, Reason: reason, CancelRequested: boolPtr(false),
	})
}

// skipBlockedDependents terminalises every still-queued job of a run whose
// `needs:` can never lead to a run (default condition) or whose job-level `if:`
// is false, wave by wave (so a skipped job's dependents are skipped too). It
// runs after any terminal job transition — no runner poll is needed for a
// doomed dependent to conclude — and finishes with one fold.
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
	// The loop terminates: a wave that concludes nothing ends it, and a victim
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
		type victim struct {
			id     string
			state  string
			reason string
		}
		var victims []victim
		for _, j := range jobs {
			if j.State != JobQueued || len(j.Needs) == 0 || stuck[j.ID] {
				continue
			}
			facts := foldNeeds(entryNeedMap(idx, j), byID)
			if facts.Waiting {
				continue // a dependency is still live; its transition re-runs this
			}
			if facts.Missing {
				victims = append(victims, victim{id: j.ID, state: JobSkipped,
					reason: "needs: a dependency record could not be read"})
				continue
			}
			verdict, reason, _ := evaluateJobIf(j, run, facts)
			switch verdict {
			case jobIfSkip:
				victims = append(victims, victim{id: j.ID, state: JobSkipped, reason: reason})
			case jobIfError:
				victims = append(victims, victim{id: j.ID, state: JobFailure, reason: reason})
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
			updated, cerr := s.concludeQueuedJob(ctx, owner, repo, run, runID, v.id, v.state, v.reason)
			if cerr != nil {
				return cerr
			}
			if updated == nil {
				stuck[v.id] = true // a concurrent claim/cancel owns it now
			}
		}
	}
}

// skipQueuedJob skips one queued job without re-entering the cascade (the
// caller drives the waves), and mirrors its check. A job that moved on (a claim
// won the race) is left to its own transition.
func (s *Service) skipQueuedJob(ctx context.Context, owner, repo string, run *Run, runID, jobID string) (*Job, error) {
	return s.concludeQueuedJob(ctx, owner, repo, run, runID, jobID, JobSkipped, "")
}

// concludeQueuedJob CASes one queued job to a terminal state with a reason
// (skipped or failed) and mirrors its check. It writes the record directly
// rather than through UpdateJob: the cascade is UpdateJob's own tail, and a
// re-entrant call would fold and cascade again per job.
func (s *Service) concludeQueuedJob(ctx context.Context, owner, repo string, run *Run, runID, jobID, state, reason string) (*Job, error) {
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
		job.State = state
		job.Reason = reason
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
