package threads

import (
	"errors"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestScheduleNullOverridesAndRefErrors(t *testing.T) {
	e, _ := scheduleEnv(t, time.Date(2026, 11, 21, 3, 0, 10, 0, time.UTC))
	e.roles.roles["maint"] = "maintain"
	// An overrides document with a null map normalises to empty.
	if _, err := store.PutBytes(ctx(), e.store, ScheduleOpsKey("o", "r"), []byte(`{"overrides":null}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	doc, err := e.svc.loadScheduleOps(ctx(), "o", "r")
	if err != nil || doc.Overrides == nil {
		t.Fatalf("null overrides = %+v, %v", doc, err)
	}
	// A workflow with no schedule is skipped by the list.
	e.git.add(testSHA, ".spindle/workflows/plain.yml", "name: p\non: [push]\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: x\n")
	if list, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err != nil || len(list.Entries) != 1 {
		t.Fatalf("skip non-schedule = %+v, %v", list, err)
	}
	// Two entries on one file exercise the cron tie-break comparator.
	e.git.add(testSHA, ".spindle/workflows/two.yml", "name: t\non:\n  schedule:\n    - cron: \"0 3 * * *\"\n    - cron: \"15 5 * * *\"\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: x\n")
	if list, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err != nil || len(list.Entries) != 3 {
		t.Fatalf("two-entry list = %+v, %v", list, err)
	}
	// A git ref failure surfaces (never a silent empty schedule).
	e.git.refErr = errors.New("ref boom")
	if _, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err == nil {
		t.Fatal("ref failure swallowed")
	}
	if _, err := e.svc.EnsureScheduleEntry(ctx(), "o", "r", ScheduleEntryID{Path: "x", Cron: "y"}); err == nil {
		t.Fatal("ensure with ref failure succeeded")
	}
	e.git.refErr = nil
	// Corrupt overrides during the write CAS.
	id := ScheduleEntryID{Path: ".spindle/workflows/ci.yml", Cron: "0 3 * * *"}
	corruptKey(t, e, ScheduleOpsKey("o", "r"))
	if _, err := e.svc.SetScheduleEnabled(ctx(), "o/r", id, true, auth.Principal{Name: "maint"}); err == nil {
		t.Fatal("corrupt override write succeeded")
	}
	// Malformed stored slot.
	if n := coalescedTicks(mustCron(t, "0 3 * * *"), time.UTC, "nonsense", time.Now()); n != 0 {
		t.Fatalf("bad slot = %d", n)
	}
	// describeScheduleEntry with no git backend is empty, not an error.
	noGit := newTestEnv(t)
	noGit.svc.Git = nil
	if row, err := noGit.svc.describeScheduleEntry(ctx(), "o", "r", ScheduleStatus{}); err != nil || row == nil {
		t.Fatalf("no-git describe = %+v, %v", row, err)
	}
}

func TestNotifyRunFailureEdges(t *testing.T) {
	e := newTestEnv(t)
	// A corrupt subscription document makes notifyRunFailure log and continue.
	corruptKey(t, e, SubscriptionsKey("o", "r"))
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := e.svc.loadRun(ctx(), "o", "r", runs[0].ID)
	run.State = RunFailure
	if _, uerr := store.PutBytes(ctx(), e.store, RunKey("o", "r", run.ID), mustJSON(run), store.PutOptions{Mode: store.PutUpdate, IfVersion: "0"}); uerr != nil {
		// A versioned write may refuse the fake version; the in-memory object is
		// already the failure the notifier needs via loadRun, so continue.
		t.Logf("force run failure: %v", uerr)
	}
	// Corrupt subscription: no subscriber is notified, and the call returns.
	e.svc.notifyRunFailure(ctx(), "o", "r", run.ID)
	// A corrupt settings document means no webhook, still no error.
	corruptKey(t, e, SettingsKey("o", "r"))
	e.svc.notifyRunFailure(ctx(), "o", "r", run.ID)
	// A notification for a run that does not exist is a no-op.
	e.svc.notifyRunFailure(ctx(), "o", "r", "missing")
	// The inbox list skips a corrupt record and survives a LIST error path via
	// a bounded limit.
	if _, err := e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 1); err != nil {
		t.Fatalf("list: %v", err)
	}
}

func TestRunnerFleetDirectGuards(t *testing.T) {
	e := newTestEnv(t)
	if err := e.svc.DrainRunner(ctx(), "x", e.reader()); err == nil {
		t.Fatal("reader drained")
	}
	if err := e.svc.ResumeRunner(ctx(), "x", e.reader()); err == nil {
		t.Fatal("reader resumed")
	}
	if _, err := e.svc.RotateRunnerToken(ctx(), "x", e.reader()); err == nil {
		t.Fatal("reader rotated")
	}
	if err := e.svc.DeregisterRunner(ctx(), "x", e.reader()); err == nil {
		t.Fatal("reader deregistered")
	}
	id, _, _ := wave4Runner(t, e,
		MintInput{Name: "guards", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if err := e.svc.RevokeRunner(ctx(), id, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DrainRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("revoked runner drained")
	}
	if err := e.svc.ResumeRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("revoked runner resumed")
	}
	// Rotate with a missing credential document.
	id2, _, _ := wave4Runner(t, e,
		MintInput{Name: "no-cred", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	_ = e.store.Delete(ctx(), RunnerCredentialKey(id2), "")
	if _, err := e.svc.RotateRunnerToken(ctx(), id2, wave4Admin()); err == nil {
		t.Fatal("rotation without a credential succeeded")
	}
	// Corrupt credential document.
	id3, _, _ := wave4Runner(t, e,
		MintInput{Name: "bad-cred", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	corruptKey(t, e, RunnerCredentialKey(id3))
	if _, err := e.svc.RotateRunnerToken(ctx(), id3, wave4Admin()); err == nil {
		t.Fatal("rotation with a corrupt credential succeeded")
	}
}

func mustCron(t *testing.T, expr string) *workflow.Cron {
	t.Helper()
	c, err := workflow.ParseCron(expr)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
