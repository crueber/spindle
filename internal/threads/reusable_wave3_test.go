package threads

// PAR-075/PAR-076 Wave 3: the enforced reusable-workflow contract end to end —
// typed `with:` inputs, a closed secret set, a callee output consumed by a later
// job, cross-repository resolution at a pinned commit with an audit record, and
// the named refusals that keep an unimplemented piece from being inert.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

const wave3Callee = `name: Deploy
on:
  workflow_call:
    inputs:
      target:
        type: string
        required: true
      dry-run:
        type: boolean
        default: false
    secrets:
      DEPLOY_TOKEN:
        required: true
    outputs:
      url:
        description: deployed URL
        value: ${{ jobs.release.outputs.url }}
jobs:
  release:
    runs-on: linux
    outputs:
      url: ${{ steps.publish.outputs.url }}
    steps:
      - id: publish
        run: echo "url=https://example.test/$TARGET" >> "$GITHUB_OUTPUT"
        env:
          TARGET: ${{ inputs.target }}
          TOKEN: ${{ secrets.DEPLOY_TOKEN }}
`

const wave3Caller = `name: Caller
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
  deploy:
    needs: [build]
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: ${{ github.ref_name }}
    secrets:
      DEPLOY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}
  verify:
    needs: [deploy]
    runs-on: linux
    steps:
      - run: echo "deployed ${{ needs.deploy.outputs.url }}"
`

func TestReusableCallInputsSecretsOutputs(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", wave3Caller)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	runs := e.mustCreate(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	runID := runs[0].ID
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	byKey := map[string]*Job{}
	for _, j := range jobs {
		byKey[j.Key] = j
	}
	callee, ok := byKey["deploy / release"]
	if !ok || len(jobs) != 3 {
		t.Fatalf("jobs = %v", keysOf(byKey))
	}
	if callee.Spec == nil || callee.Spec.Inputs["target"] != "main" || callee.Spec.Inputs["dry-run"] != "false" {
		t.Fatalf("callee inputs = %+v", callee.Spec)
	}
	if callee.Spec.ReusableSecrets == nil || callee.Spec.ReusableSecrets.Inherit ||
		callee.Spec.ReusableSecrets.Names["DEPLOY_TOKEN"] != "REGISTRY_TOKEN" {
		t.Fatalf("callee secrets = %+v", callee.Spec.ReusableSecrets)
	}
	if callee.Spec.Call == nil || callee.Spec.Call.Caller != "deploy" || callee.Spec.Call.Outputs["url"] == "" {
		t.Fatalf("call spec = %+v", callee.Spec.Call)
	}
	// The caller job is a call, not a record: the callee's job is the record.
	if _, ok := byKey["deploy"]; ok {
		t.Fatal("a hidden caller job record was created")
	}
	verify := byKey["verify"]
	if verify == nil || strings.Join(verify.Needs, ",") != "deploy" {
		t.Fatalf("verify needs = %+v", verify)
	}

	// The chain: build, then release, then verify. The caller output is folded
	// from the callee's reported outputs and reaches the dependent job.
	build := byKey["build"].ID
	if got := e.claimOne(t, runID); got != build {
		t.Fatalf("first claim = %q, want build", got)
	}
	e.finish(t, runID, build, JobSuccess)
	if got := e.claimOne(t, runID); got != callee.ID {
		t.Fatalf("second claim = %q, want %q", got, callee.ID)
	}
	if _, err := e.svc.UpdateJob(ctx(), "o/r", runID, callee.ID, JobUpdate{
		State: JobSuccess, Outputs: map[string]string{"url": "https://example.test/prod"},
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrverify", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	res, ferr := e.svc.FetchJobFrom(ctx(), a, time.Millisecond, "http://127.0.0.1:8080")
	if ferr != nil {
		t.Fatalf("FetchJobFrom: %v", ferr)
	}
	if res == nil || res.JobID != verify.ID {
		t.Fatalf("claim = %+v, want %q", res, verify.ID)
	}
	if res.NeedsOutputs["deploy"]["url"] != "https://example.test/prod" {
		t.Fatalf("needs outputs = %+v", res.NeedsOutputs)
	}
}

func TestReusableCallRefusals(t *testing.T) {
	cases := []struct {
		name    string
		with    string
		secrets string
		callee  string
		want    string
	}{
		{
			name:   "unknown-input",
			with:   "      target: prod\n      typo: x\n",
			callee: wave3Callee,
			want:   "not a declared `on.workflow_call` input",
		},
		{
			name:   "missing-required",
			with:   "",
			callee: wave3Callee,
			want:   "required input \"target\" was not supplied",
		},
		{
			name:   "wrong-type",
			with:   "      target: prod\n      dry-run: yes\n",
			callee: wave3Callee,
			want:   "is a boolean",
		},
		{
			name:    "undeclared-secret",
			with:    "      target: prod\n",
			secrets: "    secrets:\n      OTHER: ${{ secrets.OTHER }}\n",
			callee:  wave3Callee,
			want:    "is not declared by the callee's `on.workflow_call.secrets`",
		},
		{
			name:    "missing-required-secret",
			with:    "      target: prod\n",
			secrets: "    secrets: {}\n",
			callee:  wave3Callee,
			want:    "requires the secret `DEPLOY_TOKEN`",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caller := `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
` + c.secrets
			if c.with != "" {
				caller += "    with:\n" + c.with
			}
			e := newTestEnv(t)
			e.git.add(testSHA, ".spindle/workflows/ci.yml", caller)
			e.git.add(testSHA, ".spindle/workflows/deploy.yml", c.callee)
			jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
			if err != nil {
				t.Fatalf("loadJobs: %v", err)
			}
			if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, c.want) {
				t.Fatalf("refusal = %+v, want %q", jobs, c.want)
			}
		})
	}
}

