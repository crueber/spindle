package workflow

import (
	"strings"
	"testing"
)

// Pass A parser breadth (docs/features/15_threads.md §2.1): the shapes CI
// authors actually push — on string/list forms, dispatch inputs, concurrency,
// matrix, permissions, list runs-on, mapped container, job-level uses — parse
// and are carried, and unknown keys still fail (per file, at the caller).

const broad = `
name: Broad
on:
  push:
    branches: [main]
  pull_request:
    types: [opened, synchronize, reopened]
  workflow_dispatch:
    inputs:
      level:
        description: "release level"
        type: choice
        required: true
        default: patch
        options: [patch, minor, major]
      dry_run:
        type: boolean
        default: true
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
permissions:
  contents: read
  packages: write
jobs:
  build:
    runs-on: [linux, docker]
    container:
      image: golang:1.27
      env: {CGO_ENABLED: "0"}
      entrypoint: ["/bin/sh", "-c"]
      volumes: ["cache:/go/pkg/mod"]
    strategy:
      fail-fast: false
      matrix:
        go: ["1.26", "1.27"]
        os: [linux]
    permissions:
      contents: write
    steps:
      - uses: actions/checkout
        with: {fetch-depth: "0"}
      - name: build
        run: go build ./...
        env: {GOFLAGS: "-mod=readonly"}
        if: success()
        shell: bash
        working-directory: ./src
  reuse:
    uses: acme/workflows/.spindle/workflows/build.yml@main
    with: {target: linux}
`

func TestParseBroadShapes(t *testing.T) {
	w, err := Parse([]byte(broad))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := w.Events(); strings.Join(got, ",") != "push,pull_request,workflow_dispatch" {
		t.Fatalf("events = %v", got)
	}
	if w.Concurrency == nil || w.Concurrency.Group != "ci-${{ github.ref }}" || !w.Concurrency.CancelInProgress {
		t.Fatalf("workflow concurrency = %+v", w.Concurrency)
	}
	if w.Permissions["contents"] != "read" || w.Permissions["packages"] != "write" {
		t.Fatalf("workflow permissions = %+v", w.Permissions)
	}
	d := w.On.WorkflowDispatch
	if d == nil || len(d.Inputs) != 2 {
		t.Fatalf("dispatch = %+v", d)
	}
	level := d.Inputs["level"]
	if level.Type != "choice" || !level.Required || level.Default == nil || *level.Default != "patch" || len(level.Options) != 3 {
		t.Fatalf("level input = %+v", level)
	}
	dry := d.Inputs["dry_run"]
	if dry.Type != "boolean" || dry.Default == nil || *dry.Default != "true" || dry.Required {
		t.Fatalf("dry_run input = %+v", dry)
	}
	if pr := w.On.PullRequest; pr == nil || len(pr.Types) != 3 {
		t.Fatalf("pull_request = %+v", pr)
	}

	build := w.Jobs["build"]
	if strings.Join(build.RunsOn, ",") != "linux,docker" {
		t.Fatalf("runs-on = %v", build.RunsOn)
	}
	if build.Container == nil || build.Container.Image != "golang:1.27" || build.Container.Env["CGO_ENABLED"] != "0" ||
		len(build.Container.Entrypoint) != 2 || len(build.Container.Volumes) != 1 {
		t.Fatalf("container = %+v", build.Container)
	}
	if build.Strategy == nil || build.Strategy.FailFast == nil || *build.Strategy.FailFast {
		t.Fatalf("strategy = %+v", build.Strategy)
	}
	if len(build.Strategy.Matrix.Axes["go"]) != 2 || len(build.Strategy.Matrix.Axes["os"]) != 1 {
		t.Fatalf("matrix = %+v", build.Strategy.Matrix)
	}
	if build.Permissions["contents"] != "write" {
		t.Fatalf("job permissions = %+v", build.Permissions)
	}
	if len(build.Steps) != 2 {
		t.Fatalf("steps = %+v", build.Steps)
	}
	st := build.Steps[1]
	if st.If != "success()" || st.Shell != "bash" || st.WorkingDirectory != "./src" || st.Env["GOFLAGS"] != "-mod=readonly" {
		t.Fatalf("step = %+v", st)
	}

	reuse := w.Jobs["reuse"]
	if reuse.Uses == "" || reuse.With["target"] != "linux" || len(reuse.Steps) != 0 || len(reuse.RunsOn) != 0 {
		t.Fatalf("reusable job = %+v", reuse)
	}
}

