package runner

import (
	"archive/tar"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// JavaScript actions, post hooks, remote materialization and Dockerfile actions
// (PAR-071/072/074) at the runner boundary.

var nodeKey = threads.RemoteActionPrefix + "acme/hello@" + strings.Repeat("a", 40)

func nodeJob(using string, withPost, withPre bool) *threads.FetchResult {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	spec := threads.ActionSpec{
		Path: "/spindle/remote-actions/0123456789abcdef01234567", Using: using,
		Main: "dist/index.js", ThirdParty: true,
		Inputs: map[string]threads.ActionInputSpec{
			"who": {Default: "world", HasDefault: true},
		},
		Files: []threads.ActionFile{
			{Path: "action.yml", Mode: 0o644, Data: []byte("x")},
			{Path: "dist/index.js", Mode: 0o644, Data: []byte("1")},
			{Path: "dist/pre.js", Mode: 0o644, Data: []byte("2")},
			{Path: "dist/post.js", Mode: 0o644, Data: []byte("3")},
		},
		Origin: &threads.ActionOrigin{Source: "instance", Repository: "acme/hello", Ref: strings.Repeat("a", 40),
			Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("b", 64), Using: using, Policy: "sha-pinned"},
	}
	if withPre {
		spec.Pre, spec.PreIf = "dist/pre.js", "always()"
	}
	if withPost {
		spec.Post, spec.PostIf = "dist/post.js", "always()"
	}
	job.Spec.Actions = map[string]threads.ActionSpec{nodeKey: spec}
	job.Spec.Steps = []threads.StepSpec{{
		ID: "hello", Name: "hello action", Uses: nodeKey,
		With: map[string]string{"who": "there"},
	}}
	return job
}

func TestBuildPlanNodeAction(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: nodeJob("node20", true, true), AttemptToken: "wjt_secret"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("steps = %d, want pre + main", len(p.Steps))
	}
	pre, main := p.Steps[0], p.Steps[1]
	if main.Kind != "node" || main.ID != "hello" || main.Node == nil || main.Node.Main != "dist/index.js" {
		t.Fatalf("main = %+v", main)
	}
	if pre.Kind != "node" || pre.Node == nil || pre.Node.Main != "dist/pre.js" || pre.ID != "" {
		t.Fatalf("pre = %+v", pre)
	}
	joined := strings.Join(main.Argv, " ")
	for _, want := range []string{
		"exec", "--env INPUT_WHO=there", "--env GITHUB_ACTION_PATH=/spindle/remote-actions/0123456789abcdef01234567",
		"--env GITHUB_STATE=/spindle/state/", "node /spindle/remote-actions/0123456789abcdef01234567/dist/index.js",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("main argv missing %q\nargv: %s", want, joined)
		}
	}
	// A fetched action is third-party code: no implicit job token, no
	// repository secret env, no OIDC request token (D-5).
	for _, forbidden := range []string{"SPINDLE_JOB_TOKEN", "SECRET_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "SPINDLE_ID_TOKEN_REQUEST_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("third-party action received %s\nargv: %s", forbidden, joined)
		}
	}
	// The post hook is registered for the end of the job, gated on the main.
	if len(p.Posts) != 1 {
		t.Fatalf("posts = %+v", p.Posts)
	}
	if p.Posts[0].Gate != main.OutputIndex || p.Posts[0].Step.Node.Main != "dist/post.js" {
		t.Fatalf("post = %+v gate=%d main=%d", p.Posts[0], p.Posts[0].Gate, main.OutputIndex)
	}
	if !strings.Contains(strings.Join(p.Posts[0].Step.Argv, " "), "dist/post.js") {
		t.Fatalf("post argv = %v", p.Posts[0].Step.Argv)
	}
}

func TestBuildPlanNodeWithoutHooks(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: nodeJob("node24", false, false), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 1 || len(p.Posts) != 0 {
		t.Fatalf("steps/posts = %d/%d", len(p.Steps), len(p.Posts))
	}
	if !strings.Contains(strings.Join(p.Steps[0].Argv, " "), "node ") {
		t.Fatalf("argv = %v", p.Steps[0].Argv)
	}
}

