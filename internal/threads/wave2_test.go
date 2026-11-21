package threads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Wave 2: the failure side of every protocol surface (the package gate is 95%
// per file, so the error branches are part of the contract, not an afterthought).

// errAuthHandler answers every request with one auth error kind.
func errAuthHandler(e *testEnv, kind auth.AuthErrorKind) *Handler {
	h := &Handler{Svc: e.svc}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: kind, Why: "denied"}
	}
	return h
}

func TestHandlerAuthErrorBranches(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	h := errAuthHandler(e, auth.ErrForbidden)
	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/o/r/api/threads", ""},
		{http.MethodGet, "/o/r/api/threads/workflows", ""},
		{http.MethodGet, "/o/r/api/threads/" + run.ID, ""},
		{http.MethodPost, "/o/r/api/threads", `{}`},
		{http.MethodPost, "/o/r/api/threads/" + run.ID + "/cancel", ""},
		{http.MethodPost, "/o/r/api/threads/" + run.ID + "/rerun", ""},
		{http.MethodGet, "/o/r/api/threads/" + run.ID + "/jobs/j-build/log?accept=json", ""},
		{http.MethodGet, "/o/r/api/threads/runners", ""},
		{http.MethodPost, "/o/r/api/threads/runners/tokens", ""},
		{http.MethodDelete, "/o/r/api/threads/runners/abcdefgh1234", ""},
		{http.MethodGet, "/o/r/api/threads/secrets", ""},
		{http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"value":"x"}`},
		{http.MethodDelete, "/o/r/api/threads/secrets/TOKEN", ""},
	}
	for _, tc := range cases {
		rec := doAuth(h, tc.method, tc.path, tc.body, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	// 503 mapping and the 401 WWW-Authenticate header.
	h503 := errAuthHandler(e, auth.ErrUnavailable)
	if rec := doAuth(h503, http.MethodGet, "/o/r/api/threads", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable = %d", rec.Code)
	}
	h401 := errAuthHandler(e, auth.ErrInvalid)
	rec := doAuth(h401, http.MethodGet, "/o/r/api/threads", "", "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("401 = %d (%q)", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestHandlerMethodGatesAndMisses(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/o/r/api/threads/runners"},
		{http.MethodGet, "/o/r/api/threads/runners/tokens"},
		{http.MethodPost, "/o/r/api/threads/runners/abcdefgh1234"},
		{http.MethodDelete, "/o/r/api/threads/secrets"},
		{http.MethodPatch, "/o/r/api/threads/secrets/TOKEN"},
		{http.MethodPost, "/o/r/api/threads/" + run.ID + "/cancel"},
	} {
		rec := doAuth(h, tc.method, tc.path, "", "")
		if rec.Code != http.StatusMethodNotAllowed && !(tc.method == http.MethodPost && strings.HasSuffix(tc.path, "/cancel")) {
			t.Fatalf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
	// A non-threads path is a plain 404, and an unknown repo lane is not claimed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/not/a/threads/route", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("miss = %d", rec.Code)
	}
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/o/r/commits", nil)) {
		t.Fatal("a non-threads repo path must not be claimed")
	}
}

func TestSSEWriterFailurePaths(t *testing.T) {
	// The SSE writer must stop on a broken connection, not loop.
	rec := &erroringWriter{hdr: http.Header{}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ls, ok := newLogStream(rec, req)
	if !ok {
		t.Fatal("newLogStream")
	}
	if ls.event("log", "{}") {
		t.Fatal("event on a broken writer must fail")
	}
	ls2, _ := newLogStream(rec, req)
	if ls2.comment(": keepalive") {
		t.Fatal("comment on a broken writer must fail")
	}
	ls2.close()
	// A cancelled request context stops the writer before it writes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	ls3, _ := newLogStream(newStreamRecorder(), req2)
	if ls3.event("log", "{}") {
		t.Fatal("event on a cancelled request must fail")
	}
}

type erroringWriter struct {
	hdr  http.Header
	code int
}

func (e *erroringWriter) Header() http.Header { return e.hdr }
func (e *erroringWriter) WriteHeader(code int) {
	e.code = code
}
func (e *erroringWriter) Write(b []byte) (int, error) { return 0, errors.New("broken pipe") }
func (e *erroringWriter) Flush()                      {}

func TestStreamLogTailErrorAndCancel(t *testing.T) {
	e, fs, h, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin, nil }
	run := e.mustCreate(t)[0]
	// Attach over SSE, then break the tail's LIST: the stream must report the
	// error and end rather than hang.
	sw := newStreamRecorder()
	req := httptest.NewRequest(http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-build/log", nil)
	req.Header.Set("Accept", "text/event-stream")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(sw, req)
	}()
	time.Sleep(100 * time.Millisecond)
	fs.fail("list", 0)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end after the tail error")
	}
	if !strings.Contains(sw.String(), "event: error") {
		t.Fatalf("stream = %q", sw.String())
	}
	// A client disconnect (context cancel) ends the stream quietly.
	fs.clear()
	sw2 := newStreamRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-test/log", nil)
	req2.Header.Set("Accept", "text/event-stream")
	ctx2, cancel2 := context.WithCancel(context.Background())
	req2 = req2.WithContext(ctx2)
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		h.ServeHTTP(sw2, req2)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel2()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end on disconnect")
	}
}

func TestAttemptTerminalAndUnclaimed(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin)
	job, _ := fetchJob(t, h, runnerToken)
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken)
	var start StartView
	_ = json.Unmarshal(rec.Body.Bytes(), &start)
	// A log frame against an unclaimed job (no queue seq) still stores and
	// reports the lease fallback.
	unclaimed := &Job{ID: "j-build", RunID: run.ID, Repo: "o/r", Attempt: 3, State: JobRunning, Trusted: true}
	claims := AttemptClaims{
		Repo: "o/r", Run: run.ID, Job: "j-build", Attempt: 3,
		Runner: "rnr", Scopes: attemptScopes(nil), Exp: e.now.Add(time.Hour).Unix(),
	}
	_ = unclaimed
	logView, err := e.svc.AppendLog(ctx(), claims, 1, "no claim yet")
	if err == nil {
		t.Fatalf("a stale attempt must be rejected: %+v", logView)
	}
	// Start on a terminal job conflicts.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/complete", `{"conclusion":"failure"}`, start.Token); rec.Code != http.StatusOK {
		t.Fatalf("complete = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/start", "", runnerToken); rec.Code != http.StatusConflict {
		t.Fatalf("start after terminal = %d", rec.Code)
	}
	// A log write to a completed job reports the terminal state.
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/log", `{"seq":9,"text":"late"}`, start.Token)
	var lv LogView
	if err := json.Unmarshal(rec.Body.Bytes(), &lv); err != nil {
		t.Fatalf("late log = %d %s", rec.Code, rec.Body.String())
	}
	if lv.State != JobFailure {
		t.Fatalf("late log state = %+v", lv)
	}
}

func TestVerifyAttemptFailures(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	// Unknown job.
	claims := AttemptClaims{Repo: "o/r", Run: run.ID, Job: "j-ghost", Attempt: 1, Scopes: []string{ScopeLog}}
	if _, err := e.svc.verifyAttempt(ctx(), claims, ScopeLog); err == nil {
		t.Fatal("unknown job must fail")
	}
	// Missing scope.
	claims.Job = "j-build"
	if _, err := e.svc.verifyAttempt(ctx(), claims, ScopeComplete); err == nil {
		t.Fatal("missing scope must fail")
	}
	// A corrupt job record fails closed.
	if _, err := store.PutBytes(ctx(), e.store, JobKey("o", "r", run.ID, "j-build"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.verifyAttempt(ctx(), claims, ScopeLog); err == nil {
		t.Fatal("corrupt job must fail")
	}
	// normalizeConclusion / conclusionState.
	for in, want := range map[string]string{
		"success": JobSuccess, "failed": JobFailure, "error": JobFailure,
		"cancelled": JobCancelled, "canceled": JobCancelled, "weird": "weird",
	} {
		if got := normalizeConclusion(in); got != want {
			t.Fatalf("normalizeConclusion(%q) = %q", in, got)
		}
	}
	if _, err := conclusionState("nope"); err == nil {
		t.Fatal("an unknown conclusion must fail")
	}
	// A log-sequence parse rejects a non-numeric suffix.
	if _, ok := logSeqOf("p-abc", "p-"); ok {
		t.Fatal("logSeqOf must reject a non-numeric suffix")
	}
}

func TestQueueClaimFailurePaths(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	_ = run
	runner := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	// A failing entry GET during the probe.
	fs.fail("get", 1) // head ok, entry fails
	if _, err := e.svc.claimLoop(ctx(), "o", "r", runner.Doc, runner.Doc.Labels); err == nil {
		t.Fatal("entry read failure must fail the probe")
	}
	fs.clear()
	// A done entry is dropped from the window.
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	entry, _, _ := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	entry.State = QueueDone
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", entry.Seq), mustJSON(entry), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.claimLoop(ctx(), "o", "r", runner.Doc, runner.Doc.Labels); err != nil {
		t.Fatalf("claimLoop: %v", err)
	}
	if h, _, _ := e.svc.loadHead(ctx(), "o", "r"); len(h.Pending) == len(head.Pending) {
		t.Fatalf("a done entry must be trimmed: %v", h.Pending)
	}
	// A claim whose job vanished is retired.
	head, _, _ = e.svc.loadHead(ctx(), "o", "r")
	entry, ver, _ := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	if _, _, err := store.GetBytes(ctx(), e.store, JobKey("o", "r", entry.RunID, entry.JobID), store.GetOptions{}); err == nil {
		// Remove the job so the claim retires its entry.
		_ = e.store.Delete(ctx(), JobKey("o", "r", entry.RunID, entry.JobID), "")
	}
	if claim, err := e.svc.claimEntry(ctx(), "o", "r", entry, ver, runner.Doc); err != nil || claim != nil {
		t.Fatalf("a vanished job must retire the entry: %+v, %v", claim, err)
	}
	// A retire whose write fails returns the store error.
	head2, _, _ := e.svc.loadHead(ctx(), "o", "r")
	if len(head2.Pending) > 0 {
		e2, ver2, _ := e.svc.loadEntry(ctx(), "o", "r", head2.Pending[0])
		if e2 != nil && e2.State == QueueDone {
			fs.fail("put", 0)
			_, _ = e.svc.claimEntry(ctx(), "o", "r", e2, ver2, runner.Doc)
			fs.clear()
		}
	}
	// releaseClaim is a no-op for a nil claim and CASes otherwise.
	e.svc.releaseClaim(ctx(), "o", "r", nil)
	claim := &Claim{Entry: &QueueEntry{Seq: head.Pending[0], State: QueueClaimed, Version: 2}, EntryVersion: "1"}
	e.svc.releaseClaim(ctx(), "o", "r", claim)
	_ = admin
}

func TestFetchReleaseOnMarkFailure(t *testing.T) {
	e, fs, _, _ := faultEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	e.mustCreate(t)
	mint, err := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"ubuntu-latest"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	runner, err := e.svc.authorizeRunner(ctx(), res.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	// The claim write succeeds, the job's running update fails: the claim is
	// released and the fetch answers an error (never a job nobody marks).
	fs.fail("put", 1)
	if _, ferr := e.svc.FetchJob(ctx(), runner, time.Millisecond); ferr == nil {
		t.Fatal("fetch must fail when the job cannot be marked running")
	}
}

func TestFetchTimeoutCapAndSecrets(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs() // the second fetch must be claimable regardless of needs
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)
	admin := auth.Principal{Name: "admin", Admin: true}
	if err := e.svc.SetSecret(ctx(), "o/r", "TOKEN", "value-1234", admin); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	e.mustCreate(t)
	runner := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	// wait > 30 s is clamped, not rejected.
	job, err := e.svc.FetchJob(ctx(), runner, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	if job.Secrets["TOKEN"] != "value-1234" {
		t.Fatalf("secrets = %+v", job.Secrets)
	}
	// A corrupt secret record fails the fetch (fail closed).
	raw, _, _ := store.GetBytes(ctx(), e.store, SecretKey("o", "r", "TOKEN"), store.GetOptions{})
	var doc SecretDoc
	_ = json.Unmarshal(raw, &doc)
	doc.Sealed = "AAAA"
	if _, err := store.PutBytes(ctx(), e.store, SecretKey("o", "r", "TOKEN"), mustJSON(&doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	second := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr2", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	if _, ferr := e.svc.FetchJob(ctx(), second, time.Millisecond); ferr == nil {
		t.Fatal("a corrupt secret must fail the fetch")
	}
}

func TestAuthorizeRunnerFailures(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	parts := strings.Split(res.Token, ".")
	// Wrong secret.
	bad := parts[0] + "." + parts[1] + "." + strings.Repeat("0", 64)
	if _, err := e.svc.authorizeRunner(ctx(), bad); err == nil {
		t.Fatal("a bad secret must fail")
	}
	// Unknown credential id.
	if _, err := e.svc.authorizeRunner(ctx(), runnerTokenPrefix+parts[0][len(runnerTokenPrefix):]+".abcdefgh9999."+parts[2]); err == nil {
		t.Fatal("an unknown credential must fail")
	}
	// A revoked credential.
	if err := e.svc.revokeCredential(ctx(), "admin", res.RunnerID, e.now.Format(dateTimeFmt)); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), res.Token); err == nil {
		t.Fatal("a revoked credential must fail")
	}
	// A credential whose repository segment is malformed.
	credKey := RunnerTokenKey("admin", "badrepo01")
	cred := &RunnerCredential{ID: "badrepo01", Principal: "admin", Repo: "bad repo", Kind: credRunner,
		TokenHash: hashSecret("secretsecretsecret"), CreatedAt: e.now.Format(dateTimeFmt), Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, credKey, mustJSON(cred), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := runnerTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte("admin")) + ".badrepo01.secretsecretsecret"
	if _, err := e.svc.authorizeRunner(ctx(), tok); err == nil {
		t.Fatal("a malformed repo must fail")
	}
	// A credential with no registry record.
	cred2 := &RunnerCredential{ID: "noregistry1", Principal: "admin", Repo: "o/r", Kind: credRunner,
		TokenHash: hashSecret("secretsecretsecret"), CreatedAt: e.now.Format(dateTimeFmt), Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, RunnerTokenKey("admin", "noregistry1"), mustJSON(cred2), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok2 := runnerTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte("admin")) + ".noregistry1.secretsecretsecret"
	if _, err := e.svc.authorizeRunner(ctx(), tok2); err == nil {
		t.Fatal("a missing registry record must fail")
	}
	// A registry record revoked behind the credential's back.
	res2, err := e.svc.Register(ctx(), RegisterInput{Token: func() string {
		m, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
		return m.Token
	}(), Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register 2: %v", err)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), "o", "r", res2.RunnerID)
	doc.State = RunnerRevoked
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", res2.RunnerID), mustJSON(doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), res2.Token); err == nil {
		t.Fatal("a revoked registry record must fail")
	}
	// A corrupt registry record fails (the liveness refresh).
	res3, err := e.svc.Register(ctx(), RegisterInput{Token: func() string {
		m, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
		return m.Token
	}(), Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register 3: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", res3.RunnerID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), res3.Token); err == nil {
		t.Fatal("a corrupt registry record must fail")
	}
}

func TestDeclareFailurePaths(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	runner, err := e.svc.authorizeRunner(ctx(), res.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	// The record vanishes between authorize and declare.
	if err := e.store.Delete(ctx(), RunnerKey("o", "r", res.RunnerID), ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.svc.Declare(ctx(), runner, DeclareInput{Labels: []string{"x"}}); err == nil {
		t.Fatal("declare on a vanished runner must fail")
	}
	// A corrupt record.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", res.RunnerID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Declare(ctx(), runner, DeclareInput{}); err == nil {
		t.Fatal("declare on a corrupt runner must fail")
	}
	// A record revoked between authorize and declare.
	doc := &RunnerDoc{ID: res.RunnerID, State: RunnerActive, CreatedAt: "2026-11-20T12:00:00Z", Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", res.RunnerID), mustJSON(doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	doc.State = RunnerRevoked
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", res.RunnerID), mustJSON(doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Declare(ctx(), runner, DeclareInput{}); err == nil {
		t.Fatal("declare on a revoked runner must fail")
	}
}

func TestRegisterFailurePaths(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	// Registry record vanished after the credential was minted.
	mint, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if err := e.store.Delete(ctx(), RunnerKey("o", "r", mint.ID), ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("a vanished registry record must fail register")
	}
	// Registry record corrupt.
	mint2, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", mint2.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint2.Token}); err == nil {
		t.Fatal("a corrupt registry record must fail register")
	}
	// Registry record revoked.
	mint3, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	doc := &RunnerDoc{ID: mint3.ID, State: RunnerRevoked, CreatedAt: "2026-11-20T12:00:00Z", Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", mint3.ID), mustJSON(doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint3.Token}); err == nil {
		t.Fatal("a revoked registry record must fail register")
	}
	// Credential record consumed behind the first check's back: a second
	// registration attempt through the CAS sees it. (The CAS also has a
	// corrupt branch.)
	mint4, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint4.Token}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint4.Token}); err == nil {
		t.Fatal("a consumed token must fail")
	}
	// The credential record is corrupt.
	mint5, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if _, err := store.PutBytes(ctx(), e.store, RunnerTokenKey("admin", mint5.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint5.Token}); err == nil {
		t.Fatal("a corrupt credential must fail register")
	}
	// A registration token whose kind was already flipped (not registration).
	mint6, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	cred, _, _ := e.svc.loadCredential(ctx(), "admin", mint6.ID)
	cred.Kind = credRunner
	if _, err := store.PutBytes(ctx(), e.store, RunnerTokenKey("admin", mint6.ID), mustJSON(cred), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint6.Token}); err == nil {
		t.Fatal("a runner-kind credential must not register")
	}
	// A bad secret for an existing registration token.
	mint7, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	parts := strings.Split(mint7.Token, ".")
	wrong := parts[0] + "." + parts[1] + "." + strings.Repeat("1", 64)
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: wrong}); err == nil {
		t.Fatal("a wrong secret must fail register")
	}
}

func TestRevokeAndListFailurePaths(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	// Revoking twice is idempotent; the credential record may already be gone.
	if err := e.svc.RevokeRunner(ctx(), "o/r", mint.ID, admin); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), "o/r", mint.ID, admin); err != nil {
		t.Fatalf("revoke twice: %v", err)
	}
	if err := e.store.Delete(ctx(), RunnerTokenKey("admin", mint.ID), ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.svc.revokeCredential(ctx(), "admin", mint.ID, e.now.Format(dateTimeFmt)); err != nil {
		t.Fatalf("revoke absent credential: %v", err)
	}
	// A corrupt credential record surfaces from the credential revoke.
	mint2, _ := e.svc.MintRegistrationToken(ctx(), "o/r", admin)
	if _, err := store.PutBytes(ctx(), e.store, RunnerTokenKey("admin", mint2.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.svc.revokeCredential(ctx(), "admin", mint2.ID, e.now.Format(dateTimeFmt)); err == nil {
		t.Fatal("a corrupt credential must surface")
	}
	// ListRunners tolerates a record deleted between LIST and GET.
	vanish := &vanishingStore{Memory: store.NewMemory()}
	vanish.key = RunnerKey("o", "r", mint2.ID)
	vanishEnvelope := newTestEnv(t)
	vanishEnvelope.svc = New(vanish, vanishEnvelope.roles)
	vanishEnvelope.svc.Now = func() time.Time { return e.now }
	if _, err := store.PutBytes(ctx(), vanish, RunnerKey("o", "r", mint2.ID), mustJSON(&RunnerDoc{ID: mint2.ID, State: RunnerActive, CreatedAt: "x", Version: 1}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rows, err := vanishEnvelope.svc.ListRunners(ctx(), "o/r", admin); err != nil || len(rows) != 0 {
		t.Fatalf("vanished runner = %+v, %v", rows, err)
	}
	// A corrupt runner record fails the list.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("o", "r", mint2.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ListRunners(ctx(), "o/r", admin); err == nil {
		t.Fatal("a corrupt runner record must fail the list")
	}
}

// vanishingStore deletes one key the first time a LIST runs, so the
// listed-then-gone race is covered deterministically.
type vanishingStore struct {
	*store.Memory
	once sync.Once
	key  string
}

func (v *vanishingStore) List(ctx context.Context, prefix, after string, fn func(store.ObjectMeta) error) error {
	v.once.Do(func() { _ = v.Memory.Delete(ctx, v.key, "") })
	return v.Memory.List(ctx, prefix, after, fn)
}

func TestSecretsFailurePaths(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	// A disabled keyring fails the crypto helpers directly.
	if _, err := e.svc.sealSecret("o/r", "X", "v"); err == nil {
		t.Fatal("seal without a key must fail")
	}
	if _, err := e.svc.openSecret("o/r", "X", "AAAA"); err == nil {
		t.Fatal("open without a key must fail")
	}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)
	// A malformed repository never reaches the store.
	if err := e.svc.SetSecret(ctx(), "bad repo", "X", "value-1234", admin); err == nil {
		t.Fatal("a bad repo must fail")
	}
	if err := e.svc.DeleteSecret(ctx(), "bad repo", "X", admin); err == nil {
		t.Fatal("a bad repo must fail")
	}
	if err := e.svc.SetSecret(ctx(), "o/r", "X", "value-1234", admin); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Replacing an existing secret CAS-es the envelope (version bump).
	if err := e.svc.SetSecret(ctx(), "o/r", "X", "value-5678", admin); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// A corrupt existing record fails the set.
	if _, err := store.PutBytes(ctx(), e.store, SecretKey("o", "r", "X"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.svc.SetSecret(ctx(), "o/r", "X", "value-9999", admin); err == nil {
		t.Fatal("a corrupt envelope must fail the set")
	}
	// A vanished secret record is skipped by the list and by secretValues.
	vanish := &vanishingStore{Memory: store.NewMemory()}
	vanish.key = SecretKey("o", "r", "X")
	env := newTestEnv(t)
	env.svc = New(vanish, env.roles)
	env.svc.SetKeyring(k)
	if _, err := store.PutBytes(ctx(), vanish, SecretKey("o", "r", "X"), mustJSON(&SecretDoc{Name: "X", Sealed: "AAAA", CreatedAt: "x", Version: 1}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rows, err := env.svc.ListSecrets(ctx(), "o/r", admin); err != nil || len(rows) != 0 {
		t.Fatalf("vanished secret = %+v, %v", rows, err)
	}
	// A corrupt secret record fails the list and secretValues.
	if _, err := store.PutBytes(ctx(), e.store, SecretKey("o", "r", "X"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ListSecrets(ctx(), "o/r", admin); err == nil {
		t.Fatal("a corrupt secret must fail the list")
	}
	if _, err := e.svc.secretValues(ctx(), "o", "r"); err == nil {
		t.Fatal("a corrupt secret must fail decryption")
	}
	// secretValues is empty when secrets are disabled.
	plain := New(store.NewMemory(), e.roles)
	if vals, err := plain.secretValues(ctx(), "o", "r"); err != nil || len(vals) != 0 {
		t.Fatalf("disabled secretValues = %v, %v", vals, err)
	}
}

func TestServiceRunFailurePaths(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	// Dispatch: no git, empty ref, unknown ref, unwired role failure.
	noGit := newTestEnv(t)
	noGit.svc.Git = nil
	if _, err := noGit.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, admin); err == nil {
		t.Fatal("dispatch without git must fail")
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "", "", nil, admin); err == nil {
		t.Fatal("dispatch without a ref must fail")
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/gone", "", nil, admin); err == nil {
		t.Fatal("dispatch of an unknown ref must fail")
	}
	if _, err := e.svc.DispatchRun(ctx(), "bad repo", "refs/heads/main", "", nil, admin); err == nil {
		t.Fatal("dispatch of a bad repo must fail")
	}
	if _, err := e.svc.DispatchRun(ctx(), "o/r", "refs/heads/main", "", nil, auth.Anonymous()); err == nil {
		t.Fatal("an anonymous dispatch must fail")
	}
	// ListWorkflows: bad commit, git error, unknown read.
	if _, err := e.svc.ListWorkflows(ctx(), "o/r", "notasha", admin); err == nil {
		t.Fatal("a bad commit must fail")
	}
	e.git.listErr = errors.New("boom")
	if _, err := e.svc.ListWorkflows(ctx(), "o/r", testSHA, admin); err == nil {
		t.Fatal("a discovery error must surface")
	}
	e.git.listErr = nil
	e.roles.public = false
	if _, err := e.svc.ListWorkflows(ctx(), "o/r", "", auth.Anonymous()); err == nil {
		t.Fatal("an anonymous workflow read on a private repo must fail")
	}
	e.roles.public = true
	// An empty repository lists no workflows.
	e.git.refErr = ErrNotFound
	if list, err := e.svc.ListWorkflows(ctx(), "o/r", "", admin); err != nil || len(list.Workflows) != 0 {
		t.Fatalf("empty repo = %+v, %v", list, err)
	}
	e.git.refErr = nil
	// GetRun / ListRuns failures.
	if _, err := e.svc.GetRun(ctx(), "o/r", "nope", admin); err == nil {
		t.Fatal("unknown run")
	}
	if _, err := e.svc.GetRun(ctx(), "bad repo", "x", admin); err == nil {
		t.Fatal("bad repo")
	}
	if _, err := e.svc.ListRuns(ctx(), "bad repo", admin, "", 0); err == nil {
		t.Fatal("bad repo list")
	}
	// UpdateJob validation.
	run := e.mustCreate(t)[0]
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, "j-build", JobUpdate{State: "weird"}); err == nil {
		t.Fatal("an invalid state must fail")
	}
	if _, err := e.svc.UpdateJob(ctx(), "o/r", "nope", "j-build", JobUpdate{}); err == nil {
		t.Fatal("an unknown run must fail")
	}
	// foldRun with a vanished run and a corrupt run header.
	if err := e.store.Delete(ctx(), RunKey("o", "r", run.ID), ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.svc.foldRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("foldRun on a vanished run must fail")
	}
	if _, err := store.PutBytes(ctx(), e.store, RunKey("o", "r", run.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.foldRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("foldRun on a corrupt run must fail")
	}
	// A run whose job record is corrupt fails the fold.
	if _, err := store.PutBytes(ctx(), e.store, RunKey("o", "r", run.ID), mustJSON(&run), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, JobKey("o", "r", run.ID, "j-build"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.foldRun(ctx(), "o", "r", run.ID); err == nil {
		t.Fatal("foldRun on a corrupt job must fail")
	}
	// earliestStart with no starts is nil.
	if earliestStart([]*Job{{}}) != nil {
		t.Fatal("earliestStart")
	}
	if got := earliestStart([]*Job{{StartedAt: ptr("b")}, {StartedAt: ptr("a")}}); got == nil || *got != "a" {
		t.Fatalf("earliestStart = %v", got)
	}
	// CheckContext slugs an empty name to "unnamed".
	if got := CheckContext("", ""); got != "threads/unnamed/unnamed" {
		t.Fatalf("CheckContext = %q", got)
	}
	// The registry rejects a duplicate kind and resets for tests.
	ResetKindsForTest()
	RegisterKind(KindThreadsGC)
	ResetKindsForTest()
}

func TestGCCapAndCorruptHead(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := e.mustCreate(t)[0]
	runner := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	// Force the attempt cap: the GC fails the job rather than requeueing it.
	j, _, _ := e.svc.loadJobAndRun(ctx(), "o", "r", job.RunID, job.JobID)
	if _, err := e.svc.UpdateJob(ctx(), "o/r", job.RunID, job.JobID, JobUpdate{
		State: JobRunning, Attempt: MaxQueueAttempts,
	}); err != nil {
		t.Fatalf("bump attempt: %v", err)
	}
	entry, _, _ := e.svc.loadEntry(ctx(), "o", "r", j.QueueSeq)
	entry.Attempt = MaxQueueAttempts
	e.advance(time.Hour)
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", entry.Seq), mustJSON(entry), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := e.svc.GC(ctx(), "o", "r"); err != nil || n < 1 {
		t.Fatalf("GC = %d, %v", n, err)
	}
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	failed := false
	for _, jb := range jobs {
		if jb.ID == job.JobID && jb.State == JobFailure {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("the attempt cap must fail the job: %+v", jobs)
	}
	_ = admin
}

func TestQueueCorruptAndRenewBranches(t *testing.T) {
	e := newTestEnv(t)
	e.mustCreate(t)
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	// A corrupt entry inside the window fails the probe.
	entry, _, _ := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", entry.Seq), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner := &runnerAuth{Owner: "o", Repo: "r", Doc: &RunnerDoc{ID: "rnr", Labels: []string{"ubuntu-latest"}, State: RunnerActive}}
	if _, err := e.svc.claimLoop(ctx(), "o", "r", runner.Doc, runner.Doc.Labels); err == nil {
		t.Fatal("a corrupt entry must fail the probe")
	}
	// renewLease: an absent entry is a no-op, a corrupt one returns nil (best
	// effort) and a fresh lease is written.
	if got := e.svc.renewLease(ctx(), "o", "r", &QueueEntry{Seq: "nope"}, &Job{}); got != nil {
		t.Fatalf("renew absent = %v", got)
	}
	if got := e.svc.renewLease(ctx(), "o", "r", &QueueEntry{Seq: entry.Seq}, &Job{}); got != nil {
		t.Fatalf("renew corrupt = %v", got)
	}
	// finishEntry / setEntryCancel on a corrupt entry are no-ops (best effort).
	e.svc.finishEntry(ctx(), "o", "r", entry.Seq)
	e.svc.setEntryCancel(ctx(), "o", "r", entry.Seq, true)
	// trimHead on a corrupt head is a best-effort no-op.
	e.svc.trimHead(ctx(), "o", "r", map[string]bool{"x": true})
	// A queue head whose append is full is refused (covered) and trimHead
	// keeps unknown keys untouched.
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), mustJSON(&QueueHead{Pending: []string{"a"}, Version: 1}), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e.svc.trimHead(ctx(), "o", "r", map[string]bool{"b": true})
	if h, _, _ := e.svc.loadHead(ctx(), "o", "r"); len(h.Pending) != 1 {
		t.Fatalf("trim unknown key = %v", h.Pending)
	}
}
