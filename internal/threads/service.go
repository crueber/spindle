package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// This file owns run creation (15 §2), the run/job reads, the job→checks
// mirror, and the reserved threads-gc task kind.

// --- task kind (Seam 5) ----------------------------------------------------

var (
	kindsMu sync.Mutex
	kinds   = map[string]bool{}
)

// RegisterKind registers a Threads task kind (Seam 5). Composition calls it
// once per process; a duplicate is a wiring bug and panics (the
// maintain.RegisterKind contract in code terms, the repoimport/mirror shape).
func RegisterKind(name string) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if kinds[name] {
		panic(fmt.Sprintf("threads: duplicate task kind %q", name))
	}
	kinds[name] = true
}

// ResetKindsForTest clears the kind registry (test-only: the registry is
// process-global and a -count=N re-run would trip the duplicate panic; the
// production contract stays panic-on-duplicate).
func ResetKindsForTest() {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	kinds = map[string]bool{}
}

// GC is the threads-gc body. T1 writes no log frames and keeps every run
// record, so there is nothing to sweep yet: retention (log chunks and runs
// older than the window) is the T3 pass that owns the log family. The kind is
// registered today so composition names it exactly once and T3 fills the body
// without a wiring change.
func (s *Service) GC(ctx context.Context, owner, repo string) (int, error) {
	_ = owner
	_ = repo
	return 0, nil
}

// --- run creation ----------------------------------------------------------

