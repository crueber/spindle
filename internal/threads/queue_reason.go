package threads

// Queue observability (PAR-156): exactly one named reason per queued job.
//
// A queued job used to be a silent spinner: "queued" could mean no runner has
// the labels, the runner fleet is busy, the run waits at an approval or
// capacity hold, the concurrency group is held, or a `needs:` dependency has
// not concluded. The reason is derived on read (never stored): GetRun loads the
// instance runner registry once when at least one job is queued, then annotates
// each job with the FIRST applicable reason in this order:
//
//	1. the run's hold (approval / capacity) — nothing about a runner can change it;
//	2. a `needs:` dependency that is still live;
//	3. the run's pending concurrency group;
//	4. runner availability (no runner at all, labels unmatched, fork opt-in, busy).
//
// The order matters for the acceptance ("exactly one named reason"): an
// approval-held run whose labels also match nothing says "awaiting approval",
// because that is the fact that must be acted on first.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// queueReasonApprovalPrefix / queueReasonCapacity are the stable prefixes the
// page and API render (tests assert on them).
const (
	queueReasonAwaitingApproval = "awaiting approval"
	queueReasonNoRunner         = "no runner is registered"
	queueReasonWaitingRunner    = "waiting for a matching runner"
	queueReasonBusyRunner       = "all matching runners are busy"
	queueReasonForkRunner       = "fork runs are disabled on every matching runner"
)

// annotateQueueReasons fills Job.QueueReason on the read-model copies.
func (s *Service) annotateQueueReasons(ctx context.Context, owner, repo string, run *Run, jobs []*Job) {
	if run == nil || len(jobs) == 0 {
		return
	}
	needsAny := false
	for _, job := range jobs {
		if job != nil && job.State == JobQueued {
			needsAny = true
			break
		}
	}
	if !needsAny {
		return
	}
	// A held run's reason applies to every queued job: a runner cannot claim
	// any of them, whatever the labels say.
	if run.State == RunPendingApproval {
		reason := queueReasonAwaitingApproval
		switch run.ApprovalKind {
		case ApprovalEnvironment:
			if len(run.Environments) > 0 {
				reason = queueReasonAwaitingApproval + " of deployment to " + strings.Join(run.Environments, ", ")
			}
		case ApprovalWorkflowRun:
			reason = queueReasonAwaitingApproval + " (upstream run was untrusted)"
		case ApprovalFork:
			reason = queueReasonAwaitingApproval + " (first-time fork contributor)"
		}
		for _, job := range jobs {
			if job != nil && job.State == JobQueued {
				job.QueueReason = reason
			}
		}
		return
	}
	if run.HoldReason != "" {
		for _, job := range jobs {
			if job != nil && job.State == JobQueued {
				job.QueueReason = run.HoldReason
			}
		}
		return
	}
	// Runner availability is one registry LIST; only read it when a job has no
	// earlier reason, so an approval-held or grouped run never pays it.
	runnersLoaded := false
	var runners []*RunnerDoc
	loadRunners := func() []*RunnerDoc {
		if runnersLoaded {
			return runners
		}
		runnersLoaded = true
		docs, err := s.loadRunnerDocsForReasons(ctx)
		if err != nil {
			return nil // an unreadable registry is "no visible runner", never a failed page
		}
		runners = docs
		return runners
	}
	for _, job := range jobs {
		if job == nil || job.State != JobQueued {
			continue
		}
		if waiting := waitingNeeds(job, jobs); len(waiting) > 0 {
			job.QueueReason = "waiting for " + strings.Join(waiting, ", ")
			continue
		}
		if run.PendingGroup != "" {
			job.QueueReason = fmt.Sprintf("waiting for concurrency group %q", run.PendingGroup)
			continue
		}
		job.QueueReason = runnerReason(loadRunners(), job, run)
	}
}

// waitingNeeds names the job's non-terminal dependencies ('needs:' gate).
func waitingNeeds(job *Job, jobs []*Job) []string {
	if len(job.Needs) == 0 {
		return nil
	}
	byKey := make(map[string]*Job, len(jobs))
	for _, j := range jobs {
		if j != nil {
			byKey[j.Key] = j
		}
	}
	var out []string
	for _, need := range job.Needs {
		if dep := byKey[need]; dep != nil && !terminal(dep.State) {
			out = append(out, need)
		}
	}
	sort.Strings(out)
	return out
}

// runnerReason names why no registered runner can take this job (or says it is
// simply waiting).
func runnerReason(runners []*RunnerDoc, job *Job, run *Run) string {
	labels := jobRunsOn(job)
	if len(runners) == 0 {
		return queueReasonNoRunner
	}
	matching := make([]*RunnerDoc, 0, len(runners))
	for _, r := range runners {
		if r == nil || r.RevokedAt != nil {
			continue
		}
		if !r.AllowsRepo(job.Repo) {
			continue
		}
		if labelsMatch(r.Labels, labels) {
			matching = append(matching, r)
		}
	}
	if len(matching) == 0 {
		return fmt.Sprintf("no runner matches labels %s", strings.Join(labels, ", "))
	}
	fork := !run.Trusted || !job.Trusted
	forkCapable := 0
	for _, r := range matching {
		if fork && !r.AllowForkRuns {
			continue
		}
		forkCapable++
	}
	if forkCapable == 0 {
		return queueReasonForkRunner
	}
	busy := 0
	for _, r := range matching {
		if fork && !r.AllowForkRuns {
			continue
		}
		if r.CurrentJob != nil && r.Capacity <= 1 {
			busy++
		}
	}
	if busy == forkCapable {
		return queueReasonBusyRunner
	}
	return queueReasonWaitingRunner
}

// loadRunnerDocsForReasons reads the instance runner registry for the read
// model. It is a bounded LIST over a human-scale family, done at most once per
// run page.
func (s *Service) loadRunnerDocsForReasons(ctx context.Context) ([]*RunnerDoc, error) {
	var keys []string
	if err := s.Store.List(ctx, RunnersPrefix(), "", func(m store.ObjectMeta) error {
		rest := strings.TrimPrefix(m.Key, RunnersPrefix())
		if strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			keys = append(keys, m.Key)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	out := make([]*RunnerDoc, 0, len(keys))
	for _, key := range keys {
		raw, _, err := s.getJSON(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var doc RunnerDoc
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, key, jerr)
		}
		out = append(out, &doc)
	}
	return out, nil
}
