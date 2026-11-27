package runner

// `$GITHUB_STATE` for pre/post hooks (PAR-092): the pre/main phase's saved state
// reaches the post hook as `STATE_<name>`, a state value never enters argv, the
// deprecated `::save-state::` spelling feeds the same map, and the caps and
// malformed-line rules match the other command files.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// TestActionStateReachesPostHook drives the pre → post hand-off end to end: the
// pre hook's state file is collected, and the post hook's argv carries the state
// BY NAME while the value travels in the docker CLI's environment.
func TestActionStateReachesPostHook(t *testing.T) {
	job := nodeJob("node20", true, true)
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 2 || len(p.Posts) != 1 {
		t.Fatalf("steps/posts = %d/%d", len(p.Steps), len(p.Posts))
	}
	pre := p.Steps[0]
	if pre.Node == nil || pre.Node.StatePath == "" {
		t.Fatalf("pre has no state path: %+v", pre.Node)
	}
	statePath := pre.Node.StatePath

	fd := &fakeDocker{runOutputs: []runOutput{
		{match: "node --version", out: "v22.0.0"},
		// $GITHUB_STATE as actions/cache writes it: a hyphenated name (the
		// post hook reads process.env["STATE_cache-primary-key"]).
		{match: "/spindle/state/", out: "cache-primary-key=go-v1\ncache-secondary=.\n"},
	}}
	e := &Executor{Docker: fd}
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	jc := newJobCommands(quietStream(t))
	sum := &jobSummary{}

	preOut := e.runStepBody(context.Background(), p, pre, stream, steps, nil, cmds, jc, sum, time.Now())
	if preOut.Conclusion != threads.JobSuccess {
		t.Fatalf("pre = %+v (%v)", preOut, fd.calls)
	}
	if got := p.actionState().snapshot(statePath); len(got) != 2 || got["cache-primary-key"] != "go-v1" {
		t.Fatalf("state after pre = %v", got)
	}

	// The main failed: the post hook still runs and still receives the state.
	p.markRan(p.Posts[0].Gate)
	res := Result{Conclusion: threads.JobFailure}
	e.runPostSteps(context.Background(), p, stream, steps, cmds, jc, sum, &res)
	postArgv, postEnv := lastCallWith(fd, "dist/post.js")
	if postArgv == "" {
		t.Fatalf("the post hook did not run: %v", fd.calls)
	}
	for _, want := range []string{"--env STATE_cache-primary-key", "--env STATE_cache-secondary"} {
		if !strings.Contains(postArgv, want) {
			t.Fatalf("post argv misses %q\nargv: %s", want, postArgv)
		}
	}
	if strings.Contains(postArgv, "go-v1") {
		t.Fatalf("a state VALUE reached argv: %s", postArgv)
	}
	if !containsEnv(postEnv, "STATE_cache-primary-key=go-v1") {
		t.Fatalf("the state value did not reach the docker CLI environment: %v", postEnv)
	}
}

// lastCallWith returns the Stream argv and env of the last call containing want.
func lastCallWith(fd *fakeDocker, want string) (string, []string) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	for i := len(fd.calls) - 1; i >= 0; i-- {
		if strings.Contains(fd.calls[i], want) {
			return fd.calls[i], fd.envs[i]
		}
	}
	return "", nil
}

func containsEnv(env []string, want string) bool {
	for _, v := range env {
		if v == want {
			return true
		}
	}
	return false
}

// TestActionStateFileWinsOverSaveState: both spellings feed one map, and the
// file wins a collision (the same rule $GITHUB_OUTPUT uses against ::set-output).
func TestActionStateFileWinsOverSaveState(t *testing.T) {
	state := newActionState()
	job := nodeJob("node20", false, true)
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	p.exec.state = state
	path := p.Steps[0].Node.StatePath
	sink := newCommandSink(&markerBuffer{}, newJobCommands(quietStream(t)),
		StepPlan{Name: "main", Node: &NodePlan{StatePath: path}})
	sink.state = state
	_, _ = sink.Write([]byte("::save-state name=cache-primary-key::from-command\n"))
	_ = sink.Close()
	if got := state.snapshot(path)["cache-primary-key"]; got != "from-command" {
		t.Fatalf("save-state = %q", got)
	}
	fd := &fakeDocker{runOutputs: []runOutput{
		{match: "node --version", out: "v22.0.0"},
		{match: "/spindle/state/", out: "cache-primary-key=from-file\n"},
	}}
	e := &Executor{Docker: fd}
	if err := e.collectActionState(context.Background(), p, p.Steps[0], quietStream(t)); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := state.snapshot(path)["cache-primary-key"]; got != "from-file" {
		t.Fatalf("the file must win: %q", got)
	}
}

