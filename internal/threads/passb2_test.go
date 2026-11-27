package threads

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Pass B2 server-side surface: matrix expansion, concurrency groups, fail-fast
// and the checkout credential.

const matrixWorkflow = `name: Matrix CI
on: [push]
jobs:
  build:
    name: Build
    runs-on: linux
    strategy:
      matrix:
        go: ["1.26", "1.27"]
        arch: [amd64, arm64]
    steps:
      - run: go test ./...
`

func (e *testEnv) createWith(t *testing.T, path, yaml string) []*Run {
	t.Helper()
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, path, yaml)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return runs
}

func TestMatrixExpansionCreatesOneJobPerCombination(t *testing.T) {
	e := newTestEnv(t)
	run := e.createWith(t, ".spindle/workflows/m.yml", matrixWorkflow)[0]
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, e.public())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 4 {
		t.Fatalf("jobs = %d, want 4 (2×2)", len(view.Jobs))
	}
	// Axes expand in alphabetical order (arch, then go): the spec's matrix is a
	// Go map, so the YAML declaration order is not preserved. Deterministic
	// across servers is the property the job ids and check contexts need.
	wantNames := []string{
		"Build (amd64, 1.26)", "Build (amd64, 1.27)",
		"Build (arm64, 1.26)", "Build (arm64, 1.27)",
	}
	seen := map[string]bool{}
	for i, job := range view.Jobs {
		if job.Name != wantNames[i] {
			t.Fatalf("job %d name = %q, want %q", i, job.Name, wantNames[i])
		}
		if job.Spec.Matrix["go"] == "" || job.Spec.Matrix["arch"] == "" {
			t.Fatalf("cell %d = %v", i, job.Spec.Matrix)
		}
		if job.Spec.MatrixGroup != "build" {
			t.Fatalf("matrix group = %q", job.Spec.MatrixGroup)
		}
		if seen[job.ID] || seen[job.Context] {
			t.Fatalf("duplicate job identity: %s / %s", job.ID, job.Context)
		}
		seen[job.ID], seen[job.Context] = true, true
		if job.Context != CheckContext("Matrix CI", job.Name) {
			t.Fatalf("context %q", job.Context)
		}
	}
	// One checks record per cell (a required check can name any combination).
	contexts := map[string]bool{}
	for _, rep := range e.checks.reports {
		contexts[rep.Context] = true
	}
	for _, job := range view.Jobs {
		if !contexts[job.Context] {
			t.Fatalf("no check mirror for %s", job.Context)
		}
	}
}

func TestMatrixExpansionBounds(t *testing.T) {
	e := newTestEnv(t)
	var b strings.Builder
	b.WriteString("name: Big\non: [push]\njobs:\n  m:\n    runs-on: linux\n    strategy:\n      matrix:\n")
	for i := 0; i < 20; i++ {
		b.WriteString("        a" + string(rune('a'+i)) + ": [\"1\", \"2\", \"3\", \"4\", \"5\", \"6\", \"7\", \"8\"]\n")
	}
	b.WriteString("    steps:\n      - run: true\n")
	// The parser bounds the declaration, so the file is reported broken and
	// creates no run (a run record cannot be unbounded).
	if _, err := workflow.Parse([]byte(b.String())); err == nil {
		t.Fatal("expected the parser to refuse an over-large matrix")
	}
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/big.yml", b.String())
	if runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{}); err != nil || len(runs) != 0 {
		t.Fatalf("CreateRun = %d runs, %v; want none and no error", len(runs), err)
	}
}

// twoRuns creates two runs of the same workflow so they contend for a group.
func twoRuns(t *testing.T, e *testEnv, yaml string) (*Run, *Run) {
	t.Helper()
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/c.yml", yaml)
	first := e.mustCreate(t)[0]
	second := e.mustCreate(t)[0]
	if first.ID == second.ID {
		t.Fatal("run ids collided")
	}
	return first, second
}

func runnerFor(e *testEnv) *runnerAuth {
	return &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec2", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
}