func TestNodeRuntimeVerdictTable(t *testing.T) {
	for _, tc := range []struct {
		using, version string
		probeErr       error
		ok             bool
		want           string
	}{
		{"node20", "v20.11.0", nil, true, ""},
		{"node20", "v22.3.0", nil, true, ""},
		{"node24", "v24.1.0", nil, true, ""},
		{"node24", "v20.11.0", nil, false, "needs Node >= 24"},
		{"node20", "", io.EOF, false, "refused, never skipped"},
		{"node20", "garbage", nil, false, "unparseable"},
	} {
		err := nodeRuntimeVerdict(tc.using, "hello", tc.version, tc.probeErr)
		if (err == nil) != tc.ok {
			t.Fatalf("%s/%s: err = %v", tc.using, tc.version, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s/%s: err = %v, want %q", tc.using, tc.version, err, tc.want)
		}
	}
}

func TestMaterializeActionTar(t *testing.T) {
	job := nodeJob("node20", false, false)
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{}
	e := &Executor{Docker: fd}
	files := []threads.ActionFile{
		{Path: "dist/index.js", Mode: 0o644, Data: []byte("hello")},
		{Path: "run", Mode: 0o755, Data: []byte("#!/bin/sh\n")},
	}
	if err := e.materializeAction(context.Background(), p, "DIR", "hello", files); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// The extract is one docker exec with stdin; the stdin bytes are a tar.
	if len(fd.streamInSinks) != 1 {
		t.Fatalf("streamIn calls = %d", len(fd.streamInSinks))
	}
	tr := tar.NewReader(strings.NewReader(fd.streamInSinks[0].String()))
	got := map[string]int64{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got[hdr.Name] = hdr.Mode
		if hdr.Name == "dist/index.js" && string(body) != "hello" {
			t.Fatalf("body = %q", body)
		}
	}
	if got["dist/index.js"] != 0o644 || got["run"] != 0o755 {
		t.Fatalf("modes = %v", got)
	}
	// Idempotent: the second call is a no-op.
	fd2 := &fakeDocker{}
	e2 := &Executor{Docker: fd2}
	if err := e2.materializeAction(context.Background(), p, "DIR", "hello", files); err != nil {
		t.Fatal(err)
	}
	before := len(fd2.streamInSinks)
	_ = e2.materializeAction(context.Background(), p, "DIR", "hello", files)
	if len(fd2.streamInSinks) != before {
		t.Fatal("the digest directory must be extracted once")
	}
	// A path escape is refused before any tar is written.
	if err := e2.materializeAction(context.Background(), p, "DIR2", "bad", []threads.ActionFile{{Path: "../x", Data: []byte("x")}}); err == nil {
		t.Fatal("path escape must be refused")
	}
}

func TestBuildDockerfileAction(t *testing.T) {
	fd := &fakeDocker{}
	e := &Executor{Docker: fd}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	key := threads.RemoteActionPrefix + "acme/img@" + strings.Repeat("a", 40)
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/x", Using: "docker", Image: "Dockerfile", Dockerfile: true,
		ThirdParty: true,
		Files: []threads.ActionFile{
			{Path: "Dockerfile", Mode: 0o644, Data: []byte("FROM scratch\n")},
			{Path: "entry.sh", Mode: 0o755, Data: []byte("#!/bin/sh\n")},
		},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "img", Uses: key}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if step.Kind != "docker" || step.Docker == nil || step.Docker.Dockerfile == "" {
		t.Fatalf("step = %+v", step)
	}
	stream := quietStream(t)
	tag, err := e.buildDockerfileAction(context.Background(), p, step, stream)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(tag, "sp-action-") {
		t.Fatalf("tag = %q", tag)
	}
	joined := strings.Join(fd.calls, "\n")
	if !strings.Contains(joined, "build --tag "+tag) || !strings.Contains(joined, "--file") {
		t.Fatalf("docker calls = %s", joined)
	}
	// The build context was the action files, not a workspace.
	if !strings.Contains(joined, "Dockerfile") {
		t.Fatalf("docker calls = %s", joined)
	}
	if got := p.builtImageList(); len(got) != 1 || got[0] != tag {
		t.Fatalf("built images = %v", got)
	}
}

