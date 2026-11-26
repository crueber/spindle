package threads

// concurrency_pending_test.go — group pending semantics (PAR-152): at most one
// running and one pending run per group, a newer pending run replaces (cancels)
// the older pending one, cancel-in-progress cancels the holder at creation,
// trigger order sequences the rest, and a terminal fold clears both slots.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

const pendingGroupWorkflow = `name: Deploy
on: [push]
concurrency:
  group: deploy-main
  cancel-in-progress: false
jobs:
  ship:
    runs-on: linux
    steps:
      - run: echo ship
`

const cancelGroupWorkflow = `name: Deploy
on: [push]
concurrency:
  group: deploy-main
  cancel-in-progress: true
jobs:
  ship:
    runs-on: linux
    steps:
      - run: echo ship
`

// loadGroupDoc reads one group's record (test observability).
func loadGroupDoc(t *testing.T, e *testEnv, group string) ConcurrencyDoc {
	t.Helper()
	raw, _, err := store.GetBytes(ctx(), e.store, ConcurrencyKey("o", "r", group), store.GetOptions{})
	if err != nil || raw == nil {
		t.Fatalf("group %q: %v", group, err)
	}
	var doc ConcurrencyDoc
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		t.Fatalf("group %q: %v", group, jerr)
	}
	return doc
}

// holdGroup claims and admits the run's job, so the run holds the group.
func holdGroup(t *testing.T, e *testEnv, run *Run) {
	t.Helper()
	runner := needsRunner()
	claim, err := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if err != nil || claim == nil {
		t.Fatalf("claim to hold the group = %+v (%v)", claim, err)
	}
	if claim.Entry.RunID != run.ID {
		t.Fatalf("claimed another run's job: %s", claim.Entry.RunID)
	}
	ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", claim.Entry.RunID, claim.Job.Spec)
	if aerr != nil || !ok {
		t.Fatalf("admit the holder = %v (%v)", ok, aerr)
	}
}

// finishJob drives one run's only job to success (the terminal fold releases
// and promotes the group).
func finishJob(t *testing.T, e *testEnv, run *Run) {
	t.Helper()
	for _, job := range mustJobs(t, e, run.ID) {
		if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatalf("finish %s: %v", job.ID, err)
		}
	}
}

