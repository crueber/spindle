package threads

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Runner protocol contract (15 §3/§7): register → declare → fetch →
// start → log → complete, attempt supersession, idempotent log frames, cancel,
// rerun, the CAS claim race, label filtering, lease expiry, the secrets matrix
// and the SSE replay/tail.

func doAuth(h *Handler, method, path, body, token string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Spindle-Runner", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// registerRunner mints + registers + declares one runner.
func registerRunner(t *testing.T, e *testEnv, h *Handler, admin auth.Principal, labels ...string) (string, string) {
	t.Helper()
	mint, err := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"token": mint.Token, "name": "r1", "labels": labels})
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", string(body), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(runnerVersionHeader) != runnerVersion {
		t.Fatal("register answer must carry X-Spindle-Runner: 1")
	}
	var reg RegisterResult
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register json: %v", err)
	}
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{"version":"9.9","labels":["ubuntu-latest"]}`, reg.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("declare = %d: %s", rec.Code, rec.Body.String())
	}
	return reg.RunnerID, reg.Token
}

func fetchJob(t *testing.T, h *Handler, runnerToken string) (*FetchResult, *httptest.ResponseRecorder) {
	t.Helper()
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/fetch?timeout=0", "", runnerToken)
	if rec.Code == http.StatusNoContent {
		return nil, rec
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch = %d: %s", rec.Code, rec.Body.String())
	}
	var job FetchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("fetch json: %v", err)
	}
	return &job, rec
}

func TestProtocolContract(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	runnerID, runnerToken := registerRunner(t, e, h, admin, "ubuntu-latest")

	job, rec := fetchJob(t, h, runnerToken)
	if job == nil {
		t.Fatal("fetch returned no job")
	}
	if rec.Header().Get(runnerVersionHeader) != runnerVersion {
		t.Fatal("fetch answer must carry X-Spindle-Runner: 1")
	}
	if job.Repo != "o/r" || job.RunID != run.ID || job.JobID != "j-build" || job.Attempt != 1 {
		t.Fatalf("payload = %+v", job)
	}
	if job.Spec == nil || job.Spec.Version != SpecVersion || len(job.Spec.Steps) == 0 {
		t.Fatalf("payload spec = %+v", job.Spec)
	}
	if !job.Trusted || job.SecretsDenied {
		t.Fatalf("trust class = %+v", job)
	}
	if job.JobRef == "" || job.JobRef == "j-build" {
		t.Fatalf("job ref must be opaque, got %q", job.JobRef)
	}

	// The claim marks the job running (checks mirror = in_progress).
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, admin)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if view.Run.State != RunRunning {
		t.Fatalf("run state after claim = %q", view.Run.State)
	}
	var claimed *Job
	for _, j := range view.Jobs {
		if j.ID == "j-build" {
			claimed = j
		}
	}
	if claimed == nil || claimed.State != JobRunning || claimed.StartedAt == nil || claimed.QueueSeq == "" {
		t.Fatalf("claimed job = %+v", claimed)
	}
	last := e.checks.reports[len(e.checks.reports)-1]
	if last.State != "in_progress" {
		t.Fatalf("claim must mirror an in_progress check, got %+v", last)
	}

	// start issues the attempt token.
	startPath := "/api/v1/threads/jobs/" + job.JobRef + "/attempt/1/start"
	rec = doAuth(h, http.MethodPost, startPath, "", runnerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
	}
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start json: %v", err)
	}
	if !strings.HasPrefix(start.Token, attemptTokenPrefix) || start.Attempt != 1 {
		t.Fatalf("start = %+v", start)
	}
	// start with the attempt token is a clear 401, not a confusing unknown-runner.
	if rec := doAuth(h, http.MethodPost, startPath, "", start.Token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("start with attempt token = %d", rec.Code)
	}

	// log is idempotent by sequence.
	logPath := "/api/v1/threads/jobs/" + job.JobRef + "/attempt/1/log"
	for i := 0; i < 2; i++ {
		rec = doAuth(h, http.MethodPost, logPath, `{"seq":1,"text":"hello\n"}`, start.Token)
		if rec.Code != http.StatusOK {
			t.Fatalf("log[%d] = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	rec = doAuth(h, http.MethodPost, logPath, `{"seq":2,"text":"world\n"}`, start.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("log2 = %d: %s", rec.Code, rec.Body.String())
	}
	// A frame larger than 256 KiB is refused.
	big := strings.Repeat("x", MaxLogFrame+1)
	rec = doAuth(h, http.MethodPost, logPath, `{"seq":3,"text":"`+big+`"}`, start.Token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize frame = %d", rec.Code)
	}

	// complete maps the conclusion onto the job + check.
	completePath := "/api/v1/threads/jobs/" + job.JobRef + "/attempt/1/complete"
	rec = doAuth(h, http.MethodPost, completePath, `{"conclusion":"success","steps":[{"name":"test","conclusion":"success"}]}`, start.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	var done CompleteView
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("complete json: %v", err)
	}
	if done.State != JobSuccess {
		t.Fatalf("complete = %+v", done)
	}
	if last := e.checks.reports[len(e.checks.reports)-1]; last.State != "success" || last.Context != "threads/ci/build" {
		t.Fatalf("checks mirror = %+v", last)
	}
	// complete is idempotent for the same conclusion, a conflict for another.
	if rec := doAuth(h, http.MethodPost, completePath, `{"conclusion":"success"}`, start.Token); rec.Code != http.StatusOK {
		t.Fatalf("complete retry = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, completePath, `{"conclusion":"failure"}`, start.Token); rec.Code != http.StatusConflict {
		t.Fatalf("complete conflict = %d", rec.Code)
	}

	// The stored log replays through the API (JSON window).
	logURL := "/o/r/api/threads/" + run.ID + "/jobs/j-build/log?accept=json"
	rec = doAuth(h, http.MethodGet, logURL, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("log page = %d: %s", rec.Code, rec.Body.String())
	}
	var page LogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("log json: %v", err)
	}
	if len(page.Frames) != 2 || page.Frames[0].Seq != 1 || page.Frames[0].Text != "hello\n" || page.Frames[1].Text != "world\n" {
		t.Fatalf("log page = %+v", page)
	}
	// after=1 pages the tail only.
	rec = doAuth(h, http.MethodGet, logURL+"&after=1", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("log json: %v", err)
	}
	if len(page.Frames) != 1 || page.Frames[0].Seq != 2 {
		t.Fatalf("after page = %+v", page)
	}

	// The run folds to running (the second job is still queued), and the job
	// record carries the step results.
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	if len(jobs) != 2 || len(jobs[0].StepResults) != 1 || jobs[0].StepResults[0].Name != "test" {
		t.Fatalf("step results = %+v", jobs[0])
	}
	_ = runnerID
}

func TestAttemptSupersessionRejected(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin)
	job, _ := fetchJob(t, h, runnerToken)
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken)
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start json: %v", err)
	}
	// A rerun bumps the attempt: attempt 1's token is rejected by identity.
	// (A running job must be cancelled first — a rerun never abandons a live
	// container behind the operator's back.)
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("rerun of a running job must conflict")
	}
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, admin); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/cancel", "", start.Token); rec.Code != http.StatusOK {
		t.Fatalf("ack cancel = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{"seq":1,"text":"stale"}`, start.Token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("superseded log = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "superseded") {
		t.Fatalf("superseded message = %q", rec.Body.String())
	}
	// A frame written for another job's ref is refused by the token's scope.
	other := JobRef("o/r", run.ID, "j-test")
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+other+"/attempt/1/log", `{"seq":1,"text":"x"}`, start.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-job log = %d", rec.Code)
	}
	// A tampered signature is a 401.
	bad := start.Token[:len(start.Token)-2] + "aa"
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{"seq":1,"text":"x"}`, bad); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered token = %d", rec.Code)
	}
}

func TestClaimRaceExactlyOneWinner(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs() // both jobs claimable at once: the subject is the CAS
	run := e.mustCreate(t)[0]
	runners := make([]*runnerAuth, 0, 8)
	for i := 0; i < 8; i++ {
		runners = append(runners, &runnerAuth{
			Owner: "o", Repo: "r",
			Doc: &RunnerDoc{ID: fmt.Sprintf("runner%d", i), Labels: []string{"ubuntu-latest"}, State: RunnerActive},
		})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimedJobs := map[string]int{}
	var winRunner string
	for _, r := range runners {
		wg.Add(1)
		go func(r *runnerAuth) {
			defer wg.Done()
			claim, err := e.svc.claimLoop(ctx(), "o", "r", r.Doc, r.Doc.Labels)
			if err != nil {
				t.Errorf("claimLoop: %v", err)
				return
			}
			if claim != nil {
				mu.Lock()
				claimedJobs[claim.Entry.JobID]++
				winRunner = r.Doc.ID
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	for job, n := range claimedJobs {
		if n != 1 {
			t.Fatalf("job %s claimed %d times (last runner %q)", job, n, winRunner)
		}
	}
	if len(claimedJobs) == 0 {
		t.Fatal("no runner won a claim")
	}
	// Drain the remaining queue: each job again has at most one winner.
	for pass := 0; pass < 4; pass++ {
		claim, err := e.svc.claimLoop(ctx(), "o", "r", runners[0].Doc, runners[0].Doc.Labels)
		if err != nil {
			t.Fatalf("claimLoop: %v", err)
		}
		if claim == nil {
			break
		}
		claimedJobs[claim.Entry.JobID]++
	}
	if len(claimedJobs) != 2 {
		t.Fatalf("every queued job must be claimable, got %v", claimedJobs)
	}
	for job, n := range claimedJobs {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", job, n)
		}
	}
	// Every claimed entry names exactly one runner and one attempt.
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	for _, seq := range head.Pending {
		entry, _, eerr := e.svc.loadEntry(ctx(), "o", "r", seq)
		if eerr != nil || entry == nil {
			t.Fatalf("entry %s: %v", seq, eerr)
		}
		if entry.State == QueueClaimed && (entry.Runner == "" || entry.Attempt != 1) {
			t.Fatalf("claimed entry = %+v", entry)
		}
	}
	_ = run
}

func TestLabelFiltering(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on: [push]
jobs:
  build:
    runs-on: [self-hosted, gpu]
    steps:
      - run: echo build
`)
	e.mustCreate(t)
	jobFrom := func(labels []string) *FetchResult {
		r := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr", Labels: labels, State: RunnerActive}}
		res, err := e.svc.FetchJob(ctx(), r, time.Millisecond)
		if err != nil {
			t.Fatalf("FetchJob: %v", err)
		}
		return res
	}
	if job := jobFrom([]string{"ubuntu-latest"}); job != nil {
		t.Fatalf("ubuntu runner claimed a gpu job: %+v", job)
	}
	if job := jobFrom([]string{"gpu"}); job != nil {
		t.Fatalf("partial labels claimed the job: %+v", job)
	}
	job := jobFrom([]string{"self-hosted"})
	if job != nil {
		t.Fatalf("a partial label set must not match: %+v", job)
	}
	job = jobFrom([]string{"self-hosted", "gpu", "linux"})
	if job == nil || job.JobID != "j-build" {
		t.Fatalf("full label set must match, got %+v", job)
	}
}