func TestSplitArgsTable(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"--check", []string{"--check"}},
		{"-c 'echo hi'", []string{"-c", "echo hi"}},
		{`--name "a b" --flag`, []string{"--name", "a b", "--flag"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"'unterminated value", []string{"unterminated value"}},
	} {
		got := splitArgs(tc.raw)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("%q = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestRemoteCompositeRunStepsGetNoCredential: a fetched composite's run steps
// are third-party code; the plan carries no job token and no repository secret.
func TestRemoteCompositeRunStepsGetNoCredential(t *testing.T) {
	key := threads.RemoteActionPrefix + "acme/comp@" + strings.Repeat("a", 40)
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/comp", Using: "composite", ThirdParty: true,
		Files:  []threads.ActionFile{{Path: "action.yml", Data: []byte("x")}},
		Inputs: map[string]threads.ActionInputSpec{"mode": {Default: "strict", HasDefault: true}},
		Steps:  []threads.StepSpec{{ID: "inner", Run: `echo "$INPUT_MODE"`, Shell: "sh"}},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "comp", Uses: key, With: map[string]string{"mode": "fast"}}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_secret"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	caller := p.Steps[0]
	if caller.Composite == nil || len(caller.Composite.Steps) != 1 {
		t.Fatalf("caller = %+v", caller)
	}
	inner := caller.Composite.Steps[0]
	joined := strings.Join(inner.Argv, " ")
	if !strings.Contains(joined, "--env INPUT_MODE=fast") {
		t.Fatalf("inner argv = %s", joined)
	}
	for _, forbidden := range []string{"SPINDLE_JOB_TOKEN", "SECRET_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("remote composite received %s: %s", forbidden, joined)
		}
	}
	if !inner.ThirdParty {
		t.Fatal("an inner step of a fetched composite must be marked third-party")
	}
}

// TestRunPostStepsOrderAndFailure pins post ordering: reverse registration
// order, gated on the main having started, and a failed post fails the job.
func TestRunPostStepsOrderAndFailure(t *testing.T) {
	job := nodeJob("node20", true, false)
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Posts) != 1 {
		t.Fatalf("posts = %+v", p.Posts)
	}
	fd := &fakeDocker{runOutputs: []runOutput{{match: "node --version", out: "v22.0.0"}}}
	e := &Executor{Docker: fd}
	stream := quietStream(t)
	steps := newStepOutcomes()
	cmds := newCommandFiles()
	jc := newJobCommands(quietStream(t))
	sum := &jobSummary{}
	// The main did NOT run: the post must be skipped.
	res := Result{Conclusion: threads.JobFailure}
	e.runPostSteps(context.Background(), p, stream, steps, cmds, jc, sum, &res)
	if len(fd.calls) != 0 {
		t.Fatalf("post ran for a step that never started: %v", fd.calls)
	}
	// Now the main started and the job failed: the post still runs.
	p.markRan(p.Steps[len(p.Steps)-1].OutputIndex)
	res = Result{Conclusion: threads.JobFailure}
	e.runPostSteps(context.Background(), p, stream, steps, cmds, jc, sum, &res)
	found := false
	for _, call := range fd.calls {
		if strings.Contains(call, "dist/post.js") {
			found = true
		}
	}
	if !found {
		t.Fatalf("post calls = %v", fd.calls)
	}
	// A post hook failure fails an otherwise green job.
	fd2 := &fakeDocker{
		runOutputs:    []runOutput{{match: "node --version", out: "v22.0.0"}},
		streamByMatch: []runOutput{{match: "dist/post.js", code: 3}},
	}
	e2 := &Executor{Docker: fd2}
	p2, _ := BuildPlan(PlanInput{Job: nodeJob("node20", true, false), AttemptToken: "t"})
	p2.markRan(p2.Steps[len(p2.Steps)-1].OutputIndex)
	res2 := Result{}
	e2.runPostSteps(context.Background(), p2, quietStream(t), steps, cmds, jc, sum, &res2)
	if res2.Conclusion != threads.JobFailure || !strings.Contains(res2.Error, "post hook") {
		t.Fatalf("post failure = %+v", res2)
	}
}

// TestCleanupRemovesBuiltImages: a Dockerfile action's local image does not
// outlive the job.
func TestCleanupRemovesBuiltImages(t *testing.T) {
	fd := &fakeDocker{}
	e := &Executor{Docker: fd}
	job := fixtureJob()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	p.addBuiltImage("sp-action-test")
	e.cleanup(p)
	found := false
	for _, call := range fd.calls {
		if strings.Contains(call, "image rm -f sp-action-test") {
			found = true
		}
	}
	if !found {
		t.Fatalf("built image not removed: %v", fd.calls)
	}
}

