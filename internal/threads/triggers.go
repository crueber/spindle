package threads

// The GitHub-only trigger emitters and the plan-time controls around them
// (PAR-019, PAR-024, PAR-021, PAR-015).
//
// Every emitter is called from the ONE CreateRun path (composition binds the
// request handlers; this file binds the server-internal edges), so a trigger
// never grows a second scheduler. Follow-up events run through Service.detach:
// composition launches them off the emitting path, tests run them inline.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// maxWorkflowRunChain bounds a workflow_run follow-up chain. A run started by a
// workflow_run carries its parent's depth + 1; the emitter refuses to start a
// chain deeper than this, so two workflows that follow each other cannot spin
// forever. GitHub's own chain depth is finite for the same reason; three is the
// documented Threads value.
const maxWorkflowRunChain = 3

// MergeRefReader is the optional git seam that builds the simulated merge
// commit for pull_request events (PAR-024). *SubprocessGit implements it over
// `git merge-tree --write-tree` + `git commit-tree` in the serving copy
// (docs/go/04_git.md §14a). A reader that does not implement it leaves the
// pull_request run on the head commit (the documented fallback), never a
// fabricated merge sha.
type MergeRefReader interface {
	// MergeCommit builds (or reuses) the merge commit of base+head in repo and
	// returns its sha. A conflicting merge returns an error; the caller falls
	// back to the head commit.
	MergeCommit(ctx context.Context, repo, base, head string) (string, error)
}

// mergeRefCommit computes the merge commit for a pull_request delivery. It
// resolves the base tip when the caller did not hand one, asks the optional
// reader, and returns ok=false on any failure (conflict, no reader, store/git
// error) so the run falls back to the head with a log line — never a silent
// wrong sha, and never a run whose commit is unreadable.
func (s *Service) mergeRefCommit(ctx context.Context, repo string, opts CreateOpts, headSHA string, policy repoPolicy) (string, bool) {
	reader, ok := s.Git.(MergeRefReader)
	if !ok {
		return "", false
	}
	base := strings.TrimSpace(opts.BaseCommit)
	if base == "" {
		branch := strings.TrimPrefix(strings.TrimSpace(opts.BaseBranch), "refs/heads/")
		if branch == "" || s.Git == nil {
			return "", false
		}
		resolved, err := s.Git.ResolveRef(ctx, repo, "refs/heads/"+branch)
		if err != nil {
			return "", false
		}
		base = resolved
	}
	sha, err := reader.MergeCommit(ctx, repo, base, headSHA)
	if err != nil {
		slog.Info("threads: pull_request merge ref unavailable; running the head commit",
			"repo", repo, "pr", opts.PullNumber, "head", headSHA, "err", err)
		return "", false
	}
	if strings.TrimSpace(sha) == "" {
		return "", false
	}
	return sha, true
}

// prTargetHeadCheckoutRefusal applies D-2's second switch: under
// pull_request_target, a declarative checkout of the PR head is refused at plan
// time unless the repository enabled `pr_target_allow_head_checkout`. Any
// `ref:` on an actions/checkout step counts (the base tip is already checked
// out, so a ref is only there to name something else), and a `repository:`
// names another repository by definition. The check covers composite actions
// embedded in the job, because those run in the same container.
func prTargetHeadCheckoutRefusal(spec *Spec) (string, bool) {
	if spec == nil {
		return "", false
	}
	if ref := checkoutRefInSteps(spec.Steps); ref != "" {
		return fmt.Sprintf("pull_request_target refuses a head checkout at plan time: actions/checkout names ref %q, and this repository has not enabled pr_target_allow_head_checkout (D-2)", ref), true
	}
	for dir, action := range spec.Actions {
		if ref := checkoutRefInSteps(action.Steps); ref != "" {
			return fmt.Sprintf("pull_request_target refuses a head checkout at plan time: composite action %s names ref %q, and this repository has not enabled pr_target_allow_head_checkout (D-2)", dir, ref), true
		}
	}
	return "", false
}

// checkoutRefInSteps returns the first actions/checkout `ref`/`repository` value
// in a step list ("" when none).
func checkoutRefInSteps(steps []StepSpec) string {
	for _, step := range steps {
		if !isCheckoutAction(step.Uses) {
			continue
		}
		if ref := strings.TrimSpace(step.With["ref"]); ref != "" {
			return ref
		}
		if repo := strings.TrimSpace(step.With["repository"]); repo != "" {
			return "repository:" + repo
		}
	}
	return ""
}

// isCheckoutAction matches actions/checkout case-insensitively (the runner's
// built-in resolution does the same).
func isCheckoutAction(uses string) bool {
	uses = strings.ToLower(strings.TrimSpace(uses))
	return uses == "actions/checkout" || strings.HasPrefix(uses, "actions/checkout@")
}

// declaresWorkflowRun reports whether any parsed file in a discovery result
// declares on.workflow_run (the PAR-019 budget gate).
func declaresWorkflowRun(files []*WorkflowFile) bool {
	for _, f := range files {
		if f != nil && f.Workflow != nil && f.Workflow.On.WorkflowRunSet {
			return true
		}
	}
	return false
}

// workflowRunHeadBranch is the upstream run's head branch as GitHub reports it:
// the PR head branch for a pull_request/pull_request_target run (from the
// payload), otherwise the pushed branch or the ref's short name.
func workflowRunHeadBranch(run *Run) string {
	if run == nil {
		return ""
	}
	if run.Event == EventPullReq || run.Event == EventPullTarget {
		if pr, ok := run.EventPayload["pull_request"].(map[string]any); ok {
			if head, ok := pr["head"].(map[string]any); ok {
				if ref, ok := head["ref"].(string); ok && strings.TrimSpace(ref) != "" {
					return strings.TrimSpace(ref)
				}
			}
		}
	}
	if branch := strings.TrimSpace(run.Branch); branch != "" {
		return branch
	}
	return shortRefName(run.Ref)
}

