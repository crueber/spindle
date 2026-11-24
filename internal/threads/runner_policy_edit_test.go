package threads

// Editing a registered runner's repository policy in place
// (docs/threads.md §8.2/§8.7, docs/PLAN-INSTANCE-RUNNERS.md §4/§5): the audited
// SetRunnerPolicy write, its CAS and validation, the two admin lanes, and the
// claim the edit changes — without disturbing a job already running.

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

// seedRunnerDoc writes one registry record directly (the pre-tenancy / mid-job
// shapes a mint cannot produce any more).
func seedRunnerDoc(t *testing.T, e *testEnv, doc RunnerDoc) {
	t.Helper()
	if doc.Version == 0 {
		doc.Version = 1
	}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(doc.ID), mustJSON(&doc), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed runner %s: %v", doc.ID, err)
	}
}

// TestSetRunnerPolicyRewritesOnlyThePolicy: the write replaces allow_all/repos,
// normalises them, bumps the version and preserves EVERY other field — the
// current job and its lease included. Both an active runner and a pending one
// (minted, not yet registered) are editable.
func TestSetRunnerPolicyRewritesOnlyThePolicy(t *testing.T) {
	e := newTestEnv(t)
	job := &RunnerJobRef{Repo: "o/r", RunID: "1-run", JobID: "j-build"}
	lease := "2026-11-20T13:00:00Z"
	seedRunnerDoc(t, e, RunnerDoc{
		ID: "active0001", Name: "build-01", Labels: []string{"docker", "linux"}, Capacity: 3,
		State: RunnerActive, DeclaredVersion: "1.4.0", RegisteredBy: "admin",
		CreatedAt: "2026-11-01T00:00:00Z", RegisteredAt: ptr("2026-11-02T00:00:00Z"),
		LastSeenAt: ptr("2026-11-20T11:59:00Z"), CurrentJob: job, LeaseUntil: &lease,
		AllowAll: true, Version: 7,
	})
	pol, err := e.svc.SetRunnerPolicy(ctx(), "ACTIVE0001", RunnerPolicy{Repos: []string{"Acme/Mono", "acme/mono", " other/* "}}, "admin")
	if err != nil {
		t.Fatalf("SetRunnerPolicy: %v", err)
	}
	if pol.AllowAll || strings.Join(pol.Repos, ",") != "acme/mono,other/*" {
		t.Fatalf("applied policy = %+v", pol)
	}
	doc, _, derr := e.svc.loadRunner(ctx(), "active0001")
	if derr != nil || doc == nil {
		t.Fatalf("reload: %+v (%v)", doc, derr)
	}
	if doc.AllowAll || strings.Join(doc.Repos, ",") != "acme/mono,other/*" {
		t.Fatalf("recorded policy = %+v", doc)
	}
	if doc.Version != 8 {
		t.Fatalf("version = %d, want 8", doc.Version)
	}
	if doc.Name != "build-01" || doc.State != RunnerActive || doc.Capacity != 3 ||
		strings.Join(doc.Labels, ",") != "docker,linux" || doc.DeclaredVersion != "1.4.0" ||
		doc.RegisteredBy != "admin" || doc.CreatedAt != "2026-11-01T00:00:00Z" ||
		doc.RegisteredAt == nil || *doc.RegisteredAt != "2026-11-02T00:00:00Z" ||
		doc.LastSeenAt == nil || *doc.LastSeenAt != "2026-11-20T11:59:00Z" {
		t.Fatalf("edit disturbed another field: %+v", doc)
	}
	if doc.CurrentJob == nil || *doc.CurrentJob != *job || doc.LeaseUntil == nil || *doc.LeaseUntil != lease {
		t.Fatalf("edit disturbed the current job/lease: %+v", doc)
	}

	// A pending record (minted, not registered) is editable too, and the legacy
	// shape — no policy at all — is exactly the live case the edit exists for.
	seedRunnerDoc(t, e, RunnerDoc{ID: "pending0001", Name: "build-02", State: RunnerPending, CreatedAt: "2026-11-01T00:00:00Z"})
	if _, err := e.svc.SetRunnerPolicy(ctx(), "pending0001", RunnerPolicy{AllowAll: true}, "admin"); err != nil {
		t.Fatalf("pending edit: %v", err)
	}
	doc, _, _ = e.svc.loadRunner(ctx(), "pending0001")
	if doc == nil || !doc.AllowAll || len(doc.Repos) != 0 || doc.State != RunnerPending {
		t.Fatalf("pending record after edit = %+v", doc)
	}
}