const upstreamSHA = "2222222222222222222222222222222222222222"

func TestReusableCrossRepoResolvesAndAudits(t *testing.T) {
	e := newTestEnv(t)
	e.git.addIn("upstream/proj", upstreamSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	var audits []string
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits = append(audits, actor+" "+action+" "+target+" "+detail)
		return nil
	}
	caller := `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: upstream/proj/.spindle/workflows/deploy.yml@` + upstreamSHA + `
    with:
      target: prod
    secrets:
      DEPLOY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}
`
	e.git.add(testSHA, ".spindle/workflows/ci.yml", caller)
	runs := e.mustCreate(t)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Key != "deploy / release" || jobs[0].State == JobFailure {
		t.Fatalf("remote callee = %+v", jobs)
	}
	if jobs[0].Spec == nil || jobs[0].Spec.Inputs["target"] != "prod" {
		t.Fatalf("remote inputs = %+v", jobs[0].Spec)
	}
	if len(audits) != 1 || !strings.Contains(audits[0], AuditReusableCrossRepo) ||
		!strings.Contains(audits[0], "upstream/proj@222222222222") ||
		!strings.Contains(audits[0], "secrets=DEPLOY_TOKEN") {
		t.Fatalf("audit = %v", audits)
	}
	if strings.Contains(audits[0], "repo-token-value") {
		t.Fatal("an audit record carries a secret value")
	}
}

