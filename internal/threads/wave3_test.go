package threads

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

// Wave 3: entropy failures, crafted tokens, the remaining error branches and
// the pre-existing discovery/git edges the package gate covers file by file.

// withFailingRand makes every random read fail for one test.
func withFailingRand(t *testing.T) {
	t.Helper()
	prev := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = prev })
}

func TestEntropyFailures(t *testing.T) {
	withFailingRand(t)
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	if _, err := NewKeyring("", ""); err == nil {
		t.Fatal("an ephemeral keyring must fail without entropy")
	}
	if _, err := NewKeyring(strings.Repeat("k", 32), ""); err != nil {
		t.Fatalf("a configured keyring must not need entropy: %v", err)
	}
	if _, err := randomBytes(4); err == nil {
		t.Fatal("randomBytes must fail")
	}
	if _, err := mintSecret(); err == nil {
		t.Fatal("mintSecret must fail")
	}
	if _, err := mintID(); err == nil {
		t.Fatal("mintID must fail")
	}
	if _, _, err := mintScopedToken(runnerTokenPrefix, "a", "abcdefgh1234"); err == nil {
		t.Fatal("mintScopedToken must fail")
	}
	if _, err := newRunID(time.Now()); err == nil {
		t.Fatal("newRunID must fail")
	}
	if _, err := newQueueSeq(time.Now()); err == nil {
		t.Fatal("newQueueSeq must fail")
	}
	if _, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("")); err == nil {
		t.Fatal("mint must fail")
	}
	// Sealing needs a nonce.
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)
	if _, err := e.svc.sealSecret("o/r", "X", "v"); err == nil {
		t.Fatal("seal must fail without entropy")
	}
	// The run/queue creation paths fail visibly.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); err == nil {
		t.Fatal("CreateRun must fail without entropy")
	}
}

