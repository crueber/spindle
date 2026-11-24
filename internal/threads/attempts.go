package threads

import (
	"context"
	"encoding/json"
	"fmt"

	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Attempt-scoped writes (15 §3). Identity is per attempt: the token embeds
// (repo, run, job, attempt, runner) and the job record's current attempt is the
// authority, so a write carrying a superseded attempt is rejected by identity,
// never by a timestamp comparison.

// MaxLogFrame bounds one log frame (15 §2: 256 KiB frames).
const MaxLogFrame = 256 << 10

// StartView is the start answer: the attempt token plus what the runner needs
// to report.
type StartView struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	Repo      string `json:"repo"`
	RunID     string `json:"run_id"`
	JobID     string `json:"job_id"`
	Attempt   int    `json:"attempt"`
	// Permissions is the effective permission map the token is scoped to
	// (derived from the workflow's `permissions:` and the instance default);
	// Scopes is the git scope set it carries (`read`, and `push` when
	// `contents: write`). The job payload states its own authority.
	Permissions map[string]string `json:"permissions"`
	Scopes      []string          `json:"scopes"`
	// Artifacts advertises the instance's artifact caps so the runner refuses
	// an over-cap upload before streaming it (15 §2.5).
	Artifacts ArtifactCaps `json:"artifacts"`
}

// Start marks the attempt running and issues its short-lived wjt_ token. The
// caller is authenticated by the runner token; the path's job reference and
// attempt number must match the claim. An instance runner is not scoped to a
// repository: the claim (the queue entry naming this runner) is the authority.
func (s *Service) Start(ctx context.Context, runner *runnerAuth, ref string, attempt int) (*StartView, error) {
	owner, repo, runID, jobID, err := parseJobRef(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown job reference", ErrNotFound)
	}
	job, run, jerr := s.loadJobAndRun(ctx, owner, repo, runID, jobID)
	if jerr != nil {
		return nil, jerr
	}
	if job == nil {
		return nil, fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
	}
	if attempt != job.Attempt {
		return nil, fmt.Errorf("%w: attempt %d is not current (job is at attempt %d)", ErrConflict, attempt, job.Attempt)
	}
	if job.QueueSeq != "" {
		entry, _, eerr := s.loadEntry(ctx, owner, repo, job.QueueSeq)
		if eerr != nil {
			return nil, eerr
		}
		if entry != nil && entry.Runner != "" && entry.Runner != runner.Doc.ID {
			return nil, fmt.Errorf("%w: the job is claimed by another runner", ErrForbidden)
		}
	}
	if terminal(job.State) {
		return nil, fmt.Errorf("%w: job already %s", ErrConflict, job.State)
	}
	trusted := job.Trusted && (run == nil || run.Trusted)
	perms := s.EffectivePermissions(job.Spec, trusted)
	gitScopes := PermissionScopes(perms)
	claims := AttemptClaims{
		Repo: repoID(owner, repo), Run: runID, Job: jobID, Attempt: attempt,
		Runner: runner.Doc.ID, Scopes: attemptScopes(gitScopes),
		Exp: s.nowUTC().Add(attemptTokenTTL(job.Spec)).Unix(),
	}
	token, expires, merr := s.mintAttemptToken(claims)
	if merr != nil {
		return nil, merr
	}
	if _, uerr := s.UpdateJob(ctx, repoID(owner, repo), runID, jobID, JobUpdate{State: JobRunning, Attempt: attempt}); uerr != nil {
		return nil, uerr
	}
	return &StartView{
		Token: token, ExpiresAt: expires.Format(dateTimeFmt), Repo: claims.Repo,
		RunID: runID, JobID: jobID, Attempt: attempt,
		Permissions: perms, Scopes: claims.Scopes,
		Artifacts: s.ArtifactCaps(),
	}, nil
}

