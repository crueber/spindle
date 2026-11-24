package threads

// Runner tenancy (docs/threads.md §8.7): the ADMIN-set repository policy on a
// registration token, copied to the registry record at registration, enforced by
// the claim filter, and impossible for the runner to widen.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// TestValidateRunnerPolicy pins the policy grammar: patterns normalise to
// owner/name or owner/*, an empty policy is refused (no accidental "no jobs"
// token), and the pattern count is bounded.
func TestValidateRunnerPolicy(t *testing.T) {
	pol, err := ValidateRunnerPolicy(RunnerPolicyInput{Repos: []string{"Acme/Mono", " other/* ", "acme/mono", ""}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if pol.AllowAll || strings.Join(pol.Repos, "|") != "acme/mono|other/*" {
		t.Fatalf("normalised policy = %+v", pol)
	}
	for _, bad := range []string{"acme", "acme/", "/mono", "acme/mono/x", "*", "*/mono", "acme/*/x", "acme/mo no", strings.Repeat("a", 120) + "/x"} {
		if _, err := ValidateRunnerPolicy(RunnerPolicyInput{Repos: []string{bad}}); err == nil {
			t.Fatalf("pattern %q accepted", bad)
		}
	}
	// Neither all-repositories nor a pattern: refused, naming the rule.
	if _, err := ValidateRunnerPolicy(RunnerPolicyInput{}); err == nil {
		t.Fatal("an empty policy must be refused")
	} else if !strings.Contains(err.Error(), "all repositories") {
		t.Fatalf("empty-policy error = %v", err)
	}
	tooMany := make([]string, 0, MaxRunnerPolicyRepos+1)
	for i := 0; i <= MaxRunnerPolicyRepos; i++ {
		tooMany = append(tooMany, "o/r"+strings.Repeat("x", 1)+string(rune('a'+i%26))+strings.Repeat("y", i/26))
	}
	if _, err := ValidateRunnerPolicy(RunnerPolicyInput{Repos: tooMany}); err == nil {
		t.Fatal("an over-long pattern list must be refused")
	}
	// allow_all is on its own enough, and it does not need patterns.
	if pol, err := ValidateRunnerPolicy(RunnerPolicyInput{AllowAll: true}); err != nil || !pol.AllowAll || len(pol.Repos) != 0 {
		t.Fatalf("allow-all policy = %+v, %v", pol, err)
	}
}

// TestRunnerPolicyAllowsRepo pins the claim rule: all / exact / owner glob, and
// nothing else (an empty policy allows no repository at all).
func TestRunnerPolicyAllowsRepo(t *testing.T) {
	cases := []struct {
		name string
		pol  RunnerPolicy
		repo string
		want bool
	}{
		{"all", RunnerPolicy{AllowAll: true}, "acme/mono", true},
		{"exact", RunnerPolicy{Repos: []string{"acme/mono"}}, "acme/mono", true},
		{"exact other", RunnerPolicy{Repos: []string{"acme/mono"}}, "acme/other", false},
		{"glob", RunnerPolicy{Repos: []string{"acme/*"}}, "acme/anything", true},
		{"glob other owner", RunnerPolicy{Repos: []string{"acme/*"}}, "other/mono", false},
		{"empty", RunnerPolicy{}, "acme/mono", false},
		{"empty allow_all false", RunnerPolicy{AllowAll: false, Repos: nil}, "acme/mono", false},
		{"case insensitive", RunnerPolicy{Repos: []string{"acme/mono"}}, "ACME/Mono", true},
		{"no repo", RunnerPolicy{AllowAll: true}, "", true},
		{"malformed repo", RunnerPolicy{Repos: []string{"acme/*"}}, "acme", false},
	}
	for _, tc := range cases {
		if got := tc.pol.AllowsRepo(tc.repo); got != tc.want {
			t.Fatalf("%s: AllowsRepo(%q) = %v, want %v", tc.name, tc.repo, got, tc.want)
		}
	}
	// Describe is the audit/page reading; an absent policy is "none".
	if (RunnerPolicy{AllowAll: true}).Describe() != "all repositories" ||
		(RunnerPolicy{Repos: []string{"a/b", "c/*"}}).Describe() != "a/b,c/*" ||
		(RunnerPolicy{}).Describe() != "none" {
		t.Fatalf("Describe spellings changed")
	}
	// A nil doc allows nothing (defensive: the claim path never sees one, but a
	// nil must not read as "everything").
	var nilDoc *RunnerDoc
	if nilDoc.AllowsRepo("acme/mono") {
		t.Fatal("a nil runner doc allowed a repository")
	}
}

// TestRunnerPolicyCarriedFromMintToRegistration: the mint validates and
// normalises the admin's policy, both records carry it, and registration applies
// it to the registry record the claim filter reads. Declare never clears it.
func TestRunnerPolicyCarriedFromMintToRegistration(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, MintInput{
		Name:              "build-01",
		RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"Acme/Mono", "other/*", "acme/mono"}},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	const want = "acme/mono,other/*"
	if mint.Policy.AllowAll || strings.Join(mint.Policy.Repos, ",") != want {
		t.Fatalf("mint answer policy = %+v", mint.Policy)
	}
	tok, _, err := e.svc.loadRunnerToken(ctx(), mint.ID)
	if err != nil || tok == nil || strings.Join(tok.Repos, ",") != want {
		t.Fatalf("token record policy = %+v (%v)", tok, err)
	}
	doc, _, err := e.svc.loadRunner(ctx(), mint.ID)
	if err != nil || doc == nil || doc.State != RunnerPending || strings.Join(doc.Repos, ",") != want {
		t.Fatalf("pending registry policy = %+v (%v)", doc, err)
	}

	reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if strings.Join(reg.Policy.Repos, ",") != want {
		t.Fatalf("register answer policy = %+v", reg.Policy)
	}
	doc, _, err = e.svc.loadRunner(ctx(), mint.ID)
	if err != nil || doc == nil || doc.State != RunnerActive || strings.Join(doc.Repos, ",") != want {
		t.Fatalf("registered registry policy = %+v (%v)", doc, err)
	}

	// Declare states labels/capacity/forks; it must leave the policy alone and
	// echo it back so the runner can report what it operates under.
	ra, err := e.svc.authorizeRunner(ctx(), reg.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	view, err := e.svc.Declare(ctx(), ra, DeclareInput{Version: "1.0", Labels: []string{"linux"}, Capacity: 2})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if strings.Join(view.Policy.Repos, ",") != want {
		t.Fatalf("declare answer policy = %+v", view.Policy)
	}
	doc, _, _ = e.svc.loadRunner(ctx(), mint.ID)
	if strings.Join(doc.Repos, ",") != want {
		t.Fatalf("declare changed the policy: %+v", doc)
	}
}

// TestRunnerPolicyClaimFilter: the claim is offered only for an allowed
// repository — exact, owner glob, or all — and never for an unlisted one or for
// a record with no policy at all. The global hint and the per-repository claim
// loop both refuse.
func TestRunnerPolicyClaimFilter(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	for _, repo := range []string{"o/r", "o/s"} {
		if _, err := e.svc.CreateRun(ctx(), repo, testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"}); err != nil {
			t.Fatalf("create %s: %v", repo, err)
		}
	}
	admin := auth.Principal{Name: "admin", Admin: true}
	// allow_all claims the oldest job.
	_, _, allDoc := instanceRunner(t, e, admin, []string{"linux"})
	job, err := e.svc.FetchJob(ctx(), allDoc, time.Millisecond)
	if err != nil || job == nil || job.Repo != "o/r" {
		t.Fatalf("allow-all claim = %+v (%v)", job, err)
	}

	// An exact policy for o/s cannot take o/r (already claimed anyway), and it
	// does take o/s; a runner allowed only o/r is offered nothing that is left.
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, MintInput{RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"o/s"}}})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	exact, err := e.svc.authorizeRunner(ctx(), reg.Token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if job, err := e.svc.FetchJob(ctx(), exact, time.Millisecond); err != nil || job == nil || job.Repo != "o/s" {
		t.Fatalf("exact-policy claim = %+v (%v), want o/s", job, err)
	}
	// Nothing is left; the same runner polls empty, and a runner scoped to the
	// wrong owner is offered nothing even though it matches every label.
	_, _, otherDoc := instanceRunner(t, e, admin, []string{"linux"})
	if job, err := e.svc.FetchJob(ctx(), otherDoc, time.Millisecond); err != nil || job != nil {
		t.Fatalf("empty queue claim = %+v (%v)", job, err)
	}
	// A record written before tenancy (no policy fields) is offered nothing —
	// and the claim loop refuses it explicitly, not merely because the queue is
	// empty.
	if claim, err := e.svc.claimLoop(ctx(), "o", "r", &RunnerDoc{ID: "legacy0001", Labels: []string{"linux"}, State: RunnerActive}, []string{"linux"}); err != nil || claim != nil {
		t.Fatalf("legacy (no policy) claim = %+v (%v)", claim, err)
	}
	// The per-repository authority refuses too: a scoped doc cannot claim o/r
	// even with the exact label set and a queued job.
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Actor: "writer"}); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	scoped := &RunnerDoc{ID: "scoped00001", Labels: []string{"linux"}, State: RunnerActive, Repos: []string{"o/s"}}
	if claim, err := e.svc.claimLoop(ctx(), "o", "r", scoped, scoped.Labels); err != nil || claim != nil {
		t.Fatalf("out-of-policy claim = %+v (%v)", claim, err)
	}
	// owner/* is the owner-wide grant.
	wild := &RunnerDoc{ID: "wild000001", Labels: []string{"linux"}, State: RunnerActive, Repos: []string{"o/*"}}
	if claim, err := e.svc.claimLoop(ctx(), "o", "r", wild, wild.Labels); err != nil || claim == nil {
		t.Fatalf("owner-glob claim = %+v (%v)", claim, err)
	}
}

