package threads

import (
	"fmt"
	"strings"
)

// Job-level `if:` and `continue-on-error` (15 §4.1, 2026-11-21). Both keys are
// parsed by the workflow package, so before this they were accepted but inert.
//
// Evaluation point. The job `if:` is decided by the SERVER, before the job is
// claimable: the queue owns eligibility (needs.go), so a false condition must
// terminalise the job (`skipped`, with the reason on the record) without a
// runner and without a queue entry that a probe has to keep refusing.
//
// Contexts: the same subset the step `if:` evaluates (internal/threads/expr.go)
// plus the job-level ones that exist here — `github.*`, `matrix.*`, `inputs.*`,
// `env.*`, `needs.<job>.result`, `needs.<job>.outputs.<name>` (a successful
// dependency's reported outputs), and success()/failure()/cancelled() derived
// from the dependency results. `steps.*` is a runner context and reads empty
// here.
//
// Two moments decide a condition:
//   - run creation / approval / rerun: a job with no `needs:` is decided
//     immediately (nothing can change its answer), and a false one is written
//     `skipped` and never enqueued;
//   - the `needs:` cascade (needs.go): a dependent is decided once every
//     dependency is terminal, so `needs.<job>.result` is a fact.
//
// A job with a non-empty `if:` that evaluates TRUE runs even when a dependency
// failed — that is the point of writing `if: always()` or
// `if: needs.build.result == 'failure'`. A job without one keeps the default:
// every dependency must have succeeded.

// jobIfVerdict is one job-level `if:` decision.
type jobIfVerdict int

const (
	// jobIfRun: the condition holds (or there is none): the job may run.
	jobIfRun jobIfVerdict = iota
	// jobIfSkip: the condition is false: the job is skipped.
	jobIfSkip
	// jobIfError: the expression is outside the supported subset.
	jobIfError
)

// evaluateJobIf decides one job's job-level `if:`. facts is the folded view of
// the job's `needs:` (results and outputs); it is empty for a job with no
// dependencies. run supplies the github.* context and may be a header the
// caller has not written yet (run creation).
func evaluateJobIf(job *Job, run *Run, facts needFacts) (jobIfVerdict, string, error) {
	if job == nil {
		return jobIfRun, "", nil
	}
	expr := ""
	if job.Spec != nil {
		expr = strings.TrimSpace(job.Spec.If)
	}
	// The default: every dependency must have succeeded. A job with no
	// dependencies trivially qualifies.
	if expr == "" {
		for key, res := range facts.Results {
			if res != JobSuccess {
				return jobIfSkip, fmt.Sprintf("needs: %s concluded %s", key, res), nil
			}
		}
		return jobIfRun, "", nil
	}
	expr = SubstituteInputs(SubstituteMatrix(expr, specMatrix(job)), specInputs(job))
	ctx := jobIfContext(job, run, facts)
	ok, err := EvalIf(expr, ctx)
	if err != nil {
		return jobIfError, err.Error(), err
	}
	if !ok {
		return jobIfSkip, "if: " + expr, nil
	}
	return jobIfRun, "", nil
}

// jobIfContext builds the `if` context of one job record. The github.* keys are
// the same ones the runner exposes to a step (plan.go builtinEnv), so a
// condition written once behaves the same at job and step level.
func jobIfContext(job *Job, run *Run, facts needFacts) ExprContext {
	workflow := ""
	if run != nil {
		workflow = run.Workflow
	}
	if job != nil {
		workflow = job.specWorkflow(workflow)
	}
	success, failure, cancelled := needsSummary(facts.Results)
	github := githubContext(run, workflow)
	if run != nil {
		github["job"] = jobIdentifier(job)
	}
	return ExprContext{
		Success: success, Failure: failure, Cancelled: cancelled,
		GitHub: github, Matrix: specMatrix(job), Inputs: specInputs(job),
		Env: specEnv(job), Needs: facts.Results, NeedsOutputs: facts.Outputs,
	}
}

// githubContext builds the run's github.* map (the keys TemplateGitHubFields
// lists). It is shared by the `if` evaluator and the concurrency-group/job-name
// expansion, so one reference means one value.
func githubContext(run *Run, workflow string) map[string]string {
	if run == nil {
		return map[string]string{}
	}
	ref := strings.TrimSpace(run.Ref)
	if ref == "" && strings.TrimSpace(run.Branch) != "" {
		ref = "refs/heads/" + strings.TrimSpace(run.Branch)
	}
	return map[string]string{
		"ref":        ref,
		"ref_name":   run.Branch,
		"sha":        run.Commit,
		"event_name": run.Event,
		"workflow":   workflow,
		"repository": run.Repo,
		"run_id":     run.ID,
	}
}

// jobIdentifier is the GITHUB_JOB spelling of a job record (its id).
func jobIdentifier(job *Job) string {
	if job == nil {
		return ""
	}
	return job.ID
}

// needsSummary folds dependency results into the success()/failure()/
// cancelled() functions of a job-level `if:`.
func needsSummary(results map[string]string) (success, failure, cancelled bool) {
	success = true
	for _, res := range results {
		switch res {
		case JobSuccess:
			// neutral
		case JobFailure:
			success, failure = false, true
		case JobCancelled:
			success, cancelled = false, true
		default:
			success = false
		}
	}
	return success, failure, cancelled
}

// specMatrix/specInputs/specEnv read a job spec's maps nil-safely.
func specMatrix(job *Job) map[string]string {
	if job == nil || job.Spec == nil {
		return nil
	}
	return job.Spec.Matrix
}

func specInputs(job *Job) map[string]string {
	if job == nil || job.Spec == nil {
		return nil
	}
	return job.Spec.Inputs
}

func specEnv(job *Job) map[string]string {
	if job == nil || job.Spec == nil {
		return nil
	}
	return job.Spec.Env
}

// entryNeedMap resolves one job's `needs:` to key → job ids (sorted), so a
// caller can both gate on the ids and evaluate `needs.<key>.result` without a
// second index. A key with no matching jobs yields no entry.
func entryNeedMap(idx map[string][]string, job *Job) map[string][]string {
	if job == nil || len(job.Needs) == 0 {
		return nil
	}
	out := make(map[string][]string, len(job.Needs))
	for _, key := range job.Needs {
		ids := idx[key]
		if len(ids) == 0 {
			continue
		}
		out[key] = append([]string{}, ids...)
	}
	return out
}