// CreateRun creates one run per workflow file that fires on the event, reading
// the workflow files from the repository at the trust-selected commit (15 §2):
//
//   - push: the pushed commit's files, trusted.
//   - pull_request, same repository: the head commit's files, trusted.
//   - pull_request, fork: the BASE branch's files, untrusted (no secrets later).
//   - workflow_dispatch: the resolved commit's files, trusted.
//
// A repository with no workflow files — or none that fire — is a silent no-op
// returning an empty slice: no empty run is ever written. Job records are
// written before the run header (the header is the run's commit point, so an
// interrupted creation leaves invisible orphans the gc sweeps, never a
// half-visible run). A malformed workflow file fails the whole call visibly
// (ErrUnprocessable) and writes nothing.
func (s *Service) CreateRun(ctx context.Context, repo, commit, event string, opts CreateOpts) ([]*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	sha, err := normalizeSHA(commit)
	if err != nil {
		return nil, err
	}
	if !validEvent(event) {
		return nil, fmt.Errorf("%w: event must be push|pull_request|workflow_dispatch, got %q", ErrInvalid, event)
	}
	if s.Git == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	// Trust class (15 §2/§5): the server decides, a workflow cannot opt in.
	workflowCommit, trusted := sha, true
	if event == EventPullReq && opts.Fork {
		trusted = false
		workflowCommit = strings.TrimSpace(opts.BaseCommit)
		if workflowCommit == "" {
			base := strings.TrimSpace(opts.BaseBranch)
			if base == "" {
				return nil, fmt.Errorf("%w: a fork pull_request needs its base branch", ErrInvalid)
			}
			resolved, rerr := s.Git.ResolveRef(ctx, repo, "refs/heads/"+base)
			if rerr != nil {
				return nil, rerr
			}
			workflowCommit = resolved
		}
		normalized, nerr := normalizeSHA(workflowCommit)
		if nerr != nil {
			return nil, nerr
		}
		workflowCommit = normalized
	}
	files, err := Discover(ctx, s.Git, repo, workflowCommit)
	if err != nil {
		return nil, err
	}
	want := ""
	if event == EventDispatch {
		want = opts.Workflow
	}
	matched := SelectFiles(files, event, strings.TrimSpace(opts.Branch), want)
	if len(matched) == 0 {
		if event == EventDispatch && want != "" {
			return nil, fmt.Errorf("%w: no workflow_dispatch workflow named %q", ErrNotFound, want)
		}
		return []*Run{}, nil
	}
	attempt := opts.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	actor := strings.TrimSpace(opts.Actor)
	if actor == "" {
		actor = defaultRunActor
	}
	runs := make([]*Run, 0, len(matched))
	for _, f := range matched {
		run, rerr := s.createOne(ctx, owner, name, sha, event, opts, f, workflowCommit, trusted, attempt, actor)
		if rerr != nil {
			return nil, rerr
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// createOne writes one workflow file's run: jobs, then the header, then the
// checks mirrors (a queued check must never exist for a run nobody can see).
func (s *Service) createOne(ctx context.Context, owner, repo, sha, event string, opts CreateOpts, f *WorkflowFile, workflowCommit string, trusted bool, attempt int, actor string) (*Run, error) {
	now := s.nowUTC().Format(dateTimeFmt)
	runID, err := newRunID(s.nowUTC())
	if err != nil {
		return nil, fmt.Errorf("%w: mint run id: %v", ErrUnavailable, err)
	}
	jobKeys := make([]string, 0, len(f.Workflow.Jobs))
	for k := range f.Workflow.Jobs {
		jobKeys = append(jobKeys, k)
	}
	sort.Strings(jobKeys)
	used := map[string]bool{}
	jobs := make([]*Job, 0, len(jobKeys))
	for _, key := range jobKeys {
		def := f.Workflow.Jobs[key]
		display := strings.TrimSpace(def.Name)
		if display == "" {
			display = key
		}
		steps := make([]Step, 0, len(def.Steps))
		for _, st := range def.Steps {
			steps = append(steps, Step{Name: st.Name, Run: st.Run, Uses: st.Uses})
		}
		job := &Job{
			ID: jobIDFor(key, used), RunID: runID, Repo: repoID(owner, repo),
			Key: key, Name: display,
			Context:   CheckContext(f.Name, display),
			RunsOn:    def.RunsOn,
			Container: def.Container,
			Needs:     nonNilStr(def.Needs),
			Steps:     steps,
			Attempt:   attempt,
			Trusted:   trusted,
			State:     JobQueued,
			CreatedAt: now, UpdatedAt: now,
			Version: 1,
		}
		body, _ := json.Marshal(job) // cannot fail: the record is pure JSON types
		if perr := s.putCreate(ctx, JobKey(owner, repo, runID, job.ID), body); perr != nil {
			if store.IsPreconditionFailed(perr) {
				return nil, fmt.Errorf("%w: job %s already exists", ErrConflict, job.ID)
			}
			return nil, perr
		}
		jobs = append(jobs, job)
	}
	run := &Run{
		ID: runID, Repo: repoID(owner, repo), Commit: sha, Event: event,
		Branch: strings.TrimSpace(opts.Branch), Ref: strings.TrimSpace(opts.Ref),
		Workflow: f.Name, WorkflowPath: f.Path, WorkflowCommit: workflowCommit,
		WorkflowDigest: f.Digest, Trusted: trusted, State: RunQueued, Actor: actor,
		Attempt: attempt, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	body, _ := json.Marshal(run) // cannot fail: the record is pure JSON types
	if perr := s.putCreate(ctx, RunKey(owner, repo, runID), body); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: run %s already exists", ErrConflict, runID)
		}
		return nil, perr
	}
	for _, job := range jobs {
		if cerr := s.reportCheck(ctx, run, job); cerr != nil {
			return nil, cerr
		}
	}
	return run, nil
}

// reportCheck mirrors one job state into the checks surface (nil Checks = no
// integration). The description is bounded (05 §2: ≤ 256 chars).
func (s *Service) reportCheck(ctx context.Context, run *Run, job *Job) error {
	if s.Checks == nil {
		return nil
	}
	desc := fmt.Sprintf("%s / %s (run %s)", run.Workflow, job.Name, run.ID)
	if len([]rune(desc)) > 256 {
		desc = string([]rune(desc)[:253]) + "…"
	}
	if err := s.Checks.Report(ctx, run.Repo, run.Commit, job.Context, checkState(job.State), desc, job.StartedAt, job.CompletedAt); err != nil {
		return fmt.Errorf("%w: checks mirror: %v", ErrUnavailable, err)
	}
	return nil
}

// DispatchRun resolves a ref and creates a workflow_dispatch run (the manual
// API/page path). Write role is required. An empty workflow name dispatches
// every file that declares the trigger.
func (s *Service) DispatchRun(ctx context.Context, repo, ref, workflowRef string, p auth.Principal) ([]*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "write"); err != nil {
		return nil, err
	}
	if s.Git == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("%w: ref is required", ErrInvalid)
	}
	commit, err := s.Git.ResolveRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	branch := ""
	if rest, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		branch = rest
	}
	return s.CreateRun(ctx, repo, commit, EventDispatch, CreateOpts{
		Branch: branch, Ref: ref, Actor: string(p.Name), Workflow: strings.TrimSpace(workflowRef),
	})
}

