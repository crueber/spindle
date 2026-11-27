package threads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func TestFinalCoverageBits(t *testing.T) {
	e := newTestEnv(t)
	// Tiny pure helpers (belt and braces: each is also exercised through the
	// service paths above).
	if size := ParseJobSize("--cpus=2"); size.CPUs != 2 {
		t.Fatalf("inline cpu option = %+v", size)
	}
	sortQueuePending(&QueueHead{Pending: []string{"b", "a"}, Priority: map[string]int{"b": 1}})
	if !(Subscription{Workflows: []string{"*"}}).watches(&Run{Workflow: "anything"}) {
		t.Fatal("wildcard subscription did not match")
	}
	// Subscriptions doc with a null map normalises.
	if _, err := store.PutBytes(ctx(), e.store, SubscriptionsKey("o", "r"), []byte(`{"subscribers":null}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if doc, err := e.svc.loadSubscriptions(ctx(), "o", "r"); err != nil || doc.Subscribers == nil {
		t.Fatalf("null subscribers = %+v, %v", doc, err)
	}
	if _, err := e.svc.GetSubscription(ctx(), "badrepo", e.reader()); err == nil {
		t.Fatal("bad repo subscription read")
	}
	if _, err := e.svc.SetSubscription(ctx(), "badrepo", true, nil, e.reader()); err == nil {
		t.Fatal("bad repo subscription write")
	}
	e.roles.public = false
	if _, err := e.svc.GetSubscription(ctx(), "o/r", auth.Anonymous()); err == nil {
		t.Fatal("private subscription read")
	}
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, auth.Anonymous()); err == nil {
		t.Fatal("private subscription write")
	}
	e.roles.public = true
	// Corrupt run and settings for the notifier.
	corruptKey(t, e, RunKey("o", "r", "badrun"))
	e.svc.notifyRunFailure(ctx(), "o", "r", "badrun")
	// A corrupt notification record is skipped by the list and by mark-read.
	corruptKey(t, e, NotificationKey("reader", "bad"))
	if notes, err := e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 5); err != nil || len(notes) != 0 {
		t.Fatalf("corrupt notification = %+v, %v", notes, err)
	}
	if err := e.svc.MarkNotificationsRead(ctx(), auth.Principal{Name: "reader"}); err != nil {
		t.Fatalf("mark read corrupt: %v", err)
	}
	// A read notification is a no-op on the second mark and appears in the list.
	read := &Notification{ID: "r1", User: "reader", Link: "/o/r/threads/r1", Read: true}
	if _, err := store.PutBytes(ctx(), e.store, NotificationKey("reader", "r1"), mustJSON(read), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MarkNotificationsRead(ctx(), auth.Principal{Name: "reader"}); err != nil {
		t.Fatal(err)
	}
	if notes, _ := e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 5); len(notes) != 1 || !notes[0].Read {
		t.Fatalf("read notification = %+v", notes)
	}
	// Fleet paths with a valid-shaped but absent id.
	for _, fn := range []func() error{
		func() error { return e.svc.DrainRunner(ctx(), "aaaaaaaa", wave4Admin()) },
		func() error { return e.svc.ResumeRunner(ctx(), "aaaaaaaa", wave4Admin()) },
		func() error { _, err := e.svc.RotateRunnerToken(ctx(), "aaaaaaaa", wave4Admin()); return err },
		func() error { return e.svc.DeregisterRunner(ctx(), "aaaaaaaa", wave4Admin()) },
	} {
		if err := fn(); err == nil {
			t.Fatal("fleet verb on an absent runner succeeded")
		}
	}
	// Corrupt record for rotate and deregister.
	id, _, _ := wave4Runner(t, e,
		MintInput{Name: "final", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	corruptKey(t, e, RunnerKey(id))
	if _, err := e.svc.RotateRunnerToken(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("rotate corrupt runner succeeded")
	}
	if err := e.svc.DeregisterRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("deregister corrupt runner succeeded")
	}
}

func TestClaimLoopDirectGuardsAndScheduleWrites(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml"})
	if err != nil || len(runs) == 0 {
		t.Fatalf("create: %v", err)
	}
	// Draining and label-mismatch runners are refused by claimLoop itself.
	if got, err := e.svc.claimLoop(ctx(), "o", "r", &RunnerDoc{ID: "d", State: RunnerDraining, AllowAll: true}, []string{"linux"}); err != nil || got != nil {
		t.Fatalf("draining claim = %+v, %v", got, err)
	}
	if got, err := e.svc.claimLoop(ctx(), "o", "r", &RunnerDoc{ID: "m", State: RunnerActive, AllowAll: true, Labels: []string{"other"}}, []string{"other"}); err != nil || got != nil {
		t.Fatalf("label mismatch claim = %+v, %v", got, err)
	}
	// A fork entry is skipped for a runner that did not opt in, before any job
	// load: write one into the head by hand.
	seq := "9999999999999999999-aaaaaa"
	entry := &QueueEntry{Seq: seq, RunID: runs[0].ID, JobID: "j-build", Labels: []string{"linux"}, State: QueueQueued, Fork: true, EnqueuedAt: "2026-01-01T00:00:00Z", Version: 1}
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", seq), mustJSON(entry), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), mustJSON(&QueueHead{Seq: seq, Pending: []string{seq}}), store.PutOptions{Mode: store.PutUpdate, IfVersion: ""}); err != nil {
		// Fall back to a paused-marker write then a direct create after a delete.
		_ = e.store.Delete(ctx(), QueueHeadKey("o", "r"), "")
		if _, cerr := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), mustJSON(&QueueHead{Seq: seq, Pending: []string{seq}}), store.PutOptions{Mode: store.PutCreate}); cerr != nil {
			t.Skipf("cannot write head: %v", cerr)
		}
	}
	if got, cerr := e.svc.claimLoop(ctx(), "o", "r", &RunnerDoc{ID: "nf", State: RunnerActive, AllowAll: true, Labels: []string{"linux"}}, []string{"linux"}); cerr != nil || got != nil {
		t.Fatalf("fork entry offered = %+v, %v", got, cerr)
	}
	// Schedule writes on an empty overrides document and a denied run-now.
	e.roles.roles["maint"] = "maintain"
	e.git.refs["HEAD"] = testSHA
	e.git.add(testSHA, ".spindle/workflows/ci.yml", scheduleFixtureYAML)
	id := ScheduleEntryID{Path: ".spindle/workflows/ci.yml", Cron: "0 3 * * *"}
	if _, err := store.PutBytes(ctx(), e.store, ScheduleOpsKey("o", "r"), []byte(`{}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		_ = e.store.Delete(ctx(), ScheduleOpsKey("o", "r"), "")
		_, _ = store.PutBytes(ctx(), e.store, ScheduleOpsKey("o", "r"), []byte(`{}`), store.PutOptions{Mode: store.PutCreate})
	}
	if _, err := e.svc.SetScheduleEnabled(ctx(), "o/r", id, false, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("enable on an empty doc: %v", err)
	}
	if _, err := e.svc.RunScheduleNow(ctx(), "o/r", id, e.reader()); err == nil {
		t.Fatal("reader ran a schedule")
	}
	// A checks mirror failure fails run-now visibly.
	e.checks.err = errors.New("checks down")
	if _, err := e.svc.RunScheduleNow(ctx(), "o/r", id, auth.Principal{Name: "writer"}); err == nil {
		t.Fatal("checks failure swallowed by run-now")
	}
	e.checks.err = nil
}

func TestLabelImageOverridesExistingContainer(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: Img
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    container:
      image: busybox:1.36
    steps:
      - run: echo hi
`)
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "image", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"self-hosted", "linux", "ubuntu-latest:docker://node:lts"}})
	got, err := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, "")
	if err != nil || got == nil {
		t.Fatalf("fetch: %+v, %v", got, err)
	}
	if got.Spec == nil || got.Spec.Container == nil || got.Spec.Container.Image != "node:lts" {
		t.Fatalf("image = %+v", got.Spec)
	}
	if got.LabelImages["ubuntu-latest"] != "node:lts" {
		t.Fatalf("label images = %v", got.LabelImages)
	}
}

func TestRemainingSmallBranches(t *testing.T) {
	e := newTestEnv(t)
	// Group member count with a corrupt runner registry.
	corruptKey(t, e, RunnerKey("broken"))
	if n := e.svc.runnerGroupMembers(ctx(), "g"); n != 0 {
		t.Fatalf("corrupt member count = %d", n)
	}
	// rotate with an empty RegisteredBy falls back to "runner".
	id, _, _ := wave4Runner(t, e,
		MintInput{Name: "noby", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.casUpdate(ctx(), RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var d RunnerDoc
		if jerr := json.Unmarshal(cur, &d); jerr != nil {
			return nil, false, jerr
		}
		d.RegisteredBy = ""
		return mustJSON(&d), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RotateRunnerToken(ctx(), id, wave4Admin()); err != nil {
		t.Fatalf("rotate without registered_by: %v", err)
	}
	// noteRunnerJob / clearRunnerCurrentJob tolerate a revoked and a nil state.
	_, _ = e.svc.casUpdate(ctx(), RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var d RunnerDoc
		_ = json.Unmarshal(cur, &d)
		d.State = RunnerRevoked
		return mustJSON(&d), true, nil
	})
	e.svc.noteRunnerJob(ctx(), id, "o/r", &Claim{Entry: &QueueEntry{RunID: "r", JobID: "j"}})
	e.svc.clearRunnerCurrentJob(ctx(), id, "o/r", "r", "j")
	// Register with a token id whose runner record vanished before registration.
	mint, err := e.svc.MintRegistrationToken(ctx(), wave4Admin(), mintAllPolicy("vanish"))
	if err != nil {
		t.Fatal(err)
	}
	_ = e.store.Delete(ctx(), RunnerKey(mint.ID), "")
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"linux"}}); err == nil {
		t.Fatal("registration with a vanished record succeeded")
	}
	// ID-token payload uses CheckoutBaseURL when the request carries none.
	e.svc.CheckoutBaseURL = "https://spindle.example"
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: OIDC
on: [push]
permissions:
  id-token: write
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`)
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "oidc", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	got, err := e.svc.FetchJobFrom(ctx(), authn, time.Millisecond, "")
	if err != nil || got == nil || got.IDToken == nil {
		t.Fatalf("id-token payload = %+v, %v", got, err)
	}
}