const groupedWorkflow = `name: Grouped
on: [push]
concurrency: {group: shared, cancel-in-progress: false}
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

func TestConcurrencyGroupOneInFlightPerRepo(t *testing.T) {
	e := newTestEnv(t)
	first, second := twoRuns(t, e, groupedWorkflow)

	a := runnerFor(e)
	job, err := e.svc.FetchJob(ctx(), a, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("first fetch = %+v, %v", job, err)
	}
	if job.RunID != first.ID {
		t.Fatalf("first claim ran %s, want the older run %s", job.RunID, first.ID)
	}
	// The second run's job must not be admitted while the first run is live.
	b := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec3", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	if other, ferr := e.svc.FetchJob(ctx(), b, time.Millisecond); ferr != nil || other != nil {
		t.Fatalf("concurrent fetch admitted %+v (err %v)", other, ferr)
	}
	// The entry stays queued (nothing was consumed).
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	if head == nil || len(head.Pending) != 2 {
		t.Fatalf("queue head = %+v", head)
	}
	// Finish the holder: the group frees and the second run is claimable.
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	claims, err := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{Conclusion: "success"}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	got, ferr := e.svc.FetchJob(ctx(), b, time.Millisecond)
	if ferr != nil || got == nil {
		t.Fatalf("second run not claimable after release: %+v %v", got, ferr)
	}
	if got.RunID != second.ID {
		t.Fatalf("claimed %s, want %s", got.RunID, second.ID)
	}
}

const cancelInProgressWorkflow = `name: Grouped
on: [push]
concurrency: {group: shared, cancel-in-progress: true}
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo build
`

func TestConcurrencyCancelInProgressCancelsTheOlderRun(t *testing.T) {
	e := newTestEnv(t)
	first, second := twoRuns(t, e, cancelInProgressWorkflow)
	a := runnerFor(e)
	job, err := e.svc.FetchJob(ctx(), a, time.Millisecond)
	if err != nil || job == nil || job.RunID != first.ID {
		t.Fatalf("first claim = %+v, %v", job, err)
	}
	b := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec4", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	// The holder is a RUNNING job: the existing cancel path signals it
	// (cancel_requested) and the busy runner acks on its next write, which is
	// when the group frees. That hand-off is what keeps the old container from
	// being abandoned behind the operator's back.
	if got, ferr := e.svc.FetchJob(ctx(), b, time.Millisecond); ferr != nil || got != nil {
		t.Fatalf("new run admitted before the holder acked: %+v %v", got, ferr)
	}
	olderJobs, _ := e.svc.loadJobs(ctx(), "o", "r", first.ID)
	if len(olderJobs) != 1 || !olderJobs[0].CancelRequested {
		t.Fatalf("cancel-in-progress did not signal the holder: %+v", olderJobs)
	}
	// The holder's runner sees the cancel on its next write and acks.
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	view, lerr := e.svc.AppendLog(ctx(), claims, 1, LogRecord{Text: "still running"})
	if lerr != nil || !view.Cancel {
		t.Fatalf("log answer = %+v %v, want cancel=true", view, lerr)
	}
	if _, aerr := e.svc.AckCancel(ctx(), claims); aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
	got, ferr := e.svc.FetchJob(ctx(), b, time.Millisecond)
	if ferr != nil || got == nil {
		t.Fatalf("cancel-in-progress did not admit the new run after the ack: %+v %v", got, ferr)
	}
	if got.RunID != second.ID {
		t.Fatalf("admitted %s, want %s", got.RunID, second.ID)
	}
	older, err := e.svc.loadRun(ctx(), "o", "r", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !terminal(older.State) {
		t.Fatalf("older run state = %s, want terminal", older.State)
	}
	olderJobs, _ = e.svc.loadJobs(ctx(), "o", "r", first.ID)
	if len(olderJobs) != 1 || olderJobs[0].State != JobCancelled {
		t.Fatalf("older job = %+v", olderJobs)
	}
}

func TestMatrixFailFastCancelsSiblings(t *testing.T) {
	e := newTestEnv(t)
	run := e.createWith(t, ".spindle/workflows/m.yml", matrixWorkflow)[0]
	a := runnerFor(e)
	job, err := e.svc.FetchJob(ctx(), a, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := e.svc.parseAttemptToken(start.Token, e.svc.nowUTC())
	if _, cerr := e.svc.Complete(ctx(), claims, CompleteInput{Conclusion: "failure"}); cerr != nil {
		t.Fatalf("complete: %v", cerr)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed, cancelled, queued := 0, 0, 0
	for _, j := range jobs {
		switch j.State {
		case JobFailure:
			failed++
		case JobCancelled:
			cancelled++
		case JobQueued:
			queued++
		}
	}
	if failed != 1 || cancelled != 3 || queued != 0 {
		t.Fatalf("fail-fast states: failed=%d cancelled=%d queued=%d", failed, cancelled, queued)
	}
}

func TestResolveCloneCredential(t *testing.T) {
	e := newTestEnv(t)
	e.mustCreate(t)
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec2", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive, AllowForkRuns: true}}
	job, err := e.svc.FetchJob(ctx(), a, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v %v", job, err)
	}
	start, err := e.svc.Start(ctx(), a, job.JobRef, job.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	p, aerr, claimed := e.svc.ResolveCloneCredential(start.Token)
	if !claimed || aerr != nil {
		t.Fatalf("claimed=%v err=%v", claimed, aerr)
	}
	if p.Name != "ci:rnrbec2" || p.Write || p.Admin || p.Anonymous {
		t.Fatalf("principal = %+v", p)
	}
	// The pusher marker (round 3): a `ci:` name is a job principal, a human is not.
	if !IsJobPrincipal(p.Name) || IsJobPrincipal("ada") || IsJobPrincipal("") {
		t.Fatalf("IsJobPrincipal(%q) = %v", p.Name, IsJobPrincipal(p.Name))
	}
	// A token of another shape falls through (the checks hook must still see it).
	if _, _, ok := e.svc.ResolveCloneCredential("wct_abcdefgh.0123456789abcdef"); ok {
		t.Fatal("a wct_ token must not be claimed by threads")
	}
	// A malformed wjt_ token is claimed-then-rejected (a real 401).
	if _, aerr, ok := e.svc.ResolveCloneCredential("wjt_garbage.sig"); !ok || aerr == nil {
		t.Fatalf("malformed = claimed %v, err %v", ok, aerr)
	}
}

func TestFetchPayloadCarriesCheckoutURL(t *testing.T) {
	e := newTestEnv(t)
	e.mustCreate(t)
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec2", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive, AllowForkRuns: true}}
	job, err := e.svc.FetchJobFrom(ctx(), a, time.Millisecond, "https://spindle.example.com")
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v %v", job, err)
	}
	if job.Checkout == nil || job.Checkout.URL != "https://spindle.example.com/o/r.git" || !job.Checkout.ReadOnly {
		t.Fatalf("checkout = %+v", job.Checkout)
	}
	// The same canonical origin reaches the job as GITHUB_SERVER_URL (PAR-111):
	// it is the proxy-facing origin the runner reached, not a hardcoded host.
	if job.ServerURL != "https://spindle.example.com" {
		t.Fatalf("server_url = %q", job.ServerURL)
	}
}

// TestForkRunPayloadHasNoSecretsAndReadOnlyCheckout pins 15 §5.2's three fork
// guarantees that live server-side: no secrets, the base-branch workflow, and a
// read-only checkout credential.
func TestForkRunPayloadHasNoSecretsAndReadOnlyCheckout(t *testing.T) {
	e := newTestEnv(t)
	k, _ := NewKeyring(strings.Repeat("s", 32), strings.Repeat("s", 32))
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := e.svc.SetSecret(ctx(), "o/r", "DEPLOY_KEY", "very-secret-value", admin); err != nil {
		t.Fatalf("secret: %v", err)
	}
	// A fork PR runs the base branch's workflow file at baseSHA.
	e.git.add(baseSHA, ".spindle/workflows/ci.yml", `name: CI
