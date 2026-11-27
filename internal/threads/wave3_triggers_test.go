// Wave 3 trigger family (PAR-016/PAR-018/PAR-019/PAR-021/PAR-024/PAR-118):
// the GitHub-only triggers, their payload projections, the two per-repository
// security switches and the merge-ref semantics. The parser half lives in
// internal/threads/workflow/wave3_triggers_test.go.
package threads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

const followWorkflowYAML = `name: Follow
on:
  workflow_run:
    workflows: [CI]
    types: [completed]
jobs:
  consume:
    runs-on: linux
    steps:
      - run: echo follow
`

const prTargetWorkflowYAML = `name: PR Target
on:
  pull_request_target:
jobs:
  base:
    runs-on: linux
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.pull_request.head.sha }}
      - run: echo base
`

const dispatchWorkflowYAML = `name: Dispatch
on:
  repository_dispatch:
    types: [deploy]
jobs:
  deploy:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.client_payload.environment }}"
`

func (e *testEnv) admin() auth.Principal { return auth.Principal{Name: "admin", Admin: true} }

func (e *testEnv) setSettings(t *testing.T, in SettingsInput) *SettingsView {
	t.Helper()
	v, err := e.svc.UpdateSettings(ctx(), "o/r", in, e.admin())
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	return v
}

func (e *testEnv) useDefaultBranch() {
	e.git.refs["HEAD"] = testSHA
}

// --- repository_dispatch ----------------------------------------------------

func TestRepositoryDispatchTriggerAndPayload(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/dispatch.yml", dispatchWorkflowYAML)

	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventRepositoryDispatch, CreateOpts{
		Action: "deploy", Branch: "main", Ref: "refs/heads/main", RefName: "main", Actor: "writer",
		EventPayload: RepositoryDispatchEventPayload("o/r", "deploy", map[string]any{"environment": "prod"}, "writer"),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Event != EventRepositoryDispatch || run.Commit != testSHA {
		t.Fatalf("run = %+v", run)
	}
	fields := EventFieldsOf(run.EventPayload)
	if fields["event.client_payload.environment"] != "prod" {
		t.Fatalf("client_payload projection = %q", fields["event.client_payload.environment"])
	}

	// A type the workflow does not name starts nothing (the `types` filter).
	if runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventRepositoryDispatch, CreateOpts{
		Action: "other", Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	}); err != nil || len(runs) != 0 {
		t.Fatalf("non-matching type: runs=%d err=%v", len(runs), err)
	}
	// An ill-formed event_type is refused before any run exists.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventRepositoryDispatch, CreateOpts{
		Action: "bad type!", Actor: "writer",
	}); err == nil {
		t.Fatal("an ill-formed event_type must be refused")
	}
}

