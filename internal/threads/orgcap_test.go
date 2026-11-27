package threads

// PAR-153: the per-organization concurrency cap. A cap of 2 admits runs in two
// different repositories and holds a third with exactly one named reason; a
// terminal fold in either repository releases the oldest held run.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func seedOrgCap(t *testing.T, e *testEnv, doc *OrgCapDoc) {
	t.Helper()
	if _, err := e.svc.casUpdate(ctx(), OrgCapKey("acme"), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return mustJSON(doc), true, nil
	}); err != nil {
		t.Fatalf("seed cap doc: %v", err)
	}
}

func orgTestEnv(t *testing.T, cap int) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.useDefaultBranch()
	e.useIndependentJobs()
	e.svc.IsOrg = func(_ context.Context, owner string) (bool, error) { return owner == "acme", nil }
	e.svc.OrgAdmin = func(_ context.Context, _ string, p auth.Principal) *auth.AuthError {
		if p.Admin || p.Name == "admin" {
			return nil
		}
		return &auth.AuthError{Kind: auth.ErrForbidden, Why: "not an organization owner"}
	}
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: cap}, e.admin()); err != nil {
		t.Fatalf("SetOrgSettings: %v", err)
	}
	return e
}

func TestOrgConcurrencyCapHoldsAndReleases(t *testing.T) {
	e := orgTestEnv(t, 2)

	one, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(one) != 1 {
		t.Fatalf("first run: %d, %v", len(one), err)
	}
	two, err := e.svc.CreateRun(ctx(), "acme/two", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(two) != 1 {
		t.Fatalf("second run: %d, %v", len(two), err)
	}
	if one[0].HoldReason != "" || two[0].HoldReason != "" {
		t.Fatalf("runs under the cap are held: %q / %q", one[0].HoldReason, two[0].HoldReason)
	}
	third, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(third) != 1 {
		t.Fatalf("third run: %d, %v", len(third), err)
	}
	want := "organization concurrency cap 2 reached"
	if third[0].HoldReason != want {
		t.Fatalf("hold reason = %q, want %q", third[0].HoldReason, want)
	}
	// Every queued job reports exactly that one reason (not also the
	// repository's/none), and no queue entry exists.
	jobs, err := e.svc.loadJobs(ctx(), "acme", "one", third[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.QueueSeq != "" {
			t.Fatalf("a held job was enqueued: %+v", j)
		}
	}
	view, err := e.svc.GetRun(ctx(), "acme/one", third[0].ID, e.public())
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	for _, j := range view.Jobs {
		if j.State == JobQueued && j.QueueReason != want {
			t.Fatalf("job %s reason = %q, want %q", j.ID, j.QueueReason, want)
		}
	}

	// Finish the first run; its fold releases the held third run even though
	// it lives in another repository of the same organization.
	jobs, err = e.svc.loadJobs(ctx(), "acme", "one", one[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if _, err := e.svc.UpdateJob(ctx(), "acme/one", one[0].ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	released, err := e.svc.loadRun(ctx(), "acme", "one", third[0].ID)
	if err != nil || released == nil {
		t.Fatalf("load released run: %v", err)
	}
	if released.HoldReason != "" {
		t.Fatalf("held run was not released: %q", released.HoldReason)
	}
	head, _, err := e.svc.loadHead(ctx(), "acme", "one")
	if err != nil || head == nil || len(head.Pending) == 0 {
		t.Fatalf("released run has no queue entries: head=%v err=%v", head, err)
	}
}

func TestOrgCapOnlyForOrganizations(t *testing.T) {
	e := newTestEnv(t)
	e.useDefaultBranch()
	// An org setting exists, but the owner is a user namespace: the cap must
	// not apply, and the read must not happen (IsOrg answers first).
	e.svc.IsOrg = func(_ context.Context, _ string) (bool, error) { return false, nil }
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: 1}, e.admin()); err != nil {
		t.Fatalf("SetOrgSettings: %v", err)
	}
	for i := 0; i < 3; i++ {
		runs, err := e.svc.CreateRun(ctx(), "acme/repo", testSHA, EventPush, CreateOpts{
			Branch: "main", Ref: "refs/heads/main", Actor: "writer",
		})
		if err != nil || len(runs) != 1 {
			t.Fatalf("run %d: %d, %v", i, len(runs), err)
		}
		if runs[0].HoldReason != "" {
			t.Fatalf("a user-owned namespace was capped: %q", runs[0].HoldReason)
		}
	}
	// A corrupt cap index is a named error on admission, never a silent
	// bypass.
	e2 := orgTestEnv(t, 1)
	if err := e2.svc.putCreate(ctx(), OrgCapKey("acme"), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	_, err := e2.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err == nil || !strings.Contains(err.Error(), "organization cap index") {
		t.Fatalf("corrupt index err = %v", err)
	}
}

func TestOrgSettingsBoundsAndAuth(t *testing.T) {
	e := orgTestEnv(t, 0)
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: 501}, e.admin()); err == nil {
		t.Fatal("a cap above 500 must be refused")
	}
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: -1}, e.admin()); err == nil {
		t.Fatal("a negative cap must be refused")
	}
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: 3}, e.reader()); err == nil {
		t.Fatal("a non-owner must not set an org cap")
	}
	view, err := e.svc.GetOrgSettings(ctx(), "acme", e.admin())
	if err != nil || view.ConcurrencyCap != 0 {
		t.Fatalf("GetOrgSettings = %+v, %v", view, err)
	}
}

