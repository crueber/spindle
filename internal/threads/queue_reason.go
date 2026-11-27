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
	queueReasonPaused           = "CI is paused for this repository"
	queueReasonDraining         = "every matching runner is draining"
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
	// PAR-157: a paused repository is the fact to act on before runner
	// availability. One settings GET, only for a run that has queued work.
	if doc, derr := s.loadSettings(ctx, owner, repo); derr == nil && doc != nil && doc.CIPaused {
		for _, job := range jobs {
			if job != nil && job.State == JobQueued {
				job.QueueReason = queueReasonPaused
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
		if job.Deployment != nil && job.Deployment.QueuePosition > 0 {
			reason := job.Deployment.QueueReason
			if reason == "" {
				reason = fmt.Sprintf("waiting for environment %q (deployment %d in line)", job.Deployment.Environment, job.Deployment.QueuePosition)
			}
			job.QueueReason = reason
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
		if reason := matrixSlotReason(job, jobs); reason != "" {
			job.QueueReason = reason
			continue
		}
		job.QueueReason = runnerReason(loadRunners(), job, run)
	}
}

// matrixSlotReason is the PAR-052 admission reason: a queued cell whose matrix
// group already has `max-parallel` cells in flight. It reads the job list the
// page already has, so it adds no store round trip.
func matrixSlotReason(job *Job, jobs []*Job) string {
	if job == nil || job.Spec == nil || job.Spec.Strategy == nil || job.Spec.Strategy.MaxParallel <= 0 || job.Spec.MatrixGroup == "" {
		return ""
	}
	active := 0
	for _, other := range jobs {
		if other == nil || other.Spec == nil || other.Spec.MatrixGroup != job.Spec.MatrixGroup {
			continue
		}
		if other.State == JobRunning || (other.State == JobQueued && other.LeaseUntil != nil) {
			active++
		}
	}
	if active < job.Spec.Strategy.MaxParallel {
		return ""
	}
	return fmt.Sprintf("waiting for a matrix slot (max-parallel %d)", job.Spec.Strategy.MaxParallel)
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
		if r == nil || r.RevokedAt != nil || r.IsDraining() {
			continue
		}
		if !r.CanServeRepo(job.Repo) {
			continue
		}
		if labelsMatch(r.EffectiveLabels(), labels) {
			matching = append(matching, r)
		}
	}
	if len(matching) == 0 {
		if reason := nonGoalLabelReason(labels); reason != "" {
			return reason
		}
		// A fleet that is entirely draining says so rather than pretending the
		// labels are unmatched (PAR-181).
		if anyDrainingForRepo(runners, job.Repo) {
			return queueReasonDraining
		}
		return fmt.Sprintf("no runner matches labels %s", strings.Join(labels, ", "))
	}
	size := JobSizeOf(job)
	sized := matching[:0]
	for _, r := range matching {
		if RunnerOffers(r, size) {
			sized = append(sized, r)
		}
	}
	if len(sized) == 0 {
		return fmt.Sprintf("no matching runner offers %s", size.Describe())
	}
	matching = sized
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

// anyDrainingForRepo reports whether at least one draining runner is eligible
// for the repository (used only for the read-model reason).
func anyDrainingForRepo(runners []*RunnerDoc, repo string) bool {
	for _, r := range runners {
		if r != nil && r.RevokedAt == nil && r.IsDraining() && r.CanServeRepo(repo) {
			return true
		}
	}
	return false
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
