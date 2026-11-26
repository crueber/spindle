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
	"github.com/crueber/spindle/internal/threads/workflow"
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
// instead of a counter field). It is the default of threads.max_queue_attempts
// (PAR-179); a configured value replaces it within bounds checked at load.
const MaxQueueAttempts = 3

// maxAttempts is the effective requeue ceiling (threads.max_queue_attempts).
func (s *Service) maxAttempts() int {
	if s != nil && s.MaxQueueAttempts > 0 {
		return s.MaxQueueAttempts
	}
	return MaxQueueAttempts
}

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
		if entry.Attempt >= s.maxAttempts() {
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
//   - push: the pushed commit's files, trusted. A push is delivered for a
//     branch (on.push.branches, branch name in Branch) or a tag (on.push.tags,
//     tag name in RefName, no branch).
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
	action := strings.TrimSpace(opts.Action)
	if event == EventPullReq {
		if !workflow.ValidPullRequestType(action) {
			return nil, fmt.Errorf("%w: pull_request action must be one of %s, got %q", ErrInvalid,
				strings.Join(workflow.PullRequestTypes, ", "), action)
		}
	}
	if _, perr := EncodeEventPayload(opts.EventPayload); perr != nil {
		return nil, perr
	}
	if s.Git == nil {
		return nil, fmt.Errorf("%w: git backend not wired", ErrUnavailable)
	}
	// Trust class (15 §2/§5): the server decides, a workflow cannot opt in.
	workflowCommit, trusted := sha, true
	switch {
	case event == EventPullReq && opts.Fork:
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
	case event == EventRelease || event == EventIssues || event == EventIssueComment:
		// GitHub's rule for the repository-level events: the workflow file is
		// read from the DEFAULT branch, never from the release tag or the
		// commenter's checkout. A release run still reports the tag commit.
		defaultCommit, rerr := s.Git.ResolveRef(ctx, repo, "HEAD")
		if rerr != nil {
			return nil, rerr
		}
		workflowCommit = defaultCommit
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
	kind, refName, _ := refTargetOf(event, opts)
	// Path filters (PAR-012): the diff is computed ONCE per event and only when
	// a parsed workflow actually declares a path filter — a repository without
	// them pays zero extra git subprocesses and zero store round trips. The
	// changed-file set then selects the files.
	var changed []string
	pathsKnown := false
	if NeedsChangedPaths(files, event) {
		dr, ok := s.Git.(DiffReader)
		if !ok {
			return nil, fmt.Errorf("%w: path filters need a git reader that can diff", ErrUnavailable)
		}
		from := strings.TrimSpace(opts.BaseSHA)
		mergeBase := false
		if event == EventPullReq && from == "" && strings.TrimSpace(opts.BaseBranch) != "" {
			base := strings.TrimPrefix(strings.TrimSpace(opts.BaseBranch), "refs/heads/")
			resolved, rerr := s.Git.ResolveRef(ctx, repo, "refs/heads/"+base)
			if rerr != nil {
				return nil, rerr
			}
			from = resolved
		}
		if event == EventPullReq && from != "" {
			mergeBase = true
		}
		changed, err = dr.DiffNames(ctx, repo, from, sha, mergeBase)
		if err != nil {
			return nil, err
		}
		pathsKnown = true
	}
	matched := SelectFilesWithChanges(files, event, kind, refName, action, want, changed, pathsKnown)
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

// refTargetOf resolves the ref a CreateOpts describes: kind and name are what
// the trigger globs read (`on.push.branches` by branch, `on.push.tags` by tag),
// and branch is what the run header records. Only a push carries a kind — every
// other event is branch-shaped, so a caller cannot state a kind it does not
// have. A tag push has NO branch (Run.Branch stays empty; the tag lives in
// Run.Ref), which is what keeps a tag named `v1` off any branch-shaped field
// downstream. Name falls back to Branch when the caller set only that.
func refTargetOf(event string, opts CreateOpts) (workflow.RefKind, string, string) {
	kind := workflow.RefBranch
	branch := strings.TrimSpace(opts.Branch)
	if event == EventPush && opts.RefKind == workflow.RefTag {
		kind, branch = workflow.RefTag, ""
	}
	name := strings.TrimSpace(opts.RefName)
	if name == "" {
		name = branch
	}
	return kind, name, branch
}

// createOne writes one workflow file's run: jobs, then the header, then the
// checks mirrors (a queued check must never exist for a run nobody can see).
func (s *Service) createOne(ctx context.Context, owner, repo, sha, event string, opts CreateOpts, f *WorkflowFile, workflowCommit string, trusted bool, attempt int, actor string, gate bool) (*Run, error) {
	_, _, branch := refTargetOf(event, opts)
	nowAt := s.nowUTC()
	now := nowAt.Format(dateTimeFmt)
	runID, err := newRunID(s.nowUTC())
	if err != nil {
		return nil, fmt.Errorf("%w: mint run id: %v", ErrUnavailable, err)
	}
	jobKeys := make([]string, 0, len(f.Workflow.Jobs))
	for k := range f.Workflow.Jobs {
		jobKeys = append(jobKeys, k)
	}
	sort.Strings(jobKeys)
	// The header is built before the job loop: a job-level `if:` reads the
	// github.* context (commit, event, branch, run id), and the header is that
	// context. It is written after the jobs (the header is the commit point).
	run := &Run{
		ID: runID, Repo: repoID(owner, repo), Commit: sha, Event: event,
		Branch: branch, Ref: strings.TrimSpace(opts.Ref),
		Workflow: f.Name, WorkflowPath: f.Path, WorkflowCommit: workflowCommit,
		WorkflowDigest: f.Digest, Trusted: trusted, State: RunQueued, Actor: actor,
		Attempt:      attempt,
		Inputs:       runInputs(opts),
		EventPayload: opts.EventPayload,
		CreatedAt:    now, UpdatedAt: now, Version: 1,
	}
	used := map[string]bool{}
	jobs := make([]*Job, 0, len(jobKeys))
	// One resolver per workflow file: its jobs share action directories, and a
	// definition is read from git once. It is pinned to the RUN commit (the
	// head the runner checks out), so a fork PR's local actions are the fork's
	// checked-out code and a base-branch workflow can never execute an action
	// file from another ref (docs/threads.md §3.4a).
	resolver := newActionResolver(s.Git, repoID(owner, repo), sha)
	// The job list is expanded first: a local reusable-workflow caller is
	// replaced IN PLACE by its callee's jobs (PAR-075, docs/threads.md §3.4b).
	// The expansion is decided at run creation — the runner never sees a
	// `uses:` job — and a bad reference is a failed job with a named reason,
	// never a run-wide failure.
	plan, perr := s.planJobDefs(ctx, repoID(owner, repo), sha, f, opts.Inputs)
	if perr != nil {
		return nil, perr
	}
	for _, jd := range plan {
		def := jd.def
		display := jd.name
		base := buildSpec(jd.wfName, jd.wf, def, jd.inputs)
		if jd.cap != nil {
			// PAR-075: a callee may narrow permissions, never widen them.
			base.Permissions = downgradePermissions(jd.cap, base.Permissions)
		}
		// A callee's names/groups expand against the run's github context and
		// the call's inputs (the caller's `with:` values).
		ec := expandContext{GitHub: githubContext(run, f.Name), Inputs: groupInputs(jd.wf, jd.inputs)}
		instances, xerr := expandJobSpec(jd.key, display, base, ec)
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
				Needs:     nonNilStr(jd.needs),
				Spec:      spec,
				Attempt:   attempt,
				Trusted:   trusted,
				State:     JobQueued,
				CreatedAt: now, UpdatedAt: now,
				Version: 1,
			}
			// A refused `uses:` (a foreign repository, a missing file, a caller
			// key that describes a runner job) is a failed job with the named
			// reason; nothing is enqueued and no runner is asked to interpret it.
			if jd.refusal != "" {
				job.State = JobFailure
				job.CompletedAt = &now
				job.Reason = jd.refusal
			}
			// A job with no `needs:` can be decided now: nothing about it can
			// change between creation and a runner's claim, so a false `if:` is
			// written `skipped` (never enqueued) instead of being re-evaluated
			// by every probe. A dependent job is decided by the needs cascade.
			if len(jd.needs) == 0 && job.State == JobQueued {
				if verdict, reason, _ := evaluateJobIf(job, run, needFacts{}); verdict != jobIfRun {
					job.State = JobSkipped
					job.CompletedAt = &now
					job.Reason = reason
					if verdict == jobIfError {
						job.State = JobFailure
					}
				}
			}
			// Local actions are resolved before the image gate: the gate must
			// see every docker:// image an action names, and a definition that
			// cannot be used is a failed job with the reason (never a runner's
			// surprise). A job a condition already skipped runs no step, so it
			// is not refused.
			if job.State == JobQueued {
				if aerr := resolver.fill(ctx, spec); aerr != nil {
					job.State = JobFailure
					job.CompletedAt = &now
					job.Reason = aerr.Error()
				}
			}
			// The image gate (threads.image_allow_prefixes /
			// threads.image_require_digest) and the container option policy run
			// for EVERY job that could execute, whatever its trust class. They
			// are decided here — where the spec is final and before anything is
			// enqueued — so a refusal is a failed job with a Reason and a red
			// check, never a runner's surprise and never a silent skip. A job a
			// condition already skipped pulls no image and starts no container,
			// so it is not refused. A policy that cannot be evaluated refuses
			// (fail closed).
			if job.State == JobQueued {
				if reason, refused := s.jobRefusalReason(spec); refused {
					job.State = JobFailure
					job.CompletedAt = &now
					job.Reason = reason
				}
			}
			// The deployment-environment decision (docs/PLAN-THREADS-ENVIRONMENTS.md
			// §5): an undefined environment fails the job (fail closed), a defined
			// one carries its deployment record, and one with required reviewers
			// holds the run until a listed reviewer (or a repo admin) decides.
			if _, eerr := s.applyEnvironmentDecision(ctx, owner, repo, job, run, nowAt); eerr != nil {
				return nil, eerr
			}
			// The display list is built after resolution: a composite action is
			// shown expanded, in place (its definitions are embedded above).
			job.Steps = convenienceSteps(spec)
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
	run.Concurrency = runGroups(jobs)
	// The run-level hold: the fork gate, or the stricter environment gate when
	// any job names an environment with required reviewers. The environment hold
	// names the environments so the page, the check description and the API say
	// WHICH deployment is waiting instead of a generic "pending approval".
	if gate {
		run.State = RunPendingApproval
		run.ApprovalKind = ApprovalFork
	}
	gatedEnvs := pendingEnvironments(jobs)
	if len(gatedEnvs) > 0 {
		run.State = RunPendingApproval
		run.ApprovalKind = ApprovalEnvironment
		run.Environments = gatedEnvs
	}
	body, _ := json.Marshal(run) // cannot fail: the record is pure JSON types
	if perr := s.putCreate(ctx, RunKey(owner, repo, runID), body); perr != nil {
		if store.IsPreconditionFailed(perr) {
			return nil, fmt.Errorf("%w: run %s already exists", ErrConflict, runID)
		}
		return nil, perr
	}
	// Implicit supersede (PAR-150): the head for this (workflow, ref) is adopted
	// in its own goroutine here — immediately after the run header committed, so
	// a head id always names a visible run, and concurrently with the checks
	// mirror and the enqueue, so the adoption adds no sequential round trip to
	// creation. Its verdict is applied after the enqueue, so a superseded run is
	// always inspectable before it is cancelled. A run the workflow gave a
	// concurrency group, or an event that does not supersede, starts nothing.
	supersede := s.startSupersede(ctx, owner, repo, run, event, opts.Action)
	for _, job := range jobs {
		if cerr := s.reportCheck(ctx, run, job); cerr != nil {
			return nil, cerr
		}
	}
	settle := func() (*Run, error) {
		// The adoption overlapped the checks/enqueue writes; joining it here
		// keeps it out of series with them.
		if cancelled := s.finishSupersede(ctx, owner, repo, run, supersede); cancelled != nil {
			run = cancelled
		}
		if !allQueued(jobs) || terminal(run.State) {
			return s.settleDecidedJobs(ctx, owner, repo, runID)
		}
		return run, nil
	}
	if run.State == RunPendingApproval {
		// The approval gate IS the queue: the jobs exist (the page, the checks
		// mirror and the PR panel stay coherent) but no runner can see them
		// until ApproveRun enqueues them — the fork gate (15 §5.2) and an
		// environment's required reviewers share this state machine
		// (docs/PLAN-THREADS-ENVIRONMENTS.md §6). No wake: nothing to claim.
		//
		// Supersede still applies: the newer commit replaces the older run's
		// work, and cancelling it is right even though THIS run waits.
		if cancelled := s.finishSupersede(ctx, owner, repo, run, supersede); cancelled != nil {
			run = cancelled
		}
		// PAR-135: a held deployment tells its reviewers it is waiting (best
		// effort; the hold and its badge exist with or without the notice).
		if run.State == RunPendingApproval && run.ApprovalKind == ApprovalEnvironment {
			s.notifyPendingDeployment(ctx, owner, repo, run, s.pendingReviewers(ctx, owner, repo, run.Environments))
		}
		return run, nil
	}
	// Enqueue AFTER the run header (the header is the commit point, so a runner
	// can never claim a job of an invisible run). A queue failure is logged and
	// leaves the job queued exactly like a pre-runner instance (15 §8): the run
	// page shows it, and threads-gc re-enqueues it. A job the `if:`/`needs:`
	// decision already terminalised is never enqueued.
	idx := needsIndex(jobs)
	for _, job := range jobs {
		if job.State != JobQueued {
			continue
		}
		if _, eerr := s.enqueueJob(ctx, owner, repo, job, entryNeedMap(idx, job)); eerr != nil {
			slog.Warn("threads: enqueue job", "repo", repoID(owner, repo), "run", runID, "job", job.ID, "err", eerr)
		}
	}
	fetchHub.wake(repoID(owner, repo))
	// PAR-152: a grouped workflow registers in its group's single pending slot
	// when another run holds it (one group read per declared group) and stamps
	// the group on its header so the page never shows a silent spinner. This
	// runs AFTER the header commit and the enqueue: the cancellations it can
	// perform (cancel-in-progress, pending-cancel) must never destroy a live run
	// for a run that then fails to be created, and a group read or record
	// problem must never turn a created run into an error (the claim path
	// re-applies the rule and fails closed). A run the fork/environment gate
	// holds is not queued at all, so it neither waits in nor displaces a group
	// until it is approved.
	if run.State == RunQueued {
		if pendingGroup, perr := s.prepareRunGroups(ctx, owner, repo, run, jobs); perr != nil {
			slog.Warn("threads: concurrency group preparation", "repo", repoID(owner, repo), "run", runID, "err", perr)
		} else if pendingGroup != "" {
			run.PendingGroup = pendingGroup
			if _, uerr := s.casUpdate(ctx, RunKey(owner, repo, runID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
				var r Run
				if jerr := json.Unmarshal(cur, &r); jerr != nil {
					return nil, false, jerr
				}
				if r.State != RunQueued {
					return nil, false, nil // it started/ended meanwhile: no wait to show
				}
				r.PendingGroup = pendingGroup
				r.Version++
				return mustJSON(&r), true, nil
			}); uerr != nil {
				slog.Warn("threads: pending group stamp", "repo", repoID(owner, repo), "run", runID, "err", uerr)
			}
		}
	}
	// A job decided at creation (a false `if:`) can doom its dependents with no
	// runner poll, and a run whose every job was decided has nothing left to
	// wait for. Settle it now rather than leaving the run at "queued" until a
	// probe happens to visit it (a deployment with no runner included).
	return settle()
}

// settleDecidedJobs cascades the jobs a condition or dependency decided before
// any runner ran, folds the run when nothing is left to run, and returns the
// settled header. It is the shared tail of run creation, approval and rerun.
func (s *Service) settleDecidedJobs(ctx context.Context, owner, repo, runID string) (*Run, error) {
	if serr := s.skipBlockedDependents(ctx, owner, repo, runID, nil); serr != nil {
		return nil, serr
	}
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		return nil, err
	}
	if !anyLive(jobs) {
		if _, ferr := s.foldRun(ctx, owner, repo, runID); ferr != nil {
			return nil, ferr
		}
	}
	return s.loadRun(ctx, owner, repo, runID)
}

// anyLive reports a job that can still change (queued or running).
func anyLive(jobs []*Job) bool {
	for _, j := range jobs {
		if j.State == JobQueued || j.State == JobRunning {
			return true
		}
	}
	return false
}

// allQueued reports that nothing was decided before a runner ran (the common
// path, where the settlement read is skipped entirely).
func allQueued(jobs []*Job) bool {
	for _, j := range jobs {
		if j.State != JobQueued {
			return false
		}
	}
	return true
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
	if job.Deployment != nil && job.Deployment.Environment != "" {
		// A held or approved deployment names its environment in the checks
		// description, so the checks panel says WHICH deployment is waiting.
		desc = fmt.Sprintf("%s / %s · environment %s (run %s)",
			job.specWorkflow(run.Workflow), job.Name, job.Deployment.Environment, run.ID)
	}
	// Workflow-command annotations are stated on the commit's check: the full
	// text lives on the job record (the run page), and the commit reader sees
	// how many the attempt produced (PAR-093).
	if n := len(job.Annotations); n > 0 {
		desc = fmt.Sprintf("%s · %d annotation(s)", desc, n)
	}
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
		target := SelectFiles(files, EventDispatch, workflow.RefBranch, "", "", workflowRef)
		if len(target) == 0 {
			if broken := brokenWorkflowFiles(files, workflowRef); len(broken) > 0 {
				return nil, fmt.Errorf("%w: %s: %s", ErrUnprocessable, broken[0].Path, broken[0].Error)
			}
			return nil, fmt.Errorf("%w: no workflow_dispatch workflow named %q", ErrNotFound, workflowRef)
		}
		var environments []string
		if declaresEnvironmentInput(target[0]) {
			environments, derr = s.environmentNameList(ctx, owner, name)
			if derr != nil {
				return nil, derr
			}
		}
		resolved, verr := resolveDispatchInputs(target[0], inputs, environments)
		if verr != nil {
			return nil, verr
		}
		effective = resolved
	}
	branch := ""
	if rest, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		branch = rest
	}
	payload := map[string]any{"action": "workflow_dispatch"}
	if ins := eventInputs(effective); ins != nil {
		payload["inputs"] = ins
	}
	return s.CreateRun(ctx, repo, commit, EventDispatch, CreateOpts{
		Branch: branch, Ref: ref, Actor: string(p.Name), Workflow: workflowRef, Inputs: effective,
		EventPayload: payload,
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
	arts, aerr := s.runArtifactViews(ctx, owner, name, run)
	if aerr != nil {
		return nil, aerr
	}
	s.fillDurations(run, jobs)
	av := s.ApprovalFor(ctx, owner, name, run, p)
	return &RunView{Run: run, Jobs: jobs, Artifacts: arts, Approval: &av}, nil
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
	// Annotations replaces the job's annotations when non-nil (complete); a
	// rerun clears them through ClearTimings.
	Annotations []Annotation
	// Summary replaces the job's step summary when non-nil (complete); a rerun
	// clears it through ClearTimings.
	Summary *string
	// Outputs replaces the job's reported outputs when non-nil (complete); a
	// rerun clears them through ClearTimings.
	Outputs     map[string]string
	LeaseUntil  *string
	StartedAt   *string
	CompletedAt *string
	// Reason records why the job did not execute (a false job-level `if:`, a
	// blocked `needs:`, an unsupported condition). A rerun clears it.
	Reason string
	// Deployment replaces the job's deployment record when non-nil (an
	// approval/denial decision, or a rerun's re-decision). Nil leaves it alone:
	// a plain requeue must not forget an approval the job already carried.
	Deployment *Deployment
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
		if up.Annotations != nil {
			job.Annotations = up.Annotations
		}
		if up.Summary != nil {
			job.Summary = *up.Summary
		}
		if up.Outputs != nil {
			job.Outputs = up.Outputs
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
			job.Outputs = nil
			job.Annotations = nil
			job.Summary = ""
			job.Reason = ""
		}
		// The reason is applied AFTER the reset: a caller that clears an
		// attempt's timings and states a new refusal (a policy re-check on
		// rerun) means the new reason, not an empty one.
		if up.Reason != "" {
			job.Reason = up.Reason
		}
		if up.Deployment != nil {
			dep := *up.Deployment
			job.Deployment = &dep
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
		// `continue-on-error: true` on the job: the record keeps `failure`
		// (nothing is rewritten) and carries Tolerated, which the run fold
		// ignores — that is how a reader tells "failed but tolerated" from
		// "succeeded". Recomputed from the final state, so a successful retry
		// or a requeue clears it.
		job.Tolerated = job.State == JobFailure && job.Spec != nil && job.Spec.ContinueOnError
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
		// The lease is over: drop it from the runner registry's display state
		// (best effort; the runner may be gone) and from the global hint.
		s.clearRunnerCurrentJob(ctx, updated.Runner, repoID(owner, name), runID, jobID)
		s.removeQueueIndexJob(ctx, repoID(owner, name), runID, jobID)
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
		if state != RunQueued {
			// The run left the queue (running or terminal): it is no longer
			// waiting on a concurrency group.
			run.PendingGroup = ""
		}
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