// TestRunnerPolicyComposesWithForkRule: a fork job needs BOTH the repository
// allowed and the fork opt-in; either one alone leaves it queued.
func TestRunnerPolicyComposesWithForkRule(t *testing.T) {
	e, _, _ := forkGateEnv(t, true)
	admin := auth.Principal{Name: "admin", Admin: true}
	mintRunner := func(pol RunnerPolicyInput, fork bool) *runnerAuth {
		t.Helper()
		mint, err := e.svc.MintRegistrationToken(ctx(), admin, MintInput{RunnerPolicyInput: pol})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}, AllowForkRuns: fork})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		ra, err := e.svc.authorizeRunner(ctx(), reg.Token)
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		return ra
	}
	// Fork allowed but the repository is not: queued.
	elsewhere := mintRunner(RunnerPolicyInput{Repos: []string{"other/x"}}, true)
	if job, err := e.svc.FetchJob(ctx(), elsewhere, time.Millisecond); err != nil || job != nil {
		t.Fatalf("fork + wrong repo = %+v (%v)", job, err)
	}
	// Repository allowed but fork not opted in: queued.
	trustedOnly := mintRunner(RunnerPolicyInput{AllowAll: true}, false)
	if job, err := e.svc.FetchJob(ctx(), trustedOnly, time.Millisecond); err != nil || job != nil {
		t.Fatalf("fork + trusted-only runner = %+v (%v)", job, err)
	}
	// Both: offered.
	both := mintRunner(RunnerPolicyInput{Repos: []string{"o/r"}}, true)
	job, err := e.svc.FetchJob(ctx(), both, time.Millisecond)
	if err != nil || job == nil || job.Repo != "o/r" || job.Trusted {
		t.Fatalf("fork + allowed repo + opt-in = %+v (%v)", job, err)
	}
}