func TestReusableCrossRepoRefusals(t *testing.T) {
	pinned := func(uses string, extra string) string {
		return `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ` + uses + `
` + extra
	}
	cases := []struct {
		name   string
		caller string
		public bool
		add    bool
		want   string
	}{
		{
			name:   "mutable-ref",
			caller: pinned("upstream/proj/.spindle/workflows/deploy.yml@main", ""),
			want:   "pinned to a full 40-character commit SHA",
		},
		{
			name:   "not-public",
			caller: pinned("upstream/proj/.spindle/workflows/deploy.yml@"+upstreamSHA, ""),
			public: false,
			add:    true,
			want:   "not publicly readable",
		},
		{
			name:   "missing-file",
			caller: pinned("upstream/proj/.spindle/workflows/nope.yml@"+upstreamSHA, ""),
			public: true,
			want:   "could not read",
		},
		{
			name:   "required-secret-without-map",
			caller: pinned("upstream/proj/.spindle/workflows/deploy.yml@"+upstreamSHA, "    with: {target: prod}\n"),
			public: true,
			add:    true,
			want:   "D-6",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.roles.public = c.public
			if c.add {
				e.git.addIn("upstream/proj", upstreamSHA, ".spindle/workflows/deploy.yml", wave3Callee)
			}
			e.git.add(testSHA, ".spindle/workflows/ci.yml", c.caller)
			jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
			if err != nil {
				t.Fatalf("loadJobs: %v", err)
			}
			if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, c.want) {
				t.Fatalf("refusal = %+v, want %q", jobs, c.want)
			}
		})
	}
}

func TestReusableInheritAcrossRepositoriesIsAParseError(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: upstream/proj/.spindle/workflows/deploy.yml@`+upstreamSHA+`
    secrets: inherit
`)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a file that fails to parse created runs: %+v", runs)
	}
}

func TestReusableUnknownOutputRefusedByName(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with: {target: prod}
  verify:
    needs: [deploy]
    runs-on: linux
    steps:
      - run: echo "${{ needs.deploy.outputs.nope }}"
`)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	byKey := map[string]*Job{}
	for _, j := range jobs {
		byKey[j.Key] = j
	}
	verify := byKey["verify"]
	if verify == nil || verify.State != JobFailure || !strings.Contains(verify.Reason, "names no output of reusable workflow") {
		t.Fatalf("verify = %+v", verify)
	}
	if !strings.Contains(verify.Reason, "nope") || !strings.Contains(verify.Reason, "url") {
		t.Fatalf("reason does not name the undeclared output and the declared set: %q", verify.Reason)
	}
}

func TestReusableCallCycleRefused(t *testing.T) {
	e := newTestEnv(t)
	loop := func(target string) string {
		return `name: L
on: {workflow_call: {}}
jobs:
  next:
    uses: ./.spindle/workflows/` + target + `.yml
`
	}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  start:
    uses: ./.spindle/workflows/a.yml
`)
	e.git.add(testSHA, ".spindle/workflows/a.yml", loop("b"))
	e.git.add(testSHA, ".spindle/workflows/b.yml", loop("a"))
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "cycle") {
		t.Fatalf("cycle = %+v", jobs)
	}
}

func TestReusableSecretsFilteredAtClaim(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := e.svc.SetSecret(ctx(), "o/r", "REGISTRY_TOKEN", "registry-value", admin); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if err := e.svc.SetSecret(ctx(), "o/r", "UNRELATED", "unrelated-value", admin); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with: {target: prod}
    secrets:
      DEPLOY_TOKEN: ${{ secrets.REGISTRY_TOKEN }}
`)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	runs := e.mustCreate(t)
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrsecret", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	res, err := e.svc.FetchJobFrom(ctx(), a, time.Millisecond, "http://127.0.0.1:8080")
	if err != nil || res == nil {
		t.Fatalf("FetchJobFrom: %+v %v", res, err)
	}
	if res.Secrets["DEPLOY_TOKEN"] != "registry-value" {
		t.Fatalf("callee secrets = %+v", res.Secrets)
	}
	if _, ok := res.Secrets["UNRELATED"]; ok {
		t.Fatalf("a secret the call did not pass reached the callee: %+v", res.Secrets)
	}
	if _, ok := res.Secrets["REGISTRY_TOKEN"]; ok {
		t.Fatalf("the source secret name leaked as its own variable: %+v", res.Secrets)
	}
	_ = runs
}

