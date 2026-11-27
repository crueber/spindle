package threads

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// Every reusable-workflow refusal is a NAMED reason, never a silent ignore.

func TestReusableCallerRefusalTable(t *testing.T) {
	nonNil := func(s string) *workflow.Environment { return &workflow.Environment{Name: s} }
	conc := &workflow.Concurrency{Group: "g"}
	strat := &workflow.Strategy{}
	container := &workflow.Container{Image: "alpine:3.20"}
	cases := []struct {
		name string
		job  workflow.Job
		want string
	}{
		{"steps", workflow.Job{Steps: []workflow.Step{{Run: "x"}}}, "`steps`"},
		{"runs-on", workflow.Job{RunsOn: workflow.Labels{"linux"}}, "`runs-on`"},
		{"container", workflow.Job{Container: container}, "`container`"},
		{"services", workflow.Job{Services: map[string]workflow.Service{"db": {Image: "x"}}}, "`services`"},
		{"env", workflow.Job{Env: map[string]string{"A": "b"}}, "`env`"},
		{"strategy", workflow.Job{Strategy: strat}, "`strategy`"},
		{"timeout", workflow.Job{TimeoutMinutes: workflow.Minutes{Value: 5, Present: true}}, "`timeout-minutes`"},
		{"concurrency", workflow.Job{Concurrency: conc}, "`concurrency`"},
		{"environment", workflow.Job{Environment: nonNil("prod")}, "`environment`"},
		{"outputs", workflow.Job{Outputs: map[string]string{"x": "y"}}, "`outputs`"},
	}
	for _, c := range cases {
		got := reusableCallerRefusal(c.job)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: refusal = %q, want %q", c.name, got, c.want)
		}
	}
	if got := reusableCallerRefusal(workflow.Job{Uses: "./x.yml"}); got != "" {
		t.Fatalf("a valid call was refused: %q", got)
	}
}

