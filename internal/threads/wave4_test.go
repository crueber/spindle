package threads

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// Wave-4 parity tests: runner scopes/groups/ephemeral/fleet operations, the
// schedule surface and its coalescing rule, queue prioritization, CI pause,
// authoring validation, notifications and the signed non-goals.

func wave4Admin() auth.Principal { return auth.Principal{Name: "admin", Admin: true} }

// wave4Runner mints + registers one runner with the given options and returns
// its id, bearer and auth context.
func wave4Runner(t *testing.T, e *testEnv, in MintInput, reg RegisterInput) (string, string, *runnerAuth) {
	t.Helper()
	mint, err := e.svc.MintRegistrationToken(ctx(), wave4Admin(), in)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg.Token = mint.Token
	res, err := e.svc.Register(ctx(), reg)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	authn, err := e.svc.authorizeRunner(ctx(), res.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return res.RunnerID, res.Token, authn
}

func TestRunnerScopeDerivation(t *testing.T) {
	cases := []struct {
		name string
		pol  RunnerPolicy
		want string
	}{
		{"all", RunnerPolicy{AllowAll: true}, RunnerScopeInstance},
		{"none", RunnerPolicy{}, RunnerScopeNone},
		{"org", RunnerPolicy{Repos: []string{"acme/*", "other/*"}}, RunnerScopeOrg},
		{"repo", RunnerPolicy{Repos: []string{"acme/mono"}}, RunnerScopeRepo},
		{"mixed", RunnerPolicy{Repos: []string{"acme/*", "acme/mono"}}, RunnerScopeRepo},
	}
	for _, tc := range cases {
		if got := tc.pol.Scope(); got != tc.want {
			t.Fatalf("%s: scope = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := (RunnerPolicy{Repos: []string{"acme/*"}}).DescribeScope(); !strings.Contains(got, "organization") || !strings.Contains(got, "acme") {
		t.Fatalf("describe = %q", got)
	}
}

func TestOrgScopedRunnerServesOnlyThatOrg(t *testing.T) {
	e := newTestEnv(t)
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "org-runner", RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"acme/*"}}},
		RegisterInput{Labels: []string{"linux"}})
	if !authn.Doc.CanServeRepo("acme/one") {
		t.Fatal("org runner refused a repository in its org")
	}
	if authn.Doc.CanServeRepo("other/two") {
		t.Fatal("org runner may serve a repository outside its org")
	}
	if authn.Doc.policy().Scope() != RunnerScopeOrg {
		t.Fatalf("scope = %q", authn.Doc.policy().Scope())
	}
}

func TestRunnerGroupRestrictsAndInherits(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	group, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{
		Name: "gpu-boxes", Labels: []string{"gpu", "cuda"},
		RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"acme/*"}},
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "gpu-1", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, group.ID, wave4Admin()); err != nil {
		t.Fatalf("assign group: %v", err)
	}
	doc, _, lerr := e.svc.loadRunner(ctx(), authn.Doc.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !doc.CanServeRepo("acme/one") {
		t.Fatal("group member refused an allowed repository")
	}
	if doc.CanServeRepo("other/two") {
		t.Fatal("group visibility did not narrow the runner's all-repositories policy")
	}
	if !labelsMatch(doc.EffectiveLabels(), []string{"linux", "gpu", "cuda"}) {
		t.Fatalf("inherited labels = %v", doc.EffectiveLabels())
	}
	if labelsMatch(doc.EffectiveLabels(), []string{"arm64"}) {
		t.Fatal("group labels matched an undeclared label")
	}
}

func TestEphemeralRunnerRetiredAfterOneJob(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("create: %v", err)
	}
	id, token, authn := wave4Runner(t, e,
		MintInput{Name: "disposable", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}, Capacity: 1, Ephemeral: true})
	got, ferr := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, "")
	if ferr != nil || got == nil {
		t.Fatalf("fetch: %+v, %v", got, ferr)
	}
	start, err := e.svc.Start(ctx(), authn, got.JobRef, got.Attempt)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	claims, err := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if err != nil {
		t.Fatalf("attempt token: %v", err)
	}
	if _, err := e.svc.Complete(ctx(), claims, CompleteInput{Conclusion: JobSuccess}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	doc, _, err := e.svc.loadRunner(ctx(), id)
	if err != nil {
		t.Fatal(err)
	}
	if doc != nil {
		t.Fatalf("ephemeral runner record survived its job: %+v", doc)
	}
	if _, err := e.svc.authorizeRunner(ctx(), token); err == nil {
		t.Fatal("retired ephemeral credential still authenticates")
	}
}

func TestRunnerDrainClaimsNothingAndResumes(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	id, _, authn := wave4Runner(t, e,
		MintInput{Name: "drainable", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if err := e.svc.DrainRunner(ctx(), id, wave4Admin()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Each protocol request re-authorizes (one registry GET), which is how the
	// server sees a drain immediately; the test's cached auth is pre-drain.
	_, _, authn = wave4RunnerStale(t, e, authn.Doc.ID)
	got, err := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, "")
	if err != nil || got != nil {
		t.Fatalf("draining runner was offered a job: %+v, %v", got, err)
	}
	if err := e.svc.ResumeRunner(ctx(), id, wave4Admin()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_, _, authn = wave4RunnerStale(t, e, authn.Doc.ID)
	got, err = e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, "")
	if err != nil || got == nil {
		t.Fatalf("resumed runner got no job: %+v, %v", got, err)
	}
}

func TestRunnerTokenRotationOverlap(t *testing.T) {
	e := newTestEnv(t)
	_, oldToken, authn := wave4Runner(t, e,
		MintInput{Name: "rotate-me", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	rot, err := e.svc.RotateOwnRunnerToken(ctx(), authn)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rot.Token == oldToken || !strings.HasPrefix(rot.Token, runnerTokenPrefix) {
		t.Fatalf("rotation answer = %+v", rot)
	}
	if _, err := e.svc.authorizeRunner(ctx(), oldToken); err != nil {
		t.Fatalf("old credential refused inside the overlap window: %v", err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), rot.Token); err != nil {
		t.Fatalf("new credential refused: %v", err)
	}
	e.advance(RotationGrace + time.Minute)
	if _, err := e.svc.authorizeRunner(ctx(), oldToken); err == nil {
		t.Fatal("old credential still authenticates after the overlap window")
	}
	if _, err := e.svc.authorizeRunner(ctx(), rot.Token); err != nil {
		t.Fatalf("new credential stopped working: %v", err)
	}
}

func TestRunnerDeregisterRemovesCredential(t *testing.T) {
	e := newTestEnv(t)
	id, token, _ := wave4Runner(t, e,
		MintInput{Name: "retire-me", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if err := e.svc.DeregisterRunner(ctx(), id, wave4Admin()); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), token); err == nil {
		t.Fatal("deregistered runner's credential still authenticates")
	}
	doc, _, _ := e.svc.loadRunner(ctx(), id)
	if doc != nil {
		t.Fatal("deregistered runner record survived")
	}
}

func TestQueuePriorityOrdersClaims(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	low, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml"})
	if err != nil {
		t.Fatalf("low: %v", err)
	}
	high, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml", Priority: 5})
	if err != nil {
		t.Fatalf("high: %v", err)
	}
	if high[0].Priority != 5 || low[0].Priority != 0 {
		t.Fatalf("priorities = %d/%d", high[0].Priority, low[0].Priority)
	}
	snap, err := e.svc.QueueSnapshotForRepo(ctx(), "o/r", e.reader())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(snap.Rows) < 2 || snap.Rows[0].Priority != 5 || snap.Rows[0].RunID != high[0].ID {
		t.Fatalf("claim order = %+v, want the priority-5 run first", snap.Rows)
	}
}

func TestCIPauseBlocksClaimsAndNamesTheReason(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", true, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "paused", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if got, ferr := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, ""); ferr != nil || got != nil {
		t.Fatalf("paused repo was claimed: %+v, %v", got, ferr)
	}
	view, err := e.svc.GetRun(ctx(), "o/r", runs[0].ID, e.reader())
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(view.Jobs) == 0 || !strings.Contains(view.Jobs[0].QueueReason, "paused") {
		t.Fatalf("queue reason = %+v", view.Jobs)
	}
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", false, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, ferr := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, ""); ferr != nil || got == nil {
		t.Fatalf("resumed repo got no job: %+v, %v", got, ferr)
	}
}

func TestScheduleOperationsAndCoalescing(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 10, 0, time.UTC)
	e, nowp := scheduleEnv(t, at)
	e.roles.roles["maint"] = "maintain"
	id := ScheduleEntryID{Path: ".spindle/workflows/ci.yml", Cron: "0 3 * * *"}
	list, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].NextFire == "" || !list.Entries[0].Valid {
		t.Fatalf("schedule list = %+v", list)
	}
	if _, err := e.svc.SetScheduleEnabled(ctx(), "o/r", id, false, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	res, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Fired != 0 || res.Disabled != 1 {
		t.Fatalf("disabled sweep = %+v", res)
	}
	// Run-now still works on a disabled entry (a manual trigger).
	runs, err := e.svc.RunScheduleNow(ctx(), "o/r", id, auth.Principal{Name: "writer"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("run-now: %d runs, %v", len(runs), err)
	}
	// Re-enable, fire day one, then jump four days: three ticks are due and the
	// sweep coalesces them into ONE run with the count reported.
	if _, err := e.svc.SetScheduleEnabled(ctx(), "o/r", id, true, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	*nowp = time.Date(2026, 11, 22, 3, 0, 5, 0, time.UTC)
	res, err = e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil || res.Fired != 1 {
		t.Fatalf("day-2 sweep = %+v, %v", res, err)
	}
	*nowp = time.Date(2026, 11, 25, 3, 0, 5, 0, time.UTC)
	list, err = e.svc.ListSchedules(ctx(), "o", "r", e.reader())
	if err != nil || len(list.Entries) != 1 {
		t.Fatalf("list before coalesced sweep: %+v, %v", list, err)
	}
	if list.Entries[0].MissedTicks < 2 {
		t.Fatalf("missed ticks = %d, want the two days the scheduler was down", list.Entries[0].MissedTicks)
	}
	res, err = e.svc.SweepSchedule(context.Background(), "o", "r", nil)
	if err != nil || res.Fired != 1 || res.Coalesced < 2 {
		t.Fatalf("coalesced sweep = %+v, %v", res, err)
	}
	// Exactly one run per sweep, never a backlog replay.
	before := len(runsOf(t, e))
	if res.Fired != 1 || before < 3 {
		t.Fatalf("runs after coalesced sweep = %d", before)
	}
}

func TestValidateWorkflowNamesKeyAndFile(t *testing.T) {
	bad := []byte("name: ci\non: [push]\njobs:\n  build:\n    bogus-key: 1\n    runs-on: linux\n    steps:\n      - run: echo hi\n")
	res := ValidateWorkflowSource(".spindle/workflows/ci.yml", bad)
	if res.OK || len(res.Issues) == 0 {
		t.Fatalf("invalid file passed: %+v", res)
	}
	issue := res.Issues[0]
	if issue.File != ".spindle/workflows/ci.yml" {
		t.Fatalf("file = %q", issue.File)
	}
	if !strings.Contains(issue.Key, "jobs.build") {
		t.Fatalf("key = %q, want the YAML path", issue.Key)
	}
	if !strings.Contains(issue.Message, "bogus-key") {
		t.Fatalf("message = %q", issue.Message)
	}
	good := []byte("name: ci\non: [push]\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo hi\n")
	if ok := ValidateWorkflowSource("ci.yml", good); !ok.OK {
		t.Fatalf("valid file rejected: %+v", ok)
	}
	ex := ExplainWorkflow("ci.yml", good)
	if !ex.Valid || len(ex.Jobs) != 1 || ex.Jobs[0].Key != "build" || len(ex.Triggers) != 1 || ex.Triggers[0] != "push" {
		t.Fatalf("explain = %+v", ex)
	}
}

func TestNotificationsNotifySubscribersOnce(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, e.reader()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "notifier", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	got, ferr := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, "")
	if ferr != nil || got == nil {
		t.Fatalf("fetch: %+v, %v", got, ferr)
	}
	start, serr := e.svc.Start(ctx(), authn, got.JobRef, got.Attempt)
	if serr != nil {
		t.Fatalf("start: %v", serr)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{Conclusion: JobFailure}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	// Twice: the deterministic key makes the second a no-op.
	e.svc.notifyRunFailure(ctx(), "o", "r", runs[0].ID)
	e.svc.notifyRunFailure(ctx(), "o", "r", runs[0].ID)
	notes, err := e.svc.ListNotifications(ctx(), e.reader(), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("notifications = %d, want one", len(notes))
	}
	if notes[0].Link != "/o/r/threads/"+runs[0].ID || notes[0].State != RunFailure {
		t.Fatalf("notification = %+v", notes[0])
	}
	// A different user is not notified.
	other, _ := e.svc.ListNotifications(ctx(), auth.Principal{Name: "someone"}, 10)
	if len(other) != 0 {
		t.Fatalf("unrelated user got %d notifications", len(other))
	}
}

func TestSizingQueueReasonAndNonGoalLabels(t *testing.T) {
	job := &Job{Spec: &Spec{Container: &ContainerSpec{Options: "--cpus 4 --memory 16GiB"}}}
	if size := JobSizeOf(job); size.CPUs != 4 || size.MemoryBytes != 16<<30 {
		t.Fatalf("size = %+v", size)
	}
	runner := &RunnerDoc{MaxCPUs: 2, Labels: []string{"linux"}, AllowAll: true}
	if RunnerOffers(runner, JobSizeOf(job)) {
		t.Fatal("runner with 2 cpu offered a 4 cpu job")
	}
	reason := runnerReason([]*RunnerDoc{runner}, &Job{Repo: "o/r", RunsOn: "linux", Spec: job.Spec}, &Run{Trusted: true})
	if !strings.Contains(reason, "4 cpu") || !strings.Contains(reason, "16 GiB") {
		t.Fatalf("sizing reason = %q", reason)
	}
	win := &Job{Repo: "o/r", RunsOn: "windows-latest"}
	reason = runnerReason([]*RunnerDoc{{Labels: []string{"linux"}, AllowAll: true}}, win, &Run{Trusted: true})
	if !strings.Contains(reason, "non-goal") {
		t.Fatalf("windows reason = %q", reason)
	}
	if err := RefuseNonGoal("lxc"); err == nil || !strings.Contains(err.Error(), "D-15") {
		t.Fatalf("refusal = %v", err)
	}
}

func TestRunPriorityDispatchBounds(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.DispatchRunWithPriority(ctx(), "o/r", "refs/heads/main", "", nil, MaxRunPriority+1, e.writer()); err == nil {
		t.Fatal("out-of-range priority accepted")
	}
}

// Ensure the queue index still round-trips a priority (the global order reads
// it directly).
func TestQueueIndexCarriesPriority(t *testing.T) {
	e := newTestEnv(t)
	at := e.svc.nowUTC().Format(dateTimeFmt)
	if err := e.svc.appendQueueIndex(ctx(), "o/r", "run1", "job1", []string{"linux"}, false, 7, at); err != nil {
		t.Fatal(err)
	}
	idx, _, err := e.svc.loadQueueIndex(ctx())
	if err != nil || idx == nil || len(idx.Queued) != 1 || idx.Queued[0].Priority != 7 {
		t.Fatalf("index = %+v, %v", idx, err)
	}
	raw, _ := json.Marshal(idx)
	var back QueueIndex
	if json.Unmarshal(raw, &back) != nil || back.Queued[0].Priority != 7 {
		t.Fatalf("priority did not survive JSON: %s", raw)
	}
}

// wave4RunnerStale re-reads a runner through the credential path (the test's
// stand-in for the next protocol request's authorizeRunner).
func wave4RunnerStale(t *testing.T, e *testEnv, id string) (string, string, *runnerAuth) {
	t.Helper()
	doc, _, err := e.svc.loadRunner(ctx(), id)
	if err != nil || doc == nil {
		t.Fatalf("loadRunner(%s): %v", id, err)
	}
	return doc.ID, "", &runnerAuth{Doc: doc}
}
