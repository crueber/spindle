package runner

// Edge coverage for PAR-080/078/179: the paths the happy tests do not reach
// (truncation, a failing kill, an unknown control target, a docker-action
// background, registry-auth parse failures).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

func TestBufferStreamTruncatesAndFlushes(t *testing.T) {
	stream, frames := recordingStream(t)
	buf := newBufferStream(stream, 8)
	if _, err := buf.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if buf.root() != stream {
		t.Fatal("root is not the parent attempt stream")
	}
	buf.flush()
	stream.Close()
	log := frames()
	if !strings.Contains(log, "01234567") || strings.Contains(log, "89abcdef") {
		t.Fatalf("buffer boundary wrong:\n%s", log)
	}
	if !strings.Contains(log, "truncated at 8 bytes") {
		t.Fatalf("missing truncation note:\n%s", log)
	}
	// A zero limit falls back to the background default.
	if defaulted := newBufferStream(stream, 0); defaulted.limit != backgroundOutputLimit {
		t.Fatalf("default limit = %d", defaulted.limit)
	}
}

func TestJoinErrors(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", "b", "b"},
		{"a", "", "a"},
		{"a", "b", "a; b"},
		{"", "", ""},
	} {
		if got := joinErrors(tc.a, tc.b); got != tc.want {
			t.Fatalf("joinErrors(%q,%q) = %q", tc.a, tc.b, got)
		}
	}
}

func TestBackgroundWaitOnCancelledJobKills(t *testing.T) {
	docker := &fakeDocker{blockOn: "sleep 300"}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true})
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := bg.lookup("server").wait(ctx)
	if res.Conclusion != threads.JobCancelled {
		t.Fatalf("cancelled job result = %+v", res)
	}
	if !docker.called("kill -TERM") {
		t.Fatalf("no group kill on a cancelled job: %v", docker.snapshot())
	}
}

func TestBackgroundDockerActionKillIsContainerRemoval(t *testing.T) {
	docker := &fakeDocker{blockOn: "run --name sp-a-"}
	job := bgJob(threads.StepSpec{ID: "job", Name: "sidecar", Uses: "docker://alpine:3.20", Background: true})
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	if kerr := bg.lookup("job").kill("cancel"); kerr != nil {
		t.Fatalf("kill: %v", kerr)
	}
	if !docker.called("rm -f sp-a-") {
		t.Fatalf("a background docker action is not removed by name: %v", docker.snapshot())
	}
	bg.lookup("job").wait(context.Background())
}

func TestBackgroundKillLeakIsReported(t *testing.T) {
	docker := &fakeDocker{
		blockOn:    "sleep 300",
		runOutputs: []runOutput{{match: "kill -TERM", errOut: "process group 7 survived SIGKILL", code: 1}},
	}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true})
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	_, leaked := bg.finishAll()
	if len(leaked) == 0 || !strings.Contains(leaked[0], "survived SIGKILL") {
		t.Fatalf("leak not reported: %v", leaked)
	}
}

func TestControlStepUnknownTargetAndCancelError(t *testing.T) {
	e := executorWith(&fakeDocker{})
	stream := quietStream(t)
	bg := newBackgroundSet(e, &Plan{Names: Names{Container: "c"}}, stream, newStepOutcomes(), newCommandFiles(), newJobCommands(stream), &jobSummary{})
	res, _ := e.runControlStep(context.Background(), stream, bg, StepPlan{Index: 0, Name: "wait", Wait: "missing"})
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "no background step named") {
		t.Fatalf("unknown target = %+v", res)
	}
	// A cancel whose kill fails names the failure.
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true})
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	docker := &fakeDocker{blockOn: "sleep 300", runOutputs: []runOutput{{match: "kill -TERM", errOut: "no such process", code: 1}}}
	e = executorWith(docker)
	stream = quietStream(t)
	bg = newBackgroundSet(e, p, stream, newStepOutcomes(), newCommandFiles(), newJobCommands(stream), &jobSummary{})
	e.runStep(context.Background(), p, p.Steps[0], stream, bg, newStepOutcomes(), nil, false, newCommandFiles(), newJobCommands(stream), &jobSummary{})
	res, _ = e.runControlStep(context.Background(), stream, bg, StepPlan{Index: 1, Name: "cancel", Cancel: "server"})
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "kill process group") {
		t.Fatalf("cancel error = %+v", res)
	}
}