func TestRepositoryDispatchAPI(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/dispatch.yml", dispatchWorkflowYAML)
	h := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "writer"}, nil
	}}
	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/o/r/api/threads/dispatches", strings.NewReader(body))
		if !h.Handle(rec, req) {
			t.Fatal("route not claimed")
		}
		return rec
	}
	rec := do(`{"event_type":"deploy","client_payload":{"environment":"prod"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Runs []*Run `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Runs) != 1 {
		t.Fatalf("answer = %s (err=%v)", rec.Body.String(), err)
	}
	if rec := do(`{"event_type":"bad type!","client_payload":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type status = %d", rec.Code)
	}
	if rec := do(`{"event_type":"deploy","client_payload":{"k":"` + strings.Repeat("x", maxEventPayloadBytes) + `"}}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized payload status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(`{"event_type":"deploy","unknown":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", rec.Code)
	}
	// Anonymous is refused (the write role is the gate).
	anon := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), nil
	}}
	rec = httptest.NewRecorder()
	anon.Handle(rec, httptest.NewRequest(http.MethodPost, "/o/r/api/threads/dispatches",
		strings.NewReader(`{"event_type":"deploy"}`)))
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", rec.Code)
	}
}

func TestValidateClientPayloadBounds(t *testing.T) {
	if _, err := ValidateClientPayload(map[string]any{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateClientPayload(map[string]any{"bad key": 1}); err == nil {
		t.Fatal("a key outside the charset must be refused")
	}
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"e": 1}}}}}
	if _, err := ValidateClientPayload(deep); err == nil {
		t.Fatal("over-deep nesting must be refused")
	}
	many := map[string]any{}
	for i := 0; i < maxClientPayloadKeys+1; i++ {
		many[string(rune('a'+i))] = 1
	}
	if _, err := ValidateClientPayload(many); err == nil {
		t.Fatal("too many top-level keys must be refused")
	}
}

// --- workflow_run -----------------------------------------------------------

func TestWorkflowRunTriggerAndChainCap(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.branch = "main"
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	e.git.add(testSHA, ".spindle/workflows/follow.yml", followWorkflowYAML)
	e.git.add(testSHA, ".spindle/workflows/dispatch.yml", dispatchWorkflowYAML)

	parent := e.mustCreate(t)[0]
	e.svc.emitWorkflowRun(ctx(), parent, "completed")
	runs, err := e.svc.ListRunsFiltered(ctx(), "o/r", e.public(), "", 50, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var follow *Run
	for _, r := range runs.Runs {
		if r.Event == EventWorkflowRun {
			follow = r
		}
	}
	if follow == nil {
		t.Fatal("no workflow_run follow-up was created")
	}
	if follow.ParentRunID != parent.ID || !follow.ParentTrusted || follow.ChainDepth != 1 {
		t.Fatalf("follow header = %+v", follow)
	}
	if follow.Branch != "main" || follow.Commit != testSHA {
		t.Fatalf("follow reports %s@%s", follow.Branch, follow.Commit)
	}
	if follow.State == RunPendingApproval {
		t.Fatal("a trusted parent must not hold the follow-up")
	}

	// The upstream workflow NAME filter: a run of a different workflow does not
	// match.
	other := *parent
	other.Workflow = "Other"
	before := e.countRuns(t)
	e.svc.emitWorkflowRun(ctx(), &other, "completed")
	if got := e.countRuns(t); got != before {
		t.Fatalf("a non-matching upstream workflow created %d runs", got-before)
	}

	// The chain cap: a parent at the cap starts nothing.
	capped := *parent
	capped.ChainDepth = maxWorkflowRunChain
	before = e.countRuns(t)
	e.svc.emitWorkflowRun(ctx(), &capped, "completed")
	if got := e.countRuns(t); got != before {
		t.Fatalf("chain cap emitted %d runs", got-before)
	}
}

// countRuns counts every run header in the repository.
func (e *testEnv) countRuns(t *testing.T) int {
	t.Helper()
	runs, err := e.svc.listRunsForCap(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	return len(runs)
}

func TestWorkflowRunUntrustedParentRequiresApproval(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	e.git.add(testSHA, ".spindle/workflows/follow.yml", followWorkflowYAML)

	// A fork pull_request run is untrusted (base-branch file).
	prRuns, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/3/head", BaseBranch: "main",
		Fork: true, Actor: "outsider",
	})
	if err != nil || len(prRuns) != 1 {
		t.Fatalf("fork run: %d, %v", len(prRuns), err)
	}
	parent := prRuns[0]
	e.svc.emitWorkflowRun(ctx(), parent, "completed")
	runs, err := e.svc.listRunsForCap(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	var follow *Run
	for _, r := range runs {
		if r.Event == EventWorkflowRun {
			follow = r
		}
	}
	if follow == nil {
		t.Fatal("no follow-up")
	}
	if follow.State != RunPendingApproval || follow.ApprovalKind != ApprovalWorkflowRun {
		t.Fatalf("untrusted-parent follow-up state = %s/%s", follow.State, follow.ApprovalKind)
	}
	if follow.ParentTrusted {
		t.Fatal("ParentTrusted must be false")
	}
	// Approving it queues it (a maintainer's decision).
	if _, err := e.svc.ApproveRun(ctx(), "o/r", follow.ID, e.admin()); err != nil {
		t.Fatalf("ApproveRun: %v", err)
	}
}

func TestWorkflowRunDefaultTypeIsCompleted(t *testing.T) {
	// An omitted `types:` matches completed only, never requested.
	w, err := workflow.Parse([]byte(`name: F
on: workflow_run
jobs:
  j:
    runs-on: linux
    steps:
      - run: echo x
`))
	if err != nil {
		t.Fatal(err)
	}
	if !w.FiresMatching(workflow.Match{Event: "workflow_run", Action: "completed", UpstreamWorkflow: "X"}) {
		t.Fatal("completed must fire")
	}
	if w.FiresMatching(workflow.Match{Event: "workflow_run", Action: "requested", UpstreamWorkflow: "X"}) {
		t.Fatal("requested must not fire with the default types")
	}
}

// --- pull_request_target (D-2) ---------------------------------------------

func TestPullRequestTargetOffByDefault(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/prt.yml", prTargetWorkflowYAML)

	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Branch: "main", Action: "opened", BaseBranch: "main", BaseSHA: testSHA,
		PullHeadSHA: testSHA, Actor: "writer",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("pull_request_target ran while disabled: %d", len(runs))
	}
}

func TestPullRequestTargetHeadCheckoutNeedsSecondSwitch(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/prt.yml", prTargetWorkflowYAML)
	e.setSettings(t, SettingsInput{PullRequestTarget: true})

	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Branch: "main", Action: "opened", BaseBranch: "main", BaseSHA: testSHA,
		PullHeadSHA: testSHA, Actor: "writer",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%d err=%v", len(runs), err)
	}
	run := runs[0]
	if !run.PRTarget || !run.Trusted || run.Commit != testSHA {
		t.Fatalf("pr_target run = %+v", run)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
	if jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "pr_target_allow_head_checkout") {
		t.Fatalf("head checkout was not refused: %s / %s", jobs[0].State, jobs[0].Reason)
	}

	// The second, separately audited switch permits it.
	e.setSettings(t, SettingsInput{PullRequestTarget: true, PRTargetAllowHeadCheckout: true})
	runs, err = e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Branch: "main", Action: "opened", BaseBranch: "main", BaseSHA: testSHA,
		PullHeadSHA: testSHA, Actor: "writer",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("enabled: runs=%d err=%v", len(runs), err)
	}
	jobs, _ = e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if jobs[0].State != JobQueued {
		t.Fatalf("head checkout with the switch on = %s (%s)", jobs[0].State, jobs[0].Reason)
	}
}

// --- merge-ref semantics (PAR-024) -----------------------------------------

func TestPullRequestRunsTheMergeCommit(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	head := "2222222222222222222222222222222222222222"
	merge := "3333333333333333333333333333333333333333"
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	e.git.add(head, ".spindle/workflows/ci.yml", workflowYAML)
	e.git.merges[testSHA+".."+head] = merge

	runs, err := e.svc.CreateRun(ctx(), "o/r", head, EventPullReq, CreateOpts{
		Branch: "main", Action: "synchronize", Ref: "refs/pull/7/head",
		BaseBranch: "main", BaseSHA: testSHA, PullNumber: 7, Actor: "writer",
		EventPayload: PullEventPayload("o/r", "synchronize", 7, "t", "open", "writer",
			"refs/heads/main", testSHA, "refs/heads/f", head, "o/r", false, false, false, "writer"),
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%d err=%v", len(runs), err)
	}
	run := runs[0]
	if run.Commit != merge {
		t.Fatalf("run commit = %s, want the merge commit %s", run.Commit, merge)
	}
	if run.Ref != "refs/pull/7/merge" {
		t.Fatalf("run ref = %s", run.Ref)
	}
	if run.WorkflowCommit != head {
		t.Fatalf("workflow file read at %s, want the head %s", run.WorkflowCommit, head)
	}
	pr, _ := run.EventPayload["pull_request"].(map[string]any)
	if pr["merge_commit_sha"] != merge {
		t.Fatalf("payload merge_commit_sha = %v", pr["merge_commit_sha"])
	}
}

func TestPullRequestMergeFallbackKeepsTheHead(t *testing.T) {
	// The fallback is per delivery, not per action: a metadata-only edge
	// (labeled) carries the PR number now, so a conflicting merge must take the
	// same documented head fallback as the lifecycle edges.
	for _, action := range []string{"opened", "labeled"} {
		t.Run(action, func(t *testing.T) {
			e := newTestEnv(t)
			e.useDefaultBranch()
			head := "2222222222222222222222222222222222222222"
			e.git.add(head, ".spindle/workflows/ci.yml", workflowYAML)
			e.git.mergeErr = context.DeadlineExceeded

			runs, err := e.svc.CreateRun(ctx(), "o/r", head, EventPullReq, CreateOpts{
				Branch: "main", Action: action, Ref: "refs/pull/8/head",
				BaseBranch: "main", BaseSHA: testSHA, PullNumber: 8, Actor: "writer",
			})
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%d err=%v", len(runs), err)
			}
			if runs[0].Commit != head || runs[0].Ref != "refs/pull/8/head" {
				t.Fatalf("fallback run = %s @ %s", runs[0].Commit, runs[0].Ref)
			}
		})
	}
}

// --- fork write tokens (PAR-118 / D-13) ------------------------------------

func TestForkWriteTokenPolicy(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	forkWorkflow := `name: Fork
on: pull_request
permissions:
  contents: write
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo x
`
	e.git.add(testSHA, ".spindle/workflows/ci.yml", forkWorkflow)
	create := func() *Run {
		runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
			Branch: "main", Action: "opened", Ref: "refs/pull/9/head",
			BaseBranch: "main", BaseSHA: testSHA, Fork: true, Actor: "outsider",
		})
		if err != nil || len(runs) != 1 {
			t.Fatalf("fork run: %d, %v", len(runs), err)
		}
		return runs[0]
	}
	// Default: the run is untrusted and read-only.
	run := create()
	if run.WriteTokens {
		t.Fatal("send_write_tokens is off by default")
	}
	if perms := e.svc.EffectivePermissionsFor(&Spec{Permissions: map[string]string{"contents": "write"}}, false, run.WriteTokens); perms["contents"] != PermRead {
		t.Fatalf("default fork permissions = %v", perms)
	}
	// Switching it on is maintain+ and records the decision on the next run.
	e.setSettings(t, SettingsInput{SendWriteTokens: true})
	run = create()
	if !run.WriteTokens {
		t.Fatal("the switch must be recorded on the run")
	}
	perms := e.svc.EffectivePermissionsFor(&Spec{Permissions: map[string]string{"contents": "write"}}, false, run.WriteTokens)
	if perms["contents"] != PermWrite {
		t.Fatalf("enabled fork permissions = %v", perms)
	}
	// It still never grants id-token.
	if e.svc.IDTokenAllowed(&Spec{Permissions: map[string]string{"id-token": "write"}}, false) {
		t.Fatal("an untrusted run must never get an id-token")
	}
}

func TestSettingsRequireMaintainAndAudit(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	var audited []string
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audited = append(audited, action+"="+detail)
		return nil
	}
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{SendWriteTokens: true},
		auth.Principal{Name: "writer"}); err == nil {
		t.Fatal("a writer must not flip a maintain+ switch")
	}
	view, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{
		SendWriteTokens: true, PullRequestTarget: true, PRTargetAllowHeadCheckout: true, ConcurrencyCap: 2,
	}, auth.Principal{Name: "maint"})
	if err != nil {
		t.Fatalf("maintain update: %v", err)
	}
	if !view.SendWriteTokens || !view.PullRequestTarget || view.ConcurrencyCap != 2 {
		t.Fatalf("view = %+v", view)
	}
	if len(audited) != 4 {
		t.Fatalf("audit = %v (want four changed switches)", audited)
	}
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{PRTargetAllowHeadCheckout: true},
		auth.Principal{Name: "maint"}); err == nil {
		t.Fatal("head checkout without pull_request_target must be refused")
	}
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{ConcurrencyCap: 501},
		auth.Principal{Name: "maint"}); err == nil {
		t.Fatal("an out-of-range cap must be refused")
	}
	// A read-only view is available to a reader.
	if _, err := e.svc.GetSettings(ctx(), "o/r", e.reader()); err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
}

// --- concurrency cap (PAR-153) ---------------------------------------------

func TestConcurrencyCapHoldsAndReleases(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	e.setSettings(t, SettingsInput{ConcurrencyCap: 2})

	first := e.mustCreate(t)[0]
	other := "4444444444444444444444444444444444444444"
	e.git.add(other, ".spindle/workflows/ci.yml", independentWorkflowYAML)
	second, err := e.svc.CreateRun(ctx(), "o/r", other, EventDispatch, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml", Actor: "writer",
	})
	if err != nil || len(second) != 1 {
		t.Fatalf("second run: %d, %v", len(second), err)
	}
	if second[0].HoldReason != "" {
		t.Fatalf("the second run is under a cap of 2, held: %q", second[0].HoldReason)
	}
	thirdSHA := "5555555555555555555555555555555555555555"
	e.git.add(thirdSHA, ".spindle/workflows/ci.yml", independentWorkflowYAML)
	third, err := e.svc.CreateRun(ctx(), "o/r", thirdSHA, EventDispatch, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml", Actor: "writer",
	})
	if err != nil || len(third) != 1 {
		t.Fatalf("third run: %d, %v", len(third), err)
	}
	if third[0].HoldReason == "" {
		t.Fatal("the third run above a cap of 2 must be held with a named reason")
	}
	if !strings.Contains(third[0].HoldReason, "concurrency cap 2") {
		t.Fatalf("hold reason = %q", third[0].HoldReason)
	}
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", third[0].ID)
	for _, j := range jobs {
		if j.QueueSeq != "" {
			t.Fatalf("a held job was enqueued: %+v", j)
		}
	}
	// Finish the first run; the fold releases the held run.
	for _, j := range mustJobs(t, e, first.ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", first.ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	released, _ := e.svc.loadRun(ctx(), "o", "r", third[0].ID)
	if released.HoldReason != "" {
		t.Fatalf("held run was not released: %q", released.HoldReason)
	}
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil || len(head.Pending) == 0 {
		t.Fatalf("released run has no queue entries: head=%v err=%v", head, err)
	}
}

// --- queue reasons (PAR-156) ------------------------------------------------

func TestQueueReasons(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]

	// No runner at all.
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "no runner") {
		t.Fatalf("no-runner reason = %q", got)
	}
	// A runner with the wrong labels.
	putRunner := func(id string, labels []string, current *RunnerJobRef) {
		doc := &RunnerDoc{ID: id, Name: id, Labels: labels, Capacity: 1, State: "active",
			AllowAll: true, CurrentJob: current, Version: 1}
		if err := e.svc.putCreate(ctx(), RunnerKey(id), mustJSON(doc)); err != nil {
			t.Fatal(err)
		}
	}
	putRunner("r1", []string{"macos"}, nil)
	view, _ = e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "no runner matches labels") {
		t.Fatalf("label reason = %q", got)
	}
	putRunner("r2", []string{"linux"}, &RunnerJobRef{})
	view, _ = e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "busy") {
		t.Fatalf("busy reason = %q", got)
	}
	// An approval hold outranks runner availability.
	run.State = RunPendingApproval
	run.ApprovalKind = ApprovalWorkflowRun
	if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(run), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "awaiting approval") {
		t.Fatalf("approval reason = %q", got)
	}
}

// --- deployment events (PAR-021) -------------------------------------------

func TestDeploymentEventsCreateFollowUps(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/deploy.yml", `name: Deploy Events
on: [deployment, deployment_status]
jobs:
  note:
    runs-on: linux
    steps:
      - run: echo "${{ github.event.deployment_status.state }}"
`)
	run := e.mustCreate(t)[0]
	job := &Job{ID: "j1", RunID: run.ID, Repo: "o/r", State: JobQueued,
		Deployment: &Deployment{Environment: "prod", State: DeploymentPending}}
	e.svc.emitDeploymentEvents(run, job, deploymentStatusState(DeploymentPending), "")
	runs, err := e.svc.listRunsForCap(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.Event] = true
	}
	if !seen[EventDeployment] || !seen[EventDeploymentStatus] {
		t.Fatalf("deployment follow-ups = %v", seen)
	}
	// The status vocabulary maps onto GitHub's.
	cases := map[string]string{
		DeploymentPending: "pending", DeploymentApproved: "in_progress",
		DeploymentDenied: "failure", DeploymentFailed: "failure", "": "queued",
	}
	for in, want := range cases {
		if got := deploymentStatusState(in); got != want {
			t.Fatalf("deploymentStatusState(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- remaining Wave 3 edges ------------------------------------------------

func TestSettingsHTTP(t *testing.T) {
	e := newTestEnv(t)
	h := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return e.admin(), nil
	}}
	do := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/o/r/api/threads/settings", strings.NewReader(body))
		if !h.Handle(rec, req) {
			t.Fatal("settings route not claimed")
		}
		return rec
	}
	if rec := do(http.MethodPut, `{"pull_request_target":true,"concurrency_cap":3}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := do(http.MethodGet, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pull_request_target":true`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, `{"pr_target_allow_head_checkout":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("head checkout without pr_target = %d", rec.Code)
	}
	if rec := do(http.MethodPut, `{"concurrency_cap":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative cap = %d", rec.Code)
	}
	if rec := do(http.MethodPut, `{"bogus":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := do(http.MethodDelete, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	anon := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Anonymous(), nil
	}}
	rec = httptest.NewRecorder()
	anon.Handle(rec, httptest.NewRequest(http.MethodPut, "/o/r/api/threads/settings",
		strings.NewReader(`{"send_write_tokens":true}`)))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous PUT = %d", rec.Code)
	}
}

func TestClientPayloadValueShapes(t *testing.T) {
	if _, err := ValidateClientPayload(map[string]any{"list": []any{"a", 1.0, true, nil}}); err != nil {
		t.Fatalf("a scalar list must be accepted: %v", err)
	}
	if _, err := ValidateClientPayload(map[string]any{"obj": map[string]any{"n": 1.0}}); err != nil {
		t.Fatalf("a nested object must be accepted: %v", err)
	}
	if _, err := ValidateClientPayload(map[string]any{"bad": map[string]any{"": 1}}); err == nil {
		t.Fatal("an empty nested key must be refused")
	}
	if _, err := ValidateClientPayload(map[string]any{"bad": struct{}{}}); err == nil {
		t.Fatal("an unsupported value type must be refused")
	}
	if _, err := ValidateClientPayload(nil); err != nil {
		t.Fatalf("nil payload: %v", err)
	}
	// RefEventPayload names the ref and its type.
	payload := RefEventPayload("o/r", "refs/tags/v1", "tag", "push")
	if payload["ref"] != "refs/tags/v1" || payload["ref_type"] != "tag" {
		t.Fatalf("ref payload = %v", payload)
	}
}

func TestPRTargetHeadCheckoutRefusalShapes(t *testing.T) {
	if _, refused := prTargetHeadCheckoutRefusal(nil); refused {
		t.Fatal("nil spec cannot be refused")
	}
	plain := &Spec{Steps: []StepSpec{{Uses: "actions/checkout@v4"}}}
	if _, refused := prTargetHeadCheckoutRefusal(plain); refused {
		t.Fatal("a checkout with no ref is not a head checkout")
	}
	repoRef := &Spec{Steps: []StepSpec{{Uses: "actions/checkout", With: map[string]string{"repository": "fork/r"}}}}
	if reason, refused := prTargetHeadCheckoutRefusal(repoRef); !refused || !strings.Contains(reason, "repository:fork/r") {
		t.Fatalf("repository checkout = %q %v", reason, refused)
	}
	composite := &Spec{
		Steps: []StepSpec{{Uses: "./.spindle/actions/x"}},
		Actions: map[string]ActionSpec{
			"a": {Using: "composite", Steps: []StepSpec{{Uses: "Actions/Checkout@v4", With: map[string]string{"ref": "refs/pull/1/head"}}}},
		},
	}
	if reason, refused := prTargetHeadCheckoutRefusal(composite); !refused || !strings.Contains(reason, "composite action") {
		t.Fatalf("composite head checkout = %q %v", reason, refused)
	}
}

func TestRepositoryDispatchRefusals(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/dispatch.yml", dispatchWorkflowYAML)
	// An empty event_type.
	if _, err := e.svc.RepositoryDispatch(ctx(), "o/r", " ", nil, "", e.writer()); err == nil {
		t.Fatal("an empty event_type must be refused")
	}
	// A ref that is not the default branch tip.
	if _, err := e.svc.RepositoryDispatch(ctx(), "o/r", "deploy", nil, "refs/heads/dev", e.writer()); err == nil {
		t.Fatal("a non-default ref must be refused")
	}
	// An unreadable git backend.
	broken := *e.svc
	broken.Git = nil
	if _, err := broken.RepositoryDispatch(ctx(), "o/r", "deploy", nil, "", e.writer()); err == nil {
		t.Fatal("an unwired git backend must be refused")
	}
	// The write role is required.
	if _, err := e.svc.RepositoryDispatch(ctx(), "o/r", "deploy", nil, "", auth.Principal{Name: "reader"}); err == nil {
		t.Fatal("a reader must not dispatch")
	}
}

func TestDetachRunsThroughTheHook(t *testing.T) {
	e := newTestEnv(t)
	ran := false
	e.svc.RunDetached = func(f func()) { f() }
	e.svc.detach(func(context.Context) { ran = true })
	if !ran {
		t.Fatal("RunDetached must own the follow-up call")
	}
	e.svc.detach(nil) // nil work is a no-op
}

func TestSettingsCorruptRecordFailsClosed(t *testing.T) {
	e := newTestEnv(t)
	if err := e.svc.putCreate(ctx(), SettingsKey("o", "r"), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetSettings(ctx(), "o/r", e.admin()); err == nil {
		t.Fatal("a corrupt settings record must be a read error")
	}
	// Creation treats it as all switches off, not as an error/widening.
	if p := e.svc.loadRepoPolicy(ctx(), "o", "r"); p.sendWriteTokens || p.pullTarget || p.cap != 0 {
		t.Fatalf("corrupt policy = %+v", p)
	}
}

func TestMergeRefCommitEdges(t *testing.T) {
	e := newTestEnv(t)
	// An unknown base branch cannot be resolved: no merge commit.
	if _, ok := e.svc.mergeRefCommit(ctx(), "o/r", CreateOpts{BaseBranch: "nope", PullNumber: 1}, testSHA, repoPolicy{}); ok {
		t.Fatal("a missing base branch must fall back to the head")
	}
	// An empty base branch cannot be resolved either.
	if _, ok := e.svc.mergeRefCommit(ctx(), "o/r", CreateOpts{PullNumber: 1}, testSHA, repoPolicy{}); ok {
		t.Fatal("an empty base branch must fall back")
	}
}

func TestCapacityEdges(t *testing.T) {
	e := newTestEnv(t)
	// Clearing a hold on a run that is not held is a no-op, not a false win.
	run := e.mustCreate(t)[0]
	if e.svc.clearCapacityHold(ctx(), "o", "r", run.ID) {
		t.Fatal("a run with no hold cannot be released")
	}
	if e.svc.clearCapacityHold(ctx(), "o", "r", "missing-run") {
		t.Fatal("a missing run cannot be released")
	}
	// A corrupt run header in the prefix is a read error, never a panic.
	if err := e.svc.putCreate(ctx(), RunKey("o", "r", "corrupt"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.listRunsForCap(ctx(), "o", "r"); err == nil {
		t.Fatal("a corrupt run header must surface")
	}
}

func TestQueueReasonBranches(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	// Environment and fork approval reasons.
	run.State, run.ApprovalKind, run.Environments = RunPendingApproval, ApprovalEnvironment, []string{"prod"}
	if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(run), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	view, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "prod") {
		t.Fatalf("environment reason = %q", got)
	}
	run.State, run.ApprovalKind, run.Environments = RunPendingApproval, ApprovalFork, nil
	if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(run), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "first-time fork") {
		t.Fatalf("fork reason = %q", got)
	}
	// A corrupt runner record makes availability unknown but never fails the page.
	if err := e.svc.putCreate(ctx(), RunnerKey("bad"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	run.State, run.ApprovalKind = RunQueued, ""
	if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(run), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public()); err != nil {
		t.Fatalf("GetRun with a corrupt runner: %v", err)
	}
}

func TestWorkflowRunParentProjection(t *testing.T) {
	if got := workflowRunParentProjection(nil); got.ID != "" {
		t.Fatalf("nil run = %+v", got)
	}
	run := &Run{ID: "r1", Workflow: "CI", Event: EventPush, State: RunCancelled, Repo: "o/r"}
	if p := workflowRunParentProjection(run); p.Status != "completed" || p.Conclusion != RunCancelled {
		t.Fatalf("cancelled parent = %+v", p)
	}
	if !containsFoldString([]string{" Completed "}, "completed") || containsFoldString(nil, "x") {
		t.Fatal("containsFoldString")
	}
}

func TestRefusedTriggerNames(t *testing.T) {
	for name := range workflow.RefusedTriggers {
		if _, err := workflow.Parse([]byte("on: " + name + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n")); err == nil {
			t.Fatalf("%s must be a named parse error, not an inert trigger", name)
		}
	}
}

// gitReaderOnly hides the fake's optional MergeRefReader/DiffReader methods,
// pinning the fallback when a deployment's git backend cannot build a merge.
type gitReaderOnly struct{ GitReader }

func TestCreateRunWave3Validation(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/follow.yml", followWorkflowYAML)
	e.git.add(testSHA, ".spindle/workflows/prt.yml", prTargetWorkflowYAML)

	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventWorkflowRun, CreateOpts{Action: "in_progress"}); err == nil {
		t.Fatal("an unemitted workflow_run type must be refused")
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventWorkflowRun, CreateOpts{
		Action: "completed", ChainDepth: maxWorkflowRunChain + 1,
	}); err == nil {
		t.Fatal("a chain past the cap must be refused")
	}
	e.setSettings(t, SettingsInput{PullRequestTarget: true})
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{Action: "opened"}); err == nil {
		t.Fatal("pull_request_target without a base branch must be refused")
	}
	// The full disable path returns no runs (not an error).
	e.setSettings(t, SettingsInput{})
	if runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Action: "opened", BaseBranch: "main",
	}); err != nil || len(runs) != 0 {
		t.Fatalf("disabled pull_request_target = %d, %v", len(runs), err)
	}
	// A backend without the optional merge reader falls back to the head.
	plain := *e.svc
	plain.Git = gitReaderOnly{GitReader: e.git}
	runs, err := plain.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened", Branch: "main", Ref: "refs/pull/4/head", BaseBranch: "main", BaseSHA: testSHA,
		PullNumber: 4, Actor: "writer", Fork: true,
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("fallback run = %d, %v", len(runs), err)
	}
	if runs[0].Ref != "refs/pull/4/head" {
		t.Fatalf("fallback ref = %s", runs[0].Ref)
	}
}

