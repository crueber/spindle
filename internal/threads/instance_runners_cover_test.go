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

// Coverage of the instance-runner error paths and the queue-index hint.

func TestRunnerRegistryErrorPaths(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	id, token, _ := instanceRunner(t, e, admin, []string{"linux"})

	// A corrupt registry record fails the fleet, revoke and authorize.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(id), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ListRunners(ctx(), admin); err == nil {
		t.Fatal("corrupt registry list must fail")
	}
	if err := e.svc.RevokeRunner(ctx(), id, admin); err == nil {
		t.Fatal("corrupt registry revoke must fail")
	}
	if _, err := e.svc.authorizeRunner(ctx(), token); err == nil {
		t.Fatal("corrupt registry authorize must fail")
	}

	// A corrupt credential fails authorize and the credential revoke.
	if _, err := store.PutBytes(ctx(), e.store, RunnerCredentialKey("credcorrupt1"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := e.svc.loadRunnerCredential(ctx(), "credcorrupt1"); err == nil {
		t.Fatal("corrupt credential load must fail")
	}
	if err := e.svc.revokeRunnerCredential(ctx(), "credcorrupt1", "now"); err == nil {
		t.Fatal("corrupt credential revoke must fail")
	}
	// A missing credential/registry record is (nil, nil).
	if cred, _, err := e.svc.loadRunnerCredential(ctx(), "absent000001"); cred != nil || err != nil {
		t.Fatalf("absent credential = %+v, %v", cred, err)
	}
	if doc, _, err := e.svc.loadRunner(ctx(), "absent000002"); doc != nil || err != nil {
		t.Fatalf("absent runner = %+v, %v", doc, err)
	}

	// A corrupt registration token fails register and the token revoke.
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, RunnerTokenKey(mint.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("corrupt token register must fail")
	}
	if err := e.svc.revokeRunnerTokenDoc(ctx(), mint.ID, "now"); err == nil {
		t.Fatal("corrupt token revoke must fail")
	}
	if tok, _, err := e.svc.loadRunnerToken(ctx(), "absent000003"); tok != nil || err != nil {
		t.Fatalf("absent token = %+v, %v", tok, err)
	}
	// Revoking an already-revoked credential and a consumed token is a no-op.
	if err := e.svc.revokeRunnerCredential(ctx(), "absent000004", "now"); err != nil {
		t.Fatalf("absent credential revoke = %v", err)
	}
	if err := e.svc.revokeRunnerTokenDoc(ctx(), "absent000005", "now"); err != nil {
		t.Fatalf("absent token revoke = %v", err)
	}
}

func TestRegisterDefaultsAndCapacity(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("  a very long name  "))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Capacity: 999})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(reg.Labels) != 1 || reg.Labels[0] != "self-hosted" {
		t.Fatalf("default labels = %v", reg.Labels)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), reg.RunnerID)
	if doc.Capacity != runnerCapacityMax {
		t.Fatalf("capacity = %d, want %d", doc.Capacity, runnerCapacityMax)
	}
	// clampCapacity: zero means "not supplied", negatives floor.
	if clampCapacity(0) != 0 || clampCapacity(-3) != 0 || clampCapacity(-1) != 0 {
		t.Fatal("clampCapacity zero/negative")
	}
	if clampCapacity(1) != 1 || clampCapacity(4) != 4 {
		t.Fatal("clampCapacity in range")
	}
}

