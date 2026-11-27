package runner

// PAR-080: step-level background/wait/wait-all/cancel and parallel groups
// (D-7). The fake docker boundary scripts the process outcomes; the kill script
// itself is asserted by its argv, and the real process-group teardown is
// covered by the docker e2e (docker_e2e_test.go).

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// bgJob builds a job whose steps are exactly the given specs.
func bgJob(steps ...threads.StepSpec) *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Container = nil
	job.Spec.Steps = steps
	return job
}

func TestPlanBackgroundControlAndParallel(t *testing.T) {
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "sleep 30", Background: true},
		threads.StepSpec{Name: "wait", Wait: "server"},
		threads.StepSpec{Name: "cancel", Cancel: "server"},
		threads.StepSpec{Name: "wait all", WaitAll: true},
		threads.StepSpec{Name: "pair", Parallel: []threads.StepSpec{
			{Name: "a", Run: "echo a"}, {Name: "b", Run: "echo b"},
		}},
	)
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 5 {
		t.Fatalf("steps = %d (%+v)", len(p.Steps), p.Steps)
	}
	if !p.Steps[0].Background || p.Steps[0].ID != "server" {
		t.Fatalf("background step = %+v", p.Steps[0])
	}
	if p.Steps[1].Kind != "control" || p.Steps[1].Wait != "server" {
		t.Fatalf("wait step = %+v", p.Steps[1])
	}
	if p.Steps[2].Kind != "control" || p.Steps[2].Cancel != "server" {
		t.Fatalf("cancel step = %+v", p.Steps[2])
	}
	if p.Steps[3].Kind != "control" || !p.Steps[3].WaitAll {
		t.Fatalf("wait-all step = %+v", p.Steps[3])
	}
	if p.Steps[4].Kind != "parallel" || len(p.Steps[4].Parallel) != 2 {
		t.Fatalf("parallel step = %+v", p.Steps[4])
	}
	// The two members have distinct output files (no argv/output collision).
	a, b := p.Steps[4].Parallel[0], p.Steps[4].Parallel[1]
	if a.OutputPath == b.OutputPath || a.OutputIndex == b.OutputIndex {
		t.Fatalf("parallel members share an output slot: %q %q", a.OutputPath, b.OutputPath)
	}
	if a.Index != b.Index {
		t.Fatalf("parallel members must share the group display index: %d %d", a.Index, b.Index)
	}
}

func TestPlanBackgroundRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []threads.StepSpec
		want  string
	}{
		{
			"background without id",
			[]threads.StepSpec{{Run: "sleep 1", Background: true}},
			"needs an id",
		},
		{
			"wait names a foreground step",
			[]threads.StepSpec{{ID: "x", Run: "true"}, {Wait: "x"}},
			"not a background step",
		},
		{
			"wait and cancel together",
			[]threads.StepSpec{{ID: "x", Run: "true", Background: true}, {Wait: "x", Cancel: "x"}},
			"mutually exclusive",
		},
		{
			"control with run",
			[]threads.StepSpec{{ID: "x", Run: "true", Background: true}, {Run: "echo", Wait: "x"}},
			"runs no process",
		},
		{
			"parallel with run",
			[]threads.StepSpec{{Run: "echo", Parallel: []threads.StepSpec{{Run: "true"}}}},
			"takes only member steps",
		},
		{
			"nested parallel",
			[]threads.StepSpec{{Parallel: []threads.StepSpec{{Parallel: []threads.StepSpec{{Run: "true"}}}}}},
			"nested parallel",
		},
		{
			"background member",
			[]threads.StepSpec{{Parallel: []threads.StepSpec{{ID: "a", Run: "true", Background: true}}}},
			"ordinary run/uses step",
		},
		{
			"empty parallel",
			[]threads.StepSpec{{Parallel: nil}},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPlan(PlanInput{Job: bgJob(tc.steps...)})
			if tc.want == "" {
				// `parallel: []` is indistinguishable from an omitted key in
				// the decoded spec, so the parser/plan sees an ordinary (empty)
				// step; nothing to refuse.
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExecuteBackgroundWaited(t *testing.T) {
	docker := &fakeDocker{streamOutput: "hello-from-bg\n"}
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "sleep 1", Background: true},
		threads.StepSpec{Name: "wait", Wait: "server"},
	)
	stream, frames := recordingStream(t)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if res.Steps[0].Conclusion != threads.JobSuccess || res.Steps[0].ID != "server" {
		t.Fatalf("background result = %+v (a running placeholder was never replaced)", res.Steps[0])
	}
	if res.Steps[1].Conclusion != threads.JobSuccess {
		t.Fatalf("wait result = %+v", res.Steps[1])
	}
	if log := frames(); !strings.Contains(log, "hello-from-bg") || !strings.Contains(log, "(background)") {
		t.Fatalf("job log misses the background output:\n%s", log)
	}
	// The process was started in its own session and reaped on join/job end.
	if !docker.called("setsid") {
		t.Fatalf("the background step did not run under setsid: %v", docker.snapshot())
	}
}

func TestExecuteBackgroundFailureSurfacedAtWait(t *testing.T) {
	docker := &fakeDocker{streamOutput: "boom\n", streamCode: 3}
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "sleep 1", Background: true},
		threads.StepSpec{Name: "wait", Wait: "server"},
	)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !strings.Contains(res.Error, `background step "server"`) || !strings.Contains(res.Error, "exit code 3") {
		t.Fatalf("error = %q", res.Error)
	}
	if res.Steps[0].Conclusion != threads.JobFailure || res.Steps[0].ExitCode != 3 {
		t.Fatalf("background result = %+v", res.Steps[0])
	}
	if res.Steps[1].Conclusion != threads.JobFailure {
		t.Fatalf("wait step = %+v", res.Steps[1])
	}
}

func TestExecuteBackgroundFailureSurfacedAtJobEnd(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{
		{match: "sleep 1", out: "boom\n", code: 7},
	}}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 1", Background: true})
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	out, _ := e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	if out.Conclusion != "running" {
		t.Fatalf("placeholder = %+v", out)
	}
	// The worker exits non-zero on its own; wait for that observation, then run
	// the job-end pass exactly as Execute does.
	select {
	case <-bg.lookup("server").done:
	case <-time.After(5 * time.Second):
		t.Fatal("the background worker never finished")
	}
	failed, leaked := bg.finishAll()
	if len(leaked) != 0 {
		t.Fatalf("unexpected leak: %v", leaked)
	}
	if len(failed) == 0 || !strings.Contains(failed[0], `background step "server"`) || !strings.Contains(failed[0], "was not waited for") {
		t.Fatalf("an unwaited failing background was dropped: %v", failed)
	}
	if res := bg.lookup("server").join(); res.Conclusion != threads.JobFailure {
		t.Fatalf("final background result = %+v", res)
	}
}

func TestExecuteBackgroundKilledAtJobEnd(t *testing.T) {
	docker := &fakeDocker{blockOn: "sleep 300"}
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true},
		threads.StepSpec{Name: "after", Run: "true"},
	)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("a background killed at job end is cleanup, not a failure: %s (%s)", res.Conclusion, res.Error)
	}
	if !docker.called("kill -TERM") || !docker.called("kill -KILL") {
		t.Fatalf("the process group was not killed: %v", docker.snapshot())
	}
	if res.Steps[0].Conclusion != threads.JobCancelled {
		t.Fatalf("background result = %+v, want cancelled", res.Steps[0])
	}
}