func TestReusableSecretValuesUnit(t *testing.T) {
	repo := map[string]string{"A": "1", "B": "2"}
	plain := reusableSecretValues(&Spec{}, repo)
	if len(plain) != 2 {
		t.Fatalf("ordinary job = %v", plain)
	}
	inherit := reusableSecretValues(&Spec{ReusableSecrets: &reusableSecrets{Inherit: true}}, repo)
	if len(inherit) != 2 {
		t.Fatalf("inherit = %v", inherit)
	}
	explicit := reusableSecretValues(&Spec{ReusableSecrets: &reusableSecrets{Names: map[string]string{"X": "a"}}}, repo)
	if len(explicit) != 1 || explicit["X"] != "1" {
		t.Fatalf("explicit = %v", explicit)
	}
	none := reusableSecretValues(&Spec{ReusableSecrets: &reusableSecrets{}}, repo)
	if len(none) != 0 {
		t.Fatalf("empty set = %v", none)
	}
}

func TestReusableCallSpecSurvivesJSON(t *testing.T) {
	// The call context is persisted on the job record; a round trip must keep
	// the caller, path and output templates intact (the runner and the needs
	// fold read it back).
	spec := &Spec{
		Call:            &callSpec{Caller: "deploy", Path: ".spindle/workflows/x.yml", Outputs: map[string]string{"url": "${{ jobs.a.outputs.url }}"}},
		ReusableSecrets: &reusableSecrets{Names: map[string]string{"T": "SRC"}},
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Spec
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Call == nil || back.Call.Caller != "deploy" || back.Call.Outputs["url"] == "" ||
		back.ReusableSecrets == nil || back.ReusableSecrets.Names["T"] != "SRC" {
		t.Fatalf("round trip = %+v", back)
	}
}

// TestWorkflowCallWithTemplatesResolveAtCreation pins the two contexts a
// `with:` value may read and that substitution happens once (a value is data,
// never a template).
func TestWorkflowCallWithTemplatesResolveAtCreation(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: ${{ github.sha }}
      dry-run: ${{ github.event_name == 'push' && 'true' || 'false' }}
`)
	// The second value is an expression, which a with: value does not accept:
	// the file must not create a run.
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("an expression with: value was accepted: %+v", runs)
	}
	// A reference-only value expands.
	e2 := newTestEnv(t)
	e2.git.add(testSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	e2.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: ${{ github.sha }}
`)
	jobs, jerr := e2.svc.loadJobs(ctx(), "o", "r", e2.mustCreate(t)[0].ID)
	if jerr != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, jerr)
	}
	if jobs[0].Spec.Inputs["target"] != testSHA {
		t.Fatalf("target = %q", jobs[0].Spec.Inputs["target"])
	}
}