func TestQueueIndexErrorPaths(t *testing.T) {
	e := newTestEnv(t)
	// A corrupt index fails the load and the claim (never a silent 204).
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := e.svc.loadQueueIndex(ctx()); err == nil {
		t.Fatal("corrupt index load must fail")
	}
	if err := e.svc.appendQueueIndex(ctx(), "o/r", "r", "j", []string{"linux"}, false, 0, "now"); err == nil {
		t.Fatal("append onto a corrupt index must fail")
	}
	runner := &RunnerDoc{ID: "rnridx000001", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}
	if _, _, _, err := e.svc.claimGlobal(ctx(), runner, runner.Labels, nil); err == nil {
		t.Fatal("claim with a corrupt index must fail")
	}
	// removeQueueIndexRows tolerates a corrupt document (best effort).
	e.svc.removeQueueIndexJob(ctx(), "o/r", "r", "j")

	// Reconcile keeps another repository's rows and skips corrupt/non-queued
	// entries.
	fresh := &QueueIndex{Version: 1, Queued: []QueueIndexEntry{{Repo: "x/y", RunID: "r0", JobID: "j0", Labels: []string{"linux"}}}}
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), mustJSON(fresh), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, QueuePrefix("o", "r")+"0000000000000000001-abc.json", []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claimed := &QueueEntry{Seq: "0000000000000000002-abc", RunID: "r2", JobID: "j2", State: QueueClaimed}
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", claimed.Seq), mustJSON(claimed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); err != nil || n != 0 {
		t.Fatalf("reconcile empty = %d, %v", n, err)
	}
	idx, _, _ := e.svc.loadQueueIndex(ctx())
	if idx == nil || len(idx.Queued) != 1 || idx.Queued[0].Repo != "x/y" {
		t.Fatalf("reconcile preserved other rows = %+v", idx)
	}
}

func TestReconcileQueueIndexOverflow(t *testing.T) {
	e := newTestEnv(t)
	// Fill the index with other repositories' rows, then reconcile one repo.
	full := &QueueIndex{Version: 1}
	for i := 0; i < queueIndexMax; i++ {
		full.Queued = append(full.Queued, QueueIndexEntry{Repo: "x/y", RunID: "r", JobID: "j", Labels: []string{"linux"}})
	}
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), mustJSON(full), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// One queued entry in o/r not present in the index.
	entry := &QueueEntry{Seq: "0000000000000000001-abc", RunID: "r1", JobID: "j1", State: QueueQueued, EnqueuedAt: "now"}
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", entry.Seq), mustJSON(entry), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	if _, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	idx, _, _ := e.svc.loadQueueIndex(ctx())
	if idx == nil || len(idx.Queued) != queueIndexMax {
		t.Fatalf("overflow bound = %d", len(idx.Queued))
	}
}

func TestRunnerCurrentJobEdges(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	run := instanceSingleJob(t, e)
	id, _, runner := instanceRunner(t, e, admin, []string{"linux"})
	if _, err := e.svc.FetchJob(ctx(), runner, time.Millisecond); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// A different job does not clear the current lease.
	e.svc.clearRunnerCurrentJob(ctx(), id, "o/r", run.ID, "j-other")
	doc, _, _ := e.svc.loadRunner(ctx(), id)
	if doc.CurrentJob == nil {
		t.Fatal("mismatched clear removed the lease")
	}
	// noteRunnerJob on a corrupt/unregistered record is a no-op.
	claim := &Claim{Entry: &QueueEntry{RunID: run.ID, JobID: "j-build"}, EntryVersion: "1"}
	e.svc.noteRunnerJob(ctx(), "absent000009", "o/r", claim)
	e.svc.noteRunnerJob(ctx(), "", "o/r", claim)
	e.svc.noteRunnerJob(ctx(), id, "o/r", nil)
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(id), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	e.svc.noteRunnerJob(ctx(), id, "o/r", claim)
	e.svc.clearRunnerCurrentJob(ctx(), id, "o/r", run.ID, "j-build")
}

func TestAuditAdminEdges(t *testing.T) {
	e := newTestEnv(t)
	// A nil audit records nothing.
	e.svc.auditAdmin(ctx(), "admin", "x", "runner:y", "d")
	// A failing audit is logged, never fatal or reverted.
	called := false
	e.svc.Audit = func(context.Context, string, string, string, string) error {
		called = true
		return errors.New("audit down")
	}
	e.svc.auditAdmin(ctx(), "admin", "x", "runner:y", "d")
	if !called {
		t.Fatal("audit seam not called")
	}
}

func TestRunnerRegisterResultHasNoRepoField(t *testing.T) {
	// The wire answer is instance-scoped: a "repo" key would reintroduce the
	// deleted repository binding.
	raw, _ := json.Marshal(RegisterResult{RunnerID: "x", Token: "t", Labels: []string{"linux"}})
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["repo"]; ok {
		t.Fatalf("register answer has a repo: %s", raw)
	}
}