// TestSaveStateOutsideActionIsNamed: a run step has no post hook, so the
// command is refused by name instead of accepted into nowhere.
func TestSaveStateOutsideActionIsNamed(t *testing.T) {
	var buf markerBuffer
	sink := newCommandSink(&buf, newJobCommands(quietStream(t)), StepPlan{Name: "run"})
	sink.state = newActionState()
	_, _ = sink.Write([]byte("::save-state name=x::1\n"))
	_ = sink.Close()
	if !strings.Contains(buf.String(), "is ignored here") {
		t.Fatalf("log = %q", buf.String())
	}
	// A missing name is named too.
	buf.Reset()
	_, _ = sink.Write([]byte("::save-state::1\n"))
	_ = sink.Close()
	if !strings.Contains(buf.String(), "needs a name") {
		t.Fatalf("log = %q", buf.String())
	}
}

// TestParseStateFile is the parser's table: CRLF, '=' inside a value, last wins,
// and the named failures.
func TestParseStateFile(t *testing.T) {
	entries, err := parseStateFile([]byte("a=1\r\n\r\nb=x=y\ncache-primary-key=k1\na=2\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]string{}
	for _, en := range entries {
		got[en.Name] = en.Value
	}
	if len(got) != 3 || got["a"] != "2" || got["b"] != "x=y" || got["cache-primary-key"] != "k1" {
		t.Fatalf("entries = %v", got)
	}
	for _, tc := range []struct {
		data string
		want string
	}{
		{"a\n", "expected NAME=VALUE"},
		{"=1\n", "expected NAME=VALUE"},
		{"a b=1\n", "may contain letters"},
		{"-x=1\n", "may contain letters"},
		{"a=" + strings.Repeat("x", MaxStateValueBytes+1) + "\n", "over the cap"},
	} {
		if _, err := parseStateFile([]byte(tc.data)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: err = %v, want %q", tc.data[:min(len(tc.data), 20)], err, tc.want)
		}
	}
}

// TestActionStateCaps: the per-action entry cap is named, and a refused entry
// leaves no partial state.
func TestActionStateCaps(t *testing.T) {
	state := newActionState()
	for i := 0; i < MaxStateEntries; i++ {
		if err := state.set("/spindle/state/1", "k"+strconv.Itoa(i), "v"); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if err := state.set("/spindle/state/1", "overflow", "v"); err == nil || !strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("err = %v", err)
	}
	// A different action invocation has its own budget.
	if err := state.set("/spindle/state/2", "overflow", "v"); err != nil {
		t.Fatalf("second action: %v", err)
	}
	if err := state.set("/spindle/state/1", "bad name", "v"); err == nil || !strings.Contains(err.Error(), "STATE_bad name") {
		t.Fatalf("err = %v", err)
	}
	if err := state.set("/spindle/state/1", "k0", strings.Repeat("x", MaxStateValueBytes+1)); err == nil || !strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("err = %v", err)
	}
}

// TestActionStateNilPlanIsSafe: the helpers never panic on a plan built by hand
// (no per-execution state), which the unit tests rely on.
func TestActionStateNilPlanIsSafe(t *testing.T) {
	var p *Plan
	if p.actionState() != nil {
		t.Fatal("nil plan has state")
	}
	step := applyActionState(p, StepPlan{Node: &NodePlan{IsPost: true}}, map[string]string{"x": "1"})
	if step.EnvState != nil {
		t.Fatalf("step = %+v", step)
	}
	e := &Executor{Docker: &fakeDocker{}}
	if err := e.collectActionState(context.Background(), nil, StepPlan{}, quietStream(t)); err != nil {
		t.Fatalf("collect: %v", err)
	}
}

// TestCheckContainerLifetime pins the named failure for a workflow-owned
// container whose process exited before the first step (PAR-077).
func TestCheckContainerLifetime(t *testing.T) {
	workflowPlan := func() *Plan {
		return &Plan{Names: Names{Container: "c"}, ContainerLifetime: ContainerLifetimeWorkflow}
	}
	for _, tc := range []struct {
		name   string
		plan   *Plan
		output runOutput
		want   string
	}{
		{"running", workflowPlan(), runOutput{match: "inspect", out: "true|0"}, ""},
		{"exited zero", workflowPlan(), runOutput{match: "inspect", out: "false|0"}, "exited (code 0)"},
		{"exited three", workflowPlan(), runOutput{match: "inspect", out: "false|3"}, "exited (code 3)"},
		{"dead", workflowPlan(), runOutput{match: "inspect", out: "dead|1"}, `is "dead"`},
		{"inspect fails", workflowPlan(), runOutput{match: "inspect", errOut: "no such container", code: 1}, "inspect the job container"},
		{"keep-alive is never inspected", &Plan{Names: Names{Container: "c"}, ContainerLifetime: ContainerLifetimeKeepAlive}, runOutput{match: "inspect", out: "false|0"}, ""},
		{"nil plan", nil, runOutput{match: "inspect", out: "false|0"}, ""},
	} {
		fd := &fakeDocker{runOutputs: []runOutput{tc.output}}
		e := &Executor{Docker: fd}
		err := e.checkContainerLifetime(context.Background(), tc.plan)
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// A cancelled job does not wait out the settle window.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Executor{Docker: &fakeDocker{}}).checkContainerLifetime(ctx, workflowPlan()); err == nil {
		t.Fatal("a cancelled context must return its error")
	}
}

// TestCollectActionStateFailures: a read failure, a malformed file and an
// over-cap file are named on a step that would otherwise succeed.
func TestCollectActionStateFailures(t *testing.T) {
	plan := func() (*Plan, StepPlan) {
		p, err := BuildPlan(PlanInput{Job: nodeJob("node20", false, false), AttemptToken: "t"})
		if err != nil {
			t.Fatal(err)
		}
		return p, p.Steps[0]
	}
	for _, tc := range []struct {
		name   string
		output runOutput
		want   string
	}{
		{"read fails", runOutput{match: "/spindle/state/", errOut: "boom", code: 1}, "GITHUB_STATE"},
		{"malformed", runOutput{match: "/spindle/state/", out: "not a pair\n"}, "expected NAME=VALUE"},
		{"over the entry cap", runOutput{match: "/spindle/state/", out: stateEntries(MaxStateEntries + 1)}, "over the cap"},
	} {
		p, step := plan()
		e := &Executor{Docker: &fakeDocker{runOutputs: []runOutput{tc.output}}}
		err := e.collectActionState(context.Background(), p, step, quietStream(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// A cancelled step never reads a file from a killed container.
	p, step := plan()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Executor{Docker: &fakeDocker{}}).collectActionState(ctx, p, step, quietStream(t)); err != nil {
		t.Fatalf("cancelled: %v", err)
	}
	// An argv shape without the container name is left alone.
	out := applyActionState(p, StepPlan{Node: &NodePlan{IsPost: true}, Argv: []string{"exec", "x"}}, map[string]string{"a": "1"})
	if out.EnvState != nil {
		t.Fatalf("foreign argv was mangled: %+v", out)
	}
	// A plan without per-execution state is safe.
	bare := &Plan{Names: Names{Container: "c"}}
	if err := (&Executor{Docker: &fakeDocker{}}).collectActionState(context.Background(), bare, StepPlan{Node: &NodePlan{StatePath: "/spindle/state/0"}}, quietStream(t)); err != nil {
		t.Fatalf("bare plan: %v", err)
	}
}

// stateEntries renders n distinct state lines.
func stateEntries(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("k" + strconv.Itoa(i) + "=1\n")
	}
	return b.String()
}

// TestActionStateSetRequiresPath: the internal guard for a missing path.
func TestActionStateSetRequiresPath(t *testing.T) {
	if err := newActionState().set("", "a", "1"); err == nil {
		t.Fatal("an empty path must be refused")
	}
	if got := newActionState().snapshot(""); got != nil {
		t.Fatalf("snapshot(\"\") = %v", got)
	}
}
