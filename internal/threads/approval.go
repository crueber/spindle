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
// (attempt unchanged — nothing has run yet). Maintain-role only, on both
// surfaces.
func (s *Service) ApproveRun(ctx context.Context, repo, runID string, p auth.Principal) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "maintain"); err != nil {
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
	jobs, err := s.loadJobs(ctx, owner, name, runID)
	if err != nil {
		return nil, err
	}
	// Claim the decision first (CAS): two maintainers approving concurrently
	// must enqueue once.
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
	idx := needsIndex(jobs)
	for _, job := range jobs {
		if _, eerr := s.enqueueJob(ctx, owner, name, job, entryNeeds(idx, job)); eerr != nil {
			return nil, eerr
		}
	}
	fetchHub.wake(repoID(owner, name))
	return run, nil
}

// DenyRun refuses a pending_approval run: it is cancelled without executing and
// can never be approved afterwards (a maintainer may still re-run it explicitly,
// which is a new decision). Maintain-role only.
func (s *Service) DenyRun(ctx context.Context, repo, runID string, p auth.Principal) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if err := s.requireRole(ctx, owner, name, p, "maintain"); err != nil {
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
		return mustJSON(&r), true, nil
	}); cerr != nil {
		return nil, cerr
	}
	return s.cancelRunSystem(ctx, owner, name, runID)
}

// ApprovalView is the run header's approval state, shaped for the API/page.
type ApprovalView struct {
	Pending    bool    `json:"pending"`
	ApprovedBy string  `json:"approved_by,omitempty"`
	ApprovedAt *string `json:"approved_at,omitempty"`
	DeniedBy   string  `json:"denied_by,omitempty"`
	DeniedAt   *string `json:"denied_at,omitempty"`
}

// Approval projects one run onto its approval state.
func Approval(r *Run) ApprovalView {
	if r == nil {
		return ApprovalView{}
	}
	return ApprovalView{
		Pending: r.State == RunPendingApproval, ApprovedBy: r.ApprovedBy,
		ApprovedAt: r.ApprovedAt, DeniedBy: r.DeniedBy, DeniedAt: r.DeniedAt,
	}
}