func TestInstanceRunnerMoreEdges(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}

	// Register rejects a non-registration token shape and a revoked token.
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: "wrt_x"}); err == nil {
		t.Fatal("a non-registration token must be refused")
	}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := e.svc.RevokeRunner(ctx(), mint.ID, admin); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("a revoked registration token must fail")
	}
	// A valid token whose registry row vanished fails register.
	mint2, _ := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err := e.store.Delete(ctx(), RunnerKey(mint2.ID), ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint2.Token}); err == nil {
		t.Fatal("a vanished registry row must fail register")
	}
	// Revoke rejects a malformed id and an unknown id.
	if err := e.svc.RevokeRunner(ctx(), "bad!", admin); err == nil {
		t.Fatal("malformed id must fail")
	}
	if err := e.svc.RevokeRunner(ctx(), "abcdefgh9999", admin); err == nil {
		t.Fatal("unknown id must fail")
	}
	// An index append failure surfaces from enqueue (the run still exists; the
	// caller logs it and the reconciler repairs the hint).
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	job := &Job{ID: "j-idxfail", RunID: "1795176002000000000-abcdef", Attempt: 1, RunsOn: "linux", State: JobQueued}
	if _, err := e.svc.enqueueJob(ctx(), "o", "r", job, nil); err == nil {
		t.Fatal("enqueue with a corrupt index must report the hint failure")
	}
	// A malformed/foreign index row is skipped by the global claim.
	idx := &QueueIndex{Version: 1, Queued: []QueueIndexEntry{
		{Repo: "not-a-repo", RunID: "r", JobID: "j", Labels: []string{"linux"}},
		{Repo: "x/y", RunID: "r", JobID: "j", Labels: []string{"windows"}},
	}}
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), mustJSON(idx), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner := &RunnerDoc{ID: "rnrmore00001", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}
	if claim, _, _, err := e.svc.claimGlobal(ctx(), runner, runner.Labels, nil); err != nil || claim != nil {
		t.Fatalf("malformed rows = %+v, %v", claim, err)
	}
	// Capacity and name helpers.
	if clampCapacity(2) != 2 || clampCapacity(-1) != 0 {
		t.Fatal("clampCapacity")
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Fatalf("truncate = %q", got)
	}
	// An empty index is an empty claim.
	e.svc.Store = store.NewMemory()
	if claim, _, _, err := e.svc.claimGlobal(ctx(), runner, runner.Labels, nil); err != nil || claim != nil {
		t.Fatalf("empty index = %+v, %v", claim, err)
	}
}

func TestInstanceRunnerEntropyAndHintGuards(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	// A token minted before the entropy failure lets register reach its own
	// mint path.
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	prev := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = prev })
	if _, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy("")); err == nil {
		t.Fatal("mint without entropy must fail")
	}
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token}); err == nil {
		t.Fatal("register without entropy must fail")
	}
	// enqueueJob's sequence mint fails the same way (never a half entry).
	job := &Job{ID: "j-entropy", RunID: "1795176002000000000-abcdef", Attempt: 1, RunsOn: "linux", State: JobQueued}
	if _, err := e.svc.enqueueJob(ctx(), "o", "r", job, nil); err == nil {
		t.Fatal("enqueue without entropy must fail")
	}
	randRead = prev

	// The hint helpers tolerate empty/nil inputs.
	if err := e.svc.appendQueueIndex(ctx(), "", "", "", nil, false, 0, ""); err != nil {
		t.Fatalf("empty append = %v", err)
	}
	e.svc.removeQueueIndexRows(ctx(), nil)
	// A corrupt head is reported by appendHead.
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.svc.appendHead(ctx(), "o", "r", "0000000000000000009-abc", 0); err == nil {
		t.Fatal("corrupt head append must fail")
	}
}