// TestWorkflowCallWithInputsContext pins that a caller may hand its own
// workflow_dispatch input to a callee.
func TestWorkflowCallWithInputsContext(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on:
  workflow_dispatch:
    inputs:
      target:
        default: staging
jobs:
  deploy:
    uses: ./.spindle/workflows/deploy.yml
    with:
      target: ${{ inputs.target }}
`)
	runs, derr := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "ci.yml", nil, e.writer())
	if derr != nil {
		t.Fatalf("DispatchRun: %v", derr)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if jobs[0].Spec.Inputs["target"] != "staging" {
		t.Fatalf("target = %q", jobs[0].Spec.Inputs["target"])
	}
}

var _ = workflow.CallInputTypes

func TestReusableHelpers(t *testing.T) {
	upper := strings.Repeat("A", 40)
	ref, err := parseReusableUses("Owner/Repo/.spindle/workflows/x.yaml@" + upper)
	if err != nil || !ref.Remote || ref.Commit != strings.Repeat("a", 40) || ref.Owner != "Owner" {
		t.Fatalf("uppercase SHA ref = %+v, %v", ref, err)
	}
	for _, uses := range []string{
		"owner/repo/.spindle/workflows/x.yml@" + strings.Repeat("g", 40),
		"owner/repo/.spindle/workflows/x.yml@" + strings.Repeat("a", 39),
		"owner//.spindle/workflows/x.yml@" + upper,
		"owner/repo/nested/.spindle/workflows/x.yml@" + upper,
		"owner/repo/.spindle/workflows/../../x.yml@" + upper,
		"owner/repo/x.txt@" + upper,
		"owner/.spindle/workflows/x.yml@" + upper,
		"/repo/.spindle/workflows/x.yml@" + upper,
		"owner/repo/.spindle/workflows/x.yml@" + strings.Repeat("a", 41),
	} {
		if _, err := parseReusableUses(uses); err == nil {
			t.Errorf("%q was accepted", uses)
		}
	}
	if got := lookupSecretValue(map[string]string{"Token": "v"}, "TOKEN"); got != "v" {
		t.Fatalf("case-insensitive lookup = %q", got)
	}
	if got := secretNamesForAudit(nil); got != "none" {
		t.Fatalf("nil audit names = %q", got)
	}
	if got := secretNamesForAudit(&reusableSecrets{Inherit: true}); got != "inherit" {
		t.Fatalf("inherit audit names = %q", got)
	}
	if got := secretNamesForAudit(&reusableSecrets{Names: map[string]string{"B": "s", "A": "s"}}); got != "A,B" {
		t.Fatalf("map audit names = %q", got)
	}
	if shortSHA("abc") != "abc" || len(shortSHA(strings.Repeat("a", 40))) != 12 {
		t.Fatal("shortSHA")
	}
	if validRepoSegment("bad/segment") || validRepoSegment("..") || !validRepoSegment("ok.repo-1") {
		t.Fatal("validRepoSegment")
	}
	if keys := sortedKeys(map[string]string{"b": "", "a": ""}); strings.Join(keys, ",") != "a,b" {
		t.Fatalf("sortedKeys = %v", keys)
	}
}

func TestReusableRemoteWithoutSecretsGetsNone(t *testing.T) {
	callee := `name: Ping
on:
  workflow_call:
    inputs:
      target: {type: string, default: prod}
jobs:
  ping:
    runs-on: linux
    steps:
      - run: echo ping
`
	e := newTestEnv(t)
	e.git.addIn("upstream/proj", upstreamSHA, ".spindle/workflows/ping.yml", callee)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  ping:
    uses: upstream/proj/.spindle/workflows/ping.yml@`+upstreamSHA+`
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	spec := jobs[0].Spec
	if spec.ReusableSecrets == nil || spec.ReusableSecrets.Inherit || len(spec.ReusableSecrets.Names) != 0 {
		t.Fatalf("remote secrets = %+v", spec.ReusableSecrets)
	}
	if spec.Inputs["target"] != "prod" {
		t.Fatalf("default input = %+v", spec.Inputs)
	}
	if got := reusableSecretValues(spec, map[string]string{"ANY": "value"}); len(got) != 0 {
		t.Fatalf("a remote call without a secrets block received %v", got)
	}
}

func TestReusableSameRepoInherit(t *testing.T) {
	callee := `name: Ping
on:
  workflow_call:
    secrets:
      OPTIONAL: {}
jobs:
  ping:
    runs-on: linux
    steps:
      - run: echo "$SECRET"
        env:
          SECRET: ${{ secrets.OPTIONAL }}
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ping.yml", callee)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  ping:
    uses: ./.spindle/workflows/ping.yml
    secrets: inherit
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if jobs[0].Spec.ReusableSecrets == nil || !jobs[0].Spec.ReusableSecrets.Inherit {
		t.Fatalf("inherit = %+v", jobs[0].Spec.ReusableSecrets)
	}
}