// TestDockerfileContextRejectsEscape: a hostile action file path never reaches
// the host build context.
func TestDockerfileContextRejectsEscape(t *testing.T) {
	fd := &fakeDocker{}
	e := &Executor{Docker: fd}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	key := threads.RemoteActionPrefix + "acme/img@" + strings.Repeat("a", 40)
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/x", Using: "docker", Image: "Dockerfile", Dockerfile: true,
		Files: []threads.ActionFile{{Path: "Dockerfile", Data: []byte("FROM scratch\n")}, {Path: "../../etc/x", Data: []byte("x")}},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "img", Uses: key}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.buildDockerfileAction(context.Background(), p, p.Steps[0], quietStream(t)); err == nil {
		t.Fatal("a build-context escape must be refused")
	}
}

// TestRunStepBodyNodeRuntime drives the execution path: materialize, probe,
// run. A missing runtime is a named step failure; a present one runs the
// program.
func TestRunStepBodyNodeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outputs []runOutput
		ok      bool
		want    string
	}{
		{"node 20 present", []runOutput{{match: "node --version", out: "v20.11.0"}}, true, ""},
		{"node missing", []runOutput{{match: "node --version", code: 127}}, false, "needs a Node 20 runtime"},
		{"node too old", []runOutput{{match: "node --version", out: "v16.0.0"}}, false, "needs Node >= 20"},
	} {
		job := nodeJob("node20", false, false)
		p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
		if err != nil {
			t.Fatal(err)
		}
		fd := &fakeDocker{runOutputs: tc.outputs}
		e := &Executor{Docker: fd}
		step := p.Steps[0]
		res := e.runStepBody(context.Background(), p, step, quietStream(t), newStepOutcomes(), nil,
			newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, time.Now())
		if tc.ok && res.Conclusion != threads.JobSuccess {
			t.Fatalf("%s: %+v (calls %v)", tc.name, res, fd.calls)
		}
		if !tc.ok && (res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, tc.want)) {
			t.Fatalf("%s: %+v", tc.name, res)
		}
	}
}

// TestRunStepBodyMaterializeFailure: an unmaterializable action fails the step
// with the reason before any node process starts.
func TestRunStepBodyMaterializeFailure(t *testing.T) {
	job := nodeJob("node20", false, false)
	key := nodeKey
	def := job.Spec.Actions[key]
	def.Files = append(def.Files, threads.ActionFile{Path: "../escape", Data: []byte("x")})
	job.Spec.Actions[key] = def
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{}
	e := &Executor{Docker: fd}
	res := e.runStepBody(context.Background(), p, p.Steps[0], quietStream(t), newStepOutcomes(), nil,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, time.Now())
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "escapes") {
		t.Fatalf("res = %+v", res)
	}
}

// TestRunStepBodyDockerfileBuildFailure: a failed `docker build` is a named
// step failure and nothing runs the image.
func TestRunStepBodyDockerfileBuildFailure(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	key := threads.RemoteActionPrefix + "acme/img@" + strings.Repeat("a", 40)
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/x", Using: "docker", Image: "Dockerfile", Dockerfile: true,
		Files: []threads.ActionFile{{Path: "Dockerfile", Data: []byte("FROM scratch\n")}},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "img", Uses: key}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{streamByMatch: []runOutput{{match: "build --tag", code: 2}}}
	e := &Executor{Docker: fd}
	res := e.runStepBody(context.Background(), p, p.Steps[0], quietStream(t), newStepOutcomes(), nil,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, time.Now())
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "build the docker action image") {
		t.Fatalf("res = %+v", res)
	}
}

func TestHookIf(t *testing.T) {
	for _, tc := range []struct{ step, hook, want string }{
		{"", "", ""},
		{"", "always()", "always()"},
		{"steps.a.outcome == 'success'", "", "steps.a.outcome == 'success'"},
		{"a", "always()", "(a) && (always())"},
	} {
		if got := hookIf(tc.step, tc.hook); got != tc.want {
			t.Fatalf("hookIf(%q,%q) = %q, want %q", tc.step, tc.hook, got, tc.want)
		}
	}
}