// workflowRunParent projects one run onto the bounded payload the follow-up
// reads. Actor is the run's actor; the repository is the run's repo.
func workflowRunParentProjection(run *Run) WorkflowRunParent {
	if run == nil {
		return WorkflowRunParent{}
	}
	status := "in_progress"
	conclusion := ""
	if run.State == RunSuccess || run.State == RunFailure || run.State == RunCancelled || run.State == RunSkipped {
		status = "completed"
		conclusion = run.State
	}
	return WorkflowRunParent{
		ID: run.ID, Name: run.Workflow, Event: run.Event, Status: status, Conclusion: conclusion,
		RunAttempt: run.Attempt, HeadBranch: workflowRunHeadBranch(run), HeadSHA: run.Commit,
		WorkflowID: run.WorkflowPath, Repository: run.Repo, Actor: run.Actor,
	}
}

// emitWorkflowRun starts the follow-up runs of one upstream lifecycle edge
// (PAR-019): `requested` when the upstream run header commits, `completed` when
// it folds terminal. The new runs read the DEFAULT branch's files (the
// repository-event rule), filter on the upstream workflow name/branch, and are
// held for approval when the upstream was untrusted (D-3). A chain deeper than
// maxWorkflowRunChain is refused (logged), so a pair of mutual followers ends.
func (s *Service) emitWorkflowRun(ctx context.Context, run *Run, action string) {
	if s == nil || run == nil {
		return
	}
	if run.ChainDepth >= maxWorkflowRunChain {
		slog.Warn("threads: workflow_run chain depth cap reached; not starting a follow-up",
			"repo", run.Repo, "run", run.ID, "depth", run.ChainDepth)
		return
	}
	// The upstream branch is what `on.workflow_run.branches` filters on. For a
	// pull_request run GitHub's head_branch is the PR HEAD branch, not the
	// target; the payload records it, and a tag push has no branch (the ref's
	// short name is the honest value).
	branch := workflowRunHeadBranch(run)
	payload := WorkflowRunEventPayload(action, workflowRunParentProjection(run))
	opts := CreateOpts{
		Action: action, RefName: branch,
		Actor:            run.Actor,
		EventPayload:     payload,
		ParentRunID:      run.ID,
		ParentTrusted:    run.Trusted,
		ChainDepth:       run.ChainDepth + 1,
		UpstreamWorkflow: run.Workflow,
	}
	if _, err := s.CreateRun(ctx, run.Repo, run.Commit, EventWorkflowRun, opts); err != nil {
		slog.Warn("threads: workflow_run follow-up", "repo", run.Repo, "run", run.ID, "action", action, "err", err)
	}
}

// shortRefName strips refs/heads/ or refs/tags/ (a display/branch-filter name).
func shortRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			return rest
		}
	}
	return ref
}

// emitDeploymentEvents starts the deployment / deployment_status follow-ups for
// one deployment record transition (PAR-021). It runs through detach because a
// deployment_status workflow may itself deploy; the chain cap bounds that
// recursion, and the emitting request never waits on it.
func (s *Service) emitDeploymentEvents(run *Run, job *Job, state, description string) {
	if s == nil || run == nil || job == nil || job.Deployment == nil {
		return
	}
	// The same follow-up chain cap bounds a deployment-status workflow that
	// deploys again: without it two environments could ping-pong forever.
	if run.ChainDepth >= maxWorkflowRunChain {
		slog.Warn("threads: deployment event chain depth cap reached", "repo", run.Repo, "run", run.ID, "depth", run.ChainDepth)
		return
	}
	dep := job.Deployment
	actor := run.Actor
	repo := run.Repo
	ref := run.Ref
	if ref == "" && run.Branch != "" {
		ref = "refs/heads/" + run.Branch
	}
	s.detach(func(ctx context.Context) {
		if state == "pending" || state == "queued" {
			deploymentPayload := DeploymentEventPayload(repo, actor, dep.Environment, ref, run.Commit, run.ID, job.ID, "", "")
			if _, err := s.CreateRun(ctx, repo, run.Commit, EventDeployment, CreateOpts{
				Action: "created", Branch: run.Branch, Ref: ref, RefName: run.Branch, Actor: actor,
				EventPayload: deploymentPayload, ChainDepth: run.ChainDepth + 1,
			}); err != nil {
				slog.Warn("threads: deployment event", "repo", repo, "run", run.ID, "err", err)
			}
		}
		statusPayload := DeploymentEventPayload(repo, actor, dep.Environment, ref, run.Commit, run.ID, job.ID, state, description)
		if _, err := s.CreateRun(ctx, repo, run.Commit, EventDeploymentStatus, CreateOpts{
			Action: state, Branch: run.Branch, Ref: ref, RefName: run.Branch, Actor: actor,
			EventPayload: statusPayload, ChainDepth: run.ChainDepth + 1,
		}); err != nil {
			slog.Warn("threads: deployment_status event", "repo", repo, "run", run.ID, "state", state, "err", err)
		}
	})
}

// deploymentStatusState maps a Threads deployment state onto GitHub's
// deployment_status vocabulary. `not_required` is not a deployment status (no
// record was created for it in GitHub's model), so it maps to "queued" and is
// never emitted on its own.
func deploymentStatusState(state string) string {
	switch state {
	case DeploymentPending:
		return "pending"
	case DeploymentApproved:
		return "in_progress"
	case DeploymentDenied, DeploymentFailed:
		return "failure"
	}
	return "queued"
}

// containsFoldString reports whether list contains s, case-insensitively.
func containsFoldString(list []string, s string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}