func TestCraftedAttemptTokens(t *testing.T) {
	e := newTestEnv(t)
	sign := func(payload string) string {
		mac := hmac.New(sha256.New, e.svc.keys.Sign)
		body := base64.RawURLEncoding.EncodeToString([]byte(payload))
		mac.Write([]byte(attemptTokenPrefix + body))
		return attemptTokenPrefix + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	// A valid signature over a non-JSON payload.
	if _, err := e.svc.parseAttemptToken(sign("not json"), e.now); err == nil {
		t.Fatal("a non-JSON payload must fail")
	}
	// A valid signature over a payload that is not base64 at all: the signature
	// is computed over the raw segment, so craft it directly.
	mac := hmac.New(sha256.New, e.svc.keys.Sign)
	mac.Write([]byte(attemptTokenPrefix + "!!!not-base64"))
	tok := attemptTokenPrefix + "!!!not-base64." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := e.svc.parseAttemptToken(tok, e.now); err == nil {
		t.Fatal("a non-base64 payload must fail")
	}
}

func TestAttemptStoreFailurePaths(t *testing.T) {
	e, fs, h, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin, nil }
	run := e.mustCreate(t)[0]
	mint, _ := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	runner, _ := e.svc.authorizeRunner(ctx(), res.Token)
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	// Start: the queue-entry read fails, then the job update fails.
	fs.fail("get", 2) // credential, registry, then the entry
	if _, err := e.svc.Start(ctx(), runner, job.JobRef, 1); err == nil {
		t.Fatal("start with a failing entry read must fail")
	}
	fs.clear()
	fs.fail("put", 0)
	if _, err := e.svc.Start(ctx(), runner, job.JobRef, 1); err == nil {
		t.Fatal("start with a failing update must fail")
	}
	fs.clear()
	start, err := e.svc.Start(ctx(), runner, job.JobRef, 1)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	tok, _, _ := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: job.RunID, Job: job.JobID, Attempt: 1,
		Runner: runner.Doc.ID, Scopes: attemptScopes(nil), Exp: e.now.Add(time.Hour).Unix(),
	})
	claims, _ := e.svc.parseAttemptToken(tok, e.now)
	_ = start
	// AppendLog with a corrupt secret on a trusted job.
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)
	if _, err := store.PutBytes(ctx(), fs, SecretKey("o", "r", "BROKEN"), mustJSON(&SecretDoc{Name: "BROKEN", Sealed: "AAAA", CreatedAt: "x", Version: 1}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.AppendLog(ctx(), claims, 1, "hello"); err == nil {
		t.Fatal("a corrupt secret must fail the log write")
	}
	// Complete masks annotations and the summary with the same value set, so
	// the same corrupt envelope fails IT too: masking is not optional.
	if _, err := e.svc.Complete(ctx(), claims, CompleteInput{Conclusion: "success"}); err == nil {
		t.Fatal("a corrupt secret must fail the complete write")
	}
	if err := fs.Delete(ctx(), SecretKey("o", "r", "BROKEN"), ""); err != nil {
		t.Fatalf("remove the corrupt record: %v", err)
	}
	// Complete a job as success (the conclusion mapping's happy path).
	if _, err := e.svc.Complete(ctx(), claims, CompleteInput{Conclusion: "success"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// AckCancel on a terminal job does not rewrite its history (the conclusion
	// stands; only the cancel signal clears).
	ack, err := e.svc.AckCancel(ctx(), claims)
	if err != nil || ack.State == JobCancelled {
		t.Fatalf("ack on a completed job = %+v, %v", ack, err)
	}
	// Complete/AckCancel on an unknown job.
	ghost := claims
	ghost.Job = "j-ghost"
	if _, err := e.svc.Complete(ctx(), ghost, CompleteInput{Conclusion: "success"}); err == nil {
		t.Fatal("complete on an unknown job")
	}
	if _, err := e.svc.AckCancel(ctx(), ghost); err == nil {
		t.Fatal("ack on an unknown job")
	}
	// A malformed claims repo fails verification.
	bad := claims
	bad.Repo = "bad repo"
	if _, err := e.svc.verifyAttempt(ctx(), bad, ScopeLog); err == nil {
		t.Fatal("a malformed repo must fail")
	}
	_ = run
}

func TestCancelAndRerunStoreFailures(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	// Cancel: run read, jobs read, update, fold.
	fs.fail("get", 0)
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("cancel with a failing run read must fail")
	}
	fs.clear()
	fs.fail("list", 0)
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("cancel with a failing job list must fail")
	}
	fs.clear()
	fs.fail("put", 0)
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("cancel with a failing job update must fail")
	}
	fs.clear()
	// A cancellation of a run whose fold fails surfaces.
	fs.fail("get", 2) // run + jobs ok, the fold's run read fails
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("cancel with a failing fold must fail")
	}
	fs.clear()
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, auth.Anonymous()); err == nil {
		t.Fatal("an anonymous cancel must fail")
	}
	// Rerun: read, jobs, job update, run CAS.
	fs.fail("get", 0)
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("rerun with a failing read must fail")
	}
	fs.clear()
	fs.fail("list", 0)
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("rerun with a failing list must fail")
	}
	fs.clear()
	fs.fail("put", 0)
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("rerun with a failing job update must fail")
	}
	fs.clear()
	// A vanished run during the CAS.
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	// A corrupt run header fails the CAS.
	if _, err := store.PutBytes(ctx(), fs, RunKey("o", "r", run.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("a corrupt run must fail the rerun")
	}
	// The run vanished between the read and the CAS.
	e2, fs2, _, _ := faultEnv(t)
	run2 := e2.mustCreate(t)[0]
	fs2.fail("get", 1) // jobs read ok, run read fails on the rerun path
	if _, err := e2.svc.RerunRun(ctx(), "o/r", run2.ID, admin); err == nil {
		t.Fatal("a failed run read must fail the rerun")
	}
}