// verifyAttempt checks an attempt token's scope and that the job it names is
// still at that attempt. The returned job is the one every write applies to.
func (s *Service) verifyAttempt(ctx context.Context, claims AttemptClaims, scope string) (*Job, error) {
	if !claims.Allows(scope) {
		return nil, fmt.Errorf("%w: attempt token lacks the %s scope", ErrForbidden, scope)
	}
	owner, repo, err := splitRepo(claims.Repo)
	if err != nil {
		return nil, err
	}
	job, _, lerr := s.loadJobAndRun(ctx, owner, repo, claims.Run, claims.Job)
	if lerr != nil {
		return nil, lerr
	}
	if job == nil {
		return nil, fmt.Errorf("%w: unknown job %q", ErrNotFound, claims.Job)
	}
	if job.Attempt != claims.Attempt {
		return nil, fmt.Errorf("%w: attempt %d superseded by %d", errSuperseded, claims.Attempt, job.Attempt)
	}
	return job, nil
}

// LogView is the log-append answer. Cancel is the operator's cancel signal the
// runner acks (the runner is busy, so its log frames are the control channel);
// LeaseUntil is the renewed claim lease.
type LogView struct {
	Seq        int     `json:"seq"`
	State      string  `json:"state"`
	Cancel     bool    `json:"cancel"`
	LeaseUntil *string `json:"lease_until,omitempty"`
}

// AppendLog stores one immutable log frame (idempotent by attempt+sequence) and
// reports the cancel signal. Frames are masked server-side before they land
// (best effort, 15 §5.3).
func (s *Service) AppendLog(ctx context.Context, claims AttemptClaims, seq int, text string) (*LogView, error) {
	if seq < 1 || seq > 1_000_000_000 {
		return nil, fmt.Errorf("%w: seq must be 1..1e9", ErrInvalid)
	}
	if len(text) > MaxLogFrame {
		return nil, fmt.Errorf("%w: log frame exceeds 256 KiB", ErrInvalid)
	}
	job, err := s.verifyAttempt(ctx, claims, ScopeLog)
	if err != nil {
		return nil, err
	}
	owner, repo, _ := splitRepo(claims.Repo)
	if job.CancelRequested && job.State != JobCancelled {
		// Tell the runner before spending a write; it acks with /cancel.
		return &LogView{Seq: seq, State: job.State, Cancel: true, LeaseUntil: job.LeaseUntil}, nil
	}
	if terminal(job.State) && job.State != JobRunning {
		return &LogView{Seq: seq, State: job.State, Cancel: job.State == JobCancelled}, nil
	}
	body := text
	if job.Trusted {
		values, verr := s.secretValues(ctx, owner, repo)
		if verr != nil {
			return nil, verr
		}
		vals := make([]string, 0, len(values))
		for _, v := range values {
			vals = append(vals, v)
		}
		body = maskSecrets(body, vals)
	}
	key := LogKey(owner, repo, claims.Run, claims.Job, claims.Attempt, seq)
	_, perr := store.PutBytes(ctx, s.Store, key, []byte(body),
		store.PutOptions{Mode: store.PutCreate, ContentType: "text/plain; charset=utf-8"})
	if perr != nil && !store.IsPreconditionFailed(perr) {
		return nil, perr
	}
	// A stored (or already stored) frame is the runner's liveness proof.
	lease := s.renewLeaseFor(ctx, owner, repo, job)
	return &LogView{Seq: seq, State: job.State, Cancel: job.CancelRequested, LeaseUntil: lease}, nil
}

// renewLeaseFor renews the claim lease through the job's recorded queue
// sequence (no head probe on the log path).
func (s *Service) renewLeaseFor(ctx context.Context, owner, repo string, job *Job) *string {
	if job.QueueSeq == "" {
		return job.LeaseUntil
	}
	entry, _, err := s.loadEntry(ctx, owner, repo, job.QueueSeq)
	if err != nil || entry == nil || entry.Runner == "" {
		return job.LeaseUntil
	}
	return s.renewLease(ctx, owner, repo, entry, job)
}

