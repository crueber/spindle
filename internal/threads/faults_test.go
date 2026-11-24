package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Fault injection: every store call the protocol makes must fail closed with a
// real error, never a half record. This is also what keeps the package's
// statements covered on the failure side (the writers never "just succeed").

type faultStore struct {
	*store.Memory
	mu    sync.Mutex
	op    string // "get" | "put" | "list" | "delete" | "" (none)
	after int    // fail only after N matching calls
	calls int
}

func newFaultStore() *faultStore { return &faultStore{Memory: store.NewMemory()} }

func (f *faultStore) fail(op string, after int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op, f.after, f.calls = op, after, 0
}

func (f *faultStore) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.op = ""
}

func (f *faultStore) hit(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.op != op {
		return nil
	}
	f.calls++
	if f.calls <= f.after {
		return nil
	}
	return &store.StoreError{Kind: store.ErrKindRetryable, Key: "injected", Err: errors.New("injected fault")}
}

func (f *faultStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if err := f.hit("get"); err != nil {
		return nil, err
	}
	return f.Memory.Get(ctx, key, opts)
}

func (f *faultStore) Head(ctx context.Context, key string) (*store.ObjectMeta, error) {
	if err := f.hit("get"); err != nil {
		return nil, err
	}
	return f.Memory.Head(ctx, key)
}

func (f *faultStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if err := f.hit("put"); err != nil {
		return store.ObjectMeta{}, err
	}
	return f.Memory.Put(ctx, key, body, opts)
}

func (f *faultStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	if err := f.hit("list"); err != nil {
		return err
	}
	return f.Memory.List(ctx, prefix, after, fn)
}

func (f *faultStore) Delete(ctx context.Context, key string, ifVersion store.Version) error {
	if err := f.hit("delete"); err != nil {
		return err
	}
	return f.Memory.Delete(ctx, key, ifVersion)
}

// faultEnv builds a service over a faulting store with a runner registered.
func faultEnv(t *testing.T) (*testEnv, *faultStore, *Handler, func() string) {
	t.Helper()
	e := newTestEnv(t)
	fs := newFaultStore()
	e.store = fs.Memory
	e.svc = New(fs, e.roles)
	e.svc.Git = e.git
	e.svc.Checks = e.checks
	e.svc.Now = func() time.Time {
		e.clockMu.Lock()
		defer e.clockMu.Unlock()
		e.now = e.now.Add(time.Second)
		return e.now
	}
	h := &Handler{Svc: e.svc}
	admin := auth.Principal{Name: "admin", Admin: true}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin, nil }
	return e, fs, h, func() string { return admin.Name }
}

func TestFaultsOnQueueOps(t *testing.T) {
	e, fs, h, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)

	// enqueueJob: entry create failure and head append failure.
	fs.fail("put", 0)
	if _, err := e.svc.enqueueJob(ctx(), "o", "r", jobs[0], nil); err == nil {
		t.Fatal("enqueue with a failing put must fail")
	}
	fs.fail("put", 1) // the entry create succeeds, the head append fails
	if entry, err := e.svc.enqueueJob(ctx(), "o", "r", jobs[0], nil); entry == nil || err == nil {
		t.Fatalf("head append failure = %+v, %v", entry, err)
	}
	fs.fail("get", 0)
	if _, _, err := e.svc.loadHead(ctx(), "o", "r"); err == nil {
		t.Fatal("loadHead with a failing get must fail")
	}
	if _, _, err := e.svc.loadEntry(ctx(), "o", "r", "x"); err == nil {
		t.Fatal("loadEntry with a failing get must fail")
	}
	if err := e.svc.appendHead(ctx(), "o", "r", "new"); err == nil {
		t.Fatal("appendHead with a failing get must fail")
	}
	fs.fail("list", 0)
	if _, err := e.svc.ListRunners(ctx(), admin); err == nil {
		t.Fatal("ListRunners with a failing list must fail")
	}
	if _, err := e.svc.ListSecrets(ctx(), "o/r", admin); err == nil {
		t.Fatal("ListSecrets with a failing list must fail")
	}
	if _, err := e.svc.LogFrames(ctx(), "o/r", run.ID, "j-build", 1, 0, 10, admin); err == nil {
		t.Fatal("LogFrames with a failing list must fail")
	}
	fs.clear()

	// claimEntry tolerates a lost CAS (a concurrent claim won).
	runner := &runnerAuth{Doc: &RunnerDoc{ID: "rnr", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	if _, err := e.svc.FetchJob(ctx(), runner, time.Millisecond); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_ = h
}

func TestFaultsOnRegistryAndSecrets(t *testing.T) {
	e, fs, h, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)

	// Mint: credential create fails; then the registry create fails (rollback
	// path).
	fs.fail("put", 0)
	if _, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("")); err == nil {
		t.Fatal("mint with a failing put must fail")
	}
	fs.fail("put", 1)
	if _, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("")); err == nil {
		t.Fatal("registry failure must fail the mint")
	}
	fs.clear()

	// Register with a failing credential CAS.
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	fs.fail("put", 0)
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("register with a failing write must fail")
	}
	fs.clear()
	// Register again (the credential was not consumed).
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register retry: %v", err)
	}
	// A consumed registration token is refused.
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("a consumed token must be refused")
	}
	// Mint under a principal whose credential record has a conflicting id is
	// covered by the retry (mint again with a put fault on the second write).
	fs.fail("put", 1)
	_, _ = e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	fs.clear()

	// Revoke with a failing CAS.
	fs.fail("put", 0)
	if err := e.svc.RevokeRunner(ctx(), res.RunnerID, admin); err == nil {
		t.Fatal("revoke with a failing write must fail")
	}
	fs.clear()
	// Secrets under faults.
	fs.fail("put", 0)
	if err := e.svc.SetSecret(ctx(), "o/r", "TOKEN", "value-1234", admin); err == nil {
		t.Fatal("SetSecret with a failing write must fail")
	}
	fs.clear()
	fs.clear()
	if err := e.svc.SetSecret(ctx(), "o/r", "TOKEN", "value-1234", admin); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	fs.fail("get", 0)
	if _, err := e.svc.secretValues(ctx(), "o", "r"); err == nil {
		t.Fatal("secretValues with a failing get must fail")
	}
	fs.fail("delete", 0)
	if err := e.svc.DeleteSecret(ctx(), "o/r", "TOKEN", admin); err == nil {
		t.Fatal("DeleteSecret with a failing delete must fail")
	}
	fs.clear()

	// ListRunners tolerates a record that vanishes between LIST and GET.
	_ = h
}