// TestConcurrencyPendingRunReplacesTheOlderPending pins GitHub's rule with three
// rapid runs: one holds, the second is pending, and the third replaces (cancels)
// the second rather than queueing behind it.
func TestConcurrencyPendingRunReplacesTheOlderPending(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	runs := []*Run{}
	for i := 0; i < 3; i++ {
		// A fresh commit per push: the supersede rule is out of the picture
		// because a grouped workflow never takes the supersede head.
		sha := strings.Repeat(string(rune('a'+i)), 40)
		e.git.add(sha, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
		e.git.refs["refs/heads/main"] = sha
		out, err := e.svc.CreateRun(ctx(), "o/r", sha, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
		if err != nil || len(out) != 1 {
			t.Fatalf("create %d = %d runs (%v)", i, len(out), err)
		}
		runs = append(runs, out[0])
	}
	// The first run is pending on nothing; the other two see a free group until
	// a claim happens, so hold with the first and rebuild the group state.
	if runs[0].PendingGroup != "" {
		t.Fatalf("a free group made the first run pending on %q", runs[0].PendingGroup)
	}
	holdGroup(t, e, runs[0])

	// Runs 2 and 3 were created before the hold, so their headers did not know.
	// The fetch path claims in queue order and refuses through the `blocked`
	// set, exactly as FetchJobFrom does: run 2 waits, run 3 replaces it.
	runner := needsRunner()
	blocked := map[string]bool{}
	claim, err := e.svc.claimLoopSkip(ctx(), "o", "r", runner, runner.Labels, blocked)
	if err != nil || claim == nil {
		t.Fatalf("claim run 2 = %+v (%v)", claim, err)
	}
	if claim.Entry.RunID != runs[1].ID {
		t.Fatalf("claimed %s, want run 2's job", claim.Entry.RunID)
	}
	ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", claim.Entry.RunID, claim.Job.Spec)
	if aerr != nil || ok {
		t.Fatalf("run 2 admitted past the holder = %v (%v)", ok, aerr)
	}
	blocked[repoID("o", "r")+"|"+claim.Entry.Seq] = true
	e.svc.releaseClaim(ctx(), "o", "r", claim)
	doc := loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != runs[0].ID || doc.Pending != runs[1].ID {
		t.Fatalf("group after run 2 = %+v, want holder=%s pending=%s", doc, runs[0].ID, runs[1].ID)
	}

	claim3, err := e.svc.claimLoopSkip(ctx(), "o", "r", runner, runner.Labels, blocked)
	if err != nil || claim3 == nil {
		t.Fatalf("claim run 3 = %+v (%v)", claim3, err)
	}
	if claim3.Entry.RunID != runs[2].ID {
		t.Fatalf("claimed %s, want run 3's job (%s); runs=%s,%s,%s (claimed job %s)", claim3.Entry.RunID, runs[2].ID,
			runs[0].ID, runs[1].ID, runs[2].ID, claim3.Entry.JobID)
	}
	// Pending-cancel: the newer run cancels the waiting one and takes the slot.
	ok, aerr = e.svc.admitConcurrency(ctx(), "o", "r", claim3.Entry.RunID, claim3.Job.Spec)
	if aerr != nil || ok {
		t.Fatalf("run 3 admitted past the holder = %v (%v)", ok, aerr)
	}
	e.svc.releaseClaim(ctx(), "o", "r", claim3)
	doc = loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != runs[0].ID || doc.Pending != runs[2].ID {
		t.Fatalf("group after run 3 = %+v, want holder=%s pending=%s", doc, runs[0].ID, runs[2].ID)
	}
	second, err := e.svc.loadRun(ctx(), "o", "r", runs[1].ID)
	if err != nil || second == nil {
		t.Fatalf("load run 2: %v", err)
	}
	if second.State != RunCancelled || second.CancelReason != CancelReasonConcurrency {
		t.Fatalf("superseded pending run = %s / %q, want cancelled/concurrency", second.State, second.CancelReason)
	}

	// Finishing the holder promotes the pending run in trigger order.
	finishJob(t, e, runs[0])
	doc = loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != "" || doc.Pending != runs[2].ID {
		t.Fatalf("fold released the group wrongly: %+v", doc)
	}
	claim4, err := e.svc.claimLoop(ctx(), "o", "r", runner, runner.Labels)
	if err != nil || claim4 == nil || claim4.Entry.RunID != runs[2].ID {
		t.Fatalf("promoted claim = %+v (%v), want run 3", claim4, err)
	}
	ok, aerr = e.svc.admitConcurrency(ctx(), "o", "r", claim4.Entry.RunID, claim4.Job.Spec)
	if aerr != nil || !ok {
		t.Fatalf("pending run did not take the freed group: %v (%v)", ok, aerr)
	}
	doc = loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != runs[2].ID || doc.Pending != "" {
		t.Fatalf("group after promotion = %+v", doc)
	}
}

// TestConcurrencyPendingSlotBlocksALaterRun proves ordering: while a live run
// owns the pending slot, a later run cannot jump ahead of it, and a stale slot
// (a terminal or absent run) is stolen rather than wedging the group.
func TestConcurrencyPendingSlotBlocksALaterRun(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	// A real second run owns the pending slot (created behind the holder).
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(second) != 1 {
		t.Fatal(err)
	}
	// A later run (a greater id) is blocked by the live holder first...
	later := "9999999999999999999-ffffff"
	ok, holder, aerr := e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", later)
	if aerr != nil || ok || holder != first[0].ID {
		t.Fatalf("later run past the holder = %v %q %v, want blocked by %s", ok, holder, aerr, first[0].ID)
	}
	// ...and once the holder folds, by the pending run ahead of it.
	finishJob(t, e, first[0])
	ok, holder, aerr = e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", later)
	if aerr != nil || ok || holder != second[0].ID {
		t.Fatalf("later run past the pending slot = %v %q %v, want blocked by %s", ok, holder, aerr, second[0].ID)
	}
	// The pending run itself may take the group.
	ok, _, aerr = e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", second[0].ID)
	if aerr != nil || !ok {
		t.Fatalf("the pending run could not take the freed group: %v (%v)", ok, aerr)
	}
	doc := loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != second[0].ID || doc.Pending != "" {
		t.Fatalf("group after promotion = %+v", doc)
	}
}

// TestConcurrencyCancelInProgressCancelsAtCreation pins the documented timing:
// with cancel-in-progress true the holder is cancelled when the newer run is
// CREATED, not when a runner eventually polls.
func TestConcurrencyCancelInProgressCancelsAtCreation(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	e.git.add(testSHA, ".spindle/workflows/ci.yml", cancelGroupWorkflow)
	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(second) != 1 {
		t.Fatal(err)
	}
	if second[0].PendingGroup != "" {
		t.Fatalf("a cancel-in-progress run waited on %q", second[0].PendingGroup)
	}
	old, err := e.svc.loadRun(ctx(), "o", "r", first[0].ID)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	if old.State != RunCancelled || old.CancelReason != CancelReasonConcurrency {
		t.Fatalf("holder after a cancel-in-progress creation = %s / %q", old.State, old.CancelReason)
	}
	doc := loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != "" || doc.Pending != "" {
		t.Fatalf("cancelled holder left the group dirty: %+v", doc)
	}
}

// TestConcurrencyCancelInProgressSupersedesThePendingRun pins the other half of
// cancel-in-progress: it cancels the pending run it replaces too, so a group
// never keeps a stale waiter behind a cancelled holder.
func TestConcurrencyCancelInProgressSupersedesThePendingRun(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(second) != 1 {
		t.Fatal(err)
	}
	if doc := loadGroupDoc(t, e, "deploy-main"); doc.Pending != second[0].ID {
		t.Fatalf("precondition: pending = %q", doc.Pending)
	}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", cancelGroupWorkflow)
	third, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(third) != 1 {
		t.Fatalf("cancel-in-progress creation = %d (%v)", len(third), err)
	}
	for _, run := range []*Run{first[0], second[0]} {
		got, lerr := e.svc.loadRun(ctx(), "o", "r", run.ID)
		if lerr != nil || got == nil {
			t.Fatal(lerr)
		}
		if got.State != RunCancelled {
			t.Fatalf("run %s survived cancel-in-progress: %s", got.ID, got.State)
		}
	}
	doc := loadGroupDoc(t, e, "deploy-main")
	if doc.RunID != "" || doc.Pending != "" {
		t.Fatalf("cancel-in-progress left the group dirty: %+v", doc)
	}
}

// TestAdmitCancelInProgressCancelsAPendingBlocker pins the claim-path arm where
// the blocker is the PENDING run (the holder already folded): cancel-in-progress
// cancels it and takes the group.
func TestAdmitCancelInProgressCancelsAPendingBlocker(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	second, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(second) != 1 {
		t.Fatal(err)
	}
	finishJob(t, e, first[0]) // the holder folds; the pending run owns the slot
	ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", "9999999999999999999-ffffff", &Spec{
		Concurrency: &ConcurrencySpec{Group: "deploy-main", CancelInProgress: true},
	})
	if aerr != nil || !ok {
		t.Fatalf("cancel-in-progress past a pending blocker = %v (%v)", ok, aerr)
	}
	pending, err := e.svc.loadRun(ctx(), "o", "r", second[0].ID)
	if err != nil || pending == nil {
		t.Fatal(err)
	}
	if pending.State != RunCancelled {
		t.Fatalf("the pending blocker was not cancelled: %s", pending.State)
	}
}