// StepResult is one completed step's outcome (complete's payload) and the
// `steps.<id>` context a later step's `if:` reads. ExitCode and DurationMS are
// the runner's accounting (pinned by pass B2; older runners omit them and the
// fields stay zero).
type StepResult struct {
	Number int `json:"number,omitempty"`
	// ID is the step's `id:` ("" for an anonymous step).
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	Conclusion string `json:"conclusion"`
	Error      string `json:"error,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// CompleteInput is the complete request body.
type CompleteInput struct {
	Conclusion string       `json:"conclusion"`
	Steps      []StepResult `json:"steps"`
	// Outputs are the job outputs the runner evaluated from the spec's
	// `outputs:` templates (docs/threads.md §3.9). Only a successful job's
	// outputs are stored; a failed or cancelled job exposes none.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// CompleteView is the complete answer.
type CompleteView struct {
	Repo  string `json:"repo"`
	RunID string `json:"run_id"`
	JobID string `json:"job_id"`
	State string `json:"state"`
	Run   string `json:"run_state"`
}

// Complete records a job's conclusion and step results, mirrors the check and
// folds the run (the CAS'd job is the commit point).
func (s *Service) Complete(ctx context.Context, claims AttemptClaims, in CompleteInput) (*CompleteView, error) {
	state, err := conclusionState(in.Conclusion)
	if err != nil {
		return nil, err
	}
	job, verr := s.verifyAttempt(ctx, claims, ScopeComplete)
	if verr != nil {
		return nil, verr
	}
	if terminal(job.State) {
		// Idempotent retry of the same conclusion; a different one is a
		// conflict (the job's history is not rewritten).
		if job.State == state {
			return &CompleteView{Repo: claims.Repo, RunID: claims.Run, JobID: claims.Job, State: job.State}, nil
		}
		return nil, fmt.Errorf("%w: job already %s", ErrConflict, job.State)
	}
	owner, repo, _ := splitRepo(claims.Repo)
	steps := in.Steps
	if len(steps) > 512 {
		return nil, fmt.Errorf("%w: too many step results", ErrInvalid)
	}
	for i := range steps {
		steps[i].Conclusion = normalizeConclusion(steps[i].Conclusion)
		steps[i].Error = truncate(steps[i].Error, 512)
	}
	// Job outputs: the runner already enforced the caps; the server enforces
	// them again (a bugging runner must not enlarge a job record past the
	// documented bound) and stores none for a job that did not succeed — a
	// dependent job must never read outputs from a failed dependency.
	outputs := in.Outputs
	if state != JobSuccess {
		outputs = nil
	}
	if len(outputs) > 0 {
		if oerr := ValidateOutputs(outputs); oerr != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, oerr)
		}
	}
	updated, uerr := s.UpdateJob(ctx, repoID(owner, repo), claims.Run, claims.Job, JobUpdate{
		State: state, StepResults: steps, Outputs: outputs, CancelRequested: boolPtr(false),
	})
	if uerr != nil {
		return nil, uerr
	}
	// fail-fast: a failed matrix cell cancels its siblings (GitHub's default
	// is on; `fail-fast: false` opts out). The sibling cancel rides the same
	// terminal path the operator uses, so a running sibling acks on its next
	// write. A job-level `continue-on-error` tolerates the failure, so it must
	// not fail fast either.
	if state == JobFailure && job.Spec != nil && !job.Spec.ContinueOnError && job.Spec.MatrixGroup != "" &&
		(job.Spec.Strategy == nil || job.Spec.Strategy.FailFast == nil || *job.Spec.Strategy.FailFast) {
		if ferr := s.cancelMatrixSiblings(ctx, owner, repo, claims.Run, claims.Job, job.Spec.MatrixGroup); ferr != nil {
			return nil, ferr
		}
	}
	s.finishEntry(ctx, owner, repo, job.QueueSeq)
	run, _ := s.loadRun(ctx, owner, repo, claims.Run)
	runState := state
	if run != nil {
		runState = run.State
	}
	return &CompleteView{
		Repo: claims.Repo, RunID: claims.Run, JobID: claims.Job,
		State: updated.State, Run: runState,
	}, nil
}

// AckCancel acknowledges a cancellation: the attempt becomes cancelled (or was
// already terminal) and the run folds.
func (s *Service) AckCancel(ctx context.Context, claims AttemptClaims) (*CompleteView, error) {
	job, err := s.verifyAttempt(ctx, claims, ScopeCancel)
	if err != nil {
		return nil, err
	}
	owner, repo, _ := splitRepo(claims.Repo)
	state := job.State
	if !terminal(job.State) {
		updated, uerr := s.UpdateJob(ctx, repoID(owner, repo), claims.Run, claims.Job, JobUpdate{
			State: JobCancelled, CancelRequested: boolPtr(false),
		})
		if uerr != nil {
			return nil, uerr
		}
		state = updated.State
	}
	s.finishEntry(ctx, owner, repo, job.QueueSeq)
	run, _ := s.loadRun(ctx, owner, repo, claims.Run)
	runState := state
	if run != nil {
		runState = run.State
	}
	return &CompleteView{Repo: claims.Repo, RunID: claims.Run, JobID: claims.Job, State: state, Run: runState}, nil
}

// cancelMatrixSiblings cancels the other cells of one matrix product after a
// cell failed with fail-fast on. Excepted: the failing job itself.
func (s *Service) cancelMatrixSiblings(ctx context.Context, owner, repo, runID, exceptJob, group string) error {
	jobs, err := s.loadJobs(ctx, owner, repo, runID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.ID == exceptJob || j.Spec == nil || j.Spec.MatrixGroup != group || terminal(j.State) {
			continue
		}
		if j.State == JobRunning {
			if _, uerr := s.UpdateJob(ctx, repoID(owner, repo), runID, j.ID, JobUpdate{CancelRequested: boolPtr(true)}); uerr != nil {
				return uerr
			}
			s.setEntryCancel(ctx, owner, repo, j.QueueSeq, true)
			continue
		}
		if _, uerr := s.UpdateJob(ctx, repoID(owner, repo), runID, j.ID, JobUpdate{
			State: JobCancelled, CancelRequested: boolPtr(false),
		}); uerr != nil {
			return uerr
		}
		s.setEntryCancel(ctx, owner, repo, j.QueueSeq, false)
	}
	return nil
}

// conclusionState maps a runner conclusion onto a job state.
func conclusionState(c string) (string, error) {
	switch normalizeConclusion(c) {
	case JobSuccess:
		return JobSuccess, nil
	case JobFailure:
		return JobFailure, nil
	case JobCancelled:
		return JobCancelled, nil
	}
	return "", fmt.Errorf("%w: conclusion must be success|failure|cancelled", ErrInvalid)
}

// normalizeConclusion lowercases and normalizes the two spellings of cancelled.
func normalizeConclusion(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "success":
		return JobSuccess
	case "failure", "failed", "error":
		return JobFailure
	case "cancelled", "canceled":
		return JobCancelled
	}
	return strings.ToLower(strings.TrimSpace(c))
}

// boolPtr is a convenience for *bool fields.
func boolPtr(b bool) *bool { return &b }

// --- operator cancel / rerun ------------------------------------------------

// CancelRun cancels a run (repo write role). A queued job is cancelled
// immediately; a running job carries the cancel signal until its runner acks
// (the attempt's log/complete/cancel responses are the control channel).
func (s *Service) CancelRun(ctx context.Context, repo, runID string, p auth.Principal) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "write"); err != nil {
		return nil, err
	}
	return s.cancelRunSystem(ctx, owner, name, runID)
}

// cancelRunSystem is the terminal cancel path without a principal: the
// operator's CancelRun after its gate, and concurrency `cancel-in-progress`
// (which cancels the run holding a group). One implementation, so the two can
// never disagree about what cancelling means.
func (s *Service) cancelRunSystem(ctx context.Context, owner, name, runID string) (*Run, error) {
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
	for _, job := range jobs {
		if terminal(job.State) {
			continue
		}
		if job.State == JobRunning {
			if _, uerr := s.UpdateJob(ctx, repoID(owner, name), runID, job.ID, JobUpdate{
				CancelRequested: boolPtr(true),
			}); uerr != nil {
				return nil, uerr
			}
			s.setEntryCancel(ctx, owner, name, job.QueueSeq, true)
			continue
		}
		if _, uerr := s.UpdateJob(ctx, repoID(owner, name), runID, job.ID, JobUpdate{
			State: JobCancelled, CancelRequested: boolPtr(false),
		}); uerr != nil {
			return nil, uerr
		}
		s.setEntryCancel(ctx, owner, name, job.QueueSeq, false)
	}
	if _, ferr := s.foldRun(ctx, owner, name, runID); ferr != nil {
		return nil, ferr
	}
	return s.loadRun(ctx, owner, name, runID)
}

// RerunRun creates a new attempt for every job of a run (repo write role):
// state resets to queued, the attempt number advances (which invalidates every
// previous attempt token by identity) and the jobs are re-enqueued. The
// environment gate is re-applied (a record added or tightened since the run was
// created must be honoured by the re-run button), so a re-run of an
// environment-gated deployment is held again for a fresh decision.
func (s *Service) RerunRun(ctx context.Context, repo, runID string, p auth.Principal) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "write"); err != nil {
		return nil, err
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
	now := s.nowUTC().Format(dateTimeFmt)
	requeue := make([]*Job, 0, len(jobs))
	var gated []string
	anyRefused := false
	for _, job := range jobs {
		if job.State == JobRunning {
			return nil, fmt.Errorf("%w: job %s is still running; cancel it first", ErrConflict, job.ID)
		}
		// The image and container-option gates are re-applied on every attempt:
		// a policy tightened after the run was created must not be bypassed by a
		// rerun, and the refusal lands as a failed job with the same named Reason.
		if reason, refused := s.jobRefusalReason(job.Spec); refused {
			anyRefused = true
			if _, uerr := s.UpdateJob(ctx, repo, runID, job.ID, JobUpdate{
				State: JobFailure, Attempt: job.Attempt + 1, ClearTimings: true,
				CancelRequested: boolPtr(false), Reason: reason,
			}); uerr != nil {
				return nil, uerr
			}
			continue
		}
		next, uerr := s.UpdateJob(ctx, repo, runID, job.ID, JobUpdate{
			State: JobQueued, Attempt: job.Attempt + 1, ClearTimings: true,
			CancelRequested: boolPtr(false),
		})
		if uerr != nil {
			return nil, uerr
		}
		// Re-decide the deployment against the CURRENT environment record: an
		// undefined environment fails the job closed, a reviewed one holds the
		// re-run for a new approval (the previous decision is not transferable).
		held, eerr := s.applyEnvironmentDecision(ctx, owner, name, next, now)
		if eerr != nil {
			return nil, eerr
		}
		if next.State != JobQueued || next.Deployment != nil {
			if _, uerr := s.UpdateJob(ctx, repo, runID, job.ID, JobUpdate{
				State: next.State, Deployment: next.Deployment, Reason: next.Reason,
			}); uerr != nil {
				return nil, uerr
			}
		}
		if next.State != JobQueued {
			continue
		}
		if held {
			gated = append(gated, next.Deployment.Environment)
			continue
		}
		requeue = append(requeue, next)
	}
	// The run header is the run's commit point: bump its attempt after the
	// jobs, then enqueue. A failure between the two leaves jobs queued with a
	// stale header, which the next rerun (or threads-gc) repairs.
	if _, cerr := s.casUpdate(ctx, RunKey(owner, name, runID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
		}
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		r.State = RunQueued
		r.Attempt++
		r.UpdatedAt = now
		r.StartedAt = nil
		r.CompletedAt = nil
		// The previous decision belonged to the previous attempt.
		r.ApprovedBy, r.ApprovedAt = "", nil
		r.DeniedBy, r.DeniedAt = "", nil
		r.ApprovalKind, r.Environments = "", nil
		if len(gated) > 0 {
			r.State = RunPendingApproval
			r.ApprovalKind = ApprovalEnvironment
			r.Environments = uniqueEnvNames(gated)
		}
		r.Version++
		run = &r
		return mustJSON(&r), true, nil
	}); cerr != nil {
		return nil, cerr
	}
	if len(gated) > 0 {
		// The hold IS the queue: no runner may claim a deployment nobody
		// approved.
		return run, nil
	}
	idx := needsIndex(jobs)
	for _, job := range requeue {
		// A rerun is a new attempt, so a job-level `if:` is evaluated again: a
		// job with no `needs:` is decided here (a dependent is decided by the
		// cascade once its dependencies conclude).
		if len(job.Needs) == 0 {
			if verdict, reason, _ := evaluateJobIf(job, run, needFacts{}); verdict != jobIfRun {
				state := JobSkipped
				if verdict == jobIfError {
					state = JobFailure
				}
				if _, uerr := s.UpdateJob(ctx, repo, runID, job.ID, JobUpdate{State: state, Reason: reason}); uerr != nil {
					return nil, uerr
				}
				continue
			}
		}
		if _, eerr := s.enqueueJob(ctx, owner, name, job, entryNeedMap(idx, job)); eerr != nil {
			return nil, eerr
		}
		fetchHub.wake(repoID(owner, name))
	}
	// A condition that skips a job dooms its dependents; settle against the
	// final job set (the loop may have reset a dependent before its dependency
	// was decided) and fold a run with nothing left to run.
	if anyRefused || !allQueued(requeue) {
		return s.settleDecidedJobs(ctx, owner, name, runID)
	}
	return run, nil
}

// --- log reads --------------------------------------------------------------

// LogFrame is one stored log frame.
type LogFrame struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// LogPage is one bounded window of a job's log.
type LogPage struct {
	Attempt int        `json:"attempt"`
	Frames  []LogFrame `json:"frames"`
	More    bool       `json:"more"`
	State   string     `json:"state"`
}

// LogFrames pages one attempt's log: frames are immutable and key-ordered, so a
// prefix LIST is the whole replay (attaching is a human path, never the claim
// path).
func (s *Service) LogFrames(ctx context.Context, repo, runID, jobID string, attempt, after, limit int, p auth.Principal) (*LogPage, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRead(ctx, owner, name, p); err != nil {
		return nil, err
	}
	job, _, jerr := s.loadJobAndRun(ctx, owner, name, runID, jobID)
	if jerr != nil {
		return nil, jerr
	}
	if job == nil {
		return nil, fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
	}
	if attempt <= 0 {
		attempt = job.Attempt
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	prefix := LogPrefix(owner, name, runID, jobID) + padInt(attempt, 3) + "-"
	page := &LogPage{Attempt: attempt, Frames: []LogFrame{}, State: job.State}
	more := false
	err = s.Store.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		seq, ok := logSeqOf(m.Key, prefix)
		if !ok || seq <= after {
			return nil
		}
		if len(page.Frames) == limit {
			more = true
			return nil
		}
		raw, _, gerr := store.GetBytes(ctx, s.Store, m.Key, store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				return nil
			}
			return gerr
		}
		page.Frames = append(page.Frames, LogFrame{Seq: seq, Text: string(raw)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	page.More = more
	return page, nil
}

// logSeqOf parses the sequence out of "<prefix><attempt>-<seq>".
func logSeqOf(key, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// --- job/run helpers --------------------------------------------------------

// loadJobAndRun reads one job and its run header (the attempt authority is the
// job record; the run supplies the payload's commit/workflow/trust).
func (s *Service) loadJobAndRun(ctx context.Context, owner, repo, runID, jobID string) (*Job, *Run, error) {
	if !runIDRe.MatchString(strings.TrimSpace(runID)) || !jobIDRe.MatchString(strings.TrimSpace(jobID)) {
		return nil, nil, fmt.Errorf("%w: unknown job %q", ErrNotFound, jobID)
	}
	raw, _, err := s.getJSON(ctx, JobKey(owner, repo, runID, jobID))
	if err != nil {
		return nil, nil, err
	}
	if raw == nil {
		return nil, nil, nil
	}
	var job Job
	if jerr := json.Unmarshal(raw, &job); jerr != nil {
		return nil, nil, fmt.Errorf("%w: job %s: %v", ErrCorrupt, jobID, jerr)
	}
	run, rerr := s.loadRun(ctx, owner, repo, runID)
	if rerr != nil {
		return nil, nil, rerr
	}
	return &job, run, nil
}
