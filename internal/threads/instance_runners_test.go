package threads

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Instance-scoped runners (docs/PLAN-INSTANCE-RUNNERS.md): admin-only mint,
// single-use tokens, instance credentials, the global claim hint and its
// per-repository CAS authority.

const singleJobWorkflowYAML = `
name: Single
on: [push]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`

// mintAllPolicy is the registry-wide mint input most fixtures want: a token that
// may be offered every repository. The tenancy tests mint explicit patterns
// instead (TestRunnerPolicy*).
func mintAllPolicy(name string) MintInput {
	return MintInput{Name: name, RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}
}

// instanceEnv swaps the fixture workflow for one single-job file.
func instanceSingleJob(t *testing.T, e *testEnv) *Run {
	t.Helper()
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("create = %d runs, %v", len(runs), err)
	}
	return runs[0]
}

// instanceRunner registers and declares one runner through the service.
func instanceRunner(t *testing.T, e *testEnv, admin auth.Principal, labels []string) (string, string, *runnerAuth) {
	t.Helper()
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("build-01"))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: labels, Capacity: 2})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.RunnerID == "" || reg.Token == "" {
		t.Fatalf("register answer = %+v", reg)
	}
	// The answer must not carry a repository: instance scope.
	raw, err := json.Marshal(reg)
	if err != nil || strings.Contains(string(raw), `"repo"`) {
		t.Fatalf("register answer carries a repo: %s (%v)", raw, err)
	}
	authn, err := e.svc.authorizeRunner(ctx(), reg.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return reg.RunnerID, reg.Token, authn
}

func TestInstanceRunnerAdminGate(t *testing.T) {
	_, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	repoAdmin := auth.Principal{Name: "writer", Write: true} // repo writer, NOT host admin

	// Anonymous: 401 on every admin verb.
	*cur = auth.Anonymous()
	if rec := doAuth(h, http.MethodGet, "/api/v1/admin/runners", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/admin/runners/tokens", `{"name":"x"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous mint = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodDelete, "/api/v1/admin/runners/abcdefgh1234", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous revoke = %d", rec.Code)
	}
	// A repo admin is not a host admin: 403.
	*cur = repoAdmin
	if rec := doAuth(h, http.MethodGet, "/api/v1/admin/runners", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("repo-admin list = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/admin/runners/tokens", `{}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("repo-admin mint = %d", rec.Code)
	}
	// A host admin mints, lists and revokes. The mint body carries the
	// ADMIN-set repository policy: there is no policy-less token.
	*cur = admin
	rec := doAuth(h, http.MethodPost, "/api/v1/admin/runners/tokens", `{"name":"build-01","allow_all":true}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d: %s", rec.Code, rec.Body.String())
	}
	var minted RegistrationToken
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil || !strings.HasPrefix(minted.Token, "wrg_") {
		t.Fatalf("mint answer = %+v (%v)", minted, err)
	}
	if rec := doAuth(h, http.MethodGet, "/api/v1/admin/runners", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	// Both JSON lanes answer the admin fleet.
	if rec := doAuth(h, http.MethodGet, "/api-browser/v1/admin/runners", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("browser-lane list = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodDelete, "/api/v1/admin/runners/"+minted.ID, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	// The deleted repo lane is a plain 404 (no alias).
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/runners", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("old repo runner lane = %d, want 404", rec.Code)
	}
}

func TestInstanceRunnerTokenSingleUseAndTTL(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("a consumed registration token must fail")
	}
	// A fresh token expires after the TTL.
	mint2, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint2: %v", err)
	}
	e.advance(RegistrationTTL + time.Minute)
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint2.Token}); err == nil {
		t.Fatal("an expired registration token must fail")
	}
}

func TestInstanceRunnerDeclareLabelsCapacity(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	_, _, runner := instanceRunner(t, e, admin, []string{"linux", "docker"})
	view, err := e.svc.Declare(ctx(), runner, DeclareInput{Version: "9.9", Labels: []string{"linux", "gpu"}, Capacity: 3})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if view.Capacity != 3 || len(view.Labels) != 2 || view.State != RunnerActive {
		t.Fatalf("declare = %+v", view)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), runner.Doc.ID)
	if doc.DeclaredVersion != "9.9" || doc.Capacity != 3 {
		t.Fatalf("registry = %+v", doc)
	}
}

func TestInstanceRunnerClaimsAcrossRepositories(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	// Two repositories, one queued job each. o/r is enqueued first.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"}); err != nil {
		t.Fatalf("create o/r: %v", err)
	}
	if _, err := e.svc.CreateRun(ctx(), "o/s", testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"}); err != nil {
		t.Fatalf("create o/s: %v", err)
	}
	idx, _, err := e.svc.loadQueueIndex(ctx())
	if err != nil || idx == nil || len(idx.Queued) != 2 {
		t.Fatalf("index = %+v, %v", idx, err)
	}
	_, _, runner := instanceRunner(t, e, admin, []string{"linux"})
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil || job.Repo != "o/r" {
		t.Fatalf("first global claim = %+v, %v", job, err)
	}
	// The claimed row is gone from the hint.
	idx, _, _ = e.svc.loadQueueIndex(ctx())
	if idx == nil || len(idx.Queued) != 1 || idx.Queued[0].Repo != "o/s" {
		t.Fatalf("index after claim = %+v", idx)
	}
	job2, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job2 == nil || job2.Repo != "o/s" {
		t.Fatalf("second global claim = %+v, %v", job2, err)
	}
	// No matching label: the poll expires with 204 (nil result).
	_, _, other := instanceRunner(t, e, admin, []string{"windows"})
	if job3, err := e.svc.FetchJob(ctx(), other, time.Millisecond); err != nil || job3 != nil {
		t.Fatalf("non-matching label = %+v, %v", job3, err)
	}
}