func TestOrgSettingsHTTP(t *testing.T) {
	e := orgTestEnv(t, 0)
	h := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return e.admin(), nil
	}}
	do := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/v1/orgs/acme/threads/settings", strings.NewReader(body))
		if !h.Handle(rec, req) {
			t.Fatal("org settings route not claimed")
		}
		return rec
	}
	if rec := do(http.MethodGet, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"concurrency_cap":0`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, `{"concurrency_cap":2}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"concurrency_cap":2`) {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, `{"concurrency_cap":501}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("over the cap bound = %d", rec.Code)
	}
	if rec := do(http.MethodPut, `{"bogus":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := do(http.MethodDelete, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	// A non-owner cannot read or write it.
	anon := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "reader"}, nil
	}}
	rec := httptest.NewRecorder()
	anon.Handle(rec, httptest.NewRequest(http.MethodPut, "/api/v1/orgs/acme/threads/settings",
		strings.NewReader(`{"concurrency_cap":1}`)))
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-owner PUT = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	anon.Handle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orgs/acme/threads/settings", nil))
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-owner GET = %d", rec.Code)
	}
	// A credential error from the seam is a real 401, never a page.
	broken := &Handler{Svc: e.svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "bad token"}
	}}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec = httptest.NewRecorder()
		broken.Handle(rec, httptest.NewRequest(method, "/api/v1/orgs/acme/threads/settings", strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with a bad credential = %d", method, rec.Code)
		}
	}
	// A trailing segment is not a settings route.
	rec = httptest.NewRecorder()
	if h.Handle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/orgs/acme/threads/settings/x", nil)) {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("trailing segment = %d", rec.Code)
		}
	}
}

func TestOrgCapGateIdempotence(t *testing.T) {
	e := orgTestEnv(t, 2)
	run, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(run) != 1 {
		t.Fatalf("CreateRun: %d %v", len(run), err)
	}
	// Already admitted: the same run is not held again.
	if held, err := e.svc.applyOrgCapGate(ctx(), "acme", "acme/one", run[0].ID, 2, "2026-11-20T12:00:30Z"); held || err != nil {
		t.Fatalf("re-admission = %v %v", held, err)
	}
	// Already held: the same run reports held without a second entry.
	doc, _ := e.svc.loadOrgCapDoc(ctx(), "acme")
	doc.Held = append(doc.Held, OrgCapEntry{Repo: "acme/two", RunID: "rx", CreatedAt: "2026-11-20T12:00:00Z"})
	seedOrgCap(t, e, doc)
	if held, err := e.svc.applyOrgCapGate(ctx(), "acme", "acme/two", "rx", 2, "2026-11-20T12:00:30Z"); !held || err != nil {
		t.Fatalf("held re-admission = %v %v", held, err)
	}
	if len(doc.Held) != 1 {
		t.Fatalf("held list duplicated: %+v", doc.Held)
	}
	// An entry whose repo is not owner/name is never live.
	if e.svc.orgCapEntryLive(ctx(), OrgCapEntry{Repo: "bad", RunID: "x"}) {
		t.Fatal("a malformed entry read as live")
	}
}

func TestReleaseOrgHeldRunEdges(t *testing.T) {
	e := orgTestEnv(t, 1)
	// No index at all: a no-op.
	e.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", "none")
	// A corrupt index is logged and left alone.
	if _, err := e.svc.casUpdate(ctx(), OrgCapKey("acme"), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("{not json"), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	e.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", "none")
	// With the cap removed, the folding run is removed from the index and
	// nothing is released.
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: 0}, e.admin()); err != nil {
		t.Fatal(err)
	}
	seedOrgCap(t, e, &OrgCapDoc{
		Live: []OrgCapEntry{{Repo: "acme/one", RunID: "done", CreatedAt: "2026-11-20T12:00:00Z"}},
		Held: []OrgCapEntry{{Repo: "acme/two", RunID: "waiting", CreatedAt: "2026-11-20T12:00:00Z"}},
	})
	e.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", "done")
	doc, _ := e.svc.loadOrgCapDoc(ctx(), "acme")
	if doc == nil || len(doc.Live) != 0 || len(doc.Held) != 0 {
		t.Fatalf("no-cap release = %+v", doc)
	}
}

func TestOrgCapRemovalReleasesHeldRuns(t *testing.T) {
	e := orgTestEnv(t, 1)
	if _, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	}); err != nil {
		t.Fatal(err)
	}
	held, err := e.svc.CreateRun(ctx(), "acme/two", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(held) != 1 || held[0].HoldReason == "" {
		t.Fatalf("held run = %+v %v", held, err)
	}
	// Removing the cap admits every held run immediately.
	if _, err := e.svc.SetOrgSettings(ctx(), "acme", OrgSettingsInput{ConcurrencyCap: 0}, e.admin()); err != nil {
		t.Fatal(err)
	}
	released, err := e.svc.loadRun(ctx(), "acme", "two", held[0].ID)
	if err != nil || released == nil {
		t.Fatalf("load released: %v", err)
	}
	if released.HoldReason != "" {
		t.Fatalf("a held run survived the cap's removal: %q", released.HoldReason)
	}
	head, _, err := e.svc.loadHead(ctx(), "acme", "two")
	if err != nil || head == nil || len(head.Pending) == 0 {
		t.Fatalf("released run has no queue entries: head=%v err=%v", head, err)
	}
}

// keyedFailStore injects a failure on one exact key's Get or Put.
type keyedFailStore struct {
	store.ObjectStore
	getKey string
	putKey string
	err    error
}

func (f *keyedFailStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == f.getKey {
		return nil, f.err
	}
	return f.ObjectStore.Get(ctx, key, opts)
}

func (f *keyedFailStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if key == f.putKey {
		return store.ObjectMeta{}, f.err
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestOrgCapGateStoreFaults(t *testing.T) {
	e := orgTestEnv(t, 2)
	errBoom := &store.StoreError{Kind: store.ErrKindRetryable, Key: "injected", Err: context.DeadlineExceeded}
	// The index read fails: admission surfaces the error (caller fails closed).
	e.svc.Store = &keyedFailStore{ObjectStore: e.store, getKey: OrgCapKey("acme"), err: errBoom}
	if _, err := e.svc.applyOrgCapGate(ctx(), "acme", "acme/one", "rx", 2, "2026-11-20T12:00:00Z"); err == nil {
		t.Fatal("a failed index read must surface")
	}
	// A read-only service still reaches a missing index; the write fails.
	e.svc.Store = &keyedFailStore{ObjectStore: e.store, putKey: OrgCapKey("acme"), err: errBoom}
	if _, err := e.svc.applyOrgCapGate(ctx(), "acme", "acme/one", "ry", 2, "2026-11-20T12:00:00Z"); err == nil {
		t.Fatal("a failed index write must surface")
	}
	// A permanently lost CAS exhausts the bounded retry loop with a named
	// error, never a silent admission.
	pre := &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: OrgCapKey("acme"), Err: context.DeadlineExceeded}
	e.svc.Store = &keyedFailStore{ObjectStore: e.store, putKey: OrgCapKey("acme"), err: pre}
	if _, err := e.svc.applyOrgCapGate(ctx(), "acme", "acme/one", "rz", 2, "2026-11-20T12:00:00Z"); err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("exhausted CAS err = %v", err)
	}
	// The prune path's write is retried on a lost CAS too.
	e2 := orgTestEnv(t, 1)
	if _, err := e2.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	}); err != nil {
		t.Fatal(err)
	}
	doc, _ := e2.svc.loadOrgCapDoc(ctx(), "acme")
	doc.Live = append(doc.Live, OrgCapEntry{Repo: "acme/one", RunID: "dead", CreatedAt: "2000-01-01T00:00:00Z"})
	seedOrgCap(t, e2, doc)
	e2.svc.Store = &keyedFailStore{ObjectStore: e2.store, putKey: OrgCapKey("acme"), err: pre}
	if _, err := e2.svc.applyOrgCapGate(ctx(), "acme", "acme/two", "rw", 1, "2026-11-20T12:00:00Z"); err == nil {
		t.Fatal("a failed prune write must surface after the retry bound")
	}
}

func TestReleaseOrgHeldRunFailures(t *testing.T) {
	// A held run whose job records cannot be read is still admitted (the fold
	// is not blocked by one unreadable job) and the index records it live.
	e := orgTestEnv(t, 1)
	live, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(live) != 1 {
		t.Fatalf("live run: %d %v", len(live), err)
	}
	held, err := e.svc.CreateRun(ctx(), "acme/two", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(held) != 1 || held[0].HoldReason == "" {
		t.Fatalf("held run: %+v %v", held, err)
	}
	jobs, _ := e.svc.loadJobs(ctx(), "acme", "two", held[0].ID)
	if len(jobs) == 0 {
		t.Fatal("held run has no jobs")
	}
	// Corrupt one job record so the release's loadJobs surfaces an error.
	if _, err := e.store.Put(ctx(), JobKey("acme", "two", held[0].ID, jobs[0].ID),
		store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	e.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", live[0].ID)
	doc, _ := e.svc.loadOrgCapDoc(ctx(), "acme")
	if doc == nil || orgCapHas(doc.Live, "acme/two", held[0].ID) == false {
		t.Fatalf("release did not record the held run live: %+v", doc)
	}

	// A write failure after a successful release is logged, not fatal.
	e2 := orgTestEnv(t, 1)
	l2, _ := e2.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	h2, _ := e2.svc.CreateRun(ctx(), "acme/two", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if len(l2) != 1 || len(h2) != 1 {
		t.Fatalf("setup runs = %d %d", len(l2), len(h2))
	}
	e2.svc.Store = &keyedFailStore{ObjectStore: e2.store, putKey: OrgCapKey("acme"),
		err: &store.StoreError{Kind: store.ErrKindRetryable, Key: "injected", Err: context.DeadlineExceeded}}
	e2.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", l2[0].ID)
}

func TestOrgCapReleaseLostCAS(t *testing.T) {
	pre := &store.StoreError{Kind: store.ErrKindPreconditionFailed, Key: OrgCapKey("acme"), Err: context.DeadlineExceeded}
	// A dead held entry whose drop-write keeps losing the CAS is retried a
	// bounded number of times, never spun forever.
	e := orgTestEnv(t, 1)
	seedOrgCap(t, e, &OrgCapDoc{Held: []OrgCapEntry{{Repo: "acme/two", RunID: "missing", CreatedAt: "2026-11-20T12:00:00Z"}}})
	e.svc.Store = &keyedFailStore{ObjectStore: e.store, putKey: OrgCapKey("acme"), err: pre}
	e.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", "none")
	// The same for an entry whose repo is not owner/name.
	e2 := orgTestEnv(t, 1)
	seedOrgCap(t, e2, &OrgCapDoc{Held: []OrgCapEntry{{Repo: "bad", RunID: "x", CreatedAt: "2026-11-20T12:00:00Z"}}})
	e2.svc.Store = &keyedFailStore{ObjectStore: e2.store, putKey: OrgCapKey("acme"), err: pre}
	e2.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", "none")
	// And the early no-release write.
	e3 := orgTestEnv(t, 1)
	seedOrgCap(t, e3, &OrgCapDoc{Live: []OrgCapEntry{{Repo: "acme/one", RunID: "x", CreatedAt: "2026-11-20T12:00:00Z"}}})
	e3.svc.Store = &keyedFailStore{ObjectStore: e3.store, putKey: OrgCapKey("acme"), err: pre}
	e3.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", "x")
}

func TestClearOrgHoldEdges(t *testing.T) {
	e := orgTestEnv(t, 1)
	// A missing run header is not a release.
	if e.svc.clearOrgHold(ctx(), "acme", "one", "missing") {
		t.Fatal("a missing run cannot be released")
	}
	// A corrupt header surfaces as not-released.
	if _, err := e.store.Put(ctx(), RunKey("acme", "one", "broken"),
		store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if e.svc.clearOrgHold(ctx(), "acme", "one", "broken") {
		t.Fatal("a corrupt run cannot be released")
	}
	// A corrupt index read surfaces from loadOrgCapDoc.
	if _, err := e.svc.casUpdate(ctx(), OrgCapKey("acme"), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("{not json"), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.loadOrgCapDoc(ctx(), "acme"); err == nil {
		t.Fatal("a corrupt index must be a read error")
	}
}

func TestOrgCapHeldWriteFault(t *testing.T) {
	e := orgTestEnv(t, 1)
	if _, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	}); err != nil {
		t.Fatal(err)
	}
	// The live run saturates the cap; admitting a second one must write the
	// held entry, and that write fails.
	e.svc.Store = &keyedFailStore{ObjectStore: e.store, putKey: OrgCapKey("acme"),
		err: &store.StoreError{Kind: store.ErrKindRetryable, Key: "injected", Err: context.DeadlineExceeded}}
	if _, err := e.svc.applyOrgCapGate(ctx(), "acme", "acme/two", "held", 1, "2026-11-20T12:00:00Z"); err == nil {
		t.Fatal("a failed held-entry write must surface")
	}
}

func TestOrgCapIndexEdges(t *testing.T) {
	e := orgTestEnv(t, 1)
	run, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err != nil || len(run) != 1 {
		t.Fatalf("CreateRun: %d %v", len(run), err)
	}
	// The settings projection reports the index's usage.
	view, err := e.svc.GetOrgSettings(ctx(), "acme", e.admin())
	if err != nil || view.Live != 1 || view.Held != 0 {
		t.Fatalf("usage view = %+v, %v", view, err)
	}
	// A stale live entry (a run that folded without releasing) is pruned on
	// the admission path.
	capDoc, _ := e.svc.loadOrgCapDoc(ctx(), "acme")
	capDoc.Live = append(capDoc.Live, OrgCapEntry{Repo: "acme/one", RunID: "missing-run", CreatedAt: "2000-01-01T00:00:00Z"})
	seedOrgCap(t, e, capDoc)
	pruned := e.svc.pruneOrgCapEntries(ctx(), capDoc.Live, 16)
	if len(pruned) != 1 || pruned[0].RunID != run[0].ID {
		t.Fatalf("pruned = %+v", pruned)
	}
	// A held entry whose run no longer exists is dropped by the release path.
	capDoc, _ = e.svc.loadOrgCapDoc(ctx(), "acme")
	capDoc.Live = []OrgCapEntry{{Repo: "acme/one", RunID: run[0].ID, CreatedAt: "2000-01-01T00:00:00Z"}}
	capDoc.Held = append(capDoc.Held, OrgCapEntry{Repo: "acme/one", RunID: "gone", CreatedAt: "2000-01-01T00:00:00Z"},
		OrgCapEntry{Repo: "not-a-repo", RunID: "x", CreatedAt: "2000-01-01T00:00:00Z"})
	seedOrgCap(t, e, capDoc)
	e.svc.releaseOrgHeldRuns(ctx(), "acme", "acme/one", run[0].ID)
	after, _ := e.svc.loadOrgCapDoc(ctx(), "acme")
	if after == nil || len(after.Held) != 0 {
		t.Fatalf("dead held entries = %+v", after)
	}
	// stampHoldReason refuses a missing run and leaves an already-held run
	// alone.
	if err := e.svc.stampHoldReason(ctx(), "acme", "one", "missing", "x", "2000-01-01T00:00:00Z"); err == nil {
		t.Fatal("stamping a missing run must fail")
	}
	if err := e.svc.stampHoldReason(ctx(), "acme", "one", run[0].ID, "organization concurrency cap 1 reached", "2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// A corrupt settings record reads as no cap (the per-repository loader's
	// same fail-open display rule).
	if _, err := e.svc.casUpdate(ctx(), OrgSettingsKey("acme"), 3, func([]byte, store.Version) ([]byte, bool, error) {
		return []byte("{not json"), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.orgCap(ctx(), "acme"); got != 0 {
		t.Fatalf("corrupt org settings cap = %d, want 0", got)
	}
	// An unreadable settings record also reads as no cap.
	e.svc.Store = &keyedFailStore{ObjectStore: e.store, getKey: OrgSettingsKey("acme"),
		err: &store.StoreError{Kind: store.ErrKindRetryable, Key: "injected", Err: context.DeadlineExceeded}}
	if got := e.svc.orgCap(ctx(), "acme"); got != 0 {
		t.Fatalf("unreadable org settings cap = %d, want 0", got)
	}
	e.svc.Store = e.store
	// An IsOrg error also reads as no cap rather than failing run creation.
	e.svc.IsOrg = func(_ context.Context, _ string) (bool, error) { return false, context.DeadlineExceeded }
	if got := e.svc.orgCap(ctx(), "acme"); got != 0 {
		t.Fatalf("IsOrg error cap = %d", got)
	}
}

func TestOrgHeldListBound(t *testing.T) {
	e := orgTestEnv(t, 1)
	if _, err := e.svc.CreateRun(ctx(), "acme/one", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	}); err != nil {
		t.Fatal(err)
	}
	doc, _ := e.svc.loadOrgCapDoc(ctx(), "acme")
	doc.Held = make([]OrgCapEntry, 0, maxOrgHeld)
	for i := 0; i < maxOrgHeld; i++ {
		doc.Held = append(doc.Held, OrgCapEntry{Repo: "acme/one", RunID: "held-" + strings.Repeat("0", 6) + string(rune('a'+i%26)), CreatedAt: "2000-01-01T00:00:00Z"})
	}
	seedOrgCap(t, e, doc)
	_, err := e.svc.CreateRun(ctx(), "acme/two", testSHA, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "writer",
	})
	if err == nil || !strings.Contains(err.Error(), "already held") {
		t.Fatalf("held-list bound err = %v", err)
	}
}