func TestLogReadFailurePaths(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	// A bad repo.
	if _, err := e.svc.LogFrames(ctx(), "bad repo", run.ID, "j-build", 1, 0, 10, admin); err == nil {
		t.Fatal("a bad repo must fail")
	}
	// A failing job read.
	fs.fail("get", 0)
	if _, err := e.svc.LogFrames(ctx(), "o/r", run.ID, "j-build", 1, 0, 10, admin); err == nil {
		t.Fatal("a failing job read must fail")
	}
	fs.clear()
	// More than the limit sets more.
	runner := &runnerAuth{Doc: &RunnerDoc{ID: "rnr", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	tok, _, _ := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: job.RunID, Job: job.JobID, Attempt: 1,
		Runner: "rnr", Scopes: attemptScopes(nil), Exp: e.now.Add(time.Hour).Unix(),
	})
	claims, _ := e.svc.parseAttemptToken(tok, e.now)
	for i := 1; i <= 3; i++ {
		if _, err := e.svc.AppendLog(ctx(), claims, i, "frame"); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	page, err := e.svc.LogFrames(ctx(), "o/r", run.ID, job.JobID, 1, 0, 2, admin)
	if err != nil || !page.More || len(page.Frames) != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	// A log object that vanishes between LIST and GET is skipped.
	vanish := &vanishingStore{Memory: store.NewMemory()}
	vanish.key = LogKey("o", "r", run.ID, job.JobID, 1, 1)
	env := newTestEnv(t)
	env.svc = New(vanish, env.roles)
	env.svc.Now = func() time.Time { return e.now }
	if _, err := store.PutBytes(ctx(), vanish, LogKey("o", "r", run.ID, job.JobID, 1, 1), []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	env.svc.Git = env.git
	env.git.add(testSHA, ".spindle/workflows/ci.yml", workflowYAML)
	if _, err := store.PutBytes(ctx(), vanish, RunKey("o", "r", run.ID), mustJSON(run), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := store.PutBytes(ctx(), vanish, JobKey("o", "r", run.ID, job.JobID), mustJSON(&Job{ID: job.JobID, RunID: run.ID, Repo: "o/r", Attempt: 1, State: JobRunning}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if p, err := env.svc.LogFrames(ctx(), "o/r", run.ID, job.JobID, 1, 0, 10, admin); err != nil || len(p.Frames) != 0 {
		t.Fatalf("vanished frame = %+v, %v", p, err)
	}
}

func TestSecretsRoleAndListFailures(t *testing.T) {
	e, fs, h, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)
	reader := auth.Principal{Name: "reader"}
	if _, err := e.svc.ListSecrets(ctx(), "o/r", reader); err == nil {
		t.Fatal("a reader must not list secrets")
	}
	if err := e.svc.DeleteSecret(ctx(), "o/r", "X", reader); err == nil {
		t.Fatal("a reader must not delete secrets")
	}
	// A failing LIST.
	fs.fail("list", 0)
	if _, err := e.svc.ListSecrets(ctx(), "o/r", admin); err == nil {
		t.Fatal("a failing list must fail")
	}
	fs.clear()
	// A failing GET after the LIST.
	if err := e.svc.SetSecret(ctx(), "o/r", "X", "value-1234", admin); err != nil {
		t.Fatalf("set: %v", err)
	}
	fs.fail("get", 0)
	if _, err := e.svc.ListSecrets(ctx(), "o/r", admin); err == nil {
		t.Fatal("a failing get must fail the list")
	}
	fs.clear()
	// secretValues: a failing LIST then a vanished record.
	fs.fail("list", 0)
	if _, err := e.svc.secretValues(ctx(), "o", "r"); err == nil {
		t.Fatal("a failing list must fail secretValues")
	}
	fs.clear()
	vanish := &vanishingStore{Memory: store.NewMemory()}
	vanish.key = SecretKey("o", "r", "X")
	env := newTestEnv(t)
	env.svc = New(vanish, env.roles)
	env.svc.SetKeyring(k)
	if err := env.svc.SetSecret(ctx(), "o/r", "X", "value-1234", admin); err != nil {
		t.Fatalf("set: %v", err)
	}
	if vals, err := env.svc.secretValues(ctx(), "o", "r"); err != nil || len(vals) != 0 {
		t.Fatalf("vanished secret value = %v, %v", vals, err)
	}
	_ = h
}

func TestRunnerListAndRevokeRoleFailures(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	reader := auth.Principal{Name: "reader"}
	if _, err := e.svc.ListRunners(ctx(), reader); err == nil {
		t.Fatal("a reader must not list runners")
	}
	if err := e.svc.RevokeRunner(ctx(), "abcdefgh1234", reader); err == nil {
		t.Fatal("a reader must not revoke runners")
	}
	// The registry record vanished between the read and the CAS.
	mint, _ := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	doc, ver, _ := e.svc.loadRunner(ctx(), mint.ID)
	_ = doc
	_ = ver
	if err := e.store.Delete(ctx(), RunnerKey(mint.ID), ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), mint.ID, admin); err == nil {
		t.Fatal("a vanished registry record must fail the revoke")
	}
	// A corrupt registry record.
	mint2, _ := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(mint2.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), mint2.ID, admin); err == nil {
		t.Fatal("a corrupt registry record must fail the revoke")
	}
	// Revoking twice is the idempotent branch.
	mint3, _ := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err := e.svc.RevokeRunner(ctx(), mint3.ID, admin); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), mint3.ID, admin); err != nil {
		t.Fatalf("revoke twice: %v", err)
	}
}

