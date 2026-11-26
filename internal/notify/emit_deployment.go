package notify

import "context"

// Pending deployments (PAR-135, docs/threads.md §5.6).
//
// A run held for a deployment environment tells its required reviewers that it
// is waiting. The emission is repo-level (Num 0) because a Threads run is not
// an issue or a pull request; the class is "deployment_pending", which maps to
// the review-requested reason for the named reviewers and is NOT a subscribed
// class — a watcher of the repository is not asked to approve a deployment,
// only the principals the environment names.
//
// Best effort and idempotent-by-content: the notice names the run, its
// environments and who must approve. A second emission for the same run is a
// duplicate notification, so callers emit once per hold (run creation and a
// partial approval), never per read.
func (s *Service) EmitDeployment(ctx context.Context, repo, runID, workflow string, environments, reviewers []string, actor, at string) {
	if len(reviewers) == 0 {
		return
	}
	title := "A deployment is waiting for your approval"
	if workflow != "" {
		title = workflow + ": a deployment is waiting for your approval"
	}
	s.emit(ctx, Emission{
		Repo: repo, Num: 0, Kind: "repo", Class: "deployment_pending",
		Actor: actor, At: at, Title: title, Recipients: reviewers,
		Detail: map[string]any{
			"run_id":       runID,
			"workflow":     workflow,
			"environments": append([]string{}, environments...),
			"reviewers":    append([]string{}, reviewers...),
		},
	})
}
