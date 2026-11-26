package threads

// coverage_wave2_test.go — the small decision arms wave 2 added around the
// environment/team/retry helpers. They are table tests because each is one
// "what if the record is malformed / the roster is gone / the read failed"
// decision that the service paths rely on.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// TestDeploymentApproverArms pins the team-resolution failure arms: a malformed
// stored team, an unwired roster, a roster read error and an empty team all fail
// closed (no deployment is released by a set nobody can enumerate).
func TestDeploymentApproverArms(t *testing.T) {
	svc := &Service{}
	if _, err := svc.deploymentApprovers(ctx(), &EnvironmentDoc{TeamReviewers: []string{"broken"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("malformed team = %v", err)
	}
	if _, err := svc.deploymentApprovers(ctx(), &EnvironmentDoc{TeamReviewers: []string{"acme/x"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unwired roster = %v", err)
	}
	svc.TeamRoster = fakeTeamRoster{err: errors.New("directory down")}
	if _, err := svc.deploymentApprovers(ctx(), &EnvironmentDoc{TeamReviewers: []string{"acme/x"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("roster error = %v", err)
	}
	svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{}}
	if _, err := svc.deploymentApprovers(ctx(), &EnvironmentDoc{TeamReviewers: []string{"acme/empty"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty team = %v", err)
	}
	// Direct reviewers dedupe case-insensitively, an empty name is dropped, and
	// teams merge in.
	svc.TeamRoster = fakeTeamRoster{teams: map[string][]string{"acme/x": {"Grace"}}}
	got, err := svc.deploymentApprovers(ctx(), &EnvironmentDoc{
		RequiredReviewers: []string{"Ada", "ada", "  "}, TeamReviewers: []string{"acme/x"},
	})
	if err != nil || strings.Join(got, ",") != "ada,grace" {
		t.Fatalf("approvers = %v (%v)", got, err)
	}
}

// TestPendingReviewersTolerantArms pins the notice helper: an unreadable record
// or a broken roster contributes nobody (the hold is still visible on the page).
func TestPendingReviewersTolerantArms(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnv(t, "production", EnvironmentInput{RequiredReviewers: []string{"maint"}})
	if got := f.svc.pendingReviewers(ctx(), "o", "r", []string{"production", "ghost"}); strings.Join(got, ",") != "maint" {
		t.Fatalf("pending reviewers = %v", got)
	}
	f.svc.TeamRoster = fakeTeamRoster{err: errors.New("down")}
	// A record with a team can only exist from before the roster broke, so seed
	// it directly (the save path would refuse it now).
	if _, err := store.PutBytes(ctx(), f.store, EnvironmentKey("o", "r", "teams"),
		mustJSON(&EnvironmentDoc{Name: "teams", TeamReviewers: []string{"acme/x"}}), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if got := f.svc.pendingReviewers(ctx(), "o", "r", []string{"teams"}); len(got) != 0 {
		t.Fatalf("roster-broken approvers = %v", got)
	}
}

// TestApprovalForTolerantArms pins the page projection's tolerant branches: a
// vanished record, an unwired roster, a malformed team and a roster error are
// skipped without failing the page.
func TestApprovalForTolerantArms(t *testing.T) {
	f := newEnvFixture(t)
	run := &Run{ID: "0000000000000000001-abcdef", State: RunPendingApproval,
		ApprovalKind: ApprovalEnvironment, Environments: []string{"ghost"}, Actor: "writer"}
	if av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "writer"}); len(av.Reviewers) != 0 {
		t.Fatalf("vanished env reviewers = %v", av.Reviewers)
	}
	// Seed the record directly: the save path would refuse a team without a
	// roster, and these are the tolerant READ arms.
	if _, err := store.PutBytes(ctx(), f.store, EnvironmentKey("o", "r", "production"),
		mustJSON(&EnvironmentDoc{Name: "production", TeamReviewers: []string{"acme/x"}, RequiredReviewers: []string{"maint"}}),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	run.Environments = []string{"production"}
	f.svc.TeamRoster = nil
	if av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "maint"}); strings.Join(av.Reviewers, ",") != "maint" {
		t.Fatalf("unwired roster projection = %v", av.Reviewers)
	}
	// A malformed stored team is skipped.
	if _, err := f.svc.casUpdate(ctx(), EnvironmentKey("o", "r", "production"), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var doc EnvironmentDoc
		if jerr := jsonUnmarshalTest(cur, &doc); jerr != nil {
			return nil, false, jerr
		}
		doc.TeamReviewers = []string{"broken"}
		doc.Version++
		return mustJSON(&doc), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "maint"}); strings.Join(av.Reviewers, ",") != "maint" {
		t.Fatalf("malformed team projection = %v", av.Reviewers)
	}
	// A roster error is skipped too.
	if _, err := f.svc.casUpdate(ctx(), EnvironmentKey("o", "r", "production"), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var doc EnvironmentDoc
		if jerr := jsonUnmarshalTest(cur, &doc); jerr != nil {
			return nil, false, jerr
		}
		doc.TeamReviewers = []string{"acme/x"}
		doc.Version++
		return mustJSON(&doc), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.TeamRoster = fakeTeamRoster{err: errors.New("down")}
	if av := f.svc.ApprovalFor(ctx(), "o", "r", run, auth.Principal{Name: "maint"}); strings.Join(av.Reviewers, ",") != "maint" {
		t.Fatalf("roster-error projection = %v", av.Reviewers)
	}
}

// TestEnvironmentReadErrorArms pins the settings reads' store-error arms and the
// malformed-repo refusals they share.
func TestEnvironmentReadErrorArms(t *testing.T) {
	f := newEnvFixture(t)
	f.putEnv(t, "production", EnvironmentInput{})
	if _, err := f.svc.GetEnvironment(ctx(), "no-slash", "production", f.maintain()); err == nil {
		t.Fatal("a malformed repo reached the store")
	}
	if _, err := f.svc.ListEnvironments(ctx(), "no-slash", f.maintain()); err == nil {
		t.Fatal("a malformed repo reached the list")
	}
	if err := f.svc.DeleteEnvironment(ctx(), "no-slash", "production", f.maintain()); err == nil {
		t.Fatal("a malformed repo reached the delete")
	}
	if err := f.svc.SetEnvironmentSecret(ctx(), "o/r", "no such env!", "TOKEN", "v", f.maintain()); err == nil {
		t.Fatal("a malformed environment name was stored")
	}
	// A failing read surfaces from Get and List; a failing LIST surfaces from the
	// delete (which enumerates the secret family first).
	fs := &faultStore{Memory: f.store}
	f.svc.Store = fs
	fs.fail("get", 0)
	if _, err := f.svc.GetEnvironment(ctx(), "o/r", "production", f.maintain()); err == nil {
		t.Fatal("a failing get was swallowed")
	}
	fs.clear()
	fs.fail("list", 0)
	if _, err := f.svc.ListEnvironments(ctx(), "o/r", f.maintain()); err == nil {
		t.Fatal("a failing list was swallowed")
	}
	if err := f.svc.DeleteEnvironment(ctx(), "o/r", "production", f.maintain()); err == nil {
		t.Fatal("a failing secret-family list was swallowed")
	}
}

// TestRetryCeilingArms pins the configurable ceiling helper's default and
// configured forms (PAR-179).
func TestRetryCeilingArms(t *testing.T) {
	if got := (*Service)(nil).maxAttempts(); got != MaxQueueAttempts {
		t.Fatalf("nil service ceiling = %d", got)
	}
	s := &Service{MaxQueueAttempts: 7}
	if got := s.maxAttempts(); got != 7 {
		t.Fatalf("configured ceiling = %d", got)
	}
}

// TestConcurrencyStoreFaultArms pins the group record's failure and stale-slot
// arms with a fault store (creation preparation, pending registration and
// acquisition all surface a store error; a stale pending slot is stolen).
func TestConcurrencyStoreFaultArms(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	pol := groupPolicy{Group: "deploy-main"}

	// A read error surfaces from all three entry points.
	fs := &faultStore{Memory: e.store}
	e.svc.Store = fs
	fs.fail("get", 0)
	if _, perr := e.svc.prepareGroup(ctx(), "o", "r", pol, "0000000000000000002-bbbbbb"); perr == nil {
		t.Fatal("prepareGroup swallowed a read error")
	}
	if _, perr := e.svc.notePendingGroup(ctx(), "o", "r", "deploy-main", "0000000000000000002-bbbbbb"); perr == nil {
		t.Fatal("notePendingGroup swallowed a read error")
	}
	if _, _, perr := e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", "0000000000000000002-bbbbbb"); perr == nil {
		t.Fatal("tryAcquireGroup swallowed a read error")
	}
	// A write error surfaces too (the record is held by another live run, so
	// preparation wants a CAS).
	fs.clear()
	fs.fail("put", 0)
	if _, perr := e.svc.prepareGroup(ctx(), "o", "r", pol, "0000000000000000002-bbbbbb"); perr == nil {
		t.Fatal("prepareGroup swallowed a write error")
	}
	if _, perr := e.svc.notePendingGroup(ctx(), "o", "r", "deploy-main", "0000000000000000002-bbbbbb"); perr == nil {
		t.Fatal("notePendingGroup swallowed a write error")
	}
	fs.clear()
	e.svc.Store = e.store

	// A stale pending slot (a run that no longer exists) is cleared when the
	// group is acquired.
	if _, perr := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "deploy-main"),
		mustJSON(&ConcurrencyDoc{Group: "deploy-main", Pending: "9999999999999999999-ffffff", Version: 2}),
		store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatal(perr)
	}
	ok, _, aerr := e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", "0000000000000000002-bbbbbb")
	if aerr != nil || !ok {
		t.Fatalf("stale pending slot = %v (%v)", ok, aerr)
	}
	if doc := loadGroupDoc(t, e, "deploy-main"); doc.Pending != "" || doc.RunID != "0000000000000000002-bbbbbb" {
		t.Fatalf("stale slot not cleared = %+v", doc)
	}

	// A run id that names nothing is never live, and a nil service's GC is a
	// no-op (the maintenance contract).
	if e.svc.runLive(ctx(), "o", "r", "") {
		t.Fatal("an empty run id read as live")
	}
	if n, gerr := (*Service)(nil).GC(ctx(), "o", "r"); gerr != nil || n != 0 {
		t.Fatalf("nil GC = %d (%v)", n, gerr)
	}
}

// casLossStore loses every CAS: it is how the retry ceilings are exercised
// deterministically (four lost writes in a row, never a race).
type casLossStore struct{ *store.Memory }

func (c casLossStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	return store.ObjectMeta{}, &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: key}
}

// TestConcurrencyCASCeilings pins the four-attempt ceilings: preparation, the
// pending registration and acquisition all concede after repeated lost CASes
// (never spin, never corrupt the record), and admission treats the concession as
// "not now".
func TestConcurrencyCASCeilings(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", pendingGroupWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	first, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"})
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	holdGroup(t, e, first[0])
	pol := groupPolicy{Group: "deploy-main"}

	e.svc.Store = casLossStore{Memory: e.store}
	if got, perr := e.svc.prepareGroup(ctx(), "o", "r", pol, "0000000000000000002-bbbbbb"); perr != nil || got != "" {
		t.Fatalf("prepareGroup after lost CASes = %q (%v), want a quiet concession", got, perr)
	}
	if pending, perr := e.svc.notePendingGroup(ctx(), "o", "r", "deploy-main", "0000000000000000002-bbbbbb"); perr != nil || pending {
		t.Fatalf("notePendingGroup after lost CASes = %v (%v)", pending, perr)
	}
	if ok, _, aerr := e.svc.tryAcquireGroup(ctx(), "o", "r", "deploy-main", "0000000000000000002-bbbbbb"); aerr != nil || ok {
		t.Fatalf("tryAcquireGroup after lost CASes = %v (%v)", ok, aerr)
	}
	e.svc.Store = e.store
	// Free the group first: admission must see the lost CAS as "wait".
	e.svc.releaseConcurrency(ctx(), "o", "r", first[0].ID, []string{"deploy-main"})
	e.svc.Store = casLossStore{Memory: e.store}
	ok, aerr := e.svc.admitConcurrency(ctx(), "o", "r", "0000000000000000002-bbbbbb", &Spec{
		Concurrency: &ConcurrencySpec{Group: "deploy-main"},
	})
	if aerr != nil || ok {
		t.Fatalf("admission after lost CASes = %v (%v), want a quiet wait", ok, aerr)
	}
	e.svc.Store = e.store
}