// TestRunnerPolicyCannotBeSelfDeclared: the runner protocol has no policy field
// (the strict decoder refuses one), and no protocol call changes the recorded
// policy — registration copies the admin's token policy and nothing else.
func TestRunnerPolicyCannotBeSelfDeclared(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	// The admin mint carries the policy.
	rec := doAuth(h, http.MethodPost, "/api/v1/admin/runners/tokens", `{"repos":["o/r"]}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin mint = %d: %s", rec.Code, rec.Body.String())
	}
	var minted RegistrationToken
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("mint answer: %v", err)
	}
	// A registration that tries to widen is refused by the strict decoder, and
	// nothing is written: the token stays unconsumed.
	body := `{"token":"` + minted.Token + `","labels":["linux"],"allow_all":true}`
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", body, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("register with allow_all = %d, want 400", rec.Code)
	}
	tok, _, _ := e.svc.loadRunnerToken(ctx(), minted.ID)
	if tok == nil || tok.ConsumedAt != nil {
		t.Fatalf("a rejected register consumed the token: %+v", tok)
	}
	// A legal registration, then a declare that tries to widen: 400, policy
	// intact (the runner cannot change it).
	reg := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register",
		`{"token":"`+minted.Token+`","labels":["linux"]}`, "")
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", reg.Code, reg.Body.String())
	}
	var registered RegisterResult
	if err := json.Unmarshal(reg.Body.Bytes(), &registered); err != nil {
		t.Fatalf("register answer: %v", err)
	}
	if strings.Join(registered.Policy.Repos, ",") != "o/r" {
		t.Fatalf("register policy = %+v", registered.Policy)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare",
		`{"labels":["linux"],"repos":["o/*"]}`, registered.Token); rec.Code != http.StatusBadRequest {
		t.Fatalf("declare with repos = %d, want 400", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare",
		`{"labels":["linux"],"allow_all":true}`, registered.Token); rec.Code != http.StatusBadRequest {
		t.Fatalf("declare with allow_all = %d, want 400", rec.Code)
	}
	ok := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{"labels":["linux"],"capacity":1}`, registered.Token)
	if ok.Code != http.StatusOK {
		t.Fatalf("declare = %d: %s", ok.Code, ok.Body.String())
	}
	var declared DeclareView
	if err := json.Unmarshal(ok.Body.Bytes(), &declared); err != nil {
		t.Fatalf("declare answer: %v", err)
	}
	if strings.Join(declared.Policy.Repos, ",") != "o/r" {
		t.Fatalf("declare changed the policy: %+v", declared.Policy)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), registered.RunnerID)
	if doc == nil || strings.Join(doc.Repos, ",") != "o/r" || doc.AllowAll {
		t.Fatalf("registry policy after declare = %+v", doc)
	}
}