func TestInstanceRunnerStoreErrors(t *testing.T) {
	fs := newFaultStore()
	e := newTestEnv(t)
	e.svc.Store = fs
	admin := auth.Principal{Name: "admin", Admin: true}
	// Seed the families through the memory backend.
	doc := &RunnerDoc{ID: "rnrfault0001", State: RunnerActive, CreatedAt: "now", Version: 1}
	for key, body := range map[string][]byte{
		RunnerKey(doc.ID):           mustJSON(doc),
		RunnerCredentialKey(doc.ID): mustJSON(&RunnerCredential{ID: doc.ID, TokenHash: hashSecret("secretsecretsecret"), CreatedAt: "now", Version: 1}),
		RunnerTokenKey(doc.ID):      mustJSON(&runnerTokenDoc{ID: doc.ID, TokenHash: hashSecret("secretsecretsecret"), CreatedAt: "now", ExpiresAt: "later", Version: 1}),
		QueueIndexKey():             mustJSON(&QueueIndex{Version: 1}),
		QueueHeadKey("o", "r"):      mustJSON(&QueueHead{Version: 1}),
	} {
		if _, err := store.PutBytes(ctx(), fs, key, body, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	for name, call := range map[string]func() error{
		"load runner":     func() error { _, _, err := e.svc.loadRunner(ctx(), doc.ID); return err },
		"load credential": func() error { _, _, err := e.svc.loadRunnerCredential(ctx(), doc.ID); return err },
		"load token":      func() error { _, _, err := e.svc.loadRunnerToken(ctx(), doc.ID); return err },
		"list runners":    func() error { _, err := e.svc.ListRunners(ctx(), admin); return err },
		"revoke runner":   func() error { return e.svc.RevokeRunner(ctx(), doc.ID, admin) },
		"reconcile":       func() error { _, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); return err },
	} {
		fs.fail("get", 0)
		if err := call(); err == nil {
			t.Fatalf("%s with a get fault must fail", name)
		}
		fs.clear()
	}
	// A LIST fault fails the fleet/reconcile.
	fs.fail("list", 0)
	if _, err := e.svc.ListRunners(ctx(), admin); err == nil {
		t.Fatal("list with a list fault must fail")
	}
	fs.clear()
	fs.fail("list", 0)
	if _, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); err == nil {
		t.Fatal("reconcile with a list fault must fail")
	}
	fs.clear()
	// A corrupt index fails reconcile's CAS document read.
	if _, err := store.PutBytes(ctx(), fs, QueueIndexKey(), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); err == nil {
		t.Fatal("reconcile with a corrupt index must fail")
	}
}

func TestReconcileKeepsLiveRowsWhenDroppingStale(t *testing.T) {
	e := newTestEnv(t)
	seed := &QueueIndex{Version: 1, Queued: []QueueIndexEntry{
		{Repo: "o/r", RunID: "r1", JobID: "j1", Labels: []string{"linux"}, CreatedAt: "a"},
		{Repo: "o/r", RunID: "r2", JobID: "j2", Labels: []string{"linux"}, CreatedAt: "b"},
		{Repo: "o/r", RunID: "r3", JobID: "j3", Labels: []string{"linux"}, CreatedAt: "c"}, // stale
		{Repo: "x/y", RunID: "r0", JobID: "j0", Labels: []string{"linux"}, CreatedAt: "0"},
	}}
	if _, err := store.PutBytes(ctx(), e.store, QueueIndexKey(), mustJSON(seed), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i, row := range []QueueIndexEntry{{RunID: "r1", JobID: "j1"}, {RunID: "r2", JobID: "j2"}} {
		entry := &QueueEntry{Seq: padInt(i+1, 19) + "-abc", RunID: row.RunID, JobID: row.JobID, State: QueueQueued, EnqueuedAt: "now"}
		if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", entry.Seq), mustJSON(entry), store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}
	if _, err := e.svc.ReconcileQueueIndex(ctx(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	idx, _, _ := e.svc.loadQueueIndex(ctx())
	if idx == nil || len(idx.Queued) != 3 {
		t.Fatalf("reconciled rows = %+v", idx)
	}
	seen := map[string]bool{}
	for _, row := range idx.Queued {
		seen[row.RunID+"/"+row.JobID] = true
	}
	for _, want := range []string{"r1/j1", "r2/j2", "r0/j0"} {
		if !seen[want] {
			t.Fatalf("row %s lost: %+v", want, idx.Queued)
		}
	}
	if seen["r3/j3"] {
		t.Fatalf("stale row survived: %+v", idx.Queued)
	}
}
