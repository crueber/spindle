// parity_w5_test.go — PAR-039 on the runner side: the plan carries the matrix
// cell's strategy context and `job.status`, and both are substituted into a
// step field. `job.status` follows continue-on-error exactly like
// success()/failure(): a tolerated failure leaves the job a success.
package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// TestJobStatusAfterToleratedFailure is the acceptance line end to end through
// the executor: a step that fails WITH continue-on-error leaves `job.status` a
// success, so the next step's condition runs and its substitution says success.
func TestJobStatusAfterToleratedFailure(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "exit 9", code: 9}}}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "tolerated", Run: "exit 9", ContinueOnError: true},
		{Name: "observe", Run: `test "${{ job.status }}" = success`, If: "job.status == 'success'"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s), a tolerated failure must keep the job green", res.Conclusion, res.Error)
	}
	var observed *threads.StepResult
	for i := range res.Steps {
		if res.Steps[i].Name == "observe" {
			observed = &res.Steps[i]
		}
	}
	if observed == nil || observed.Conclusion != threads.JobSuccess {
		t.Fatalf("observe step = %+v; job.status did not read a tolerated failure as success", observed)
	}
}

func TestPlanCarriesStrategyAndJobStatus(t *testing.T) {
	job := fixtureJob()
	job.Spec.MatrixIndex = 1
	job.Spec.MatrixTotal = 2
	off := false
	job.Spec.Strategy = &threads.StrategySpec{
		Matrix:   map[string][]string{"go": {"1.26", "1.27"}},
		FailFast: &off, MaxParallel: 1,
	}
	job.Spec.Steps = []threads.StepSpec{{Name: "echo", Run: `echo "${{ job.status }} ${{ strategy.job-index }}/${{ strategy.job-total }}"`}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := map[string]string{"job-index": "1", "job-total": "2", "fail-fast": "false", "max-parallel": "1"}
	for key, val := range want {
		if p.Strategy[key] != val {
			t.Fatalf("strategy.%s = %q, want %q", key, p.Strategy[key], val)
		}
	}
	if p.jobStatus() != threads.JobSuccess {
		t.Fatalf("initial job status = %q", p.jobStatus())
	}
	resolved, err := p.ResolveStep(p.Steps[0], nil)
	if err != nil {
		t.Fatalf("ResolveStep: %v", err)
	}
	if got := strings.TrimSpace(resolved.Run); got != `echo "success 1/2"` {
		t.Fatalf("resolved run = %q", got)
	}
	p.JobStatus = threads.JobFailure
	resolved, err = p.ResolveStep(p.Steps[0], nil)
	if err != nil {
		t.Fatalf("ResolveStep (failed job): %v", err)
	}
	if got := strings.TrimSpace(resolved.Run); got != `echo "failure 1/2"` {
		t.Fatalf("failed-job resolved run = %q", got)
	}
}

// TestJobStatusToleratedFailure is the acceptance line's second half: a step
// tolerated by continue-on-error never turns `job.status` into failure.
func TestJobStatusToleratedFailure(t *testing.T) {
	cases := []struct {
		hard      bool
		cancelled bool
		want      string
	}{
		{false, false, threads.JobSuccess}, // a tolerated failure arrives here as hard=false
		{true, false, threads.JobFailure},
		{false, true, threads.JobCancelled},
		{true, true, threads.JobCancelled}, // a cancel wins over a hard failure
	}
	for _, tc := range cases {
		stream := &Streamer{cancelled: tc.cancelled}
		if got := jobStatusValue(tc.hard, stream); got != tc.want {
			t.Fatalf("hard=%v cancelled=%v: %q, want %q", tc.hard, tc.cancelled, got, tc.want)
		}
	}
	if got := jobStatusValue(false, nil); got != threads.JobSuccess {
		t.Fatalf("nil stream = %q", got)
	}
}

// TestPlanStrategyContextEmptyForPlainJob: a plain job's strategy references
// substitute empty, never the literal text.
func TestPlanStrategyContextEmptyForPlainJob(t *testing.T) {
	job := fixtureJob()
	job.Spec.Matrix = nil
	job.Spec.Strategy = nil
	job.Spec.MatrixIndex, job.Spec.MatrixTotal = 0, 0
	job.Spec.Steps = []threads.StepSpec{{Name: "echo", Run: `echo "x${{ strategy.job-index }}y"`}}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Strategy != nil {
		t.Fatalf("plain job strategy = %v", p.Strategy)
	}
	resolved, err := p.ResolveStep(p.Steps[0], nil)
	if err != nil {
		t.Fatalf("ResolveStep: %v", err)
	}
	if got := strings.TrimSpace(resolved.Run); got != `echo "xy"` {
		t.Fatalf("resolved run = %q", got)
	}
}