// TestPlanNodeActionRefusals names the plan-time errors.
func TestPlanNodeActionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*threads.ActionSpec, *threads.StepSpec)
		want string
	}{
		{"unknown input", func(_ *threads.ActionSpec, st *threads.StepSpec) { st.With["nope"] = "x" }, "is not declared"},
		{"no main", func(def *threads.ActionSpec, _ *threads.StepSpec) { def.Main = "" }, "no main program"},
		{"no dir", func(def *threads.ActionSpec, _ *threads.StepSpec) { def.Path = "" }, "no directory"},
		{"bad using", func(def *threads.ActionSpec, _ *threads.StepSpec) { def.Using = "wasm" }, "unsupported using"},
	} {
		job := nodeJob("node20", false, false)
		def := job.Spec.Actions[nodeKey]
		tc.mut(&def, &job.Spec.Steps[0])
		job.Spec.Actions[nodeKey] = def
		if _, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// TestRunPostStepsBranches covers the skip/error arms.
func TestRunPostStepsBranches(t *testing.T) {
	// A post-if that fails to parse fails the job with a named error.
	job := nodeJob("node20", true, false)
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	p.Posts[0].Step.If = "this is not an expression ("
	p.markRan(p.Steps[len(p.Steps)-1].OutputIndex)
	fd := &fakeDocker{runOutputs: []runOutput{{match: "node --version", out: "v22.0.0"}}}
	e := &Executor{Docker: fd}
	res := Result{}
	e.runPostSteps(context.Background(), p, quietStream(t), newStepOutcomes(), newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, &res)
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "post hook") {
		t.Fatalf("bad post-if = %+v", res)
	}
	// A false post-if skips the hook.
	p2, _ := BuildPlan(PlanInput{Job: nodeJob("node20", true, false), AttemptToken: "t"})
	p2.Posts[0].Step.If = "false"
	p2.markRan(p2.Steps[len(p2.Steps)-1].OutputIndex)
	res2 := Result{}
	e2 := &Executor{Docker: &fakeDocker{runOutputs: []runOutput{{match: "node --version", out: "v22.0.0"}}}}
	e2.runPostSteps(context.Background(), p2, quietStream(t), newStepOutcomes(), newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, &res2)
	if res2.Conclusion != "" {
		t.Fatalf("skipped post = %+v", res2)
	}
	// A cancelled job skips the hook with a stated line.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p3, _ := BuildPlan(PlanInput{Job: nodeJob("node20", true, false), AttemptToken: "t"})
	p3.markRan(p3.Steps[len(p3.Steps)-1].OutputIndex)
	res3 := Result{}
	e3 := &Executor{Docker: &fakeDocker{}}
	e3.runPostSteps(ctx, p3, quietStream(t), newStepOutcomes(), newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, &res3)
	if len(e3.Docker.(*fakeDocker).calls) != 0 {
		t.Fatalf("cancelled post ran: %v", e3.Docker.(*fakeDocker).calls)
	}
}

// TestEnsureNodeRuntimeCaches: one probe per job, reused by later node steps.
func TestEnsureNodeRuntimeCaches(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: nodeJob("node20", false, false), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{runOutputs: []runOutput{{match: "node --version", out: "v20.1.0"}}}
	e := &Executor{Docker: fd}
	for i := 0; i < 3; i++ {
		if err := e.ensureNodeRuntime(context.Background(), p, "node20", "x"); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	if len(fd.calls) != 1 {
		t.Fatalf("probes = %d, want 1", len(fd.calls))
	}
	// No files: materialization is a no-op.
	if err := e.materializeAction(context.Background(), p, "DIR", "x", nil); err != nil {
		t.Fatal(err)
	}
}

// TestCompositeMaterializeFailure: a fetched composite whose content escapes
// fails the step before any of its inner steps run.
func TestCompositeMaterializeFailure(t *testing.T) {
	key := threads.RemoteActionPrefix + "acme/comp@" + strings.Repeat("a", 40)
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/comp", Using: "composite", ThirdParty: true,
		Files: []threads.ActionFile{{Path: "../../etc/passwd", Data: []byte("x")}},
		Steps: []threads.StepSpec{{ID: "a", Run: "echo hi", Shell: "sh"}},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "comp", Uses: key}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Docker: &fakeDocker{}}
	res, _ := e.runStep(context.Background(), p, p.Steps[0], quietStream(t), nil, newStepOutcomes(), nil, false,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{})
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "escapes") {
		t.Fatalf("res = %+v", res)
	}
}