func TestParallelPlanRefusalsMore(t *testing.T) {
	for _, tc := range []struct {
		name string
		step threads.StepSpec
		want string
	}{
		{"member neither run nor uses", threads.StepSpec{Parallel: []threads.StepSpec{{Name: "x"}}}, "neither run nor uses"},
		{"group background", threads.StepSpec{Background: true, Parallel: []threads.StepSpec{{Run: "true"}}}, "cannot itself be background"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPlan(PlanInput{Job: bgJob(tc.step)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFinishAllJoinsAnExitedSuccessfulBackground(t *testing.T) {
	docker := &fakeDocker{streamOutput: "done\n"}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "true", Background: true})
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
	b := bg.lookup("server")
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("background never finished")
	}
	failed, leaked := bg.finishAll()
	if len(failed) != 0 || len(leaked) != 0 {
		t.Fatalf("failed=%v leaked=%v", failed, leaked)
	}
	if res := b.join(); res.Conclusion != threads.JobSuccess {
		t.Fatalf("result = %+v", res)
	}
}

func TestMentionsDockerPodman(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "podman build", code: 127}}}
	job := bgJob(threads.StepSpec{Name: "build", Run: "podman build ."})
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if !strings.Contains(res.Error, "daemonless") {
		t.Fatalf("podman remedy = %q", res.Error)
	}
}

func TestRegistryAuthNilHelpers(t *testing.T) {
	var auth *RegistryAuth
	if auth.Env() != nil || auth.Masks() != nil {
		t.Fatal("nil auth must be inert")
	}
	auth.Cleanup()
	var e *Executor
	if e.RegistryMasks() != nil {
		t.Fatal("nil executor must be inert")
	}
}