on: [pull_request]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo from-base
`)
	e.git.refs["refs/heads/main"] = baseSHA
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Action: "opened",
		Branch: "main", BaseBranch: "main", Fork: true,
	})
	if err != nil {
		t.Fatalf("fork CreateRun: %v", err)
	}
	if len(runs) != 1 || runs[0].Trusted || runs[0].WorkflowCommit != baseSHA {
		t.Fatalf("fork run = %+v", runs)
	}
	a := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec2", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive, AllowForkRuns: true}}
	job, ferr := e.svc.FetchJobFrom(ctx(), a, time.Millisecond, "http://127.0.0.1:8080")
	if ferr != nil || job == nil {
		t.Fatalf("fetch = %+v %v", job, ferr)
	}
	if !job.SecretsDenied || len(job.Secrets) != 0 || len(job.Mask) != 0 {
		t.Fatalf("fork secrets = %+v denied=%v mask=%v", job.Secrets, job.SecretsDenied, job.Mask)
	}
	if job.Commit != testSHA || job.Spec.Steps[0].Run != "echo from-base" {
		t.Fatalf("fork payload = commit %s steps %+v", job.Commit, job.Spec.Steps)
	}
	if job.Checkout == nil || !job.Checkout.ReadOnly {
		t.Fatalf("fork checkout = %+v", job.Checkout)
	}
}

func TestClaimRunnerCredentialShape(t *testing.T) {
	p, aerr, ok := ClaimRunnerCredential("wrt_YWRtaW4.abcdefgh1234." + strings.Repeat("a", 40))
	if !ok || aerr != nil || p.Name != "ci:abcdefgh1234" || p.Write || p.Admin || p.Anonymous {
		t.Fatalf("principal = %+v err=%v claimed=%v", p, aerr, ok)
	}
	if _, _, ok := ClaimRunnerCredential("wgt_something"); ok {
		t.Fatal("a wgt_ token must not be claimed")
	}
	if _, aerr, ok := ClaimRunnerCredential("wrt_garbage"); !ok || aerr == nil {
		t.Fatalf("malformed wrt_ = claimed %v err %v", ok, aerr)
	}
}

func TestRunGroupsAndCheckoutBase(t *testing.T) {
	jobs := []*Job{
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "b"}}},
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "a"}}},
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "a"}}},
		{State: JobQueued, Spec: nil},
	}
	if got := runGroups(jobs); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("runGroups = %v", got)
	}
	// A job a job-level `if:`/`needs:` terminalised holds nothing: its group
	// must not block another run while this one is still live.
	skipped := []*Job{
		{State: JobSkipped, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "skipped"}}},
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "live"}}},
	}
	if got := runGroups(skipped); len(got) != 1 || got[0] != "live" {
		t.Fatalf("runGroups with a skipped job = %v", got)
	}
	if runGroups(nil) != nil {
		t.Fatal("no jobs, no groups")
	}
	// checkoutBase derives scheme + host; a proxy header wins.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/v1/threads/runners/fetch", nil)
	if got := checkoutBase(req); got != "http://127.0.0.1:8080" {
		t.Fatalf("checkoutBase = %q", got)
	}
	req.Header.Set("X-Forwarded-Proto", "https, http")
	if got := checkoutBase(req); got != "https://127.0.0.1:8080" {
		t.Fatalf("proxied checkoutBase = %q", got)
	}
	req.Header.Set("X-Forwarded-Proto", "gopher")
	if got := checkoutBase(req); got != "http://127.0.0.1:8080" {
		t.Fatalf("bogus proto = %q", got)
	}
	// A TLS request is https.
	tlsReq := httptest.NewRequest(http.MethodPost, "https://spindle.example.com/x", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if got := checkoutBase(tlsReq); got != "https://spindle.example.com" {
		t.Fatalf("tls checkoutBase = %q", got)
	}
	// No host → no base (the runner then has no checkout URL).
	noHost := httptest.NewRequest(http.MethodPost, "/x", nil)
	noHost.Host = ""
	if got := checkoutBase(noHost); got != "" {
		t.Fatalf("empty host = %q", got)
	}
}

func TestFailFastCancelsARunningSibling(t *testing.T) {
	e := newTestEnv(t)
	run := e.createWith(t, ".spindle/workflows/m.yml", matrixWorkflow)[0]
	a := runnerFor(e)
	first, err := e.svc.FetchJob(ctx(), a, time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first fetch: %+v %v", first, err)
	}
	b := &runnerAuth{Doc: &RunnerDoc{ID: "rnrbec5", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	second, err := e.svc.FetchJob(ctx(), b, time.Millisecond)
	if err != nil || second == nil {
		t.Fatalf("second fetch: %+v %v", second, err)
	}
	if first.JobID == second.JobID {
		t.Fatal("two fetches claimed the same job")
	}
	startB, err := e.svc.Start(ctx(), b, second.JobRef, second.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	claimsB, _ := e.svc.parseAttemptToken(startB.Token, e.svc.nowUTC())
	if _, cerr := e.svc.Complete(ctx(), claimsB, CompleteInput{Conclusion: "failure"}); cerr != nil {
		t.Fatalf("complete B: %v", cerr)
	}
	// The running cell now carries the cancel signal (its runner acks it).
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	signalled := 0
	for _, j := range jobs {
		if j.CancelRequested || j.State == JobCancelled {
			signalled++
		}
	}
	if signalled < 2 {
		t.Fatalf("fail-fast signalled %d siblings (%v)", signalled, jobs)
	}
}

func TestConcurrencyCorruptIndexIsReported(t *testing.T) {
	e := newTestEnv(t)
	e.mustCreate(t)
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "shared"), []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	spec := &Spec{Concurrency: &ConcurrencySpec{Group: "shared"}}
	if _, err := e.svc.admitConcurrency(ctx(), "o", "r", "run", spec); err == nil {
		t.Fatal("a corrupt group index must surface an error")
	}
}

func TestConcurrencyStealAndReleaseEdges(t *testing.T) {
	e := newTestEnv(t)
	runs := e.mustCreate(t)
	runID := runs[0].ID
	group := "shared"
	// A doc naming a run that is already terminal is stolen (self-healing).
	doc := &ConcurrencyDoc{Group: group, RunID: "1795235695430845790-ffffff", Since: e.now.Format(dateTimeFmt), Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", group), mustJSON(doc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	ok, _, err := e.svc.tryAcquireGroup(ctx(), "o", "r", group, runID)
	if err != nil || !ok {
		t.Fatalf("steal = %v %v", ok, err)
	}
	// Releasing with a foreign run id is a no-op; releasing the holder clears it.
	e.svc.releaseConcurrency(ctx(), "o", "r", "1795235695430845790-ffffff", []string{group})
	raw, _, _ := store.GetBytes(ctx(), e.store, ConcurrencyKey("o", "r", group), store.GetOptions{})
	var after ConcurrencyDoc
	_ = json.Unmarshal(raw, &after)
	if after.RunID != runID {
		t.Fatalf("a foreign release cleared the holder: %+v", after)
	}
	e.svc.releaseConcurrency(ctx(), "o", "r", runID, []string{group, ""})
	raw, _, _ = store.GetBytes(ctx(), e.store, ConcurrencyKey("o", "r", group), store.GetOptions{})
	_ = json.Unmarshal(raw, &after)
	if after.RunID != "" {
		t.Fatalf("the holder was not released: %+v", after)
	}
	// Renewal for a job that never recorded a queue sequence is a no-op.
	if got := e.svc.renewLeaseFor(ctx(), "o", "r", &Job{}); got != nil {
		t.Fatalf("renewLeaseFor = %v", got)
	}
	// A corrupt group record fails admission rather than silently allowing.
	if _, err := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "bad"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.admitConcurrency(ctx(), "o", "r", runID, &Spec{Concurrency: &ConcurrencySpec{Group: "bad"}}); err == nil {
		t.Fatal("corrupt group = no error")
	}
}

func TestAttemptCancelHTTPErrors(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	e.mustCreate(t)
	runnerID, runnerToken := registerRunner(t, e, h, admin, "linux")
	job, _ := fetchJob(t, h, runnerToken)
	if job == nil {
		t.Fatal("no job")
	}
	// A runner token on a cancel path is not an attempt token: a real 401.
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/cancel", "", runnerToken)
	if rec.Code != 401 {
		t.Fatalf("cancel with a runner token = %d, want 401", rec.Code)
	}
	// The attempt token acks the cancel.
	start := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken)
	var sv struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &sv); err != nil || sv.Token == "" {
		t.Fatalf("start = %d %s", start.Code, start.Body.String())
	}
	ack := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/cancel", "", sv.Token)
	if ack.Code != 200 {
		t.Fatalf("cancel ack = %d %s", ack.Code, ack.Body.String())
	}
	_ = runnerID
}
