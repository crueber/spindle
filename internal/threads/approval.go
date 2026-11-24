package threads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// First-time-contributor approval gate (15 §5.2, round-2 pass 2).
//
// ## The rule, precisely
//
// A run is created in `pending_approval` — with its jobs written but NOT
// enqueued, so no runner can claim it — when ALL of these hold:
//
//  1. the trigger is a `pull_request` whose head is a FORK (a same-repository
//     branch author already has write access, so their run is trusted);
//  2. the actor holds no `maintain` or `admin` role in the repository
//     (maintainer-authored runs skip the gate);
//  3. the actor has no previously MERGED pull request in this repository —
//     "merged" means a PR whose merge task landed and whose author is this
//     principal (the `pulls` projection, one exact-key GET). It is authorship
//     of the PR that counts, never a commit's author field or an email;
//  4. the instance has the gate enabled (`threads.approve_first_time_forks`,
//     default true).
//
// A maintainer then approves (the run queues and runners may claim it) or denies
// it (the run is cancelled without ever executing). Approval and denial are
// maintain-role operations, on both the JSON API and the run page.
//
// ## Why the queue is the gate
//
// The run's jobs exist (so the page, the checks mirror and the PR checks panel
// stay coherent) but their queue entries do not, so `fetch` cannot see them.
// Approving enqueues them through the same `enqueueJob` the normal creation
// path uses — one code path, no second scheduler. Denying uses the operator's
// cancel path with queued jobs, which terminalises them immediately.

// ApprovalPolicy is the Seam 4-ish policy hook the gate consults: "has this
// principal ever had a pull request merged in this repository?". Composition
// binds it to pulls.Service.MergedAuthor (one exact-key GET). Nil means the
// evidence is unavailable, and the gate then holds the run (fail closed) rather
// than letting an unknown contributor execute.
type ApprovalPolicy interface {
	MergedAuthor(ctx context.Context, owner, repo, author string) (bool, error)
	// Maintainer reports whether actor holds maintain/admin in the repository
	// (a repo role binding, or a configured instance admin principal).
	Maintainer(ctx context.Context, owner, repo, actor string) (bool, error)
}