// TestRunnerPolicyMintLanes: both admin JSON lanes mint with a policy and refuse
// a policy-less mint; the audit entry records the actor and the policy.
func TestRunnerPolicyMintLanes(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	var mu sync.Mutex
	var details []string
	e.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		mu.Lock()
		defer mu.Unlock()
		details = append(details, actor+"|"+action+"|"+detail)
		return nil
	}
	*cur = admin
	for _, lane := range []string{"/api/v1/admin/runners/tokens", "/api-browser/v1/admin/runners/tokens"} {
		rec := doAuth(h, http.MethodPost, lane, `{"name":"build-01","repos":["acme/mono","acme/*"]}`, "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s mint = %d: %s", lane, rec.Code, rec.Body.String())
		}
		var minted RegistrationToken
		if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
			t.Fatalf("%s answer: %v", lane, err)
		}
		if minted.Policy.AllowAll || strings.Join(minted.Policy.Repos, ",") != "acme/*,acme/mono" {
			t.Fatalf("%s policy = %+v", lane, minted.Policy)
		}
		// A policy-less mint is refused on the same lane (no accidental token).
		if rec := doAuth(h, http.MethodPost, lane, `{"name":"x"}`, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s policy-less mint = %d, want 400", lane, rec.Code)
		}
		if rec := doAuth(h, http.MethodPost, lane, `{"allow_all":false,"repos":["nope"]}`, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s bad-pattern mint = %d, want 400", lane, rec.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(details) != 2 {
		t.Fatalf("audit entries = %v", details)
	}
	for _, d := range details {
		if !strings.Contains(d, AuditRunnerTokenMint) || !strings.Contains(d, "policy=acme/*,acme/mono") || !strings.HasPrefix(d, "admin|") {
			t.Fatalf("mint audit entry = %q", d)
		}
	}
	// The fleet projection carries the policy (never a secret).
	rows, err := e.svc.ListRunners(ctx(), admin)
	if err != nil || len(rows) != 2 {
		t.Fatalf("fleet = %+v (%v)", rows, err)
	}
	for _, row := range rows {
		if row.AllowAll || strings.Join(row.Repos, ",") != "acme/*,acme/mono" {
			t.Fatalf("fleet row policy = %+v", row)
		}
	}
	// A record with no policy reads as "no repositories", never as "all".
	legacy := RunnerDoc{ID: "legacyfleet1", State: RunnerActive, CreatedAt: "2026-11-20T00:00:00Z", Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(legacy.ID), mustJSON(&legacy), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	rows, err = e.svc.ListRunners(ctx(), admin)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	for _, row := range rows {
		if row.ID == legacy.ID && (row.AllowAll || len(row.Repos) != 0) {
			t.Fatalf("legacy fleet row = %+v", row)
		}
	}
}