func TestReusableInternalEdges(t *testing.T) {
	// A value that is not a template but names an unknown context still fails
	// through the substitution path (defensive: the parser runs first).
	if _, err := resolveCallInputs(map[string]string{"x": "${{ nope.x }}"}, nil, nil); err == nil {
		t.Fatal("an unknown context must fail")
	}
	if got := lookupSecretValue(map[string]string{"A": "1"}, "MISSING"); got != "" {
		t.Fatalf("missing secret = %q", got)
	}
	// A need key that is not a call keeps the plain union (no call spec).
	byID := map[string]*Job{
		"j1": {ID: "j1", Key: "build", Outputs: map[string]string{"x": "1"}},
		"j2": {ID: "j2", Key: "test"},
	}
	if _, ok := callOutputs("build", []string{"j1", "j2"}, byID); ok {
		t.Fatal("an ordinary dependency was treated as a call")
	}
	// A job with no spec cannot be a callee record either.
	if _, ok := callOutputs("deploy", []string{"nil"}, map[string]*Job{"nil": {ID: "nil"}}); ok {
		t.Fatal("a spec-less job was treated as a call")
	}
	// A callee reference with no commit is refused before any read.
	for _, uses := range []string{"owner/repo/.spindle/workflows/x.yml@", "@" + strings.Repeat("a", 40)} {
		if _, err := parseReusableUses(uses); err == nil {
			t.Fatalf("%q was accepted", uses)
		}
	}
	if _, err := localReusablePath("x.yml"); err == nil {
		t.Fatal("a non-relative local path was accepted")
	}
	// A service without a git reader cannot expand a call (infrastructure
	// error, never a silent job).
	s := New(memStore(), nil)
	s.Git = nil
	if _, _, err := s.expandReusable(ctx(), "o/r", testSHA, &WorkflowFile{Workflow: &workflow.Workflow{}},
		"job", "job", workflow.Job{Uses: "./.spindle/workflows/x.yml"}, nil, planContext{}, 1, nil, "job", nil, nil); err == nil {
		t.Fatal("a missing git reader must fail")
	}
}

// memStore builds a memory store for the bare Service above.
func memStore() *store.Memory { return store.NewMemory() }

func TestReusableNestedSecretsPropagate(t *testing.T) {
	outer := `name: Outer
on: {push: {branches: [main]}}
jobs:
  call:
    uses: ./.spindle/workflows/middle.yml
    secrets:
      MID: ${{ secrets.SRC }}
`
	middle := `name: Middle
on:
  workflow_call:
    secrets:
      MID: {required: true}
jobs:
  inner:
    uses: ./.spindle/workflows/inner.yml
    secrets:
      INNER: ${{ secrets.MID }}
`
	inner := `name: Inner
on:
  workflow_call:
    secrets:
      INNER: {required: true}
jobs:
  run:
    runs-on: linux
    steps:
      - run: echo "$TOKEN"
        env:
          TOKEN: ${{ secrets.INNER }}
`
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", outer)
	e.git.add(testSHA, ".spindle/workflows/middle.yml", middle)
	e.git.add(testSHA, ".spindle/workflows/inner.yml", inner)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil || len(jobs) != 1 || jobs[0].State == JobFailure {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	// The nested callee's INNER resolves through MID to the run repository's SRC.
	if got := jobs[0].Spec.ReusableSecrets.Names["INNER"]; got != "SRC" {
		t.Fatalf("transitive secret = %q, want SRC (spec %+v)", got, jobs[0].Spec.ReusableSecrets)
	}
}

func TestReusableRemoteNestedCannotWidenSecrets(t *testing.T) {
	remote := `name: Remote
on:
  workflow_call:
jobs:
  local:
    uses: ./.spindle/workflows/inner.yml
`
	inner := `name: Inner
on:
  workflow_call:
    secrets:
      INNER: {required: true}
jobs:
  run:
    runs-on: linux
    steps:
      - run: echo "$TOKEN"
        env:
          TOKEN: ${{ secrets.INNER }}
`
	e := newTestEnv(t)
	e.git.addIn("upstream/proj", upstreamSHA, ".spindle/workflows/deploy.yml", remote)
	e.git.addIn("upstream/proj", upstreamSHA, ".spindle/workflows/inner.yml", inner)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  deploy:
    uses: upstream/proj/.spindle/workflows/deploy.yml@`+upstreamSHA+`
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "D-6") {
		t.Fatalf("a nested local call inside a remote callee inherited the run's secrets: %+v", jobs)
	}
}