func TestParseOnStringAndList(t *testing.T) {
	w, err := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatalf("scalar on: %v", err)
	}
	if !w.On.PushSet || w.On.PullRequestSet || w.On.DispatchSet {
		t.Fatalf("scalar triggers = %+v", w.On)
	}
	w, err = Parse([]byte("on: [push, pull_request]\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatalf("list on: %v", err)
	}
	if !w.Fires("push", RefBranch, "main", "") || !w.Fires("pull_request", RefBranch, "dev", "opened") || w.Fires("workflow_dispatch", RefBranch, "", "") {
		t.Fatalf("list triggers = %+v", w.On)
	}
	w, err = Parse([]byte("on: workflow_dispatch\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"))
	if err != nil || !w.On.DispatchSet {
		t.Fatalf("scalar dispatch = %+v, %v", w, err)
	}
	// A bare `on: schedule` has no cron to evaluate: it is now a parse error
	// (round-2 pass 2: the scheduler evaluates a documented cron subset, so an
	// entry it cannot read must never be a silently dead schedule).
	if _, err := Parse([]byte("on: schedule\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("bare `on: schedule` accepted with no cron entries")
	}
	// The mapping form is the legal spelling and records the entries.
	w, err = Parse([]byte("on:\n  schedule:\n    - cron: \"0 3 * * *\"\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"))
	if err != nil || !w.On.ScheduleSet || len(w.On.Schedule) != 1 {
		t.Fatalf("schedule mapping = %+v, %v", w.On, err)
	}
	if !w.Fires("schedule", RefBranch, "", "") {
		t.Fatal("a schedule workflow must answer Fires(schedule)")
	}
	// An unsupported cron is a parse error, not a dead trigger.
	if _, err := Parse([]byte("on:\n  schedule:\n    - cron: \"0 3 * * MON\"\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("named weekday accepted in cron")
	}
	// An unknown event is a typo, not a feature.
	if _, err := Parse([]byte("on: [push, teleport]\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("unknown list event accepted")
	}
	if _, err := Parse([]byte("on:\n  teleport: {}\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("unknown mapping event accepted")
	}
	if _, err := Parse([]byte("on:\n  - {push: {}}\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("mapping inside a list accepted")
	}
}

func TestParseInputValidation(t *testing.T) {
	base := "on:\n  workflow_dispatch:\n    inputs:\n      x:\n        %s\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"
	for name, frag := range map[string]string{
		"unknown type":        "type: password",
		"choice no options":   "type: choice",
		"unknown input key":   "type: string\n        bogus: 1",
		"non-scalar default":  "type: string\n        default: [a]",
		"unknown trigger key": "type: string\n  bogus: 1",
	} {
		if _, err := Parse([]byte(strings.Replace(base, "%s", frag, 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseConcurrencyShapes(t *testing.T) {
	base := "on: push\nconcurrency: %s\njobs:\n  a:\n    runs-on: l\n    %s\n    steps:\n      - run: x\n"
	cases := map[string]struct{ conc, job string }{
		"scalar":    {"ci-main", ""},
		"mapping":   {"{group: ci-main, cancel-in-progress: true}", ""},
		"job-level": {"", "concurrency: {group: job-1}"},
	}
	for name, tc := range cases {
		src := strings.Replace(strings.Replace(base, "%s", tc.conc, 1), "%s", tc.job, 1)
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Parse([]byte(strings.Replace(strings.Replace(base, "%s", "{group: g, bogus: 1}", 1), "%s", "", 1))); err == nil {
		t.Error("unknown concurrency key accepted")
	}
	if _, err := Parse([]byte(strings.Replace(strings.Replace(base, "%s", "[a]", 1), "%s", "", 1))); err == nil {
		t.Error("sequence concurrency accepted")
	}
}

func TestParseMatrixLimits(t *testing.T) {
	src := func(matrix string) []byte {
		return []byte("on: push\njobs:\n  a:\n    runs-on: l\n    strategy:\n      matrix:\n" + matrix + "\n    steps:\n      - run: x\n")
	}
	if _, err := Parse(src("        go: [1.26]")); err != nil {
		t.Fatalf("simple matrix: %v", err)
	}
	if _, err := Parse(src("        include:\n          - go: 1.27")); err != nil {
		t.Fatalf("matrix include rejected: %v", err)
	}
	if _, err := Parse(src("        include:\n          - go: [1.26, 1.27]")); err == nil {
		t.Fatal("array-valued include entry accepted")
	}
	if _, err := Parse(src("        go:\n          version: 1.27")); err == nil {
		t.Fatal("nested matrix value accepted")
	}
	if _, err := Parse([]byte("on: push\njobs:\n  a:\n    runs-on: l\n    strategy: [a]\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("sequence strategy accepted")
	}
}

func TestParsePermissionsAndContainerShapes(t *testing.T) {
	base := "on: push\npermissions: %s\njobs:\n  a:\n    runs-on: l\n    container: %s\n    steps:\n      - run: x\n"
	if w, err := Parse([]byte(strings.Replace(strings.Replace(base, "%s", "read-all", 1), "%s", "golang:1.27", 1))); err != nil {
		t.Fatalf("scalar shapes: %v", err)
	} else if w.Permissions["all"] != "read-all" || w.Jobs["a"].Container.Image != "golang:1.27" {
		t.Fatalf("scalar shapes = %+v / %+v", w.Permissions, w.Jobs["a"].Container)
	}
	if _, err := Parse([]byte(strings.Replace(strings.Replace(base, "%s", "{contents: [read]}", 1), "%s", "golang:1.27", 1))); err == nil {
		t.Fatal("non-scalar permission value accepted")
	}
	if _, err := Parse([]byte(strings.Replace(strings.Replace(base, "%s", "read-all", 1), "%s", "{image: x, bogus: 1}", 1))); err == nil {
		t.Fatal("unknown container key accepted")
	}
	if _, err := Parse([]byte(strings.Replace(strings.Replace(base, "%s", "read-all", 1), "%s", "[x]", 1))); err == nil {
		t.Fatal("sequence container accepted")
	}
}

func TestParseUnknownJobKeyStillRejected(t *testing.T) {
	for name, frag := range map[string]string{
		"job":   "    runson: x",
		"top":   "unknown: 1",
		"push":  "    push:\n      paths: [x]",
		"steps": "    steps:\n      - run: x\n        shellx: bash",
	} {
		src := "name: n\non:\n  push:\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"
		switch name {
		case "top":
			src = "name: n\non:\n  push:\n" + frag + "\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"
		case "push":
			src = "name: n\non:\n" + frag + "\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"
		case "job":
			src = "name: n\non:\n  push:\njobs:\n  a:\n    runs-on: l\n" + frag + "\n    steps:\n      - run: x\n"
		case "steps":
			src = "name: n\non:\n  push:\njobs:\n  a:\n    runs-on: l\n" + frag
		}
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: unknown key accepted", name)
		}
	}
}

func TestParseReusableJobNeedsNoSteps(t *testing.T) {
	if _, err := Parse([]byte("on: push\njobs:\n  a:\n    uses: o/r/.spindle/workflows/x.yml@main\n")); err != nil {
		t.Fatalf("reusable job rejected: %v", err)
	}
	// A plain job still needs runs-on and steps.
	if _, err := Parse([]byte("on: push\njobs:\n  a:\n    name: x\n")); err == nil {
		t.Fatal("plain job without runs-on/steps accepted")
	}
}