// TestSetRunnerPolicyNoOpWritesNothing: re-submitting the recorded policy is a
// no-op — no version bump, no rewrite of the lease display state.
func TestSetRunnerPolicyNoOpWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	seedRunnerDoc(t, e, RunnerDoc{
		ID: "noop000001", Name: "build-01", State: RunnerActive, Repos: []string{"acme/mono"},
		CurrentJob: &RunnerJobRef{Repo: "acme/mono", RunID: "1-run", JobID: "j-build"}, Version: 4,
	})
	before, _, err := e.svc.loadRunner(ctx(), "noop000001")
	if err != nil || before == nil {
		t.Fatalf("load: %v", err)
	}
	applied, err := e.svc.SetRunnerPolicy(ctx(), "noop000001", RunnerPolicy{Repos: []string{"ACME/Mono"}}, "admin")
	if err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	if strings.Join(applied.Repos, ",") != "acme/mono" {
		t.Fatalf("applied = %+v", applied)
	}
	after, _, _ := e.svc.loadRunner(ctx(), "noop000001")
	if after.Version != before.Version {
		t.Fatalf("a no-op edit bumped the version: %d → %d", before.Version, after.Version)
	}
	if after.CurrentJob == nil || *after.CurrentJob != *before.CurrentJob {
		t.Fatalf("a no-op edit touched the current job: %+v", after.CurrentJob)
	}
}

// TestSetRunnerPolicyRefusals: a revoked runner cannot be re-armed, an unknown
// id is a 404-class refusal, a bad id never reaches the store, a corrupt record
// is reported, and an unnamed actor (a skipped gate) is refused — every one of
// them leaves the record untouched.
func TestSetRunnerPolicyRefusals(t *testing.T) {
	e := newTestEnv(t)
	revoked := "2026-11-20T10:00:00Z"
	seedRunnerDoc(t, e, RunnerDoc{ID: "revoked0001", Name: "old", State: RunnerRevoked, RevokedAt: &revoked, Version: 3})
	before, _, _ := e.svc.loadRunner(ctx(), "revoked0001")
	if _, err := e.svc.SetRunnerPolicy(ctx(), "revoked0001", RunnerPolicy{AllowAll: true}, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked edit = %v, want ErrConflict", err)
	} else if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked refusal does not say why: %v", err)
	}
	if after, _, _ := e.svc.loadRunner(ctx(), "revoked0001"); after == nil || after.Version != before.Version ||
		after.State != RunnerRevoked || after.AllowAll || len(after.Repos) != 0 {
		t.Fatalf("a refused edit rewrote the revoked record: %+v", after)
	}

	// A revoked STATE with no revoked_at stamp is refused too (defensive: one
	// of the two is enough).
	seedRunnerDoc(t, e, RunnerDoc{ID: "revoked0002", State: RunnerRevoked})
	if _, err := e.svc.SetRunnerPolicy(ctx(), "revoked0002", RunnerPolicy{AllowAll: true}, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("state-only revoked edit = %v, want ErrConflict", err)
	}

	if _, err := e.svc.SetRunnerPolicy(ctx(), "missing0001", RunnerPolicy{AllowAll: true}, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown runner edit = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"", "UPPER!", "../etc", strings.Repeat("a", 40)} {
		if _, err := e.svc.SetRunnerPolicy(ctx(), id, RunnerPolicy{AllowAll: true}, "admin"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("bad id %q = %v, want ErrNotFound", id, err)
		}
	}
	// An empty actor means the credential gate was skipped: fail closed.
	if _, err := e.svc.SetRunnerPolicy(ctx(), "revoked0001", RunnerPolicy{AllowAll: true}, "  "); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("nameless actor = %v, want ErrUnauthorized", err)
	}

	// A corrupt record is reported, never overwritten with a clean one.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey("corrupt001"), []byte("{not json"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if _, err := e.svc.SetRunnerPolicy(ctx(), "corrupt001", RunnerPolicy{AllowAll: true}, "admin"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt runner edit = %v, want ErrCorrupt", err)
	}
}