// needsApproval decides whether one run must wait for a maintainer (the rule in
// the file comment; each condition is a cheap check).
func (s *Service) needsApproval(ctx context.Context, owner, repo, actor string, opts CreateOpts, event string) bool {
	if s == nil || !s.ApproveFirstTimeForks {
		return false
	}
	if event != EventPullReq || !opts.Fork {
		return false
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || actor == defaultRunActor || actor == "push" || actor == "merge" || actor == "schedule" {
		return false
	}
	// A maintainer's own run skips the gate (condition 2). The policy hook owns
	// the answer because instance admins are only knowable from config; the
	// role ladder is the fallback when no hook is wired.
	if s.Approvals != nil {
		if maintainer, merr := s.Approvals.Maintainer(ctx, owner, repo, actor); merr == nil && maintainer {
			return false
		}
	} else if roleRank(s.roleOf(ctx, owner, repo, auth.Principal{Name: actor})) >= roleRank("maintain") {
		return false
	}
	if s.Approvals == nil {
		return true // no evidence source: hold, never guess
	}
	merged, err := s.Approvals.MergedAuthor(ctx, owner, repo, actor)
	if err != nil {
		return true // an unreadable projection fails closed
	}
	return !merged
}

// ApproveRun releases a pending_approval run: state queued, jobs enqueued
// (attempt unchanged — nothing has run yet). A fork-gate hold is a maintain-role
// decision; an environment hold is decided by a listed reviewer or a repo admin
// (docs/PLAN-THREADS-ENVIRONMENTS.md §6), on both surfaces.
func (s *Service) ApproveRun(ctx context.Context, repo, runID string, p auth.Principal) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	run, err := s.loadRun(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	if run.State != RunPendingApproval {
		return nil, fmt.Errorf("%w: run is %s, not awaiting approval", ErrConflict, run.State)
	}
	if err := s.decisionRole(ctx, owner, name, p, run); err != nil {
		return nil, err
	}
	jobs, err := s.loadJobs(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	// Claim the decision first (CAS): two approvers approving concurrently must
	// enqueue once.
	approvedBy := strings.TrimSpace(p.Name)
	approvedAt := s.nowUTC().Format(dateTimeFmt)
	_, err = s.casUpdate(ctx, RunKey(owner, name, runID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
		}
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		if r.State != RunPendingApproval {
			return nil, false, fmt.Errorf("%w: run is %s, not awaiting approval", ErrConflict, r.State)
		}
		r.State = RunQueued
		r.ApprovedBy = approvedBy
		r.ApprovedAt = &approvedAt
		r.UpdatedAt = approvedAt
		r.Version++
		run = &r
		return mustJSON(&r), true, nil
	})
	if err != nil {
		return nil, err
	}
	// An environment-gated job carries its deployment decision onto its own
	// record. An environment deleted between run creation and approval fails
	// that job closed (never a deployment with no reviewers left to name it).
	if run.ApprovalKind == ApprovalEnvironment {
		if serr := s.stampEnvironmentDecisions(ctx, owner, name, runID, jobs, true, approvedBy, approvedAt); serr != nil {
			return nil, serr
		}
	}
	idx := needsIndex(jobs)
	for _, job := range jobs {
		if job.State != JobQueued {
			continue // already skipped/failed by a job-level `if:`, the policy or a vanished environment
		}
		if _, eerr := s.enqueueJob(ctx, owner, name, job, entryNeedMap(idx, job)); eerr != nil {
			return nil, eerr
		}
	}
	fetchHub.wake(repoID(owner, name))
	if run.ApprovalKind == ApprovalEnvironment {
		for _, env := range run.Environments {
			s.auditAdmin(ctx, p.Name, AuditEnvironmentApprove, "environment:"+env, "run="+runID+" approved_by="+approvedBy)
		}
	}
	// A job decided before approval (a false `if:`) dooms its dependents and
	// may leave the run with nothing to run; settle it once the gate is open.
	if !allQueued(jobs) {
		return s.settleDecidedJobs(ctx, owner, name, runID)
	}
	return run, nil
}

// stampEnvironmentDecisions writes the approval (or denial) onto the deployment
// record of every environment-gated job. A gated environment that vanished is a
// failed job with a named reason — the decision cannot be attributed to a
// reviewer set that no longer exists.
func (s *Service) stampEnvironmentDecisions(ctx context.Context, owner, name, runID string, jobs []*Job, approve bool, by, at string) error {
	for _, job := range jobs {
		if job.Deployment == nil || job.Deployment.State != DeploymentPending || job.State != JobQueued {
			continue
		}
		dep := *job.Deployment
		if approve {
			doc, err := s.loadEnvironment(ctx, owner, name, dep.Environment)
			if err != nil {
				return err
			}
			if doc == nil {
				dep.State = DeploymentFailed
				if _, uerr := s.patchDeployment(ctx, owner, name, runID, job, dep, fmt.Sprintf(
					"deployment to environment %q could not be approved: the environment no longer exists", dep.Environment)); uerr != nil {
					return uerr
				}
				continue
			}
			dep.State = DeploymentApproved
			dep.ApprovedBy = by
			dep.ApprovedAt = &at
		} else {
			dep.State = DeploymentDenied
			dep.DeniedBy = by
			dep.DeniedAt = &at
		}
		reason := ""
		if !approve {
			// A denied deployment fails its job WITH the reason: the run page,
			// the checks description and the logs all name the environment and
			// the principal who refused it.
			reason = fmt.Sprintf("deployment to environment %q denied by %s", dep.Environment, by)
		}
		if _, uerr := s.patchDeployment(ctx, owner, name, runID, job, dep, reason); uerr != nil {
			return uerr
		}
	}
	return nil
}

// patchDeployment CAS-updates one job's deployment record (and, when reason is
// non-empty, terminalises the job as failed with that reason). It is the only
// writer of a deployment decision, so approve and deny cannot disagree.
func (s *Service) patchDeployment(ctx context.Context, owner, name, runID string, job *Job, dep Deployment, reason string) (*Job, error) {
	up := JobUpdate{Deployment: &dep}
	if reason != "" {
		up.State = JobFailure
		up.Reason = reason
	}
	next, err := s.UpdateJob(ctx, repoID(owner, name), runID, job.ID, up)
	if err != nil {
		return nil, err
	}
	*job = *next
	return next, nil
}

// decisionRole validates who may decide one held run.
func (s *Service) decisionRole(ctx context.Context, owner, name string, p auth.Principal, run *Run) error {
	if run.ApprovalKind == ApprovalEnvironment {
		return s.holdsForEnvironment(ctx, owner, name, p, run)
	}
	return s.requireRole(ctx, owner, name, p, "maintain")
}

// DenyRun refuses a pending_approval run. A fork-gate hold is cancelled without
// executing (existing behaviour). An environment hold fails exactly the
// environment-gated jobs with a named reason — the deployment never runs, its
// secrets are never resolved — and then lets the rest of the run proceed, so a
// refused deployment reads `failure` rather than erasing the build jobs beside
// it. A maintainer may still re-run it explicitly, which is a new decision.
func (s *Service) DenyRun(ctx context.Context, repo, runID string, p auth.Principal) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	run, err := s.loadRun(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
	}
	if run.State != RunPendingApproval {
		return nil, fmt.Errorf("%w: run is %s, not awaiting approval", ErrConflict, run.State)
	}
	if err := s.decisionRole(ctx, owner, name, p, run); err != nil {
		return nil, err
	}
	// Stamp the denial before cancelling, so the header records WHO refused it
	// even if the cancel fold is retried.
	deniedBy := strings.TrimSpace(p.Name)
	deniedAt := s.nowUTC().Format(dateTimeFmt)
	if _, cerr := s.casUpdate(ctx, RunKey(owner, name, runID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: unknown run %q", ErrNotFound, runID)
		}
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, fmt.Errorf("%w: run %s: %v", ErrCorrupt, runID, jerr)
		}
		r.DeniedBy = deniedBy
		r.DeniedAt = &deniedAt
		r.UpdatedAt = deniedAt
		r.Version++
		run = &r
		return mustJSON(&r), true, nil
	}); cerr != nil {
		return nil, cerr
	}
	if run.ApprovalKind != ApprovalEnvironment {
		return s.cancelRunSystem(ctx, owner, name, runID)
	}
	jobs, err := s.loadJobs(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	if serr := s.stampEnvironmentDecisions(ctx, owner, name, runID, jobs, false, deniedBy, deniedAt); serr != nil {
		return nil, serr
	}
	idx := needsIndex(jobs)
	for _, job := range jobs {
		if job.State != JobQueued {
			continue
		}
		if _, eerr := s.enqueueJob(ctx, owner, name, job, entryNeedMap(idx, job)); eerr != nil {
			return nil, eerr
		}
	}
	fetchHub.wake(repoID(owner, name))
	for _, env := range run.Environments {
		s.auditAdmin(ctx, p.Name, AuditEnvironmentDeny, "environment:"+env, "run="+runID+" denied_by="+deniedBy)
	}
	return s.settleDecidedJobs(ctx, owner, name, runID)
}