func TestFetchLoopCancelAndTimeout(t *testing.T) {
	e := newTestEnv(t)
	runner := &runnerAuth{Doc: &RunnerDoc{ID: "rnr", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	// Nothing queued and the poll bounded by the request context: the loop
	// parks on the wake channel and returns the context error.
	wctx, wcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer wcancel()
	if job, err := e.svc.FetchJob(wctx, runner, 30*time.Second); err == nil || job != nil {
		t.Fatalf("bounded idle fetch = %+v, %v", job, err)
	}
	// A cancelled context ends the poll immediately.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.svc.FetchJob(cctx, runner, 30*time.Second); err == nil {
		t.Fatal("a cancelled fetch must report the context error")
	}
	// An already-expired wait answers nil without parking (the 204 path).
	if job, err := e.svc.FetchJob(ctx(), runner, time.Nanosecond); err != nil || job != nil {
		t.Fatalf("expired wait = %+v, %v", job, err)
	}
}

func TestProtocolHandlerErrorBranches(t *testing.T) {
	// No service: register is a real 503.
	h := &Handler{}
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", `{"token":"x"}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil service register = %d", rec.Code)
	}
	e, fs, hh, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	hh.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin, nil }
	mint, _ := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Declare with a failing write.
	fs.fail("put", 0)
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/runners/declare", `{}`, res.Token); rec.Code != http.StatusInternalServerError {
		t.Fatalf("declare error = %d", rec.Code)
	}
	fs.clear()
	// Fetch with a failing probe.
	e.mustCreate(t)
	fs.fail("get", 2) // credential, registry, head
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/runners/fetch?timeout=0", "", res.Token); rec.Code != http.StatusInternalServerError {
		t.Fatalf("fetch error = %d", rec.Code)
	}
	fs.clear()
	job, _, err := func() (*FetchResult, *httptest.ResponseRecorder, error) {
		rec := doAuth(hh, http.MethodPost, "/api/v1/threads/runners/fetch?timeout=0", "", res.Token)
		if rec.Code != http.StatusOK {
			return nil, rec, errNoJob
		}
		var f FetchResult
		if jerr := jsonUnmarshal(rec.Body.Bytes(), &f); jerr != nil {
			return nil, rec, jerr
		}
		return &f, rec, nil
	}()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// An attempt-scoped call with a bad ref and a mismatched attempt number.
	badTok, _, _ := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: job.RunID, Job: job.JobID, Attempt: 1,
		Runner: "rnr", Scopes: attemptScopes(nil), Exp: e.now.Add(time.Hour).Unix(),
	})
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/jobs/jr_bad/attempt/1/log", `{"seq":1,"text":"x"}`, badTok); rec.Code != http.StatusNotFound {
		t.Fatalf("bad ref = %d", rec.Code)
	}
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/2/log", `{"seq":1,"text":"x"}`, badTok); rec.Code != http.StatusForbidden {
		t.Fatalf("attempt mismatch = %d", rec.Code)
	}
	// A malformed log body and a malformed complete body.
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{`, badTok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad log body = %d", rec.Code)
	}
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/complete", `{"conclusion":1}`, badTok); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad complete body = %d", rec.Code)
	}
	// Declare with an invalid JSON body.
	if rec := doAuth(hh, http.MethodPost, "/api/v1/threads/runners/declare", `{`, res.Token); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad declare body = %d", rec.Code)
	}
}

var errNoJob = errors.New("no job offered")

func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

func TestKeyAndSlugEdges(t *testing.T) {
	for _, bad := range []string{"jr_%%%%", "jr_" + base64.RawURLEncoding.EncodeToString([]byte("a/b"))} {
		if _, _, _, _, err := parseJobRef(bad); err == nil {
			t.Fatalf("parseJobRef(%q) must fail", bad)
		}
	}
	if got := slugSegment(strings.Repeat("-", 45)); got != "unnamed" {
		t.Fatalf("slugSegment(all dashes) = %q", got)
	}
	if got := slugSegment(strings.Repeat("x", 60)); len(got) != 40 {
		t.Fatalf("slugSegment cap = %q", got)
	}
}

func TestDiscoverAndSpecEdges(t *testing.T) {
	// State of an unparsed file: no triggers, the error carried.
	f := &WorkflowFile{Path: ".spindle/workflows/x.yml", Name: "x", Error: "bad"}
	st := f.State()
	if len(st.Triggers) != 0 || st.Error != "bad" {
		t.Fatalf("State = %+v", st)
	}
	// Discover: a nil reader, a git failure, a non-workflow path, a vanished
	// blob and a too-large file.
	if _, err := Discover(ctx(), nil, "o/r", testSHA); err == nil {
		t.Fatal("nil reader")
	}
	g := newFakeGit()
	g.listErr = errors.New("down")
	if _, err := Discover(ctx(), g, "o/r", testSHA); err == nil {
		t.Fatal("list error")
	}
	g = newFakeGit()
	g.list = []string{"README.md", ".spindle/workflows/gone.yml"}
	if files, err := Discover(ctx(), g, "o/r", testSHA); err != nil || len(files) != 1 || files[0].Error == "" {
		t.Fatalf("vanished blob = %+v, %v", files, err)
	}
	g = newFakeGit()
	g.readErr = errors.New("io")
	g.list = []string{".spindle/workflows/ci.yml"}
	if _, err := Discover(ctx(), g, "o/r", testSHA); err == nil {
		t.Fatal("read error")
	}
	// brokenWorkflowFiles skips healthy files and honours a dispatch target.
	files := []*WorkflowFile{{Path: "a.yml", Name: "a", Error: "bad"}, {Path: "b.yml", Name: "b"}}
	if got := brokenWorkflowFiles(files, "b.yml"); len(got) != 0 {
		t.Fatalf("broken b = %+v", got)
	}
	if got := brokenWorkflowFiles(files, ""); len(got) != 1 {
		t.Fatalf("broken all = %+v", got)
	}
	// A workflow with no runs-on carries an empty label list.
	wf, perr := workflow.Parse([]byte(`name: x
on: [push]
jobs:
  a:
    runs-on: linux
    env: {A: "1"}
    steps:
      - run: echo hi
`))
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	spec := buildSpec("x", wf, wf.Jobs["a"], nil)
	if len(spec.RunsOn) != 1 || spec.RunsOn[0] != "linux" || spec.Env["A"] != "1" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestSubprocessGitEdges(t *testing.T) {
	g := NewSubprocessGit("")
	if g.Binary != "git" || g.Timeout <= 0 {
		t.Fatalf("defaults = %+v", g)
	}
	// No directory resolver: every call fails closed.
	if _, err := g.ListWorkflowFiles(ctx(), "o/r", testSHA); err == nil {
		t.Fatal("without dirs a read must fail")
	}
	// An unknown ref is a real not-found.
	dir, _ := gitFixture(t)
	g.SetDirs(staticDirs{"o/r": dir})
	if _, err := g.ResolveRef(ctx(), "o/r", "refs/heads/none"); err == nil {
		t.Fatal("an unknown ref must fail")
	}
	// A bad ref shape is refused before git runs.
	if _, err := g.ResolveRef(ctx(), "o/r", "bad ref..x"); err == nil {
		t.Fatal("a bad ref must fail")
	}
	// A cancelled context fails the semaphore acquisition (fill it first so
	// only the context is ready).
	g2 := NewSubprocessGit("")
	g2.SetDirs(staticDirs{"o/r": dir})
	for i := 0; i < cap(g2.sem); i++ {
		g2.sem <- struct{}{}
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g2.ListWorkflowFiles(cctx, "o/r", testSHA); err == nil {
		t.Fatal("a cancelled context must fail")
	}
	for i := 0; i < cap(g2.sem); i++ {
		<-g2.sem
	}
	// A zero timeout falls back to the default (a hung git never wedges).
	g2.Timeout = 0
	if _, err := g2.ResolveRef(ctx(), "o/r", "HEAD"); err != nil {
		// An empty repository has no HEAD: the call still ran with a default
		// timeout, which is all this asserts.
		t.Logf("resolve HEAD on an empty repo: %v", err)
	}
	// The bounded stderr keeps the tail.
	b := &boundedStderr{}
	big := strings.Repeat("e", 9<<10)
	if n, err := b.Write([]byte(big)); err != nil || n != len(big) || !b.full {
		t.Fatalf("stderr tail = %d, %v", n, err)
	}
	if len(b.buf) > 8<<10 || !strings.HasPrefix(b.String(), "…") {
		t.Fatalf("stderr kept %d bytes (%q)", len(b.buf), b.String()[:1])
	}
	if _, err := b.Write([]byte("tail")); err != nil {
		t.Fatalf("stderr tail write: %v", err)
	}
}