// TestSetRunnerPolicyValidation: the mint's own rule applies to the edit —
// empty, malformed and over-long pattern lists are refused with the service's
// message and change nothing.
func TestSetRunnerPolicyValidation(t *testing.T) {
	e := newTestEnv(t)
	seedRunnerDoc(t, e, RunnerDoc{ID: "valid00001", Name: "build-01", State: RunnerActive, AllowAll: true, Version: 2})
	tooMany := make([]string, 0, MaxRunnerPolicyRepos+1)
	for i := 0; i <= MaxRunnerPolicyRepos; i++ {
		tooMany = append(tooMany, fmt.Sprintf("o/r%d", i))
	}
	cases := []struct {
		name string
		in   RunnerPolicy
		want string
	}{
		{"empty", RunnerPolicy{}, "all repositories"},
		{"blank patterns", RunnerPolicy{Repos: []string{"  ", ""}}, "all repositories"},
		{"bad pattern", RunnerPolicy{Repos: []string{"not-a-repo"}}, "owner/name or owner/*"},
		{"too many", RunnerPolicy{Repos: tooMany}, "at most 50"},
	}
	for _, tc := range cases {
		_, err := e.svc.SetRunnerPolicy(ctx(), "valid00001", tc.in, "admin")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s = %v, want ErrInvalid", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s message = %q, want %q", tc.name, err, tc.want)
		}
	}
	doc, _, _ := e.svc.loadRunner(ctx(), "valid00001")
	if doc == nil || !doc.AllowAll || doc.Version != 2 {
		t.Fatalf("a refused edit changed the record: %+v", doc)
	}
}

// TestSetRunnerPolicyCASOneWinner: concurrent edits all land on a valid record
// (the CAS loop re-reads), the last writer's policy is the recorded one and
// nothing but the policy moved.
func TestSetRunnerPolicyCASOneWinner(t *testing.T) {
	e := newTestEnv(t)
	seedRunnerDoc(t, e, RunnerDoc{
		ID: "cas0000001", Name: "build-01", State: RunnerActive, Labels: []string{"linux"}, Capacity: 2,
		CurrentJob: &RunnerJobRef{Repo: "o/r", RunID: "1-run", JobID: "j-build"}, Version: 1,
	})
	pols := []RunnerPolicy{
		{Repos: []string{"acme/mono"}},
		{Repos: []string{"acme/*"}},
		{AllowAll: true},
		{Repos: []string{"other/lib"}},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(pols))
	for i := range pols {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.SetRunnerPolicy(ctx(), "cas0000001", pols[i], "admin")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent edit %d: %v", i, err)
		}
	}
	doc, _, _ := e.svc.loadRunner(ctx(), "cas0000001")
	if doc == nil {
		t.Fatal("runner vanished")
	}
	var matched bool
	for _, p := range pols {
		if doc.AllowAll == p.AllowAll && strings.Join(doc.Repos, ",") == strings.Join(p.Repos, ",") {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("recorded policy %+v is none of the submitted ones", doc.policy())
	}
	if doc.Version < 2 {
		t.Fatalf("version = %d, want at least one committed write", doc.Version)
	}
	if doc.Name != "build-01" || doc.Capacity != 2 || doc.State != RunnerActive ||
		doc.CurrentJob == nil || doc.CurrentJob.JobID != "j-build" {
		t.Fatalf("concurrent edits lost a field: %+v", doc)
	}
}