// TestConcurrencyRecordReadErrors pins the two lookups acquisition performs on a
// holder and a pending run (a store error is surfaced, never read as free).
func TestConcurrencyRecordReadErrors(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	if _, perr := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "g"),
		mustJSON(&ConcurrencyDoc{Group: "g", RunID: run.ID, Version: 1}), store.PutOptions{Mode: store.PutCreate}); perr != nil {
		t.Fatal(perr)
	}
	fs := &faultStore{Memory: e.store}
	e.svc.Store = fs
	fs.fail("get", 1) // the group record reads, the holder run does not
	if _, _, aerr := e.svc.tryAcquireGroup(ctx(), "o", "r", "g", "0000000000000000002-bbbbbb"); aerr == nil {
		t.Fatal("a holder read error was swallowed")
	}
	// A pending slot whose run cannot be read fails acquisition too.
	fs.clear()
	if _, perr := store.PutBytes(ctx(), e.store, ConcurrencyKey("o", "r", "p"),
		mustJSON(&ConcurrencyDoc{Group: "p", Pending: run.ID, Version: 1}), store.PutOptions{Mode: store.PutCreate}); perr != nil {
		t.Fatal(perr)
	}
	e.svc.Store = fs
	fs.fail("get", 1)
	if _, _, aerr := e.svc.tryAcquireGroup(ctx(), "o", "r", "p", "0000000000000000002-bbbbbb"); aerr == nil {
		t.Fatal("a pending read error was swallowed")
	}
	e.svc.Store = e.store
	// A newer live pending run owns the slot: this run is superseded as pending.
	if pending, perr := e.svc.notePendingGroup(ctx(), "o", "r", "p", "0000000000000000001-aaaaaa"); perr != nil || !pending {
		t.Fatalf("newer pending owner = %v (%v), want it recognised", pending, perr)
	}
}