func TestInstanceRunnerRaceOneJob(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := instanceSingleJob(t, e)
	_, _, a := instanceRunner(t, e, admin, []string{"linux"})
	// A second runner record for the same job (registered normally).
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	b, err := e.svc.authorizeRunner(ctx(), reg.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var wg sync.WaitGroup
	claims := make([]*Claim, 2)
	for i, runner := range []*runnerAuth{a, b} {
		wg.Add(1)
		go func(i int, runner *runnerAuth) {
			defer wg.Done()
			claim, cerr := e.svc.claimLoopSkip(ctx(), "o", "r", runner.Doc, runner.Doc.Labels, nil)
			if cerr != nil {
				t.Errorf("claim %d: %v", i, cerr)
			}
			claims[i] = claim
		}(i, runner)
	}
	wg.Wait()
	winners := 0
	for _, c := range claims {
		if c != nil {
			winners++
			if c.Entry.RunID != run.ID {
				t.Fatalf("claimed another run: %+v", c.Entry)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (the entry CAS is the authority)", winners)
	}
}

func TestInstanceRunnerRevokeStopsClaims(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	id, token, _ := instanceRunner(t, e, admin, []string{"linux"})
	if err := e.svc.RevokeRunner(ctx(), id, admin); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), token); err == nil {
		t.Fatal("a revoked runner must not authenticate")
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/fetch?timeout=0", "", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked fetch = %d", rec.Code)
	}
	rows, err := e.svc.ListRunners(ctx(), admin)
	if err != nil || len(rows) != 1 || rows[0].State != RunnerRevoked {
		t.Fatalf("fleet after revoke = %+v, %v", rows, err)
	}
}

func TestInstanceRunnerCurrentJobAndHeartbeat(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := instanceSingleJob(t, e)
	id, token, runner := instanceRunner(t, e, admin, []string{"linux"})
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	rows, _ := e.svc.ListRunners(ctx(), admin)
	if len(rows) != 1 || rows[0].CurrentJob == nil || rows[0].CurrentJob.RunID != run.ID || rows[0].LastSeenAt == nil {
		t.Fatalf("current job = %+v", rows)
	}
	// A terminal transition clears the display state.
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, "j-build", JobUpdate{State: JobSuccess, Runner: id}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	rows, _ = e.svc.ListRunners(ctx(), admin)
	if len(rows) != 1 || rows[0].CurrentJob != nil {
		t.Fatalf("current job after terminal = %+v", rows[0])
	}
	// The bearer's last_seen is refreshed (the heartbeat).
	e.advance(2 * time.Minute)
	if _, err := e.svc.authorizeRunner(ctx(), token); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), id)
	if doc.LastSeenAt == nil || e.svc.olderThan(*doc.LastSeenAt, time.Minute) {
		t.Fatalf("last_seen not refreshed: %+v", doc.LastSeenAt)
	}
}

func TestQueueIndexBoundAndReconcile(t *testing.T) {
	e := newTestEnv(t)
	// The bound refuses an append instead of growing the object.
	full := &QueueIndex{Version: 1}
	for i := 0; i < queueIndexMax; i++ {
		full.Queued = append(full.Queued, QueueIndexEntry{Repo: "x/y", RunID: "r", JobID: "j", Labels: []string{"linux"}})
	}
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), mustJSON(full), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.svc.appendQueueIndex(ctx(), "o/r", "r1", "j-build", []string{"linux"}, false, "now"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("full index append = %v, want ErrUnavailable", err)
	}
	// Reconcile rebuilds the repository's rows from authoritative queue entries.
	e.store = store.NewMemory()
	e.svc.Store = e.store
	instanceSingleJob(t, e)
	if err := e.store.Delete(ctx(), QueueIndexKey(), ""); err != nil {
		t.Fatalf("delete index: %v", err)
	}
	if idx, _, _ := e.svc.loadQueueIndex(ctx()); idx != nil {
		t.Fatalf("index should be absent after the delete: %+v", idx)
	}
	n, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r")
	if err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	idx, _, _ := e.svc.loadQueueIndex(ctx())
	if idx == nil || len(idx.Queued) != 1 || idx.Queued[0].Repo != "o/r" || idx.Queued[0].JobID != "j-build" {
		t.Fatalf("reconciled index = %+v", idx)
	}
	// Idempotent: a second pass changes nothing.
	if n, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("second reconcile = %d, %v", n, err)
	}
}

func TestInstanceRunnerAuditRecords(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	var mu sync.Mutex
	var calls []string
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, actor+" "+action+" "+target)
		return nil
	}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), mint.ID, admin); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !strings.Contains(calls[0], AuditRunnerTokenMint) || !strings.Contains(calls[1], AuditRunnerRevoke) {
		t.Fatalf("audit calls = %v", calls)
	}
}