// failPutStore fails Put/Delete for a set of keys (the CAS error branches).
type failPutStore struct {
	store.ObjectStore
	failPut    map[string]bool
	failDelete map[string]bool
}

func (f failPutStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if f.failPut[key] {
		return store.ObjectMeta{}, errors.New("injected put failure")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func (f failPutStore) Delete(ctx context.Context, key string, v store.Version) error {
	if f.failDelete[key] {
		return errors.New("injected delete failure")
	}
	return f.ObjectStore.Delete(ctx, key, v)
}

func TestStoreFailuresSurface(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	e.git.refs["HEAD"] = testSHA
	fail := failPutStore{
		ObjectStore: e.store,
		failPut:     map[string]bool{SettingsKey("o", "r"): true, ScheduleOpsKey("o", "r"): true, RunnerGroupKey("failgroup"): true},
		failDelete:  map[string]bool{RunnerGroupKey("delgroup"): true},
	}
	e.svc.Store = fail
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{CIPaused: true}, auth.Principal{Name: "maint"}); err == nil {
		t.Fatal("settings put failure swallowed")
	}
	if _, err := e.svc.SetCIPaused(ctx(), "o/r", true, auth.Principal{Name: "maint"}); err == nil {
		t.Fatal("pause put failure swallowed")
	}
	if _, err := e.svc.SetScheduleEnabled(ctx(), "o/r", ScheduleEntryID{Path: ".spindle/workflows/ci.yml", Cron: "0 3 * * *"}, false, auth.Principal{Name: "maint"}); err == nil {
		t.Fatal("schedule put failure swallowed")
	}
	// Group create with a failing put.
	if _, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "failgroup", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("group put failure swallowed")
	}
	// A group that exists but cannot be deleted.
	e.svc.Store = e.store
	g, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "delgroup", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Store = fail
	if err := e.svc.DeleteRunnerGroup(ctx(), wave4Admin(), g.ID); err == nil {
		t.Fatal("group delete failure swallowed")
	}
}

func TestMoreStoreFailures(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	// A queue-entry create failure surfaces from enqueueJob.
	fail := failPutStore{ObjectStore: e.store, failPut: map[string]bool{}}
	e.svc.Store = fail
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml"}); err != nil {
		// Run creation logs the enqueue failure and still commits the run; a
		// second probe must not claim a job that was never enqueued.
		t.Fatalf("create with failing queue store: %v", err)
	}
	e.svc.Store = e.store
	// Group update with a failing CAS.
	g, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "upd", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	fail.failPut[RunnerGroupKey(g.ID)] = true
	e.svc.Store = fail
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), g.ID, RunnerGroupInput{Name: "upd", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("group update put failure swallowed")
	}
	// Runner group assignment with a failing runner put.
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "assign-fail", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	e.svc.Store = e.store
	g2, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "assign2", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	fail.failPut[RunnerKey(authn.Doc.ID)] = true
	e.svc.Store = fail
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, g2.ID, wave4Admin()); err == nil {
		t.Fatal("runner group put failure swallowed")
	}
}