func TestPrepareRegistryAuthEdges(t *testing.T) {
	if got, err := PrepareRegistryAuth(context.Background(), "docker", ""); got != nil || err != nil {
		t.Fatalf("empty path = %v %v", got, err)
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRegistryAuth(context.Background(), "docker", empty); err == nil || !strings.Contains(err.Error(), "names no registry") {
		t.Fatalf("empty file error = %v", err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("registry.example.com bob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRegistryAuth(context.Background(), "docker", bad); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("malformed line error = %v", err)
	}
	// A tab-separated line is accepted, and a failing login is a startup error
	// that never echoes the password.
	tab := filepath.Join(dir, "tab")
	if err := os.WriteFile(tab, []byte("registry.example.com\tbob\ts3cr3t-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fail-docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"login failed for $4\" >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRegistryAuth(context.Background(), script, tab)
	if err == nil || !strings.Contains(err.Error(), "docker login") {
		t.Fatalf("login failure error = %v", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("password leaked in the error: %v", err)
	}
}

func TestParseRegistryAuthFileTabsAndComments(t *testing.T) {
	creds, err := parseRegistryAuthFile([]byte("# comment\n\nreg.example.com\tu\tp\nother r s\n"))
	if err != nil || len(creds) != 2 || creds[0].registry != "reg.example.com" || creds[1].password != "s" {
		t.Fatalf("creds = %+v err = %v", creds, err)
	}
	if _, err := parseRegistryAuthFile([]byte("reg u\n")); err == nil {
		t.Fatal("a two-field line must be refused")
	}
}

func TestCommandFilesCloneAndMerge(t *testing.T) {
	base := newCommandFiles()
	if err := base.addEnv(map[string]string{"A": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := base.addPath([]string{"/bin"}); err != nil {
		t.Fatal(err)
	}
	clone := base.clone()
	if clone.env["A"] != "1" || !clone.pathSet["/bin"] {
		t.Fatalf("clone = %+v", clone)
	}
	if err := clone.addEnv(map[string]string{"B": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := clone.mergeFrom(base); err != nil {
		t.Fatalf("merge unchanged: %v", err)
	}
	if err := base.mergeFrom(clone); err != nil {
		t.Fatalf("merge delta: %v", err)
	}
	if base.env["B"] != "2" {
		t.Fatalf("merged env = %v", base.env)
	}
	if err := base.mergeFrom(nil); err != nil {
		t.Fatalf("nil merge: %v", err)
	}
}

func TestWithProcessGroupRejectsMissingContainer(t *testing.T) {
	if _, ok := withProcessGroup([]string{"exec", "sh", "-c", "true"}, "absent", "/tmp/p"); ok {
		t.Fatal("a missing container must not produce a wrapper")
	}
}

func TestControlStepWaitAllWithNothingAndNilSet(t *testing.T) {
	e := executorWith(&fakeDocker{})
	stream := quietStream(t)
	res, _ := e.runControlStep(context.Background(), stream, nil, StepPlan{Name: "wait"})
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "outside a job") {
		t.Fatalf("nil background set = %+v", res)
	}
	bg := newBackgroundSet(e, &Plan{Names: Names{Container: "c"}}, stream, newStepOutcomes(), newCommandFiles(), newJobCommands(stream), &jobSummary{})
	res, _ = e.runControlStep(context.Background(), stream, bg, StepPlan{Name: "wait all", WaitAll: true})
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("wait-all with nothing outstanding = %+v", res)
	}
	res, _ = e.runControlStep(context.Background(), stream, bg, StepPlan{Name: "cancel all", Cancel: "all"})
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("cancel-all with nothing outstanding = %+v", res)
	}
}

func TestValidateBackgroundPlanCompositeAndIDs(t *testing.T) {
	// background inside a composite action is refused by name.
	job := fixtureJob()
	job.Spec.Actions = map[string]threads.ActionSpec{
		"act": {Path: "act", Using: "composite", Steps: []threads.StepSpec{{ID: "x", Run: "true", Background: true}}},
	}
	job.Spec.Steps = []threads.StepSpec{{Uses: "./act"}}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "not accepted inside a composite action") {
		t.Fatalf("composite background error = %v", err)
	}
	// a control step cannot own an id.
	if _, err := BuildPlan(PlanInput{Job: bgJob(
		threads.StepSpec{ID: "x", Run: "true", Background: true},
		threads.StepSpec{ID: "w", Wait: "x"},
	)}); err == nil || !strings.Contains(err.Error(), "cannot own an id") {
		t.Fatalf("control id error = %v", err)
	}
	// a background composite step is refused.
	job2 := fixtureJob()
	job2.Spec.Actions = map[string]threads.ActionSpec{
		"act": {Path: "act", Using: "composite", Steps: []threads.StepSpec{{Run: "true"}}},
	}
	job2.Spec.Steps = []threads.StepSpec{{ID: "c", Uses: "./act", Background: true}}
	if _, err := BuildPlan(PlanInput{Job: job2}); err == nil || !strings.Contains(err.Error(), "only on a run or docker step") {
		t.Fatalf("background composite error = %v", err)
	}
}

func TestParallelMemberResolveErrorFailsTheGroup(t *testing.T) {
	// A member naming a step output that was never published fails at resolve
	// time; the group reports it and the member's failure.
	job := bgJob(
		threads.StepSpec{ID: "first", Run: "true"},
		threads.StepSpec{Name: "pair", Parallel: []threads.StepSpec{
			{Name: "a", Run: "echo ${{ steps.missing.outputs.x }}"},
			{Name: "b", Run: "echo b"},
		}},
	)
	res := executorWith(&fakeDocker{}).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "missing") {
		t.Fatalf("resolve error = %s (%q)", res.Conclusion, res.Error)
	}
}

func TestExecuteReportsAnUnwaitedFailedBackgroundAtJobEnd(t *testing.T) {
	docker := &fakeDocker{
		streamByMatch: []runOutput{{match: "bg-fails", out: "boom\n", code: 9}},
		delayByMatch:  map[string]time.Duration{"slow-step": 250 * time.Millisecond},
	}
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "bg-fails", Background: true},
		threads.StepSpec{Name: "slow", Run: "slow-step"},
	)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "was not waited for") {
		t.Fatalf("job = %s (%q)", res.Conclusion, res.Error)
	}
	if res.Steps[0].Conclusion != threads.JobFailure {
		t.Fatalf("background result = %+v", res.Steps[0])
	}
}

func TestRegistryAuthEnvAndCleanup(t *testing.T) {
	auth := &RegistryAuth{ConfigDir: t.TempDir(), masks: []string{"x"}}
	if env := auth.Env(); len(env) != 1 || !strings.HasPrefix(env[0], "DOCKER_CONFIG=") {
		t.Fatalf("env = %v", env)
	}
	auth.Cleanup()
	if _, err := os.Stat(auth.ConfigDir); !os.IsNotExist(err) {
		t.Fatalf("config dir survived cleanup: %v", err)
	}
}

func TestBackgroundStartRefusesUnwrappableArgv(t *testing.T) {
	e := executorWith(&fakeDocker{})
	stream := quietStream(t)
	bg := newBackgroundSet(e, &Plan{Names: Names{Container: "c"}}, stream, newStepOutcomes(), newCommandFiles(), newJobCommands(stream), &jobSummary{})
	step := StepPlan{Index: 0, ID: "x", Name: "x", Kind: "run", Background: true, Argv: []string{"exec", "sh", "-c", "true"}}
	out := bg.start(context.Background(), step, stream, newStepOutcomes(), newCommandFiles(), newJobCommands(stream), &jobSummary{})
	if out.Conclusion != threads.JobFailure || !strings.Contains(out.Error, "could not wrap") {
		t.Fatalf("unwrappable argv = %+v", out)
	}
}

func TestBackgroundKillIsIdempotent(t *testing.T) {
	docker := &fakeDocker{blockOn: "sleep 300"}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true})
	p, _ := BuildPlan(PlanInput{Job: job})
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	b := bg.lookup("server")
	if err := b.kill("cancel"); err != nil {
		t.Fatal(err)
	}
	if err := b.kill("timeout"); err != nil {
		t.Fatalf("second kill = %v", err)
	}
	b.wait(context.Background())
	n := docker.count("kill -TERM")
	if n != 1 {
		t.Fatalf("kill ran %d times", n)
	}
}