func TestReusableCallerIfConjunction(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  build:
    runs-on: linux
    steps: [{run: echo build}]
  deploy:
    needs: [build]
    if: github.ref_name == 'main'
    uses: ./.spindle/workflows/deploy.yml
    with: {target: prod}
    secrets: {DEPLOY_TOKEN: "${{ secrets.T }}"}
  other:
    if: github.ref_name == 'main'
    uses: ./.spindle/workflows/unnamed.yml
    with: {target: prod}
`)
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", wave3Callee)
	e.git.add(testSHA, ".spindle/workflows/unnamed.yml", `on: {workflow_call: {inputs: {target: {type: string, required: true}}}}
jobs:
  a:
    runs-on: linux
    if: github.ref_name == 'main'
    steps: [{run: echo a}]
  b:
    runs-on: linux
    steps: [{run: echo b}]
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	byKey := map[string]*Job{}
	for _, j := range jobs {
		byKey[j.Key] = j
	}
	if got := byKey["deploy / release"].Spec.If; got != "github.ref_name == 'main'" {
		t.Fatalf("caller if not inherited by a callee job without its own: %q", got)
	}
	if got := byKey["other / a"].Spec.If; !strings.Contains(got, ") && (") {
		t.Fatalf("the caller's if must be conjoined with a callee job's own: %q", got)
	}
	// An unnamed callee takes its file name.
	if name := byKey["other / a"].Name; !strings.HasPrefix(name, "other / a") {
		t.Fatalf("unnamed callee job = %q", name)
	}
}

func TestReusableNestedSecretMustBePassed(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  call:
    uses: ./.spindle/workflows/middle.yml
    secrets: {MID: "${{ secrets.SRC }}"}
`)
	e.git.add(testSHA, ".spindle/workflows/middle.yml", `name: Middle
on: {workflow_call: {secrets: {MID: {required: true}}}}
jobs:
  inner:
    uses: ./.spindle/workflows/inner.yml
    secrets: {INNER: "${{ secrets.OTHER }}"}
`)
	e.git.add(testSHA, ".spindle/workflows/inner.yml", `name: Inner
on: {workflow_call: {secrets: {INNER: {required: true}}}}
jobs:
  run:
    runs-on: linux
    steps: [{run: echo x}]
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "did not receive") {
		t.Fatalf("a nested call named a secret its caller never received: %+v", jobs)
	}
}

func TestReusableNestedInheritPropagates(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: C
on: {push: {branches: [main]}}
jobs:
  call:
    uses: ./.spindle/workflows/middle.yml
    secrets: {MID: "${{ secrets.SRC }}"}
`)
	e.git.add(testSHA, ".spindle/workflows/middle.yml", `name: Middle
on: {workflow_call: {secrets: {MID: {required: true}}}}
jobs:
  inner:
    uses: ./.spindle/workflows/inner.yml
    secrets: inherit
`)
	e.git.add(testSHA, ".spindle/workflows/inner.yml", `name: Inner
on: {workflow_call: {secrets: {MID: {required: true}}}}
jobs:
  run:
    runs-on: linux
    steps:
      - run: echo "$T"
        env: {T: "${{ secrets.MID }}"}
`)
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", e.mustCreate(t)[0].ID)
	if err != nil || len(jobs) != 1 || jobs[0].State == JobFailure {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if got := jobs[0].Spec.ReusableSecrets.Names["MID"]; got != "SRC" {
		t.Fatalf("inherit did not propagate the calling workflow's set: %+v", jobs[0].Spec.ReusableSecrets)
	}
}