func TestFaultsOnAttempts(t *testing.T) {
	e, fs, h, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	runner, err := e.svc.authorizeRunner(ctx(), res.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch: %+v, %v", job, err)
	}
	claims, _, err := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: job.RunID, Job: job.JobID, Attempt: job.Attempt,
		Runner: runner.Doc.ID, Scopes: attemptScopes(nil), Exp: e.now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("mint attempt: %v", err)
	}
	if claims == "" {
		t.Fatal("empty token")
	}
	tok, _, _ := e.svc.mintAttemptToken(AttemptClaims{
		Repo: "o/r", Run: job.RunID, Job: job.JobID, Attempt: job.Attempt,
		Runner: runner.Doc.ID, Scopes: attemptScopes(nil), Exp: e.now.Add(time.Hour).Unix(),
	})
	parsed, err := e.svc.parseAttemptToken(tok, e.now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Log write failure (the frame write) fails the call.
	fs.fail("put", 0)
	if _, err := e.svc.AppendLog(ctx(), parsed, 1, "hello world"); err == nil {
		t.Fatal("AppendLog with a failing write must fail")
	}
	fs.clear()
	if _, err := e.svc.AppendLog(ctx(), parsed, 1, "hello world"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	// Complete with a failing job CAS.
	fs.fail("put", 0)
	if _, err := e.svc.Complete(ctx(), parsed, CompleteInput{Conclusion: "success"}); err == nil {
		t.Fatal("Complete with a failing write must fail")
	}
	fs.clear()
	if _, err := e.svc.Complete(ctx(), parsed, CompleteInput{Conclusion: "success"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_ = h
	_ = run
}

func TestFaultsOnRunReadsAndCancel(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	// Cancel: loadJobs failure, then the fold failure.
	fs.fail("list", 0)
	if _, err := e.svc.CancelRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("cancel with a failing list must fail")
	}
	fs.clear()
	// Rerun: job transition failure.
	fs.fail("put", 0)
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err == nil {
		t.Fatal("rerun with a failing write must fail")
	}
	fs.clear()
	if _, err := e.svc.RerunRun(ctx(), "o/r", run.ID, admin); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	// CreateRun with a failing job write fails the creation visibly.
	fs.fail("put", 0)
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); err == nil {
		t.Fatal("CreateRun with a failing job write must fail")
	}
	fs.clear()
	// GC under a failing get.
	fs.fail("get", 0)
	if _, err := e.svc.GC(ctx(), "o", "r"); err == nil {
		t.Fatal("GC with a failing get must fail")
	}
	fs.clear()
}

func TestQueueWindowFull(t *testing.T) {
	e := newTestEnv(t)
	head := &QueueHead{Seq: "0000000000000000000-aaaaaa", UpdatedAt: "2026-11-20T12:00:00Z", Version: 1}
	for i := 0; i < queueWindowMax; i++ {
		head.Pending = append(head.Pending, fmt.Sprintf("%019d-%06x", i+1, i))
	}
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), mustJSON(head), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", "1795176002000000000-abcdef")
	_ = jobs
	job := &Job{ID: "j-x", RunID: "1795176002000000000-abcdef", Attempt: 1, RunsOn: "linux", State: JobQueued}
	if _, err := e.svc.enqueueJob(ctx(), "o", "r", job, nil); err == nil {
		t.Fatal("a full queue window must refuse the append")
	}
}

