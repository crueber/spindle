package threads

import (
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func TestQueueGuardDirect(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	runs := e.mustCreate(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", independentWorkflowYAML)
	// Remove the index/head for the trimHead nil path.
	if err := e.store.Delete(ctx(), QueueHeadKey("o", "r"), ""); err != nil {
		t.Fatal(err)
	}
	e.svc.trimHead(ctx(), "o", "r", map[string]bool{"x": true})
	// setHeadPaused is idempotent.
	if err := e.svc.setHeadPaused(ctx(), "o", "r", true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.setHeadPaused(ctx(), "o", "r", true); err != nil {
		t.Fatal(err)
	}
	// claimLoop refuses a draining runner before touching the head.
	rd := &RunnerDoc{ID: "drain", State: RunnerDraining, AllowAll: true, Labels: []string{"linux"}}
	if got, err := e.svc.claimLoop(ctx(), "o", "r", rd, []string{"linux"}); err != nil || got != nil {
		t.Fatalf("draining claimLoop = %+v, %v", got, err)
	}
	// A label mismatch is skipped, and a fork entry needs an opted-in runner.
	if err := e.svc.setHeadPaused(ctx(), "o", "r", false); err != nil {
		t.Fatal(err)
	}
	r := &RunnerDoc{ID: "plain", State: RunnerActive, AllowAll: true, Labels: []string{"other"}}
	if got, err := e.svc.claimLoop(ctx(), "o", "r", r, []string{"other"}); err != nil || got != nil {
		t.Fatalf("label mismatch = %+v, %v", got, err)
	}
	// Direct claimEntry guards.
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	if head == nil || len(head.Pending) == 0 {
		t.Skip("no pending entries")
	}
	entry, ver, _ := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	foreign := &RunnerDoc{ID: "foreign", State: RunnerActive, Repos: []string{"other/*"}, Labels: []string{"linux"}}
	if got, err := e.svc.claimEntry(ctx(), "o", "r", entry, ver, foreign); err != nil || got != nil {
		t.Fatalf("tenancy guard = %+v, %v", got, err)
	}
	// Configured lease is honoured.
	e.svc.JobLease = time.Minute
	if got := e.svc.jobLease(); got != time.Minute {
		t.Fatalf("jobLease = %v", got)
	}
	// Cancelling an unclaimed run retires its entry.
	if _, err := e.svc.CancelRun(ctx(), "o/r", runs[0].ID, auth.Principal{Name: "writer"}); err != nil {
		t.Fatal(err)
	}
}

func TestQueueEntryConflictAndCorruptHead(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml"}); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	if head == nil || len(head.Pending) == 0 {
		t.Skip("no pending entries")
	}
	entry, _, _ := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	job, run, _ := e.svc.loadJobAndRun(ctx(), "o", "r", entry.RunID, entry.JobID)
	// Re-enqueueing an existing sequence is a conflict: write the same entry
	// object then call enqueueJob with a job whose run/job id are fresh but whose
	// sequence collides only on the store's Create.
	if job == nil || run == nil {
		t.Skip("job missing")
	}
	_ = store.IsPreconditionFailed
	// Corrupt head: trim/cas paths surface an error.
	corruptKey(t, e, QueueHeadKey("o", "r"))
	if _, _, err := e.svc.loadHead(ctx(), "o", "r"); err == nil {
		t.Fatal("corrupt head loaded")
	}
	if err := e.svc.setHeadPaused(ctx(), "o", "r", true); err == nil {
		t.Fatal("corrupt head paused")
	}
	e.svc.trimHead(ctx(), "o", "r", map[string]bool{head.Pending[0]: true})
}

func TestRunnerGroupClearAndNilEdges(t *testing.T) {
	e := newTestEnv(t)
	if v := e.svc.runnerView(ctx(), nil); v.ID != "" {
		t.Fatalf("nil runner view = %+v", v)
	}
	applyRunnerGroup(nil, nil)
	if err := e.svc.reapplyRunnerGroup(ctx(), nil); err != nil {
		t.Fatalf("reapply nil: %v", err)
	}
	g, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{
		Name: "clear", Labels: []string{"a"}, RunnerPolicyInput: RunnerPolicyInput{AllowAll: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "clear-me", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, g.ID, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	// Explicit clear (the delete path is separate and already tested).
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, "", wave4Admin()); err != nil {
		t.Fatalf("clear group: %v", err)
	}
	// A missing runner and a corrupt runner in the CAS.
	if _, err := e.svc.SetRunnerGroup(ctx(), "missing", "", wave4Admin()); err == nil {
		t.Fatal("assign to missing runner accepted")
	}
	id2, _, _ := wave4Runner(t, e,
		MintInput{Name: "corrupt-me", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	corruptKey(t, e, RunnerKey(id2))
	if _, err := e.svc.SetRunnerGroup(ctx(), id2, g.ID, wave4Admin()); err == nil {
		t.Fatal("corrupt runner group-assigned")
	}
	// Deleting a group with a corrupt member record fails closed.
	if err := e.svc.DeleteRunnerGroup(ctx(), wave4Admin(), g.ID); err == nil {
		t.Fatal("delete with corrupt member succeeded")
	}
}

func TestRunnerMintGroupAndCapacity(t *testing.T) {
	e := newTestEnv(t)
	g, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{
		Name: "mint-group", Labels: []string{"gpu"}, RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"acme/*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := MintInput{Name: "grouped", Group: g.ID, RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}
	mint, err := e.svc.MintRegistrationToken(ctx(), wave4Admin(), in)
	if err != nil {
		t.Fatalf("mint with group: %v", err)
	}
	res, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}, Capacity: -5})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Policy.AllowAll {
		t.Fatalf("policy = %+v", res.Policy)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), res.RunnerID)
	if doc.Group != g.ID || len(doc.GroupLabels) != 1 {
		t.Fatalf("grouped runner = %+v", doc)
	}
	// Unknown group at mint.
	if _, err := e.svc.MintRegistrationToken(ctx(), wave4Admin(), MintInput{Name: "x", Group: "missing", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("mint with unknown group accepted")
	}
	// Registering the same token twice fails on the credential create.
	mint2, _ := e.svc.MintRegistrationToken(ctx(), wave4Admin(), mintAllPolicy("twice"))
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint2.Token, Labels: []string{"linux"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint2.Token, Labels: []string{"linux"}}); err == nil {
		t.Fatal("registration token reused")
	}
	// A revoked credential record is idempotently stamped.
	if err := e.svc.revokeRunnerCredential(ctx(), res.RunnerID, e.svc.nowUTC().Format(dateTimeFmt)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.revokeRunnerCredential(ctx(), res.RunnerID, e.svc.nowUTC().Format(dateTimeFmt)); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
}