func TestBackgroundJoinBeforeTheWorkerEnds(t *testing.T) {
	docker := &fakeDocker{blockOn: "sleep 300"}
	job := bgJob(threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true})
	p, _ := BuildPlan(PlanInput{Job: job})
	e := executorWith(docker)
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	bg := newBackgroundSet(e, p, stream, steps, cmds, newJobCommands(stream), &jobSummary{})
	e.runStep(context.Background(), p, p.Steps[0], stream, bg, steps, nil, false, cmds, newJobCommands(stream), &jobSummary{})
	// join without wait: the worker is still blocked, so the result is a
	// cancelled step with a stated bounded-wait error (never a silent success).
	res := bg.lookup("server").join()
	if res.Conclusion != threads.JobCancelled || !strings.Contains(res.Error, "did not stop") {
		t.Fatalf("join before done = %+v", res)
	}
	_ = bg.lookup("server").kill("job end")
}

func TestBufferStreamSecondWriteAfterLimit(t *testing.T) {
	stream, frames := recordingStream(t)
	buf := newBufferStream(stream, 4)
	buf.StepStart(StepPlan{Kind: "checkout", Name: "co"})
	buf.Write([]byte("abcdef"))
	buf.Write([]byte("more"))
	buf.Write([]byte("even more"))
	buf.flush()
	second := newBufferStream(stream, 256)
	second.StepStart(StepPlan{Kind: "checkout", Name: "co"})
	second.StepEnd(StepPlan{Name: "co"}, threads.JobFailure, 2, 5)
	second.flush()
	stream.Close()
	log := frames()
	for _, want := range []string{"truncated at 4 bytes", "(actions/checkout)", "exit 2"} {
		if !strings.Contains(log, want) {
			t.Fatalf("buffer log misses %q:\n%s", want, log)
		}
	}
}

func TestMentionsDockerPlainScript(t *testing.T) {
	if mentionsDocker("echo hello") {
		t.Fatal("a plain script was treated as a docker command")
	}
}

func TestPlanCompositeParallelRefusal(t *testing.T) {
	job := fixtureJob()
	job.Spec.Actions = map[string]threads.ActionSpec{
		"act": {Path: "act", Using: "composite", Steps: []threads.StepSpec{{Parallel: []threads.StepSpec{{Run: "true"}}}}},
	}
	job.Spec.Steps = []threads.StepSpec{{Uses: "./act"}}
	if _, err := BuildPlan(PlanInput{Job: job}); err == nil || !strings.Contains(err.Error(), "not accepted inside a composite") {
		t.Fatalf("composite parallel error = %v", err)
	}
}