// TestRunStepBodyDockerfileBuildAndRun: a successful build substitutes the tag
// into the run argv before the container starts.
func TestRunStepBodyDockerfileBuildAndRun(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	key := threads.RemoteActionPrefix + "acme/img@" + strings.Repeat("a", 40)
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/x", Using: "docker", Image: "Dockerfile", Dockerfile: true,
		Files: []threads.ActionFile{{Path: "Dockerfile", Data: []byte("FROM scratch\n")}},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "img", Uses: key}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{}
	e := &Executor{Docker: fd}
	res := e.runStepBody(context.Background(), p, p.Steps[0], quietStream(t), newStepOutcomes(), nil,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, time.Now())
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("res = %+v (calls %v)", res, fd.calls)
	}
	sawBuild, sawRun := false, false
	for _, call := range fd.calls {
		if strings.HasPrefix(call, "build --tag") {
			sawBuild = true
		}
		if strings.HasPrefix(call, "run ") && strings.Contains(call, "sp-action-") {
			sawRun = true
		}
	}
	if !sawBuild || !sawRun {
		t.Fatalf("calls = %v", fd.calls)
	}
}

// TestRunPostStepsSucceeds: a successful post leaves the job green and appends
// its result.
func TestRunPostStepsSucceeds(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: nodeJob("node20", true, false), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	p.markRan(p.Steps[len(p.Steps)-1].OutputIndex)
	fd := &fakeDocker{runOutputs: []runOutput{{match: "node --version", out: "v22.0.0"}}}
	e := &Executor{Docker: fd}
	res := Result{}
	e.runPostSteps(context.Background(), p, quietStream(t), newStepOutcomes(), newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, &res)
	if res.Conclusion == threads.JobFailure {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Steps) != 1 || res.Steps[0].Name != "hello action (post)" {
		t.Fatalf("steps = %+v", res.Steps)
	}
}

// TestMaterializeExtractFailure: a failed tar extract fails the step.
func TestMaterializeExtractFailure(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: nodeJob("node20", false, false), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{streamByMatch: []runOutput{{match: "tar -xf", code: 2}}}
	e := &Executor{Docker: fd}
	if err := e.materializeAction(context.Background(), p, "DIR", "x", []threads.ActionFile{{Path: "a", Data: []byte("x")}}); err == nil || !strings.Contains(err.Error(), "extract") {
		t.Fatalf("err = %v", err)
	}
	// The no-Dockerfile guard.
	if _, err := e.buildDockerfileAction(context.Background(), p, StepPlan{}, quietStream(t)); err == nil {
		t.Fatal("a missing Dockerfile action must be an internal error")
	}
}

// TestRunStepBodyNodeOutputs: a node action's id publishes outputs from
// $GITHUB_OUTPUT like any other step.
func TestRunStepBodyNodeOutputs(t *testing.T) {
	job := nodeJob("node20", false, false)
	job.Spec.Steps[0].ID = "hello"
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	step := p.Steps[0]
	fd := &fakeDocker{
		runOutputs: []runOutput{
			{match: "node --version", out: "v22.0.0"},
			// collectStepOutputs reads the output file inside the container.
			{match: "head -c", out: "greeting=hi\n"},
		},
	}
	e := &Executor{Docker: fd}
	steps := newStepOutcomes()
	res := e.runStepBody(context.Background(), p, step, quietStream(t), steps, nil,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{}, time.Now())
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("res = %+v (calls %v)", res, fd.calls)
	}
	if got := steps.Outputs["hello"]["greeting"]; got != "hi" {
		t.Fatalf("outputs = %+v", steps.Outputs)
	}
}