// ApprovalView is the run header's approval state, shaped for the API/page.
type ApprovalView struct {
	Pending      bool     `json:"pending"`
	Kind         string   `json:"kind,omitempty"`
	Environments []string `json:"environments,omitempty"`
	// Reviewers are the required reviewer names of the gated environments, at
	// read time (display + "who may decide"; the verb re-reads the records, so
	// a removal after this read cannot be bypassed).
	Reviewers  []string `json:"reviewers,omitempty"`
	CanDecide  bool     `json:"can_decide"`
	ApprovedBy string   `json:"approved_by,omitempty"`
	ApprovedAt *string  `json:"approved_at,omitempty"`
	DeniedBy   string   `json:"denied_by,omitempty"`
	DeniedAt   *string  `json:"denied_at,omitempty"`
}

// Approval projects one run onto its approval state.
func Approval(r *Run) ApprovalView {
	if r == nil {
		return ApprovalView{}
	}
	return ApprovalView{
		Pending: r.State == RunPendingApproval, Kind: r.ApprovalKind, Environments: nonNilStr(r.Environments),
		ApprovedBy: r.ApprovedBy,
		ApprovedAt: r.ApprovedAt, DeniedBy: r.DeniedBy, DeniedAt: r.DeniedAt,
	}
}

// ApprovalFor projects one run for ONE principal: the page renders the
// approve/deny controls from CanDecide, and Reviewers names who is awaited. The
// projection is advisory — ApproveRun/DenyRun re-check the same rule on the
// live environment records.
func (s *Service) ApprovalFor(ctx context.Context, owner, repo string, run *Run, p auth.Principal) ApprovalView {
	av := Approval(run)
	if run == nil || !av.Pending {
		return av
	}
	if run.ApprovalKind == ApprovalEnvironment {
		seen := map[string]bool{}
		for _, env := range run.Environments {
			doc, err := s.loadEnvironment(ctx, owner, repo, env)
			if err != nil || doc == nil {
				continue
			}
			for _, name := range doc.RequiredReviewers {
				if seen[name] {
					continue
				}
				seen[name] = true
				av.Reviewers = append(av.Reviewers, name)
			}
		}
	}
	av.CanDecide = s.decisionRole(ctx, owner, repo, p, run) == nil
	return av
}