func TestPlanParallelMemberPlanErrorPropagates(t *testing.T) {
	if _, err := BuildPlan(PlanInput{Job: bgJob(threads.StepSpec{Parallel: []threads.StepSpec{{Uses: "./missing"}}})}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("member error = %v", err)
	}
}

func TestExecuteReportsALeakedBackgroundAlongsideAFailure(t *testing.T) {
	docker := &fakeDocker{
		blockOn:    "sleep 300",
		runOutputs: []runOutput{{match: "kill -TERM", errOut: "process group 5 survived SIGKILL", code: 1}},
	}
	job := bgJob(
		threads.StepSpec{ID: "server", Name: "server", Run: "sleep 300", Background: true},
		threads.StepSpec{Name: "fail", Run: "fail-now"},
	)
	docker.streamByMatch = []runOutput{{match: "fail-now", code: 1}}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	if !strings.Contains(res.Error, "exit code 1") || !strings.Contains(res.Error, "survived SIGKILL") {
		t.Fatalf("error must carry both the step failure and the leak: %q", res.Error)
	}
}

func TestExecuteWaitAllJoinsEveryBackground(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{
		{match: "first-bg", out: "one\n"}, {match: "second-bg", out: "two\n"},
	}}
	job := bgJob(
		threads.StepSpec{ID: "one", Name: "one", Run: "first-bg", Background: true},
		threads.StepSpec{ID: "two", Name: "two", Run: "second-bg", Background: true},
		threads.StepSpec{Name: "wait all", WaitAll: true},
	)
	stream, frames := recordingStream(t)
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, stream)
	stream.Close()
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%s)", res.Conclusion, res.Error)
	}
	if res.Steps[0].Conclusion != threads.JobSuccess || res.Steps[1].Conclusion != threads.JobSuccess {
		t.Fatalf("backgrounds = %+v", res.Steps[:2])
	}
	log := frames()
	if strings.Index(log, "one") > strings.Index(log, "two") {
		t.Fatalf("wait-all output order:\n%s", log)
	}
}

func TestParallelContinueOnErrorTolerance(t *testing.T) {
	docker := &fakeDocker{streamByMatch: []runOutput{{match: "echo b", code: 3}}}
	job := bgJob(threads.StepSpec{Name: "pair", ContinueOnError: true, Parallel: []threads.StepSpec{
		{Name: "a", Run: "echo a"},
		{Name: "b", Run: "echo b", ContinueOnError: true},
	}})
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("tolerated member failure = %s (%s)", res.Conclusion, res.Error)
	}
}

func TestCompositeStepGetsActionPath(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Container = nil
	job.Spec.Actions = map[string]threads.ActionSpec{
		"act": {Path: "act", Using: "composite", Steps: []threads.StepSpec{
			{Name: "here", Run: "echo ${{ github.action_path }}"},
		}},
	}
	job.Spec.Steps = []threads.StepSpec{{Name: "caller", Uses: "./act"}}
	p, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	var inner StepPlan
	for _, st := range p.Steps[0].Composite.Steps {
		if st.Kind == "run" {
			inner = st
		}
	}
	want := p.Workspace + "/act"
	if !strings.Contains(strings.Join(inner.Argv, " "), want) {
		t.Fatalf("github.action_path not substituted: %v (want %q)", inner.Argv, want)
	}
	// A nested action gets its own directory, not the caller's.
	job.Spec.Actions["outer"] = threads.ActionSpec{Path: "outer", Using: "composite", Steps: []threads.StepSpec{
		{Name: "nested", Uses: "./act"},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "caller", Uses: "./outer"}}
	p2, err := BuildPlan(PlanInput{Job: job})
	if err != nil {
		t.Fatalf("nested BuildPlan: %v", err)
	}
	nested := p2.Steps[0].Composite.Steps[0].Composite.Steps[0]
	if !strings.Contains(strings.Join(nested.Argv, " "), p2.Workspace+"/act") {
		t.Fatalf("nested action_path = %v", nested.Argv)
	}
}