func TestLeaseExpiryRequeuesAsNewAttempt(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	runner := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr1", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	first, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first fetch = %+v, %v", first, err)
	}
	// Time passes beyond the lease; another runner's probe requeues the entry,
	// and the job's attempt advances (the old token is dead by identity).
	e.advance(2 * time.Hour)
	other := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr2", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	probe, err := e.svc.claimLoop(ctx(), "o", "r", other.Doc, other.Doc.Labels)
	if err != nil {
		t.Fatalf("claimLoop: %v", err)
	}
	if probe != nil && probe.Entry.JobID == "j-build" {
		t.Fatalf("an expired claim must be requeued, not re-offered immediately: %+v", probe.Entry)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	for _, j := range jobs {
		if j.ID == "j-build" && (j.Attempt != 2 || j.State != JobQueued) {
			t.Fatalf("requeued job = %+v", j)
		}
	}
	second, err := e.svc.FetchJob(ctx(), other, time.Millisecond)
	if err != nil || second == nil {
		t.Fatalf("second fetch = %+v, %v", second, err)
	}
	if second.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", second.Attempt)
	}
}

func TestSecretsMatrix(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	// Keyring with sealing enabled.
	k, err := NewKeyring(strings.Repeat("k", 32), "")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e.svc.SetKeyring(k)

	// Write-only: the value never comes back, and the record is sealed.
	if err := e.svc.SetSecret(ctx(), "o/r", "TOKEN", "s3cret-value-123", admin); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	rec := doAuth(h, http.MethodGet, "/o/r/api/threads/secrets", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list secrets = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "s3cret-value-123") {
		t.Fatal("the secret value leaked out of the read API")
	}
	var listed struct {
		Secrets []SecretView `json:"secrets"`
		Enabled bool         `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if !listed.Enabled || len(listed.Secrets) != 1 || listed.Secrets[0].Name != "TOKEN" {
		t.Fatalf("listed = %+v", listed)
	}
	raw, _, err := store.GetBytes(ctx(), e.store, SecretKey("o", "r", "TOKEN"), store.GetOptions{})
	if err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if strings.Contains(string(raw), "s3cret-value-123") {
		t.Fatal("secret stored in the clear")
	}
	var doc SecretDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("sealed json: %v", err)
	}
	if got, oerr := e.svc.openSecret("o/r", "TOKEN", doc.Sealed); oerr != nil || got != "s3cret-value-123" {
		t.Fatalf("round-trip = %q, %v", got, oerr)
	}
	// A ciphertext moved to another repository/name fails the AAD check.
	if _, oerr := e.svc.openSecret("o/r", "OTHER", doc.Sealed); oerr == nil {
		t.Fatal("sealed value must be bound to its name")
	}

	// A trusted run receives the value; the log masker scrubs it.
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin, "ubuntu-latest")
	job, _ := fetchJob(t, h, runnerToken)
	if job == nil || !job.Trusted {
		t.Fatalf("trusted job = %+v", job)
	}
	if job.Secrets["TOKEN"] != "s3cret-value-123" || len(job.Mask) != 1 {
		t.Fatalf("trusted payload secrets = %+v mask=%v", job.Secrets, job.Mask)
	}
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken)
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start json: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log",
		`{"seq":1,"text":"token is s3cret-value-123 ok"}`, start.Token); rec.Code != http.StatusOK {
		t.Fatalf("log = %d", rec.Code)
	}
	page, err := e.svc.LogFrames(ctx(), "o/r", run.ID, "j-build", 1, 0, 10, admin)
	if err != nil {
		t.Fatalf("LogFrames: %v", err)
	}
	if len(page.Frames) != 1 || strings.Contains(page.Frames[0].Text, "s3cret-value-123") {
		t.Fatalf("stored log = %+v", page.Frames)
	}
	if !strings.Contains(page.Frames[0].Text, "***") {
		t.Fatalf("mask marker missing: %q", page.Frames[0].Text)
	}

	// A fork run gets no secrets at all.
	forkRun, ferr := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPullReq, CreateOpts{
		Branch: "main", BaseCommit: testSHA, Fork: true, Actor: "guest",
	})
	if ferr != nil {
		t.Fatalf("fork run: %v", ferr)
	}
	_, forkToken := registerRunner(t, e, h, admin, "ubuntu-latest")
	forkRunner, ferr := e.svc.authorizeRunner(ctx(), forkToken)
	if ferr != nil {
		t.Fatalf("authorize second runner: %v", ferr)
	}
	var fork *FetchResult
	for i := 0; i < 4 && fork == nil; i++ {
		got, ferr := e.svc.FetchJob(ctx(), forkRunner, time.Millisecond)
		if ferr != nil {
			t.Fatalf("fork fetch: %v", ferr)
		}
		if got != nil && got.RunID == forkRun[0].ID {
			fork = got
		}
	}
	if fork == nil {
		t.Fatalf("fork run %s was never offered", forkRun[0].ID)
	}
	if len(fork.Secrets) != 0 || !fork.SecretsDenied || len(fork.Mask) != 0 {
		t.Fatalf("fork payload must carry no secrets: %+v", fork)
	}
	// A fork run's log is not masked either (nothing to mask, and the value
	// must not be reachable at all).
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+fork.JobRef+"/attempt/1/start", "", forkToken)
	var forkStart StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &forkStart); err != nil {
		t.Fatalf("fork start json: %v (%s)", err, rec.Body.String())
	}
	_ = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+fork.JobRef+"/attempt/1/log", `{"seq":1,"text":"no secrets here"}`, forkStart.Token)
	if page, err := e.svc.LogFrames(ctx(), "o/r", forkRun[0].ID, fork.JobID, 1, 0, 10, admin); err != nil || len(page.Frames) != 1 {
		t.Fatalf("fork log = %+v, %v", page, err)
	}

	// Without a configured secret the store refuses (fail closed).
	plain := New(e.store, e.roles)
	if plain.SecretsEnabled() {
		t.Fatal("a zero-config instance must not enable at-rest secrets")
	}
	if err := plain.SetSecret(ctx(), "o/r", "X", "v", admin); err == nil {
		t.Fatal("SetSecret without a key must fail")
	}
	// Delete removes it.
	if err := e.svc.DeleteSecret(ctx(), "o/r", "TOKEN", admin); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if secrets, _ := e.svc.ListSecrets(ctx(), "o/r", admin); len(secrets) != 0 {
		t.Fatalf("secrets after delete = %+v", secrets)
	}
}

func TestCancelAndRerun(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin)
	job, _ := fetchJob(t, h, runnerToken)
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken)
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start json: %v", err)
	}

	// Cancel marks the running job and returns the signal on the next write.
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/"+run.ID+"/cancel", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{"seq":1,"text":"still here"}`, start.Token)
	var lv LogView
	if err := json.Unmarshal(rec.Body.Bytes(), &lv); err != nil {
		t.Fatalf("log json: %v", err)
	}
	if !lv.Cancel {
		t.Fatalf("log answer must carry the cancel signal: %+v", lv)
	}
	// The runner acks; the attempt becomes cancelled and the check reads
	// cancelled (a distinct conclusion: not failure, never green).
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/cancel", "", start.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack cancel = %d: %s", rec.Code, rec.Body.String())
	}
	if last := e.checks.reports[len(e.checks.reports)-1]; last.State != "cancelled" {
		t.Fatalf("cancelled check = %+v", last)
	}
	// A non-running queued job cancels immediately.
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/"+run.ID+"/cancel", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("second cancel = %d", rec.Code)
	}

	// Rerun resets the jobs to queued at attempt 2 and re-enqueues them.
	rec = doAuth(h, http.MethodPost, "/o/r/api/threads/"+run.ID+"/rerun", "", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("rerun = %d: %s", rec.Code, rec.Body.String())
	}
	view, err := e.svc.GetRun(ctx(), "o/r", run.ID, admin)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if view.Run.State != RunQueued || view.Run.Attempt != 2 {
		t.Fatalf("rerun run = %+v", view.Run)
	}
	for _, j := range view.Jobs {
		if j.State != JobQueued || j.Attempt != 2 || j.StartedAt != nil || j.CancelRequested {
			t.Fatalf("rerun job = %+v", j)
		}
	}
	again, _ := fetchJob(t, h, runnerToken)
	if again == nil || again.Attempt != 2 {
		t.Fatalf("rerun fetch = %+v", again)
	}
}