// TestConcurrencyPendingRunCreatedBehindAHolder pins the CREATION-time header:
// a run created while another holds the group carries pending_group (the run
// list badge names it) and never takes the supersede head.
func TestConcurrencyPendingRunCreatedBehindAHolder(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	e.git.add(sha1, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.add(sha2, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = sha1
	first, err := e.svc.CreateRun(ctx(), "o/r", sha1, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	e.git.refs["refs/heads/main"] = sha2
	second, err := e.svc.CreateRun(ctx(), "o/r", sha2, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(second) != 1 {
		t.Fatalf("second creation = %d (%v)", len(second), err)
	}
	if second[0].PendingGroup != "deploy-main" {
		t.Fatalf("pending run header = %q, want deploy-main", second[0].PendingGroup)
	}
	doc := loadGroupDoc(t, e, "deploy-main")
	if doc.Pending != second[0].ID {
		t.Fatalf("group = %+v, want pending %s", doc, second[0].ID)
	}
	// The holder finishing clears pending_group on the promoted run's fold, and
	// the promoted run's own fold clears its header field.
	finishJob(t, e, first[0])
	claim, err := e.svc.claimLoop(ctx(), "o", "r", needsRunner(), needsRunner().Labels)
	if err != nil || claim == nil || claim.Entry.RunID != second[0].ID {
		t.Fatalf("promoted claim = %+v (%v)", claim, err)
	}
	ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", claim.Entry.RunID, claim.Job.Spec)
	if aerr != nil || !ok {
		t.Fatalf("admit promoted = %v (%v)", ok, aerr)
	}
	if _, uerr := e.svc.UpdateJob(ctx(), "o/r", second[0].ID, claim.Entry.JobID, JobUpdate{State: JobRunning}); uerr != nil {
		t.Fatalf("start promoted job: %v", uerr)
	}
	reloaded, err := e.svc.loadRun(ctx(), "o", "r", second[0].ID)
	if err != nil || reloaded == nil {
		t.Fatal(err)
	}
	if reloaded.PendingGroup != "" {
		t.Fatalf("a running run still claims to wait on %q", reloaded.PendingGroup)
	}
}

// TestConcurrencyGroupFreeRunIsNotPending pins the no-cost path: with a free
// group the run header carries no pending group.
func TestConcurrencyGroupFreeRunIsNotPending(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(runs) != 1 {
		t.Fatal(err)
	}
	if runs[0].PendingGroup != "" {
		t.Fatalf("a free group made the run pending: %q", runs[0].PendingGroup)
	}
	// A job of a group-free workflow never touches a group record.
	e.git.add(testSHA, ".spindle/workflows/other.yml", "name: Other\non: [push]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: 'true'\n")
	runs, err = e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Workflow == "Other" && len(r.Concurrency) != 0 {
			t.Fatalf("group-free run holds %v", r.Concurrency)
		}
	}
}
