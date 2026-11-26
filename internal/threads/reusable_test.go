package threads

import (
	"strings"
	"testing"
)

// PAR-075: repository-local reusable workflows. The callee's jobs become jobs of
// the same run (each with its own record, check and log), `with:` becomes
// `inputs.*`, the caller's needs gate the whole callee, and permissions are
// downgrade-only. Cross-repository `uses:` is refused by name (D-6).

const reusableCallerYAML = `name: Caller
on:
  push:
    branches: [main]
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    permissions:
      contents: read
`

const reusableCalleeYAML = `name: Deploy
jobs:
  plan:
    runs-on: linux
    permissions:
      contents: write
      issues: write
    steps:
      - run: echo planning
  apply:
    runs-on: linux
    needs: [plan]
    steps:
      - run: echo applying
`

func reusableEnv(t *testing.T, caller, callee string) (*testEnv, string) {
	t.Helper()
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", caller)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", callee)
	runs := e.mustCreate(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 (the callee has no trigger, so it does not fire)", len(runs))
	}
	return e, runs[0].ID
}

func TestLocalReusableWorkflowExpandsJobs(t *testing.T) {
	e, runID := reusableEnv(t, reusableCallerYAML, reusableCalleeYAML)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	byKey := map[string]*Job{}
	for _, j := range jobs {
		byKey[j.Key] = j
	}
	plan, ok := byKey["deploy / plan"]
	apply, ok2 := byKey["deploy / apply"]
	if !ok || !ok2 || len(jobs) != 2 {
		t.Fatalf("expanded jobs = %+v", keysOf(byKey))
	}
	if plan.Name != "deploy / plan" || plan.Spec == nil {
		t.Fatalf("plan job = %+v spec=%+v", plan, plan.Spec)
	}
	if got := strings.Join(apply.Needs, ","); got != "deploy / plan" {
		t.Fatalf("apply needs = %q", got)
	}
	// Downgrade-only: the callee asked for contents/issues write under a caller
	// that grants contents read.
	perms := e.svc.EffectivePermissions(plan.Spec, true)
	if perms["contents"] != PermRead || perms["issues"] != PermNone {
		t.Fatalf("callee permissions = %v", perms)
	}
	// The chain runs: only plan is claimable first.
	if got := e.claimOne(t, runID); got != plan.ID {
		t.Fatalf("first claim = %q, want %q", got, plan.ID)
	}
	if got := e.claimOne(t, runID); got != "" {
		t.Fatalf("apply claimed before plan finished: %q", got)
	}
	e.finish(t, runID, plan.ID, JobSuccess)
	if got := e.claimOne(t, runID); got != apply.ID {
		t.Fatalf("apply claim = %q, want %q", got, apply.ID)
	}
}

func TestLocalReusableWorkflowNested(t *testing.T) {
	outer := `name: Outer
on:
  push:
    branches: [main]
jobs:
  release:
    uses: ./.spindle/workflows/middle.yml
`
	middle := `name: Middle
jobs:
  inner:
    uses: ./.spindle/workflows/inner.yml
`
	inner := `name: Inner
jobs:
  ship:
    runs-on: linux
    steps:
      - run: echo ship
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", outer)
	e.git.add(testSHA, ".spindle/workflows/middle.yml", middle)
	e.git.add(testSHA, ".spindle/workflows/inner.yml", inner)
	runs := e.mustCreate(t)
	runID := runs[0].ID
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Key != "release / inner / ship" {
		byKey := map[string]*Job{}
		for _, j := range jobs {
			byKey[j.Key] = j
		}
		t.Fatalf("nested jobs = %+v", keysOf(byKey))
	}
}

func TestReusableCallerRefusals(t *testing.T) {
	cases := []struct {
		name    string
		caller  string
		callee  string
		wantSub string
	}{
		{
			name: "cross-repository",
			caller: `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: acme/other/.spindle/workflows/deploy.yml@v1
`,
			wantSub: "D-6",
		},
		{
			name: "missing-callee",
			caller: `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./..spindle/workflows/nope.yml
`,
			wantSub: "is not",
		},
		{
			name: "with-requires-workflow-call",
			caller: `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: production
`,
			callee:  reusableCalleeYAML,
			wantSub: "on.workflow_call",
		},
		{
			name: "caller-with-steps",
			caller: `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    runs-on: linux
    steps:
      - run: echo nope
`,
			callee:  reusableCalleeYAML,
			wantSub: "may not declare `steps`",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.git.add(testSHA, ".spindle/workflows/ci.yml", c.caller)
			if c.callee != "" {
				e.git.add(testSHA, ".spindle/workflows/deploy.yml", c.callee)
			}
			runs := e.mustCreate(t)
			if len(runs) != 1 {
				t.Fatalf("runs = %d", len(runs))
			}
			jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
			if err != nil {
				t.Fatalf("loadJobs: %v", err)
			}
			if len(jobs) != 1 || jobs[0].State != JobFailure {
				t.Fatalf("refusal jobs = %+v", jobs)
			}
			if !strings.Contains(jobs[0].Reason, c.wantSub) {
				t.Fatalf("reason = %q, want %q", jobs[0].Reason, c.wantSub)
			}
			if got := e.claimOne(t, runs[0].ID); got != "" {
				t.Fatalf("a refused caller was enqueued: %q", got)
			}
		})
	}
}

// keysOf is a small readable projection for failure messages.
func keysOf(m map[string]*Job) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