func TestExecuteBackgroundCancelStep(t *testing.T) {
	docker := &fakeDocker{blockOn: "sleep 300"}
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true},
		threads.StepSpec{Name: "stop", Cancel: "server"},
		threads.StepSpec{Name: "wait", Wait: "server"},
	)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !docker.called("kill -TERM") {
		t.Fatalf("cancel never killed the group: %v", docker.snapshot())
	}
	if res.Steps[1].Conclusion != threads.JobSuccess {
		t.Fatalf("cancel step = %+v", res.Steps[1])
	}
	if res.Steps[0].Conclusion != threads.JobCancelled {
		t.Fatalf("cancelled background = %+v", res.Steps[0])
	}
	if res.Steps[2].Conclusion != threads.JobSuccess {
		t.Fatalf("a wait after a deliberate cancel is not a failure: %+v", res.Steps[2])
	}
}

func TestBackgroundTimeoutKillsTheGroup(t *testing.T) {
	docker := &fakeDocker{blockOn: "sleep 300"}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true})
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	p.Steps[0].Timeout = 80 * time.Millisecond
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	bg := newBackgroundSet(e, p, stream, steps, newCommandFiles(), newJobCommands(stream), &jobSummary{})
	out, _ := e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, newCommandFiles(), newJobCommands(stream), &jobSummary{})
	if out.Conclusion != "running" {
		t.Fatalf("placeholder = %+v", out)
	}
	res := bg.lookup("server").wait(context.Background())
	if res.Conclusion != threads.JobTimedOut || !strings.Contains(res.Error, "timed out") {
		t.Fatalf("timeout result = %+v", res)
	}
	if !docker.called("kill -TERM") {
		t.Fatalf("timeout did not kill the group: %v", docker.snapshot())
	}
	_, _ = bg.finishAll()
}

// parallelDocker is the fake with a concurrency counter; it satisfies DockerCLI.
type parallelDocker struct {
	fakeDocker
	mu     sync.Mutex
	active int
	max    int
	delay  time.Duration
}

func TestExecuteParallelMembersRunConcurrently(t *testing.T) {
	docker := &parallelDocker{delay: 120 * time.Millisecond}
	job := bgJob(threads.StepSpec{Name: "pair", Parallel: []threads.StepSpec{
		{Name: "a", Run: "echo a"}, {Name: "b", Run: "echo b"},
	}})
	started := time.Now()
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if docker.max < 2 {
		t.Fatalf("parallel members never overlapped (max concurrency %d)", docker.max)
	}
	if time.Since(started) > 2*docker.delay {
		t.Fatalf("parallel group took %s, longer than two sequential members", time.Since(started))
	}
	if len(res.Steps) != 3 {
		t.Fatalf("steps = %+v (group + two members expected)", res.Steps)
	}
	if res.Steps[1].Name != "a" || res.Steps[2].Name != "b" {
		t.Fatalf("member results are not in declaration order: %+v", res.Steps)
	}
}

func (p *parallelDocker) Stream(ctx context.Context, env []string, w io.Writer, argv ...string) (int, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.max {
		p.max = p.active
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(p.delay):
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	_, _ = w.Write([]byte("member\n"))
	return 0, nil
}

func TestExecuteParallelMemberFailureFailsGroup(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "echo b", code: 4}}}
	job := bgJob(threads.StepSpec{Name: "pair", Parallel: []threads.StepSpec{
		{Name: "a", Run: "echo a"}, {Name: "b", Run: "echo b"},
	}})
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if !strings.Contains(res.Error, "member 2") {
		t.Fatalf("error = %q", res.Error)
	}
	if res.Steps[0].Conclusion != threads.JobFailure {
		t.Fatalf("group result = %+v", res.Steps[0])
	}
}

func TestExecuteParallelLogsInMemberOrder(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{
		{match: "echo a", out: "AAA\n"}, {match: "echo b", out: "BBB\n"},
	}}
	job := bgJob(threads.StepSpec{Name: "pair", Parallel: []threads.StepSpec{
		{Name: "a", Run: "echo a"}, {Name: "b", Run: "echo b"},
	}})
	stream, frames := recordingStream(t)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	log := frames()
	if ia, ib := strings.Index(log, "AAA"), strings.Index(log, "BBB"); ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("member logs are not flushed in declaration order:\n%s", log)
	}
}