func TestLocalReusablePathTable(t *testing.T) {
	ok := map[string]string{
		"./.spindle/workflows/deploy.yml":         ".spindle/workflows/deploy.yml",
		"./.spindle/workflows/deploy.yaml":        ".spindle/workflows/deploy.yaml",
		"./.spindle/workflows/../workflows/x.yml": ".spindle/workflows/x.yml",
	}
	for in, want := range ok {
		ref, err := parseReusableUses(in)
		if err != nil || ref.Remote || ref.Path != want {
			t.Errorf("%q = %+v, %v; want local %q", in, ref, err, want)
		}
	}
	remote, err := parseReusableUses("acme/other/.spindle/workflows/deploy.yml@1111111111111111111111111111111111111111")
	if err != nil || !remote.Remote || remote.Owner != "acme" || remote.Repo != "other" ||
		remote.Path != ".spindle/workflows/deploy.yml" || remote.Commit != strings.Repeat("1", 40) {
		t.Fatalf("remote ref = %+v, %v", remote, err)
	}
	bad := map[string]string{
		"":                                       "empty",
		"docker://alpine:3.20":                   "not a workflow file",
		"https://example.com/x.yml":              "not a workflow file",
		".spindle/workflows/x.yml":               "is neither",
		"acme/other/.spindle/workflows/x.yml@v1": "pinned to a full 40-character commit SHA",
		"acme/other/.spindle/workflows/x.yml":    "is neither",
		"acme/other/.github/workflows/x.yml@1111111111111111111111111111111111111111": "is not in .spindle/workflows/",
		"./../escape.yml":            "inside this repository",
		"./.spindle/actions/x.yml":   "is not in .spindle/workflows/",
		"./.spindle/workflows/x.txt": "must name a .yml or .yaml file",
	}
	for in, want := range bad {
		if _, err := parseReusableUses(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}

func TestReusableExpandFailures(t *testing.T) {
	caller := `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/missing.yml
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", caller)
	runs := e.mustCreate(t)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "could not read") {
		t.Fatalf("missing callee = %+v", jobs)
	}

	// A callee that does not parse is named, and the run continues.
	broken := `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/broken.yml
`
	e2 := newTestEnv(t)
	e2.git.add(testSHA, ".spindle/workflows/ci.yml", broken)
	e2.git.add(testSHA, ".spindle/workflows/broken.yml", "name: Broken\non: {workflow_call: {}}\njobs: {}\n")
	jobs2, err := e2.svc.loadJobs(ctx(), "o", "r", e2.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs2) != 1 || !strings.Contains(jobs2[0].Reason, "no jobs") {
		t.Fatalf("empty callee = %+v", jobs2)
	}
}

func TestReusableDepthLimit(t *testing.T) {
	if MaxReusableDepth != 4 {
		t.Fatalf("MaxReusableDepth = %d", MaxReusableDepth)
	}
	// Five callee levels: the fifth is refused by name.
	e := newTestEnv(t)
	caller := `name: C
on: {push: {branches: [main]}}
jobs:
  l0:
    uses: ./.spindle/workflows/l1.yml
`
	e.git.add(testSHA, ".spindle/workflows/ci.yml", caller)
	for i := 1; i <= MaxReusableDepth; i++ {
		body := `name: L` + string(rune('0'+i)) + `
on: {workflow_call: {}}
jobs:
  l` + string(rune('0'+i)) + `:
    uses: ./.spindle/workflows/l` + string(rune('0'+i+1)) + `.yml
`
		e.git.add(testSHA, ".spindle/workflows/l"+string(rune('0'+i))+".yml", body)
	}
	e.git.add(testSHA, ".spindle/workflows/l5.yml", "name: L5\non: {workflow_call: {}}\njobs:\n  leaf:\n    runs-on: linux\n    steps:\n      - run: echo leaf\n")
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "nested deeper than the limit") {
		t.Fatalf("depth = %+v", jobs)
	}
}

func TestDowngradePermissions(t *testing.T) {
	cases := []struct {
		name     string
		cap      map[string]string
		declared map[string]string
		want     map[string]string
	}{
		{"nil both", nil, nil, nil},
		{"callee nil keeps cap contents", map[string]string{"contents": "read"}, nil, map[string]string{"contents": "read"}},
		{"narrow", map[string]string{"contents": "write", "issues": "write"}, map[string]string{"contents": "read"}, map[string]string{"contents": "read"}},
		{"widen refused", map[string]string{"contents": "read"}, map[string]string{"contents": "write"}, map[string]string{"contents": "read"}},
		{"unlisted scope is none", map[string]string{"contents": "read"}, map[string]string{"pull-requests": "write"}, map[string]string{"pull-requests": "none", "contents": "read"}},
		{"all expands over the baseline and is capped", map[string]string{"contents": "read"}, map[string]string{"all": "write"},
			map[string]string{"contents": "read", "issues": "none", "pull-requests": "none"}},
	}
	for _, c := range cases {
		got := downgradePermissions(c.cap, c.declared)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.name, k, got[k], v)
			}
		}
	}
	// A caller that grants contents: write lets a callee keep it.
	if got := downgradePermissions(map[string]string{"contents": "write"}, map[string]string{"contents": "write"}); got["contents"] != "write" {
		t.Fatalf("write = %q", got["contents"])
	}
}

func TestAppendUnique(t *testing.T) {
	list := appendUnique(nil, "a")
	list = appendUnique(list, "a")
	list = appendUnique(list, "b")
	if strings.Join(list, ",") != "a,b" {
		t.Fatalf("list = %v", list)
	}
}

func TestValidateAnnotationBounds(t *testing.T) {
	if _, err := ValidateAnnotations([]Annotation{{Level: "fatal"}}); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	many := make([]Annotation, MaxAnnotationsPerJob+1)
	for i := range many {
		many[i] = Annotation{Level: "notice"}
	}
	if _, err := ValidateAnnotations(many); err == nil {
		t.Fatal("the job annotation cap was not enforced")
	}
	got, err := ValidateAnnotations([]Annotation{{
		Level: "error", Message: strings.Repeat("m", MaxAnnotationMessage+1),
		Title: strings.Repeat("t", MaxAnnotationTitle+1), File: strings.Repeat("f", MaxAnnotationFile+1),
		Step: strings.Repeat("s", MaxAnnotationStep+1), Line: -3, Col: 2_000_000,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Message) != MaxAnnotationMessage || len(got[0].Title) != MaxAnnotationTitle ||
		len(got[0].File) != MaxAnnotationFile || len(got[0].Step) != MaxAnnotationStep {
		t.Fatalf("caps = %+v", got[0])
	}
	if got[0].Line != 0 || got[0].Col != 1_000_000 {
		t.Fatalf("number clamp = %+v", got[0])
	}
}