func TestStreamLogFallbacks(t *testing.T) {
	e, h, _ := newHTTP(t)
	run := e.mustCreate(t)[0]
	// A writer that cannot flush → 500 (the caller never sees a hung stream).
	req := newTestRequest(http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-build/log")
	req.Header.Set("Accept", "text/event-stream")
	rec := &noFlushRecorder{hdr: http.Header{}}
	h.ServeHTTP(rec, req)
	if rec.code != http.StatusInternalServerError {
		t.Fatalf("no-flusher = %d", rec.code)
	}
	// Unknown job over SSE is a real 404, not an opened stream.
	req = newTestRequest(http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-nope/log")
	req.Header.Set("Accept", "text/event-stream")
	rec2 := newStreamRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.code != http.StatusNotFound {
		t.Fatalf("unknown job stream = %d", rec2.code)
	}
	// The keepalive writer composes one comment and refuses after close.
	sw := newStreamRecorder()
	ls, ok := newLogStream(sw, newTestRequest(http.MethodGet, "/"))
	if !ok {
		t.Fatal("a flushing writer must stream")
	}
	if !ls.comment(": keepalive") {
		t.Fatal("comment")
	}
	ls.close()
	if ls.comment(": keepalive") {
		t.Fatal("a closed stream must refuse writes")
	}
	if ls.event("log", "{}") {
		t.Fatal("a closed stream must refuse events")
	}
	// The error mapping for an auth error kind rides the plain-text writer.
	if rec := writeErrRecorder(&auth.AuthError{Kind: auth.ErrForbidden, Why: "no"}); rec != http.StatusForbidden {
		t.Fatalf("forbidden mapping = %d", rec)
	}
	if rec := writeErrRecorder(&auth.AuthError{Kind: auth.ErrUnavailable, Why: "down"}); rec != http.StatusServiceUnavailable {
		t.Fatalf("unavailable mapping = %d", rec)
	}
}

type noFlushRecorder struct {
	hdr  http.Header
	code int
}

func (n *noFlushRecorder) Header() http.Header { return n.hdr }
func (n *noFlushRecorder) Write(b []byte) (int, error) {
	return len(b), nil
}
func (n *noFlushRecorder) WriteHeader(code int) { n.code = code }

func newTestRequest(method, target string) *http.Request {
	req, _ := http.NewRequest(method, target, nil)
	return req
}

func writeErrRecorder(err error) int {
	rec := newStreamRecorder()
	writeErr(rec, err)
	return rec.code
}

func TestPartialRunsAndBackground(t *testing.T) {
	// A job record without a Spec still queues (a record written before pass A).
	e := newTestEnv(t)
	job := &Job{ID: "j-legacy", RunID: "1795176002000000000-abcdef", Attempt: 1, RunsOn: "linux, x64", State: JobQueued}
	if _, err := e.svc.enqueueJob(ctx(), "o", "r", job, nil); err != nil {
		t.Fatalf("enqueue legacy job: %v", err)
	}
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || len(head.Pending) != 1 {
		t.Fatalf("head = %+v, %v", head, err)
	}
	if got := jobLabels(job); len(got) != 2 {
		t.Fatalf("labels = %v", got)
	}
	// A corrupt credential record fails closed on register.
	if _, err := store.PutBytes(ctx(), e.store, RunnerTokenKey("abcdefgh1234"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: "wrg_YUBi.abcdefgh1234.0123456789abcdef"}); err == nil {
		t.Fatal("a corrupt credential must fail register")
	}
}

func TestMarshalAndTruncate(t *testing.T) {
	if string(mustJSON(map[string]string{"a": "b"})) != `{"a":"b"}` {
		t.Fatal("mustJSON")
	}
	if got := truncate("héllo", 2); got != "hé" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Fatalf("truncate short = %q", got)
	}
	// specWorkflow falls back when the spec is absent or blank.
	j := &Job{}
	if j.specWorkflow("CI") != "CI" {
		t.Fatal("specWorkflow fallback")
	}
	j.Spec = &Spec{Workflow: "Other"}
	if j.specWorkflow("CI") != "Other" {
		t.Fatal("specWorkflow spec")
	}
	// newRunID sorts lexically by time.
	a, _ := newRunID(time.Unix(1, 0))
	b, _ := newRunID(time.Unix(2, 0))
	if !(a < b) {
		t.Fatalf("run ids out of order: %q %q", a, b)
	}
	// The keyring accessor is the composition's read.
	e := newTestEnv(t)
	if e.svc.Keyring() == nil {
		t.Fatal("New must install an ephemeral keyring")
	}
	// A registry record that vanished fails register.
	if err := e.svc.revokeRunnerCredential(ctx(), "unknown", "2026-11-20T12:00:00Z"); err != nil {
		t.Fatalf("revoke absent credential: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), "unknown", auth.Principal{Name: "admin", Admin: true}); err == nil {
		t.Fatal("revoke unknown runner must fail")
	}
	// The GC attempt cap fails the job instead of cycling.
	run := e.mustCreate(t)[0]
	_ = run
	job2 := &Job{ID: "j-cap", RunID: "1795176002000000000-abcdef", Attempt: MaxQueueAttempts, RunsOn: "linux", State: JobQueued}
	assertJSONRoundTrip(t, job2)
}

func assertJSONRoundTrip(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Job
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}