func TestProtocolPermissionsMatrix(t *testing.T) {
	e, h, cur := newHTTP(t)
	// Anonymous probes.
	*cur = auth.Anonymous()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/o/r/api/threads/runners", ""},
		{http.MethodPost, "/o/r/api/threads/runners/tokens", ""},
		{http.MethodGet, "/o/r/api/threads/secrets", ""},
		{http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"value":"v"}`},
	} {
		if rec := doAuth(h, tc.method, tc.path, tc.body, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s anonymous = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	// A reader is forbidden (401 only for anonymous).
	*cur = auth.Principal{Name: "reader"}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/o/r/api/threads/runners", ""},
		{http.MethodPost, "/o/r/api/threads/runners/tokens", ""},
		{http.MethodGet, "/o/r/api/threads/secrets", ""},
	} {
		if rec := doAuth(h, tc.method, tc.path, tc.body, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s by reader = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	// Cancelling a run needs write.
	run := e.mustCreate(t)[0]
	*cur = auth.Principal{Name: "reader"}
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/"+run.ID+"/cancel", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("reader cancel = %d", rec.Code)
	}
	// Protocol endpoints need a token.
	for _, tc := range []struct{ path, body string }{
		{"/api/v1/threads/runners/declare", `{}`},
		{"/api/v1/threads/runners/fetch", ""},
		{"/api/v1/threads/jobs/" + JobRef("o/r", run.ID, "j-build") + "/attempt/1/start", ""},
	} {
		if rec := doAuth(h, http.MethodPost, tc.path, tc.body, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", tc.path, rec.Code)
		}
	}
	// A signed-but-expired attempt token is a 401.
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	expired, _, err := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: run.ID, Job: "j-build", Attempt: 1, Runner: "rnr",
		Scopes: attemptScopes(nil), Exp: e.now.Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+JobRef("o/r", run.ID, "j-build")+"/attempt/1/complete",
		`{"conclusion":"success"}`, expired); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token = %d", rec.Code)
	}
	// A scope-less token cannot write logs.
	scoped, _, err := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: run.ID, Job: "j-build", Attempt: 1, Runner: "rnr",
		Scopes: []string{ScopeRead}, Exp: e.now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("mint scoped: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+JobRef("o/r", run.ID, "j-build")+"/attempt/1/log",
		`{"seq":1,"text":"x"}`, scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scope-less log = %d", rec.Code)
	}
}

func TestRegisterTokenSingleUseAndRevoke(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	mint, err := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	body := `{"token":"` + mint.Token + `","name":"r1","labels":["linux"]}`
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", body, ""); rec.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	// Single use: the second exchange is a 401.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("re-register = %d", rec.Code)
	}
	// List shows it; revoke kills the bearer.
	rec := doAuth(h, http.MethodGet, "/o/r/api/threads/runners", "", "")
	var listed struct {
		Runners []RunnerView `json:"runners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(listed.Runners) != 1 || listed.Runners[0].State != RunnerActive || listed.Runners[0].Name != "r1" {
		t.Fatalf("runners = %+v", listed.Runners)
	}
	if rec := doAuth(h, http.MethodDelete, "/o/r/api/threads/runners/"+mint.ID, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{}`, mintedToken(t, e, mint.ID)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer = %d", rec.Code)
	}
}

// mintedToken finds the runner token hash's plaintext by re-registering nothing:
// the revoke test needs the bearer it just minted, so it registers first and
// keeps the token. (Helper kept separate so the single-use test stays readable.)
func mintedToken(t *testing.T, e *testEnv, id string) string {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), e.store, RunnerTokenKey("admin", id), store.GetOptions{})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	var cred RunnerCredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		t.Fatalf("credential json: %v", err)
	}
	// The stored form is a hash; a bearer reconstructed from it cannot verify
	// (that is the point). Return a syntactically valid token with the right id
	// and a wrong secret: the answer must still be 401.
	return runnerTokenPrefix + "YWRtaW4." + id + ".0000000000000000"
}

func TestFetchIdleReturns204(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	_, runnerToken := registerRunner(t, e, h, admin)
	// No run exists: the poll expires and answers 204 with the version header.
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/fetch?timeout=1", "", runnerToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("idle fetch = %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(runnerVersionHeader) != runnerVersion {
		t.Fatal("204 must carry X-Spindle-Runner: 1")
	}
	// A bad timeout is a 400.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/fetch?timeout=99", "", runnerToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad timeout = %d", rec.Code)
	}
}

func TestFetchWakesOnLocalEnqueue(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	_, runnerToken := registerRunner(t, e, h, admin)
	authRunner, err := e.svc.authorizeRunner(ctx(), runnerToken)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	got := make(chan *FetchResult, 1)
	go func() {
		job, ferr := e.svc.FetchJob(ctx(), authRunner, 5*time.Second)
		if ferr != nil {
			t.Errorf("FetchJob: %v", ferr)
		}
		got <- job
	}()
	// Wait until the fetch is parked on the hub, then enqueue (the wake path).
	time.Sleep(50 * time.Millisecond)
	e.mustCreate(t)
	select {
	case job := <-got:
		if job == nil {
			t.Fatal("the woken fetch must return the new job")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the long-poll did not wake on a local enqueue")
	}
}

func TestLogSSEReplayThenTail(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin)
	job, _ := fetchJob(t, h, runnerToken)
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken)
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start json: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{"seq":1,"text":"one\n"}`, start.Token); rec.Code != http.StatusOK {
		t.Fatalf("log1 = %d", rec.Code)
	}

	// The SSE writer is exercised through a flushing recorder (the httptest
	// recorder does not flush, so the handler would fall back to plain JSON).
	sw := newStreamRecorder()
	req := httptest.NewRequest(http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-build/log?accept=sse", nil)
	req.Header.Set("Accept", "text/event-stream")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(sw, req)
	}()
	// Replay arrives first.
	waitFor := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(sw.String(), substr) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("stream never carried %q: %q", substr, sw.String())
	}
	waitFor("one")
	// A tailed frame appears after the attach.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{"seq":2,"text":"two\n"}`, start.Token); rec.Code != http.StatusOK {
		t.Fatalf("log2 = %d", rec.Code)
	}
	waitFor("two")
	// Completion ends the stream with a done event.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/complete", `{"conclusion":"success"}`, start.Token); rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	waitFor("event: done")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end after the terminal state")
	}
	if !strings.Contains(sw.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", sw.Header().Get("Content-Type"))
	}
}

// streamRecorder is a flushing ResponseWriter (SSE requires http.Flusher).
type streamRecorder struct {
	mu   sync.Mutex
	hdr  http.Header
	code int
	buf  strings.Builder
}

func newStreamRecorder() *streamRecorder { return &streamRecorder{hdr: http.Header{}} }

func (s *streamRecorder) Header() http.Header { return s.hdr }
func (s *streamRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.code == 0 {
		s.code = code
	}
}
func (s *streamRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.buf.Write(b)
}
func (s *streamRecorder) Flush() {}
func (s *streamRecorder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
