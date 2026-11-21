package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

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

// MaxQueueAttempts bounds requeues of one job: past it the job fails rather
// than cycling forever (12_runner D3's attempts cap, now backed by attempts
// instead of a counter field).
const MaxQueueAttempts = 3

// GC is the threads-gc body (Seam 5): it requeues lapsed claims, fails a job
// that has exhausted its attempts, and trims retired entries out of the queue
// head. It is a maintenance pass over one repository's head window — never a
// claim path (15 §3 keeps that to one probe) and never a repo-wide LIST.
// Retention of old run/log objects stays T3 (the log family is young).
func (s *Service) GC(ctx context.Context, owner, repo string) (int, error) {
	if s == nil || s.Store == nil {
		return 0, nil
	}
	head, _, err := s.loadHead(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	if head == nil {
		return 0, nil
	}
	swept := 0
	drop := map[string]bool{}
	for _, seq := range head.Pending {
		entry, ver, eerr := s.loadEntry(ctx, owner, repo, seq)
		if eerr != nil {
			return swept, eerr
		}
		if entry == nil || entry.State == QueueDone {
			drop[seq] = true
			continue
		}
		if entry.State != QueueClaimed || !s.claimExpired(entry) {
			continue
		}
		if entry.Attempt >= MaxQueueAttempts {
			if _, uerr := s.UpdateJob(ctx, repoID(owner, repo), entry.RunID, entry.JobID, JobUpdate{
				State: JobFailure, CancelRequested: boolPtr(false),
			}); uerr != nil && !errors.Is(uerr, ErrNotFound) {
				return swept, uerr
			}
			s.finishEntry(ctx, owner, repo, entry.Seq)
			drop[seq] = true
			swept++
			continue
		}
		if rerr := s.requeueEntry(ctx, owner, repo, entry, ver); rerr != nil {
			return swept, rerr
		}
		swept++
	}
	s.trimHead(ctx, owner, repo, drop)
	return swept, nil
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
		return nil, fmt.Errorf("%w: event must be push|pull_request|workflow_dispatch|schedule, got %q", ErrInvalid, event)
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
			// A caller may hand the base as a ref or a plain branch name; both
			// resolve to the same tip.
			base := strings.TrimPrefix(strings.TrimSpace(opts.BaseBranch), "refs/heads/")
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
	// A dispatch and a scheduled tick both name exactly ONE file (the file's
	// inputs or cron, respectively, were what selected it).
	want := ""
	if event == EventDispatch || event == EventSchedule {
		want = strings.TrimSpace(opts.Workflow)
	}
	if broken := brokenWorkflowFiles(files, want); len(broken) > 0 {
		if want != "" {
			// A dispatch naming one file must fail loudly on that file's
			// error rather than report "not found".
			return nil, fmt.Errorf("%w: %s: %s", ErrUnprocessable, broken[0].Path, broken[0].Error)
		}
		paths := make([]string, 0, len(broken))
		for _, f := range broken {
			paths = append(paths, f.Path)
		}
		slog.Warn("threads: workflow files failed to parse; their workflows are disabled, the rest still run",
			"repo", repo, "commit", workflowCommit, "files", paths)
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
	// First-time-contributor gate (15 §5.2): decided once per event, before any
	// run exists, so every workflow file of one fork PR is held or released
	// together.
	gate := s.needsApproval(ctx, owner, name, actor, opts, event)
	runs := make([]*Run, 0, len(matched))
	for _, f := range matched {
		run, rerr := s.createOne(ctx, owner, name, sha, event, opts, f, workflowCommit, trusted, attempt, actor, gate)
		if rerr != nil {
			return nil, rerr
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// createOne writes one workflow file's run: jobs, then the header, then the
// checks mirrors (a queued check must never exist for a run nobody can see).
func (s *Service) createOne(ctx context.Context, owner, repo, sha, event string, opts CreateOpts, f *WorkflowFile, workflowCommit string, trusted bool, attempt int, actor string, gate bool) (*Run, error) {
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
		base := buildSpec(f.Name, f.Workflow, def, opts.Inputs)
		instances, xerr := expandJobSpec(key, display, base)
		if xerr != nil {
			return nil, xerr
		}
		for _, inst := range instances {
			spec := inst.Spec
			job := &Job{
				ID: jobIDFor(inst.Key, used), RunID: runID, Repo: repoID(owner, repo),
				Key: inst.Key, Name: inst.Name,
				Context:   CheckContext(spec.Workflow, inst.Name),
				RunsOn:    strings.Join(spec.RunsOn, ", "),
				Container: containerImage(spec.Container),
				Needs:     nonNilStr(def.Needs),
				Steps:     convenienceSteps(spec.Steps),
				Spec:      spec,
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
	}
	state := RunQueued
	if gate {
		state = RunPendingApproval
	}
	run := &Run{
		ID: runID, Repo: repoID(owner, repo), Commit: sha, Event: event,
		Branch: strings.TrimSpace(opts.Branch), Ref: strings.TrimSpace(opts.Ref),
		Workflow: f.Name, WorkflowPath: f.Path, WorkflowCommit: workflowCommit,
		WorkflowDigest: f.Digest, Trusted: trusted, State: state, Actor: actor,
		Attempt: attempt, Concurrency: runGroups(jobs),
		Inputs:    runInputs(opts),
		CreatedAt: now, UpdatedAt: now, Version: 1,
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
	if gate {
		// The approval gate IS the queue: the jobs exist (the page, the checks
		// mirror and the PR panel stay coherent) but no runner can see them
		// until ApproveRun enqueues them (15 §5.2). No wake: there is nothing to
		// claim.
		return run, nil
	}
	// Enqueue AFTER the run header (the header is the commit point, so a runner
	// can never claim a job of an invisible run). A queue failure is logged and
	// leaves the job queued exactly like a pre-runner instance (15 §8): the run
	// page shows it, and threads-gc re-enqueues it.
	idx := needsIndex(jobs)
	for _, job := range jobs {
		if _, eerr := s.enqueueJob(ctx, owner, repo, job, entryNeeds(idx, job)); eerr != nil {
			slog.Warn("threads: enqueue job", "repo", repoID(owner, repo), "run", runID, "job", job.ID, "err", eerr)
		}
	}
	fetchHub.wake(repoID(owner, repo))
	return run, nil
}

// reportCheck mirrors one job state into the checks surface (nil Checks = no
// integration). The description is bounded (05 §2: ≤ 256 chars). The workflow
// label comes from the job's spec, so the mirror never needs the workflow file
// or the run header.
func (s *Service) reportCheck(ctx context.Context, run *Run, job *Job) error {
	if s.Checks == nil {
		return nil
	}
	desc := fmt.Sprintf("%s / %s (run %s)", job.specWorkflow(run.Workflow), job.Name, run.ID)
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
// every file that declares the trigger; naming a workflow is required when
// inputs are supplied (inputs are validated against THAT file's declared
// workflow_dispatch.inputs schema, §1). A mismatch is ErrUnprocessable (422)
// and the message names the offending input.
func (s *Service) DispatchRun(ctx context.Context, repo, ref, workflowRef string, inputs map[string]string, p auth.Principal) ([]*Run, error) {
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
	workflowRef = strings.TrimSpace(workflowRef)
	if len(inputs) > 0 && workflowRef == "" {
		return nil, fmt.Errorf("%w: inputs need an explicit workflow (one file declares their schema)", ErrUnprocessable)
	}
	commit, err := s.Git.ResolveRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	// Validate before any run exists: a bad input is a request error, not a
	// half-created run.
	var effective map[string]string
	if workflowRef != "" {
		files, derr := Discover(ctx, s.Git, repo, commit)
		if derr != nil {
			return nil, derr
		}
		target := SelectFiles(files, EventDispatch, "", workflowRef)
		if len(target) == 0 {
			if broken := brokenWorkflowFiles(files, workflowRef); len(broken) > 0 {
				return nil, fmt.Errorf("%w: %s: %s", ErrUnprocessable, broken[0].Path, broken[0].Error)
			}
			return nil, fmt.Errorf("%w: no workflow_dispatch workflow named %q", ErrNotFound, workflowRef)
		}
		resolved, verr := resolveDispatchInputs(target[0], inputs)
		if verr != nil {
			return nil, verr
		}
		effective = resolved
	}
	branch := ""
	if rest, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		branch = rest
	}
	return s.CreateRun(ctx, repo, commit, EventDispatch, CreateOpts{
		Branch: branch, Ref: ref, Actor: string(p.Name), Workflow: workflowRef, Inputs: effective,
	})
}

// runInputs copies the dispatch inputs onto the run header (nil when none).
func runInputs(opts CreateOpts) map[string]string {
	if len(opts.Inputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(opts.Inputs))
	for k, v := range opts.Inputs {
		out[k] = v
	}
	return out
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
	s.fillDurations(run, jobs)
	return &RunView{Run: run, Jobs: jobs}, nil
}

// fillDurations sets the derived DurationSeconds on a run and its jobs (a read
// model only: the value is never stored or CAS'd). A live item measures to now;
// a never-started one stays zero.
func (s *Service) fillDurations(run *Run, jobs []*Job) {
	now := s.nowUTC()
	if run != nil {
		run.DurationSeconds = elapsedSeconds(run.StartedAt, run.CompletedAt, now)
	}
	for _, j := range jobs {
		if j == nil {
			continue
		}
		j.DurationSeconds = elapsedSeconds(j.StartedAt, j.CompletedAt, now)
	}
}

// elapsedSeconds is completed − started (or now − started while live). A job
// with no start has no duration; a completed timestamp without a start still
// yields zero rather than a nonsense value.
func elapsedSeconds(started, completed *string, now time.Time) int64 {
	if started == nil || strings.TrimSpace(*started) == "" {
		return 0
	}
	start, err := time.Parse(dateTimeFmt, strings.TrimSpace(*started))
	if err != nil {
		return 0
	}
	end := now
	if completed != nil && strings.TrimSpace(*completed) != "" {
		if parsed, perr := time.Parse(dateTimeFmt, strings.TrimSpace(*completed)); perr == nil {
			end = parsed
		}
	}
	if end.Before(start) {
		return 0
	}
	return int64(end.Sub(start) / time.Second)
}

// RunFilter narrows the run list (round-2 pass 2: the list's workflow, state
// and actor filters). Empty fields do not filter.
type RunFilter struct {
	Workflow string
	State    string
	Actor    string
}

// empty reports whether nothing is filtered.
func (f RunFilter) empty() bool { return f.Workflow == "" && f.State == "" && f.Actor == "" }

// match reports whether one run satisfies the filter.
func (f RunFilter) match(r *Run) bool {
	if f.Workflow != "" && !strings.EqualFold(r.Workflow, f.Workflow) && !strings.EqualFold(r.WorkflowPath, f.Workflow) {
		return false
	}
	if f.State != "" && !strings.EqualFold(r.State, f.State) {
		return false
	}
	if f.Actor != "" && !strings.EqualFold(r.Actor, f.Actor) {
		return false
	}
	return true
}

// filterScanMax bounds a filtered list's header scan (a filter must never turn
// one page request into a whole-repo walk).
const filterScanMax = 1000

// ListRuns pages the run headers newest-first (no filter). after is the last
// run id of the previous page; n defaults to 50 and caps at 200 (P5).
func (s *Service) ListRuns(ctx context.Context, repo string, p auth.Principal, after string, n int) (*RunPage, error) {
	return s.ListRunsFiltered(ctx, repo, p, after, n, RunFilter{})
}

// ListRunsFiltered pages the run headers newest-first, optionally narrowed by
// workflow/state/actor. The list is one prefix LIST behind a page request — a
// run get stays by id — and a filtered page scans at most filterScanMax headers.
func (s *Service) ListRunsFiltered(ctx context.Context, repo string, p auth.Principal, after string, n int, f RunFilter) (*RunPage, error) {
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
	scanned := 0
	for _, id := range ids {
		if after != "" && id >= after {
			continue
		}
		if len(page.Runs) == n {
			page.More = true
			break
		}
		if !f.empty() {
			if scanned >= filterScanMax {
				page.More = true
				break
			}
			scanned++
		}
		run, rerr := s.loadRun(ctx, owner, name, id)
		if rerr != nil {
			return nil, rerr
		}
		if run == nil {
			continue // raced a retention delete
		}
		if !f.match(run) {
			continue
		}
		s.fillDurations(run, nil)
		page.Runs = append(page.Runs, run)
	}
	return page, nil
}

// ListWorkflows reports every workflow file at commit and its parse state (the
// Threads page and GET …/api/threads/workflows). commit defaults to the
// repository's default branch tip; an empty repository is an empty list, not an
// error. Discovery is per file, so a broken workflow is one row with its Error
// and the others still list (15 §2.1). The source of truth is git — nothing is
// persisted, so the list can never go stale.
func (s *Service) ListWorkflows(ctx context.Context, repo, commit string, p auth.Principal) (*WorkflowList, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	if s.Git == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		resolved, rerr := s.Git.ResolveRef(ctx, repo, "HEAD")
		if rerr != nil {
			if errors.Is(rerr, ErrNotFound) {
				return &WorkflowList{Workflows: []*WorkflowState{}}, nil // empty repo
			}
			return nil, rerr
		}
		commit = resolved
	} else if _, nerr := normalizeSHA(commit); nerr != nil {
		return nil, nerr
	}
	files, derr := Discover(ctx, s.Git, repo, commit)
	if derr != nil {
		return nil, derr
	}
	list := &WorkflowList{Commit: commit, Workflows: make([]*WorkflowState, 0, len(files))}
	for _, f := range files {
		list.Workflows = append(list.Workflows, f.State())
	}
	return list, nil
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
	State string
	// Attempt advances the job's attempt (a lease requeue or a rerun). It is
	// the identity authority: every attempt-scoped write compares against it.
	Attempt int
	// ClearTimings resets started/completed (a requeue or rerun does).
	ClearTimings bool
	// QueueSeq records the queue entry the attempt was claimed from.
	QueueSeq string
	// Runner/RunnerName record the claim's attribution (set once per attempt).
	Runner          string
	RunnerName      string
	CancelRequested *bool
	StepResults     []StepResult
	LeaseUntil      *string
	StartedAt       *string
	CompletedAt     *string
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
		if up.Attempt > 0 {
			job.Attempt = up.Attempt
		}
		if up.QueueSeq != "" {
			job.QueueSeq = up.QueueSeq
		}
		if up.Runner != "" {
			job.Runner = up.Runner
		}
		if up.RunnerName != "" {
			job.RunnerName = up.RunnerName
		}
		if up.CancelRequested != nil {
			job.CancelRequested = *up.CancelRequested
		}
		if up.StepResults != nil {
			job.StepResults = up.StepResults
		}
		if up.LeaseUntil != nil {
			job.LeaseUntil = up.LeaseUntil
		}
		job.UpdatedAt = now
		if up.ClearTimings {
			job.StartedAt = nil
			job.CompletedAt = nil
			job.LeaseUntil = nil
			job.QueueSeq = ""
			job.Runner = ""
			job.RunnerName = ""
			job.StepResults = nil
		}
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
			job.LeaseUntil = nil // a terminal attempt holds no lease
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
	jobs, ferr := s.foldRun(ctx, owner, name, runID)
	if ferr != nil {
		return nil, ferr
	}
	if terminal(updated.State) {
		// `needs:` dependents (15 §4.1): a concluded job unblocks its
		// dependents (the wake below) or dooms them (a failed/cancelled need
		// terminalises them as cancelled, transitively). No runner poll is
		// needed for the cascade to converge. The fold's job snapshot is
		// reused, so a run with no `needs` costs zero extra reads.
		if serr := s.skipBlockedDependents(ctx, owner, name, runID, jobs); serr != nil {
			return nil, serr
		}
		// A dependent may now be claimable in the same process; nudge the
		// long-polling fetches instead of waiting for their 2 s re-probe.
		fetchHub.wake(repoID(owner, name))
	}
	return updated, nil
}

// foldRun recomputes the run header's state and timings from its jobs (CAS'd;
// a concurrent loser re-reads and converges to the same fold). A terminal fold
// releases the run's concurrency groups (pass B2). It returns the job snapshot
// it folded, so the `needs:` cascade reuses it instead of LISTing again.
func (s *Service) foldRun(ctx context.Context, owner, repo, runID string) ([]*Job, error) {
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		return nil, err
	}
	state := foldRunState(jobs)
	now := s.nowUTC().Format(dateTimeFmt)
	key := RunKey(owner, repo, runID)
	var released []string
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
		// The approval gate owns the header until a maintainer decides: a run
		// whose jobs are all still queued must not be "folded" back to queued
		// (that would release it without a decision).
		if run.State == RunPendingApproval && state == RunQueued {
			return nil, false, nil
		}
		run.State = state
		run.UpdatedAt = now
		run.StartedAt = earliestStart(jobs)
		if terminal(state) {
			run.CompletedAt = &now
			released = append([]string(nil), run.Concurrency...)
		} else {
			run.CompletedAt = nil
		}
		run.Version++
		body, _ := json.Marshal(&run) // cannot fail: the record is pure JSON types
		return body, true, nil
	})
	if err == nil && len(released) > 0 {
		s.releaseConcurrency(ctx, owner, repo, runID, released)
	}
	return jobs, err
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