func TestUpdateSettingsNothingToClear(t *testing.T) {
	e := newTestEnv(t)
	view, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{}, e.admin())
	if err != nil {
		t.Fatalf("empty update: %v", err)
	}
	if view.Version != 0 || view.SendWriteTokens {
		t.Fatalf("empty update wrote a record: %+v", view)
	}
}

func TestReleaseHeldRunsReleasesOneSlot(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	e.setSettings(t, SettingsInput{ConcurrencyCap: 1})
	first := e.mustCreate(t)[0]
	mk := func(sha, branch string) *Run {
		e.git.add(sha, ".spindle/workflows/ci.yml", independentWorkflowYAML)
		runs, err := e.svc.CreateRun(ctx(), "o/r", sha, EventDispatch, CreateOpts{
			Branch: branch, Ref: "refs/heads/" + branch, Workflow: "ci.yml", Actor: "writer",
		})
		if err != nil || len(runs) != 1 {
			t.Fatalf("create %s: %d %v", branch, len(runs), err)
		}
		return runs[0]
	}
	heldA := mk("6666666666666666666666666666666666666666", "a")
	heldB := mk("7777777777777777777777777777777777777777", "b")
	if heldA.HoldReason == "" || heldB.HoldReason == "" {
		t.Fatalf("expected both held: %q %q", heldA.HoldReason, heldB.HoldReason)
	}
	for _, j := range mustJobs(t, e, first.ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", first.ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := e.svc.loadRun(ctx(), "o", "r", heldA.ID)
	b, _ := e.svc.loadRun(ctx(), "o", "r", heldB.ID)
	if a.HoldReason != "" || b.HoldReason == "" {
		t.Fatalf("one slot should release oldest-first: a=%q b=%q", a.HoldReason, b.HoldReason)
	}
}

func TestQueueReasonNeedsAndGroup(t *testing.T) {
	dep := &Job{Key: "build", State: JobRunning}
	waiter := &Job{Key: "test", State: JobQueued, Needs: []string{"build", "missing"}}
	if got := waitingNeeds(waiter, []*Job{dep, waiter}); len(got) != 1 || got[0] != "build" {
		t.Fatalf("waitingNeeds = %v", got)
	}
	// A pending group outranks runner availability.
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	run.PendingGroup = "ci-main"
	run.HoldReason = ""
	if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(run), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	view, _ := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if got := view.Jobs[0].QueueReason; !strings.Contains(got, "concurrency group") {
		t.Fatalf("group reason = %q", got)
	}
}

func TestRunnerReasonBranches(t *testing.T) {
	job := &Job{Repo: "o/r", RunsOn: "linux", Trusted: true}
	run := &Run{Trusted: true}
	if got := runnerReason(nil, job, run); !strings.Contains(got, "no runner") {
		t.Fatalf("nil = %q", got)
	}
	forkLoader := &RunnerDoc{ID: "r", Labels: []string{"linux"}, Capacity: 1, AllowAll: true}
	if got := runnerReason([]*RunnerDoc{forkLoader}, job, &Run{Trusted: false}); !strings.Contains(got, "fork runs are disabled") {
		t.Fatalf("fork = %q", got)
	}
	available := &RunnerDoc{ID: "r", Labels: []string{"linux"}, Capacity: 2, AllowAll: true}
	if got := runnerReason([]*RunnerDoc{available}, job, run); !strings.Contains(got, "waiting for a matching runner") {
		t.Fatalf("waiting = %q", got)
	}
	revoked := &RunnerDoc{ID: "r", Labels: []string{"linux"}, AllowAll: true, RevokedAt: strPtr("2026-01-01T00:00:00Z")}
	if got := runnerReason([]*RunnerDoc{revoked}, job, run); !strings.Contains(got, "no runner matches") {
		t.Fatalf("revoked = %q", got)
	}
}

func TestWorkflowRunPayloadRequested(t *testing.T) {
	payload := WorkflowRunEventPayload("requested", WorkflowRunParent{ID: "r1", Name: "CI", Repository: "o/r"})
	wr, _ := payload["workflow_run"].(map[string]any)
	if wr["status"] != "queued" {
		t.Fatalf("requested status = %v", wr["status"])
	}
	completed := WorkflowRunEventPayload("completed", WorkflowRunParent{ID: "r1", Conclusion: "success"})
	wr, _ = completed["workflow_run"].(map[string]any)
	if wr["status"] != "completed" || wr["conclusion"] != "success" {
		t.Fatalf("completed payload = %v", wr)
	}
}

func TestRepositoryDispatchExplicitDefaultRef(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/dispatch.yml", dispatchWorkflowYAML)
	runs, err := e.svc.RepositoryDispatch(ctx(), "o/r", "deploy", map[string]any{"environment": "prod"},
		"refs/heads/main", e.writer())
	if err != nil || len(runs) != 1 {
		t.Fatalf("explicit ref dispatch = %d, %v", len(runs), err)
	}
	if runs[0].Ref != "refs/heads/main" || runs[0].Branch != "main" {
		t.Fatalf("dispatch ref = %q branch=%q", runs[0].Ref, runs[0].Branch)
	}
}

func TestCapacityGateAlreadyHeldIsIdempotent(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	run.HoldReason = capacityHoldReason(1)
	if _, err := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(run), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// countLiveRuns excludes held runs, so with limit 0 the gate holds; ask for
	// limit 0 explicitly to exercise the already-held no-write branch.
	held, err := e.svc.applyCapacityGate(ctx(), "o", "r", run.ID, 0, "now")
	if err != nil || !held {
		t.Fatalf("already-held gate = %v, %v", held, err)
	}
}

func TestSettingsViewBeforeAnyRecord(t *testing.T) {
	e := newTestEnv(t)
	view, err := e.svc.GetSettings(ctx(), "o/r", e.reader())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if view.SendWriteTokens || view.ConcurrencyCap != 0 || view.Version != 0 {
		t.Fatalf("default view = %+v", view)
	}
}

func TestDeploymentEmitterEdges(t *testing.T) {
	e := newTestEnv(t)
	e.svc.emitDeploymentEvents(nil, nil, "pending", "")
	e.svc.emitDeploymentEvents(&Run{}, nil, "pending", "")
	e.svc.emitDeploymentEvents(&Run{}, &Job{}, "pending", "")
	// A non-pending status emits the status event only (the deployment event is
	// creation-only); no workflows exist here, so both are silent no-ops.
	e.svc.emitDeploymentEvents(&Run{Repo: "o/r"}, &Job{Deployment: &Deployment{Environment: "prod"}}, "in_progress", "")
}

func TestAnnotateQueueReasonsNoQueuedJobs(t *testing.T) {
	e := newTestEnv(t)
	e.svc.annotateQueueReasons(ctx(), "o", "r", nil, nil)
	e.svc.annotateQueueReasons(ctx(), "o", "r", &Run{}, []*Job{{State: JobSuccess}})
}

func TestMergeRefCommitWithExplicitBase(t *testing.T) {
	e := newTestEnv(t)
	head := "8888888888888888888888888888888888888888"
	merge := "9999999999999999999999999999999999999999"
	e.git.merges[testSHA+".."+head] = merge
	sha, ok := e.svc.mergeRefCommit(ctx(), "o/r", CreateOpts{BaseCommit: testSHA, PullNumber: 2}, head, repoPolicy{})
	if !ok || sha != merge {
		t.Fatalf("explicit base merge = %q %v", sha, ok)
	}
}

func TestCapacityGateMissingRun(t *testing.T) {
	e := newTestEnv(t)
	if held, err := e.svc.applyCapacityGate(ctx(), "o", "r", "missing", 0, "now"); err == nil || held {
		t.Fatalf("missing run gate = %v, %v", held, err)
	}
	e.svc.releaseHeldRuns(ctx(), "o", "r") // no cap configured: a no-op
}

func TestEmitWorkflowRunFailureIsLogged(t *testing.T) {
	e := newTestEnv(t)
	// An unreadable default branch makes the follow-up creation fail; the
	// emitter logs it and never panics.
	e.git.refErr = context.DeadlineExceeded
	e.svc.emitWorkflowRun(ctx(), &Run{ID: "r1", Repo: "o/r", Workflow: "CI"}, "completed")
}

func TestEventFieldsClientPayloadNonScalar(t *testing.T) {
	fields := EventFieldsOf(map[string]any{
		"client_payload": map[string]any{"obj": map[string]any{"a": 1}, "list": []any{"x", "y"}},
	})
	if fields["event.client_payload.list"] != "x,y" {
		t.Fatalf("list projection = %q", fields["event.client_payload.list"])
	}
	if _, ok := fields["event.client_payload.obj"]; ok {
		t.Fatal("a nested object is not a scalar projection")
	}
}

func TestAnnotateQueueReasonsHoldBranch(t *testing.T) {
	e := newTestEnv(t)
	run := &Run{State: RunQueued, HoldReason: "repository concurrency cap 1 reached"}
	job := &Job{State: JobQueued}
	e.svc.annotateQueueReasons(ctx(), "o", "r", run, []*Job{job})
	if job.QueueReason != run.HoldReason {
		t.Fatalf("hold reason = %q", job.QueueReason)
	}
}

func TestMergeCommitValidation(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.git.MergeCommit(ctx(), "o/r", "not-a-sha", testSHA); err == nil {
		t.Fatal("an invalid base sha must be refused")
	}
	if _, err := e.git.MergeCommit(ctx(), "o/r", testSHA, "short"); err == nil {
		t.Fatal("an invalid head sha must be refused")
	}
	// A real subprocess reader against an unknown repo is the not-found class,
	// never a fabricated commit.
	g := NewSubprocessGit("")
	g.SetDirs(staticDirs{})
	if _, err := g.MergeCommit(ctx(), "o/r", testSHA, testSHA); err == nil {
		t.Fatal("an unresolvable repo must fail")
	}
}

func TestGetSettingsForbidden(t *testing.T) {
	e := newTestEnv(t)
	e.roles.public = false
	if _, err := e.svc.GetSettings(ctx(), "o/r", auth.Anonymous()); err == nil {
		t.Fatal("an anonymous read on a private repo must be refused")
	}
}

func TestReleaseHeldRunsCorruptHeader(t *testing.T) {
	e := newTestEnv(t)
	e.setSettings(t, SettingsInput{ConcurrencyCap: 1})
	if err := e.svc.putCreate(ctx(), RunKey("o", "r", "corrupt"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	e.svc.releaseHeldRuns(ctx(), "o", "r") // a read error is swallowed (best effort)
}

func TestRepositoryDispatchDefaultBranchReader(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.branch = "main"
	e.git.add(testSHA, ".spindle/workflows/dispatch.yml", dispatchWorkflowYAML)
	runs, err := e.svc.RepositoryDispatch(ctx(), "o/r", "deploy", nil, "", e.writer())
	if err != nil || len(runs) != 1 {
		t.Fatalf("dispatch = %d, %v", len(runs), err)
	}
	if runs[0].Ref != "refs/heads/main" {
		t.Fatalf("resolved ref = %q", runs[0].Ref)
	}
}

func TestWave3SplitRepoErrors(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.GetSettings(ctx(), "bad", e.admin()); err == nil {
		t.Fatal("GetSettings must reject a malformed repo id")
	}
	if _, err := e.svc.UpdateSettings(ctx(), "bad", SettingsInput{}, e.admin()); err == nil {
		t.Fatal("UpdateSettings must reject a malformed repo id")
	}
	if _, err := e.svc.RepositoryDispatch(ctx(), "bad", "deploy", nil, "", e.admin()); err == nil {
		t.Fatal("RepositoryDispatch must reject a malformed repo id")
	}
}

func TestSettingsCorruptWrite(t *testing.T) {
	e := newTestEnv(t)
	if err := e.svc.putCreate(ctx(), SettingsKey("o", "r"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{SendWriteTokens: true}, e.admin()); err == nil {
		t.Fatal("updating a corrupt settings record must fail")
	}
}

func TestRepositoryDispatchHeadUnreadable(t *testing.T) {
	e := newTestEnv(t)
	e.git.refErr = context.DeadlineExceeded
	if _, err := e.svc.RepositoryDispatch(ctx(), "o/r", "deploy", nil, "", e.writer()); err == nil {
		t.Fatal("an unreadable HEAD must fail the dispatch")
	}
}

func TestRunnerReasonTenancyAndMixedFork(t *testing.T) {
	job := &Job{Repo: "o/r", RunsOn: "linux", Trusted: true}
	run := &Run{Trusted: true}
	tenanted := &RunnerDoc{ID: "t", Labels: []string{"linux"}, Capacity: 1, AllowAll: false, Repos: []string{"other/r"}}
	if got := runnerReason([]*RunnerDoc{tenanted}, job, run); !strings.Contains(got, "no runner matches") {
		t.Fatalf("tenancy = %q", got)
	}
	mixed := []*RunnerDoc{
		{ID: "a", Labels: []string{"linux"}, Capacity: 1, AllowAll: true, AllowForkRuns: false},
		{ID: "b", Labels: []string{"linux"}, Capacity: 1, AllowAll: true, AllowForkRuns: true},
	}
	if got := runnerReason(mixed, job, &Run{Trusted: false}); !strings.Contains(got, "waiting for a matching runner") {
		t.Fatalf("mixed fork = %q", got)
	}
}

func TestWave3FaultPaths(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	fs := &faultStore{Memory: e.store}
	e.svc.Store = fs

	// applyCapacityGate: the run-list read fails closed.
	fs.fail("list", 0)
	if _, err := e.svc.applyCapacityGate(ctx(), "o", "r", "x", 1, "now"); err == nil {
		t.Fatal("a failing list must fail the gate")
	}
	// releaseHeldRuns: the same read is best-effort (logged/returned).
	e.setSettings(t, SettingsInput{ConcurrencyCap: 1})
	e.svc.releaseHeldRuns(ctx(), "o", "r")
	fs.clear()

	// clearCapacityHold: a corrupt run header is an error, not a release.
	if err := e.svc.putCreate(ctx(), RunKey("o", "r", "bad"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	if e.svc.clearCapacityHold(ctx(), "o", "r", "bad") {
		t.Fatal("a corrupt header cannot be released")
	}

	// queue reasons: a failing runner LIST degrades to "no visible runner".
	fs.fail("list", 0)
	if _, err := e.svc.loadRunnerDocsForReasons(ctx()); err == nil {
		t.Fatal("a failing runner list must surface")
	}
	fs.clear()
	// A runner record read failure is surfaced too.
	if err := e.svc.putCreate(ctx(), RunnerKey("bad"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.loadRunnerDocsForReasons(ctx()); err == nil {
		t.Fatal("a corrupt runner record must surface")
	}
	// GetRun still renders with that registry (the reason degrades).
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{}, e.admin()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetRun(ctx(), "o/r", e.mustCreate(t)[0].ID, e.public()); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
}

func TestNilDispatchInputs(t *testing.T) {
	if got := dispatchInputs(nil); got != nil {
		t.Fatalf("dispatchInputs(nil) = %v", got)
	}
	if declaresEnvironmentInput(nil) {
		t.Fatal("a nil workflow declares no environment input")
	}
}

func TestSubprocessMergeCommitInputValidation(t *testing.T) {
	g := NewSubprocessGit("")
	if _, err := g.MergeCommit(ctx(), "o/r", "nope", testSHA); err == nil {
		t.Fatal("an invalid base sha must be refused before git runs")
	}
	if _, err := g.MergeCommit(ctx(), "o/r", testSHA, "nope"); err == nil {
		t.Fatal("an invalid head sha must be refused before git runs")
	}
}

func TestEncodeEventPayloadRejectsUnmarshalable(t *testing.T) {
	if _, err := EncodeEventPayload(map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("an unmarshalable payload must be a named error")
	}
	// A payload whose repository member is a scalar (not an object) projects
	// nothing rather than panicking.
	if fields := EventFieldsOf(map[string]any{"repository": "o/r"}); len(fields) != 0 {
		t.Fatalf("fields = %v", fields)
	}
}

func TestWorkflowRunFollowerGate(t *testing.T) {
	if declaresWorkflowRun(nil) || declaresWorkflowRun([]*WorkflowFile{nil, {Error: "broken"}}) {
		t.Fatal("no parsed follower must be reported")
	}
	w, err := workflow.Parse([]byte("on: workflow_run\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !declaresWorkflowRun([]*WorkflowFile{{Workflow: w}}) {
		t.Fatal("a workflow_run file must be reported")
	}
	// A run with no follower flag does not emit on completion.
	e := newTestEnv(t)
	e.useIndependentJobs()
	run := e.mustCreate(t)[0]
	for _, j := range mustJobs(t, e, run.ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.countRuns(t); got != 1 {
		t.Fatalf("a repository with no follower created %d extra runs", got-1)
	}
}

func TestWorkflowRunRequestedSkipsHeldRuns(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/follow.yml", followWorkflowYAML)
	// A held run never emits requested (it has not started); the approval path
	// owns it.
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/1/head", BaseBranch: "main",
		RefName: "main", Fork: true, Actor: "outsider",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("fork run = %d, %v", len(runs), err)
	}
	if got := e.countRuns(t); got != 1 {
		t.Fatalf("a held fork run emitted %d follow-ups", got-1)
	}
}

func TestPullRequestTargetForkUsesTheApprovalGate(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/prt.yml", prTargetWorkflowYAML)
	e.setSettings(t, SettingsInput{PullRequestTarget: true, PRTargetAllowHeadCheckout: true})
	e.svc.ApproveFirstTimeForks = true
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Branch: "main", Action: "opened", BaseBranch: "main", BaseSHA: testSHA,
		PullHeadSHA: testSHA, Fork: true, Actor: "outsider",
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("pr_target fork run = %d, %v", len(runs), err)
	}
	if runs[0].State != RunPendingApproval || runs[0].ApprovalKind != ApprovalFork {
		t.Fatalf("pr_target fork run = %s/%s (the first-time fork gate must apply)", runs[0].State, runs[0].ApprovalKind)
	}
}

func TestWave3MoreBranches(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.setSettings(t, SettingsInput{PullRequestTarget: true})
	// An invalid base commit is refused before any run exists.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Action: "opened", BaseBranch: "main", BaseCommit: "short",
	}); err == nil {
		t.Fatal("an invalid base commit must be refused")
	}
	// An unknown base branch is the resolve error.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullTarget, CreateOpts{
		Action: "opened", BaseBranch: "nope",
	}); err == nil {
		t.Fatal("an unknown base branch must be refused")
	}
	// A path-filtered pull_request_target diffs base...head (the head sha is
	// separate from the base commit the run reports).
	e.git.add(testSHA, ".spindle/workflows/prt.yml", `name: PRT
on:
  pull_request_target:
    paths: ['src/**']
jobs:
  j:
    runs-on: linux
    steps:
      - run: echo x
`)
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e.git.add(head, ".spindle/workflows/prt.yml", `name: PRT
on:
  pull_request_target:
    paths: ['src/**']
jobs:
  j:
    runs-on: linux
    steps:
      - run: echo x
`)
	e.git.diffs[testSHA+"..."+head] = []string{"src/main.go"}
	runs, err := e.svc.CreateRun(ctx(), "o/r", head, EventPullTarget, CreateOpts{
		Action: "opened", BaseBranch: "main", BaseSHA: testSHA, PullHeadSHA: head,
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("path-filtered pr_target = %d, %v", len(runs), err)
	}

	// applyCapacityGate on a corrupt header surfaces the decode error.
	if err := e.svc.putCreate(ctx(), RunKey("o", "r", "corrupt-cap"), []byte("{nope")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.applyCapacityGate(ctx(), "o", "r", "corrupt-cap", 0, "now"); err == nil {
		t.Fatal("a corrupt run header must fail the gate")
	}

	// repository_dispatch: a ref that resolves to another commit is refused.
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e.git.refs["refs/heads/dev"] = other
	if _, err := e.svc.RepositoryDispatch(ctx(), "o/r", "deploy", nil, "refs/heads/dev", e.writer()); err == nil {
		t.Fatal("a non-tip ref must be refused")
	}

	// The head-branch projection reads the PR head for a pull_request run.
	prRun := &Run{Event: EventPullReq, Branch: "main",
		EventPayload: map[string]any{"pull_request": map[string]any{"head": map[string]any{"ref": "topic"}}}}
	if got := workflowRunHeadBranch(prRun); got != "topic" {
		t.Fatalf("head branch = %q", got)
	}
	if got := workflowRunHeadBranch(&Run{Ref: "refs/tags/v1"}); got != "v1" {
		t.Fatalf("tag head = %q", got)
	}
}

func TestRunnerDocsReadFailure(t *testing.T) {
	e := newTestEnv(t)
	fs := &faultStore{Memory: e.store}
	e.svc.Store = fs
	if err := e.svc.putCreate(ctx(), RunnerKey("r1"), mustJSON(&RunnerDoc{ID: "r1"})); err != nil {
		t.Fatal(err)
	}
	fs.fail("get", 0)
	if _, err := e.svc.loadRunnerDocsForReasons(ctx()); err == nil {
		t.Fatal("a failing runner GET must surface")
	}
}

func TestForkFailureEmitsHeldWorkflowRun(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.git.add(testSHA, ".spindle/workflows/build.yml", `name: Build
on: [push, pull_request]
jobs:
  build:
    runs-on: linux
    environment: missing
    steps:
      - run: echo x
`)
	e.git.add(testSHA, ".spindle/workflows/follow.yml", `name: Follow
on:
  workflow_run:
    workflows: [Build]
    types: [completed]
jobs:
  consume:
    runs-on: linux
    steps:
      - run: echo follow
`)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", Action: "opened", Ref: "refs/pull/1/head", BaseBranch: "main",
		BaseSHA: testSHA, Fork: true, Actor: "outsider",
	})
	if err != nil || len(runs) < 1 {
		t.Fatalf("fork run = %d, %v", len(runs), err)
	}
	for _, rr := range runs {
		t.Logf("created event=%s wf=%s state=%s", rr.Event, rr.Workflow, rr.State)
	}
	if runs[0].State != RunFailure {
		t.Fatalf("upstream state = %s", runs[0].State)
	}
	all, err := e.svc.listRunsForCap(ctx(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	var follow *Run
	for _, r := range all {
		if r.Event == EventWorkflowRun {
			follow = r
		}
	}
	if follow == nil {
		t.Fatalf("no follow-up; upstream followers=%v", runs[0].WorkflowRunFollowers)
	}
	if follow.State != RunPendingApproval || follow.ApprovalKind != ApprovalWorkflowRun {
		t.Fatalf("follow state = %s/%s", follow.State, follow.ApprovalKind)
	}
}