// TestSetRunnerPolicyAudit: every accepted edit (a no-op re-submission
// included) records the actor, the runner and the resulting policy in the host
// audit family.
func TestSetRunnerPolicyAudit(t *testing.T) {
	e := newTestEnv(t)
	var mu sync.Mutex
	var entries []string
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, actor+"|"+action+"|"+target+"|"+detail)
		return nil
	}
	seedRunnerDoc(t, e, RunnerDoc{ID: "audit00001", Name: "build-01", State: RunnerActive, AllowAll: true})
	if _, err := e.svc.SetRunnerPolicy(ctx(), "audit00001", RunnerPolicy{Repos: []string{"Acme/Mono"}}, "Alice"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := e.svc.SetRunnerPolicy(ctx(), "audit00001", RunnerPolicy{Repos: []string{"acme/mono"}}, "Alice"); err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	// A refused edit is not an accepted change: it records nothing.
	if _, err := e.svc.SetRunnerPolicy(ctx(), "audit00001", RunnerPolicy{}, "Alice"); err == nil {
		t.Fatal("empty policy accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(entries) != 2 {
		t.Fatalf("audit entries = %v", entries)
	}
	for _, entry := range entries {
		if entry != "Alice|"+AuditRunnerPolicy+"|runner:audit00001|policy=acme/mono" {
			t.Fatalf("audit entry = %q", entry)
		}
	}
	// A nil Audit seam (a test/restricted build) is a logged no-op, never a
	// failed write.
	e.svc.Audit = nil
	if _, err := e.svc.SetRunnerPolicy(ctx(), "audit00001", RunnerPolicy{AllowAll: true}, "Alice"); err != nil {
		t.Fatalf("edit without an audit seam: %v", err)
	}
}

// TestSetRunnerPolicyChangesTheNextClaim is the operator-visible proof: the
// claim filter reads the policy from the registry record, so a repository the
// runner could not take is offered on its NEXT fetch (a fresh authorization —
// the credential boundary is per request) while a job already running keeps its
// lease and its report path.
func TestSetRunnerPolicyChangesTheNextClaim(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, MintInput{
		Name: "build-01", RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"o/other"}},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Before the edit the only queued repository is outside the policy.
	ra, err := e.svc.authorizeRunner(ctx(), reg.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if job, ferr := e.svc.FetchJob(ctx(), ra, time.Millisecond); ferr != nil || job != nil {
		t.Fatalf("out-of-policy claim = %+v (%v)", job, ferr)
	}

	if _, err := e.svc.SetRunnerPolicy(ctx(), reg.RunnerID, RunnerPolicy{Repos: []string{"o/r"}}, admin.Name); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	// The next protocol request loads the record again, so the policy it
	// enforces is the edited one.
	ra, err = e.svc.authorizeRunner(ctx(), reg.Token)
	if err != nil {
		t.Fatalf("re-authorize: %v", err)
	}
	job, err := e.svc.FetchJob(ctx(), ra, time.Millisecond)
	if err != nil || job == nil || job.Repo != "o/r" {
		t.Fatalf("claim after the edit = %+v (%v), want o/r", job, err)
	}

	// A job already running is untouched by a later edit: its record, the
	// registry's current-job display and the lease all survive, and narrow the
	// policy away from the running repository changes nothing about it.
	if _, err := e.svc.SetRunnerPolicy(ctx(), reg.RunnerID, RunnerPolicy{Repos: []string{"o/other"}}, admin.Name); err != nil {
		t.Fatalf("narrowing edit: %v", err)
	}
	running, jerr := e.svc.loadJobCached(ctx(), "o", "r", job.RunID, job.JobID, nil)
	if jerr != nil || running == nil || running.State != JobRunning || running.Attempt != job.Attempt {
		t.Fatalf("in-flight job after the edit = %+v (%v)", running, jerr)
	}
	doc, _, derr := e.svc.loadRunner(ctx(), reg.RunnerID)
	if derr != nil || doc == nil || doc.CurrentJob == nil || doc.CurrentJob.JobID != job.JobID || doc.LeaseUntil == nil {
		t.Fatalf("registry current job after the edit = %+v (%v)", doc, derr)
	}
}

// TestSetRunnerPolicyAPILanes: both admin JSON lanes edit a policy (the same
// service call), anonymous is a real 401, a non-admin a 403, a bad body a 400,
// an empty policy a 400 and a revoked runner a 409; the fleet read shows the
// result.
func TestSetRunnerPolicyAPILanes(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	seedRunnerDoc(t, e, RunnerDoc{ID: "lanes00001", Name: "build-01", State: RunnerActive, AllowAll: true})
	revoked := "2026-11-20T10:00:00Z"
	seedRunnerDoc(t, e, RunnerDoc{ID: "lanes00002", Name: "old", State: RunnerRevoked, RevokedAt: &revoked})

	// Anonymous: 401 on both lanes, before any body validation.
	*cur = auth.Anonymous()
	for _, lane := range []string{"/api/v1/admin/runners/lanes00001/policy", "/api-browser/v1/admin/runners/lanes00001/policy"} {
		if rec := doAuth(h, http.MethodPost, lane, `{"allow_all":true}`, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d, want 401", lane, rec.Code)
		}
	}
	// Authenticated but not a host admin: 403.
	*cur = auth.Principal{Name: "bob", Write: true}
	if rec := doAuth(h, http.MethodPost, "/api/v1/admin/runners/lanes00001/policy", `{"allow_all":true}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("repo admin edit = %d, want 403", rec.Code)
	}
	*cur = admin
	// Both lanes accept the edit; the answer echoes the normalised policy.
	for _, lane := range []string{"/api/v1/admin/runners/lanes00001/policy", "/api-browser/v1/admin/runners/lanes00001/policy"} {
		rec := doAuth(h, http.MethodPost, lane, `{"repos":["Acme/Mono","acme/*"]}`, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", lane, rec.Code, rec.Body.String())
		}
		var got struct {
			ID     string       `json:"id"`
			Policy RunnerPolicy `json:"policy"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s answer: %v", lane, err)
		}
		if got.ID != "lanes00001" || got.Policy.AllowAll || strings.Join(got.Policy.Repos, ",") != "acme/*,acme/mono" {
			t.Fatalf("%s answer = %+v", lane, got)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s Cache-Control = %q", lane, rec.Header().Get("Cache-Control"))
		}
	}
	rows, err := e.svc.ListRunners(ctx(), admin)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.ID == "lanes00001" {
			found = true
			if row.AllowAll || strings.Join(row.Repos, ",") != "acme/*,acme/mono" {
				t.Fatalf("fleet row after the edit = %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("edited runner missing from the fleet")
	}

	// Refusals: an unknown field, an empty policy, a bad pattern, a revoked
	// runner and an unknown id.
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/admin/runners/lanes00001/policy", `{"name":"x"}`, http.StatusBadRequest},
		{"/api/v1/admin/runners/lanes00001/policy", `{}`, http.StatusBadRequest},
		{"/api/v1/admin/runners/lanes00001/policy", `{"repos":["nope"]}`, http.StatusBadRequest},
		{"/api/v1/admin/runners/lanes00002/policy", `{"allow_all":true}`, http.StatusConflict},
		{"/api/v1/admin/runners/missing001/policy", `{"allow_all":true}`, http.StatusNotFound},
	} {
		if rec := doAuth(h, http.MethodPost, tc.path, tc.body, ""); rec.Code != tc.want {
			t.Fatalf("POST %s %s = %d, want %d: %s", tc.path, tc.body, rec.Code, tc.want, rec.Body.String())
		}
	}
	// GET is not a verb on the edit route.
	if rec := doAuth(h, http.MethodGet, "/api/v1/admin/runners/lanes00001/policy", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET policy = %d, want 405", rec.Code)
	}
	// The revoked record was not re-armed by the refused edit.
	if doc, _, _ := e.svc.loadRunner(ctx(), "lanes00002"); doc == nil || doc.State != RunnerRevoked || doc.AllowAll {
		t.Fatalf("revoked record after a refused edit = %+v", doc)
	}
}