// TestPlanRemoteActionEdges covers the docker-input refusal and the display
// name derived from the resolution record when the step has no name.
func TestPlanRemoteActionEdges(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	key := threads.RemoteActionPrefix + "acme/img@" + strings.Repeat("a", 40)
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/x", Using: "docker", Image: "alpine:3.20",
		Inputs: map[string]threads.ActionInputSpec{"mode": {Default: "x", HasDefault: true}},
		Origin: &threads.ActionOrigin{Repository: "acme/img", Path: "sub"},
	}}
	job.Spec.Steps = []threads.StepSpec{{Uses: key, With: map[string]string{"nope": "1"}}}
	if _, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"}); err == nil || !strings.Contains(err.Error(), "is not declared") {
		t.Fatalf("docker input refusal = %v", err)
	}
	// A composite with no step name displays its repository/path.
	compKey := threads.RemoteActionPrefix + "acme/comp@" + strings.Repeat("a", 40)
	job.Spec.Actions[compKey] = threads.ActionSpec{
		Path: "/spindle/remote-actions/c", Using: "composite", ThirdParty: true,
		Origin: &threads.ActionOrigin{Repository: "acme/comp", Path: "sub"},
		Steps:  []threads.StepSpec{{ID: "a", Run: "echo hi", Shell: "sh"}},
	}
	job.Spec.Steps = []threads.StepSpec{{Uses: compKey}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Steps[0].Name != "acme/comp/sub" {
		t.Fatalf("name = %q", p.Steps[0].Name)
	}
	// And the executor path (StepStart/StepEnd) for a node step.
	np, err := BuildPlan(PlanInput{Job: nodeJob("node20", false, false), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	efd := &fakeDocker{runOutputs: []runOutput{{match: "node --version", out: "v22.0.0"}}}
	e := &Executor{Docker: efd}
	res, _ := e.runStep(context.Background(), np, np.Steps[0], quietStream(t), nil, newStepOutcomes(), nil, false,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{})
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("runStep node = %+v", res)
	}
}

// TestRunStepBackgroundWithoutJob and the composite-output failure arms.
func TestRunnerRemainingArms(t *testing.T) {
	// A background step outside a job is a named internal failure.
	p, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Docker: &fakeDocker{}}
	res, _ := e.runStep(context.Background(), p, StepPlan{Kind: "run", Background: true, Name: "bg"}, quietStream(t),
		nil, newStepOutcomes(), nil, false, newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{})
	if res.Conclusion != threads.JobFailure || !strings.Contains(res.Error, "background step outside") {
		t.Fatalf("background = %+v", res)
	}
	// A composite output that reads an output no step published fails the
	// action with a named error.
	key := threads.RemoteActionPrefix + "acme/comp@" + strings.Repeat("a", 40)
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.Spec.Actions = map[string]threads.ActionSpec{key: {
		Path: "/spindle/remote-actions/c", Using: "composite", ThirdParty: true,
		Outputs: map[string]string{"o": "${{ steps.say.outputs.missing }}"},
		Steps:   []threads.StepSpec{{ID: "say", Run: "echo hi", Shell: "sh"}},
	}}
	job.Spec.Steps = []threads.StepSpec{{Name: "comp", Uses: key}}
	cp, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ce := &Executor{Docker: &fakeDocker{}}
	cr, _ := ce.runStep(context.Background(), cp, cp.Steps[0], quietStream(t), nil, newStepOutcomes(), nil, false,
		newCommandFiles(), newJobCommands(quietStream(t)), &jobSummary{})
	if cr.Conclusion != threads.JobFailure || !strings.Contains(cr.Error, "missing") {
		t.Fatalf("composite output = %+v", cr)
	}
	// A remote step with no embedded definition is a plan error.
	job.Spec.Steps = []threads.StepSpec{{Uses: threads.RemoteActionPrefix + "acme/gone@" + strings.Repeat("a", 40)}}
	delete(job.Spec.Actions, key)
	if _, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"}); err == nil || !strings.Contains(err.Error(), "not in the job payload") {
		t.Fatalf("missing definition = %v", err)
	}
}

// TestNilPlanExecGuards: the execution-state helpers are nil-safe (a preview
// Plan or a hand-built plan in a test has no exec map).
func TestNilPlanExecGuards(t *testing.T) {
	var nilPlan *Plan
	nilPlan.markRan(1)
	if nilPlan.wasRan(1) {
		t.Fatal("nil plan has no ran steps")
	}
	if !nilPlan.markMaterialized("x") {
		t.Fatal("nil plan must report already-materialized")
	}
	nilPlan.addBuiltImage("i")
	if nilPlan.builtImageList() != nil {
		t.Fatal("nil plan has no built images")
	}
	empty := &Plan{}
	empty.markRan(1)
	if empty.wasRan(1) || !empty.markMaterialized("x") || empty.builtImageList() != nil {
		t.Fatal("a plan without exec state must be inert")
	}
}