// --- reads -----------------------------------------------------------------

// GetRun reads one run header plus its jobs (a run get is by id: one GET + one
// bounded prefix LIST over the run's own job keys, never a repo-wide scan).
func (s *Service) GetRun(ctx context.Context, repo, runID string, p auth.Principal) (*RunView, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	if !runIDRe.MatchString(strings.TrimSpace(runID)) {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	run, err := s.loadRun(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	jobs, err := s.loadJobs(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	return &RunView{Run: run, Jobs: jobs}, nil
}

// ListRuns pages the run headers newest-first. after is the last run id of the
// previous page; n defaults to 50 and caps at 200 (P5). The list is one prefix
// LIST behind a page request — a run get stays by id.
func (s *Service) ListRuns(ctx context.Context, repo string, p auth.Principal, after string, n int) (*RunPage, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	after = strings.TrimSpace(after)
	if after != "" && !runIDRe.MatchString(after) {
		return nil, fmt.Errorf("%w: invalid after cursor", ErrInvalid)
	}
	if n <= 0 {
		n = defaultPageSize
	}
	if n > maxPageSize {
		n = maxPageSize
	}
	ids, err := s.runIDs(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	// Newest first.
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	page := &RunPage{Runs: []*Run{}}
	for _, id := range ids {
		if after != "" && id >= after {
			continue
		}
		if len(page.Runs) == n {
			page.More = true
			break
		}
		run, rerr := s.loadRun(ctx, owner, name, id)
		if rerr != nil {
			return nil, rerr
		}
		if run == nil {
			continue // raced a retention delete
		}
		page.Runs = append(page.Runs, run)
	}
	return page, nil
}

// runIDs lists the direct run headers under RunsPrefix (the nested jobs are
// filtered out by the single-segment rule).
func (s *Service) runIDs(ctx context.Context, owner, repo string) ([]string, error) {
	prefix := RunsPrefix(owner, repo)
	var ids []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest, ok := strings.CutPrefix(m.Key, prefix)
		if !ok || strings.Contains(rest, "/") {
			return nil
		}
		id, ok := strings.CutSuffix(rest, ".json")
		if ok && runIDRe.MatchString(id) {
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

// loadRun reads one run header; (nil, nil) when absent.
func (s *Service) loadRun(ctx context.Context, owner, repo, runID string) (*Run, error) {
	raw, _, err := s.getJSON(ctx, RunKey(owner, repo, runID))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var run Run
	if jerr := json.Unmarshal(raw, &run); jerr != nil {
		return nil, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
	}
	return &run, nil
}

// loadJobs reads a run's job records, name-sorted (the deterministic order the
// page and the fold use).
func (s *Service) loadJobs(ctx context.Context, owner, repo, runID string) ([]*Job, error) {
	prefix := JobsPrefix(owner, repo, runID)
	var keys []string
	if err := s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		rest, ok := strings.CutPrefix(m.Key, prefix)
		if !ok || strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".json") {
			return nil
		}
		keys = append(keys, m.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	jobs := make([]*Job, 0, len(keys))
	for _, k := range keys {
		raw, _, err := s.getJSON(ctx, k)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var job Job
		if jerr := json.Unmarshal(raw, &job); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, k, jerr)
		}
		jobs = append(jobs, &job)
	}
	return jobs, nil
}

// --- job transitions (the T2 runner's write surface) -----------------------

// JobUpdate is one attempt-scoped job transition. Empty fields are unchanged;
// a terminal State stamps CompletedAt (and clears it again if a requeue moves
// the job back to a live state).
type JobUpdate struct {
	State       string
	StartedAt   *string
	CompletedAt *string
}

// UpdateJob CAS-updates one job record, mirrors the new state into the checks
// surface, and folds the run header's state/timings (the CAS'd job is the
// commit point; the run header is a projection of its jobs). Used by the T2
// runner protocol; T1's tests drive it directly.
func (s *Service) UpdateJob(ctx context.Context, repo, runID, jobID string, up JobUpdate) (*Job, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if up.State != "" && !validJobState(up.State) {
		return nil, fmt.Errorf("%w: state must be queued|running|success|failure|cancelled, got %q", ErrInvalid, up.State)
	}
	if !runIDRe.MatchString(strings.TrimSpace(runID)) || !jobIDRe.MatchString(strings.TrimSpace(jobID)) {
		return nil, fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
	}
	run, err := s.loadRun(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	now := s.nowUTC().Format(dateTimeFmt)
	key := JobKey(owner, name, runID, jobID)
	var updated *Job
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
		}
		var job Job
		if jerr := json.Unmarshal(cur, &job); jerr != nil {
			return nil, false, fmt.Errorf("%w: job %s: %v", ErrCorrupt, jobID, jerr)
		}
		if up.State != "" {
			job.State = up.State
		}
		job.UpdatedAt = now
		switch {
		case up.StartedAt != nil:
			job.StartedAt = up.StartedAt
		case job.State == JobRunning && job.StartedAt == nil:
			job.StartedAt = &now
		}
		switch {
		case up.CompletedAt != nil:
			job.CompletedAt = up.CompletedAt
		case terminal(job.State):
			job.CompletedAt = &now
		default:
			job.CompletedAt = nil
		}
		job.Version++
		body, _ := json.Marshal(&job) // cannot fail: the record is pure JSON types
		updated = &job
		return body, true, nil
	})
	if err != nil {
		return nil, err
	}
	// Mirror the check after the job CAS commits (P8: the CAS is the commit
	// point; a lost mirror self-heals on the next transition).
	if cerr := s.reportCheck(ctx, run, updated); cerr != nil {
		return nil, cerr
	}
	if ferr := s.foldRun(ctx, owner, name, runID); ferr != nil {
		return nil, ferr
	}
	return updated, nil
}

// foldRun recomputes the run header's state and timings from its jobs (CAS'd;
// a concurrent loser re-reads and converges to the same fold).
func (s *Service) foldRun(ctx context.Context, owner, repo, runID string) error {
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		return err
	}
	state := foldRunState(jobs)
	now := s.nowUTC().Format(dateTimeFmt)
	key := RunKey(owner, repo, runID)
	_, err = s.casUpdate(ctx, key, 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
		}
		var run Run
		if jerr := json.Unmarshal(cur, &run); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		if run.State == state && (run.CompletedAt != nil) == terminal(state) {
			return nil, false, nil // already folded
		}
		run.State = state
		run.UpdatedAt = now
		run.StartedAt = earliestStart(jobs)
		if terminal(state) {
			run.CompletedAt = &now
		} else {
			run.CompletedAt = nil
		}
		run.Version++
		body, _ := json.Marshal(&run) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	return err
}

// earliestStart picks the first job start time (nil when no job started).
func earliestStart(jobs []*Job) *string {
	var first *string
	for _, j := range jobs {
		if j.StartedAt == nil {
			continue
		}
		if first == nil || *j.StartedAt < *first {
			started := *j.StartedAt
			first = &started
		}
	}
	return first
}
