package packages

// gcadmin_test.go — the instance-scope packages GC (PLAN-ADMIN §8.6,
// docs/features/14_packages.md §6): the dry run counts without deleting, the
// applying pass deletes and records, the stored summary is honest across
// restarts, and the manual trigger writes its audit record and answers the
// task id before the pass finishes.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// gcInstanceFixture builds a fixture whose store carries two registries:
// acme/demo with one live image and one orphaned blob, and acme/other with an
// expired upload. A third repo (acme/plain) has no registry at all.
func gcInstanceFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	f.svc.BlobGrace = time.Minute
	f.exists["acme/other"] = true
	f.exists["acme/plain"] = true

	config := f.pushBlob(t, "writer", []byte("config"))
	layer := f.pushBlob(t, "writer", []byte("layer!"))
	manifest := imageWith(config, layer)
	if w := f.as("writer", "PUT", "/v2/acme/demo/manifests/app", manifest); w.Code != 201 {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	// An orphan: a blob no manifest names.
	f.pushBlob(t, "writer", []byte("orphaned-layer-bytes"))
	// An expired upload in the second registry (past the 24h TTL).
	old := f.now.Add(-48 * time.Hour)
	stagingID := uploadIDAt(old, "abc")
	if _, err := store.PutBytes(context.Background(), f.st, uploadKey("acme", "other", stagingID), []byte("partial"),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	// One hour on: the grace guard has expired for the orphan, while the
	// live image's blobs stay referenced.
	f.now = f.now.Add(time.Hour)
	return f, uploadKey("acme", "other", stagingID)
}

// uploadIDAt builds a well-formed staging id for an arbitrary creation time.
func uploadIDAt(at time.Time, nonce string) string {
	return itoaHex(at.Unix()) + "-" + nonce + "-sig"
}

func itoaHex(v int64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	i := len(buf)
	if v == 0 {
		return "0"
	}
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}

func TestPlanGCCountsWithoutDeleting(t *testing.T) {
	f, stagingKey := gcInstanceFixture(t)
	ctx := context.Background()

	before, err := f.svc.GCState(ctx)
	if err != nil || before.Last != nil {
		t.Fatalf("state before = (%+v, %v)", before, err)
	}
	log := &gcLogger{}
	deletesBefore := f.st.deletes()
	plan, err := f.svc.SweepAll(ctx, GCSweepRequest{DryRun: true, Actor: "alice", TaskID: "t1"}, log)
	if err != nil {
		t.Fatalf("dry run = %v", err)
	}
	if !plan.DryRun || plan.Repos != 2 {
		t.Fatalf("plan = %+v, want a dry run over 2 registries", plan)
	}
	if plan.Unreferenced != 1 || plan.Bytes <= 0 || plan.Uploads != 1 {
		t.Fatalf("plan counts = %+v, want 1 orphan blob + 1 expired upload", plan)
	}
	// Nothing was deleted: the orphan and the staging object are still there.
	if _, err := f.st.Head(ctx, blobKey("acme", "demo", DigestHex(digestOf([]byte("config"))))); err != nil {
		t.Fatalf("live blob gone after a dry run: %v", err)
	}
	if _, err := f.st.Head(ctx, stagingKey); err != nil {
		t.Fatalf("staging object gone after a dry run: %v", err)
	}
	if n := f.st.deletes(); n != deletesBefore {
		t.Fatalf("a dry run issued %d delete(s), want 0 (before=%d, after=%d)", n-deletesBefore, deletesBefore, n)
	}
	// The dry run is recorded as a count, never as an applying pass.
	st, err := f.svc.GCState(ctx)
	if err != nil || st.Last == nil || !st.Last.DryRun || st.LastApply != nil {
		t.Fatalf("state after dry run = (%+v, %v)", st, err)
	}
	if st.Last.Actor != "alice" || st.Last.TaskID != "t1" {
		t.Fatalf("run attribution = %+v", st.Last)
	}
	if len(log.notices) == 0 {
		t.Fatal("the dry run narrated nothing")
	}
}

func TestSweepAllAppliesAndRecords(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	ctx := context.Background()
	before := len(f.svc.recent)
	deletesBefore := f.st.deletes()
	got, err := f.svc.SweepAll(ctx, GCSweepRequest{Actor: "alice"}, nil)
	if err != nil {
		t.Fatalf("apply = %v", err)
	}
	if n := f.st.deletes(); n == deletesBefore {
		t.Fatal("an applying pass issued no delete")
	}
	if got.DryRun || got.Repos != 2 || got.Unreferenced != 1 || got.Uploads != 1 {
		t.Fatalf("apply = %+v", got)
	}
	if len(f.svc.recent) > before {
		// pruneRecent may shrink it; it must never grow on a pass.
		t.Fatalf("recent grew from %d to %d", before, len(f.svc.recent))
	}
	st, err := f.svc.GCState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Last == nil || st.Last.DryRun || !st.Last.OK || st.LastApply == nil {
		t.Fatalf("state = %+v", st)
	}
	if st.LastApply.Deleted != 1 || st.LastApply.Bytes <= 0 {
		t.Fatalf("apply run = %+v", st.LastApply)
	}
	if got.Summary() == "" {
		t.Fatal("a pass must summarize itself")
	}
}

// TestSweepAllPartialFailureIsAFailedPass: one unreadable registry fails the
// pass (the task and the summary say so) while the registries it could read
// still get their work done.
func TestSweepAllPartialFailureIsAFailedPass(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	ctx := context.Background()
	// The uploads prefix of one registry becomes unreadable.
	bad := uploadsPrefix("acme", "other")
	f.st.listKeyFn = func(prefix string, err error) error {
		if prefix == bad {
			return store.NewRetryable(prefix, errors.New("boom"))
		}
		return err
	}
	got, err := f.svc.SweepAll(ctx, GCSweepRequest{Actor: "alice"}, nil)
	if err == nil {
		t.Fatalf("a partial failure must fail the pass: %+v", got)
	}
	if got.Failed != 1 || got.Unreferenced == 0 {
		t.Fatalf("pass = %+v, want 1 failure and the other registry's work done", got)
	}
	st, serr := f.svc.GCState(ctx)
	if serr != nil || st.Last == nil || st.Last.OK || st.Last.Error == "" {
		t.Fatalf("summary = (%+v, %v), want a recorded failure", st, serr)
	}
}

// TestSweepAllIsIdempotent pins the second-pass no-op at instance scope.
func TestSweepAllIsIdempotent(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	ctx := context.Background()
	if _, err := f.svc.SweepAll(ctx, GCSweepRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.SweepAll(ctx, GCSweepRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Unreferenced != 0 || second.Uploads != 0 {
		t.Fatalf("second pass reclaimed %+v, want nothing", second)
	}
}

// TestGCStateCorruptFailsClosed: a mangled summary is an error the page shows,
// never a silent "no runs yet".
func TestGCStateCorruptFailsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := store.PutBytes(ctx, f.st, GCStateKey(), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GCState(ctx); err == nil {
		t.Fatal("a corrupt summary must error")
	}
	if err := f.svc.putGCState(ctx, func(*GCState) {}); err == nil {
		t.Fatal("putGCState over a corrupt summary must error")
	}
}

// TestGCStateWriteFailureDoesNotFailThePass: the summary is bookkeeping; the
// deletions already committed.
func TestGCStateWriteFailure(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	ctx := context.Background()
	if _, err := store.PutBytes(ctx, f.st, GCStateKey(), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	f.svc.Logf = func(string, ...any) {}
	got, err := f.svc.SweepAll(ctx, GCSweepRequest{}, nil)
	if err != nil {
		t.Fatalf("a failing summary write must not fail the pass: %v", err)
	}
	if got.Unreferenced != 1 {
		t.Fatalf("pass = %+v", got)
	}
}

// TestRegistryReposSkipsEmptyPrefixes proves the enumerator only names
// registries that carry objects (the no-registry repo is skipped without
// walking its keys).
func TestRegistryReposSkipsEmptyPrefixes(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	repos, err := f.svc.registryRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range repos {
		seen[id[0]+"/"+id[1]] = true
	}
	if !seen["acme/demo"] || !seen["acme/other"] {
		t.Fatalf("repos = %v", repos)
	}
	if seen["acme/plain"] {
		t.Fatalf("a repo with no registry was named: %v", repos)
	}
	// An injected enumerator wins (composition may pass identity's walk).
	f.svc.Repos = func(context.Context) ([][2]string, error) { return [][2]string{{"x", "y"}}, nil }
	repos, err = f.svc.registryRepos(context.Background())
	if err != nil || len(repos) != 1 || repos[0] != [2]string{"x", "y"} {
		t.Fatalf("injected repos = (%v, %v)", repos, err)
	}
}

// TestStartGCStartsJoinsAndAudits is the trigger contract: a task id comes back
// before the pass finishes, every trigger writes one packages_gc record, and a
// second start joins instead of starting another pass.
func TestStartGCStartsJoinsAndAudits(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	reg := wal.NewRegistry(context.Background(), f.st, config.Defaults())
	t.Cleanup(reg.Close)
	table := reg.Tasks()

	var audits []string
	f.svc.Audit = func(_ context.Context, actor, action, target, detail string) error {
		audits = append(audits, actor+"|"+action+"|"+target+"|"+detail)
		return nil
	}

	// Block the pass on a gate so the join path is deterministic.
	gate := make(chan struct{})
	f.svc.Repos = func(ctx context.Context) ([][2]string, error) {
		<-gate
		return nil, nil
	}
	task, err := f.svc.StartGC(context.Background(), table, "alice", true)
	if err != nil {
		t.Fatalf("start = %v", err)
	}
	if task.ID == "" || task.Kind != TaskKindGC || !task.DryRun || task.Joined {
		t.Fatalf("task = %+v", task)
	}
	if len(audits) != 1 || !containsAll(audits[0], "alice", AuditActionPackagesGC, "instance:packages", "dry-run") {
		t.Fatalf("audits = %v", audits)
	}
	// A second start joins the running pass and audits that trigger too.
	joined, err := f.svc.StartGC(context.Background(), table, "bob", false)
	if err != nil {
		t.Fatalf("join = %v", err)
	}
	if !joined.Joined || joined.ID != task.ID {
		t.Fatalf("join = %+v, want the same id %q", joined, task.ID)
	}
	if len(audits) != 2 || !containsAll(audits[1], "bob", "apply") {
		t.Fatalf("audits = %v", audits)
	}
	close(gate)
	// The pass completes; its own summary lands in the store.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, err := f.svc.GCState(context.Background()); err == nil && st.Last != nil {
			if st.Last.TaskID != task.ID {
				t.Fatalf("stored task id = %q, want %q", st.Last.TaskID, task.ID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pass never recorded its summary")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartGCWithoutTasksIsHonest: no task table = a named refusal, not a
// fabricated id.
func TestStartGCWithoutTasks(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.StartGC(context.Background(), nil, "alice", true); err == nil {
		t.Fatal("StartGC without a task table must error")
	}
	st, err := f.svc.GCStatus(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running != nil || st.State == nil {
		t.Fatalf("status = %+v", st)
	}
}

// TestStartGCAuditFailureDoesNotBlock: an audit write failure never hides a
// started pass.
func TestStartGCAuditFailure(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	reg := wal.NewRegistry(context.Background(), f.st, config.Defaults())
	t.Cleanup(reg.Close)
	table := reg.Tasks()
	f.svc.Logf = func(string, ...any) {}
	f.svc.Audit = func(context.Context, string, string, string, string) error {
		return errors.New("audit down")
	}
	task, err := f.svc.StartGC(context.Background(), table, "alice", true)
	if err != nil || task.ID == "" {
		t.Fatalf("start with a failing audit = (%+v, %v)", task, err)
	}
}

// TestStartGCRefusalSurfaces proves a draining table's refusal reaches the
// caller instead of a 2s stall with an empty id.
func TestStartGCRefusalSurfaces(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.StartGC(context.Background(), refusingRunner{}, "alice", true); err == nil {
		t.Fatal("a refusing table must refuse the start")
	}
}

// refusingRunner is the drained-table shape: Run refuses before a record
// exists (identity's deadRunner precedent).
type refusingRunner struct{}

func (refusingRunner) Run(context.Context, string, string, map[string]string, func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	return nil, errors.New("503 interrupted: instance shut down")
}

func (refusingRunner) Running(string, string) *wal.TaskRecord { return nil }

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// --- coverage of the report/state/trigger edges -------------------------------

func TestGCSweepSummaryShapes(t *testing.T) {
	cases := []struct {
		name string
		in   GCSweep
		want []string
	}{
		{"apply one", GCSweep{Unreferenced: 1, Repos: 1, Manifests: 3}, []string{"reclaimed 1 blob(s)", "1 registry"}},
		{"dry many", GCSweep{DryRun: true, Unreferenced: 4, Repos: 2}, []string{"would reclaim 4 blob(s)", "2 registries"}},
		{"staging", GCSweep{Uploads: 2, Tombstones: 1, Repos: 1}, []string{"2 expired upload(s)", "1 tombstone(s)"}},
		{"failure", GCSweep{Failed: 1, Repos: 1}, []string{"1 registry failed"}},
		{"failures", GCSweep{Failed: 2, Repos: 4}, []string{"2 registries failed"}},
		{"empty", GCSweep{}, []string{"reclaimed 0 blob(s)", "0 registries"}},
	}
	for _, c := range cases {
		got := c.in.Summary()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: %q missing %q", c.name, got, w)
			}
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", -5: "0 B", 1023: "1023 B", 1024: "1.0 KiB",
		1536: "1.5 KiB", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestGCStateShapes covers the absent/empty/zero-version reads.
func TestGCStateShapes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	st, err := f.svc.GCState(ctx)
	if err != nil || st.Version != 1 || st.Last != nil {
		t.Fatalf("absent = (%+v, %v)", st, err)
	}
	// A zero-value document reads as v1 with no runs (never a decode error).
	if err := f.st.Delete(ctx, GCStateKey(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, f.st, GCStateKey(), []byte(`{}`), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if st, err := f.svc.GCState(ctx); err != nil || st.Version != 1 || st.Last != nil {
		t.Fatalf("zero doc = (%+v, %v)", st, err)
	}
	// A v0 document (a hand-written summary) reads as v1.
	if err := f.st.Delete(ctx, GCStateKey(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, f.st, GCStateKey(), []byte(`{"last":{"at":"2026-01-01T00:00:00Z","ok":true}}`),
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if st, err := f.svc.GCState(ctx); err != nil || st.Version != 1 || st.Last == nil {
		t.Fatalf("v0 = (%+v, %v)", st, err)
	}
}

// TestPutGCStateRetriesAContendedCAS proves the CAS loop converges after one
// lost race and gives up (an error) after the attempts are spent.
func TestPutGCStateRetriesAContendedCAS(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	calls := 0
	f.st.putFn = func(key string, err error) error {
		if key != GCStateKey() {
			return err
		}
		calls++
		if calls == 1 {
			return store.NewPrecondition("", "raced")
		}
		return err
	}
	if err := f.svc.putGCState(ctx, func(st *GCState) { st.Last = &GCRun{At: "2026-01-01T00:00:00Z"} }); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want a retry", calls)
	}
	// Always-lost CAS: the honest error, no silent drop.
	f2 := newFixture(t)
	f2.st.putFn = func(key string, err error) error {
		if key == GCStateKey() {
			return store.NewPrecondition("", "raced")
		}
		return err
	}
	if err := f2.svc.putGCState(ctx, func(*GCState) {}); err == nil {
		t.Fatal("a permanently contended summary must error")
	}
	// A plain write failure maps to the store error.
	f3 := newFixture(t)
	f3.st.putFn = func(key string, err error) error {
		if key == GCStateKey() {
			return store.NewRetryable("", errors.New("down"))
		}
		return err
	}
	if err := f3.svc.putGCState(ctx, func(*GCState) {}); err == nil {
		t.Fatal("a failing write must error")
	}
}

// TestSweepAllEnumerationFailure: an unreadable repo listing fails the pass
// and is recorded.
func TestSweepAllEnumerationFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.svc.Repos = func(context.Context) ([][2]string, error) { return nil, errors.New("catalog down") }
	got, err := f.svc.SweepAll(ctx, GCSweepRequest{DryRun: true}, nil)
	if err == nil {
		t.Fatalf("enumeration failure = %+v", got)
	}
	st, serr := f.svc.GCState(ctx)
	if serr != nil || st.Last == nil || st.Last.OK {
		t.Fatalf("summary = (%+v, %v)", st, serr)
	}
}

// TestSweepAllCancelled: a canceled pass records the honest "cancelled" note
// and stops enumerating.
func TestSweepAllCancelled(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.svc.Repos = func(context.Context) ([][2]string, error) {
		cancel()
		return [][2]string{{"acme", "demo"}}, nil
	}
	got, err := f.svc.SweepAll(ctx, GCSweepRequest{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pass = (%+v, %v)", got, err)
	}
	st, _ := f.svc.GCState(context.Background())
	if st.Last == nil || !strings.Contains(st.Last.Error, "cancelled") {
		t.Fatalf("summary = %+v", st.Last)
	}
}

// prefixFailStore fails ListPrefixes for one prefix (the delimiter listing the
// registry enumeration walks; it lives on the Memory store, so a wrapper is the
// seam a fault takes).
type prefixFailStore struct {
	store.ObjectStore
	prefix string
}

func (p *prefixFailStore) ListPrefixes(ctx context.Context, prefix string, fn func(string) error) error {
	if prefix == p.prefix {
		return store.NewRetryable(prefix, errors.New("boom"))
	}
	return p.ObjectStore.ListPrefixes(ctx, prefix, fn)
}

// TestRegistryReposErrors: every delimiter listing fails closed.
func TestRegistryReposErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.svc.Store = &prefixFailStore{ObjectStore: f.st, prefix: "repos/"}
	if _, err := f.svc.registryRepos(ctx); err == nil {
		t.Fatal("an unreadable repos/ listing must error")
	}
	f2 := newFixture(t)
	if _, err := store.PutBytes(ctx, f2.st, "repos/acme/demo/manifest.pb", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	f2.svc.Store = &prefixFailStore{ObjectStore: f2.st, prefix: "repos/acme/"}
	if _, err := f2.svc.registryRepos(ctx); err == nil {
		t.Fatal("an unreadable owner listing must error")
	}
	f3 := newFixture(t)
	if _, err := store.PutBytes(ctx, f3.st, "repos/acme/demo/manifest.pb", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	f3.svc.Store = &prefixFailStore{ObjectStore: f3.st, prefix: RepoBase("acme", "demo") + "/"}
	if _, err := f3.svc.registryRepos(ctx); err == nil {
		t.Fatal("a failing registry probe must error")
	}
}

// TestGCStatusErrorAndRunning: a corrupt summary surfaces, a live pass is named.
func TestGCStatusErrorAndRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if st, err := f.svc.GCStatus(ctx, nil); err != nil || st.Running != nil || st.State == nil {
		t.Fatalf("idle = (%+v, %v)", st, err)
	}
	if _, err := store.PutBytes(ctx, f.st, GCStateKey(), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GCStatus(ctx, nil); err == nil {
		t.Fatal("a corrupt summary must surface")
	}
}

// TestStartGCJoinsViaTheHint: when the pre-check misses a pass that starts in
// the same instant, the table's join hint still answers its id.
func TestStartGCJoinHint(t *testing.T) {
	f := newFixture(t)
	runner := &raceRunner{rec: &wal.TaskRecord{ID: "joined-1", Kind: TaskKindGC, Repo: InstanceScope}}
	audits := 0
	f.svc.Audit = func(context.Context, string, string, string, string) error { audits++; return nil }
	task, err := f.svc.StartGC(context.Background(), runner, "alice", false)
	if err != nil {
		t.Fatalf("join = %v", err)
	}
	if task.ID != "joined-1" || !task.Joined {
		t.Fatalf("task = %+v", task)
	}
	if audits != 1 {
		t.Fatalf("audits = %d", audits)
	}
}

// raceRunner never runs the body and never returns until its context dies: the
// "another caller owns the record" shape.
type raceRunner struct {
	rec *wal.TaskRecord
}

func (r *raceRunner) Run(ctx context.Context, repo, kind string, _ map[string]string, _ func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *raceRunner) Running(repo, kind string) *wal.TaskRecord { return r.rec }

// TestStartGCCallerContextEnds: a caller whose context dies before the record
// exists gets its error, not a hang.
func TestStartGCCallerContextEnds(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	runner := &blockingRunner{started: make(chan struct{})}
	go func() {
		<-runner.started
		cancel()
	}()
	if _, err := f.svc.StartGC(ctx, runner, "alice", true); err == nil {
		t.Fatal("a canceled caller must not get a task")
	}
}

// blockingRunner signals Run's entry and then blocks (no record, no hint).
type blockingRunner struct{ started chan struct{} }

func (r *blockingRunner) Run(ctx context.Context, repo, kind string, _ map[string]string, _ func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	close(r.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *blockingRunner) Running(string, string) *wal.TaskRecord { return nil }

// TestStartGCNoID: a table that returns without a record and without an error
// (the draining shape) yields the honest refusal.
func TestStartGCNoID(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.StartGC(context.Background(), emptyRunner{}, "alice", true); err == nil {
		t.Fatal("a start with no record must not answer an id")
	}
}

type emptyRunner struct{}

func (emptyRunner) Run(context.Context, string, string, map[string]string, func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	return nil, nil
}

func (emptyRunner) Running(string, string) *wal.TaskRecord { return nil }

func TestBoolDigit(t *testing.T) {
	if boolDigit(true) != "1" || boolDigit(false) != "0" {
		t.Fatal("boolDigit")
	}
}

// emptyBodyStore answers one Get with a zero-byte object (an S3 zero-byte
// summary is not a decode error).
type emptyBodyStore struct {
	store.ObjectStore
	key string
}

func (e *emptyBodyStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == e.key {
		return store.Object{Body: nopReadCloser{}, Meta: store.ObjectMeta{Key: key, Version: "v"}}, nil
	}
	return e.ObjectStore.Get(ctx, key, opts)
}

type nopReadCloser struct{}

func (nopReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (nopReadCloser) Close() error             { return nil }

func TestGCStateEmptyBody(t *testing.T) {
	f := newFixture(t)
	f.svc.Store = &emptyBodyStore{ObjectStore: f.st, key: GCStateKey()}
	st, err := f.svc.GCState(context.Background())
	if err != nil || st.Version != 1 || st.Last != nil {
		t.Fatalf("empty body = (%+v, %v)", st, err)
	}
}

// TestStartGCWithoutAuditSeam: a build with no audit family still starts (and
// records nothing) instead of panicking.
func TestStartGCWithoutAuditSeam(t *testing.T) {
	f := newFixture(t)
	runner := &raceRunner{rec: &wal.TaskRecord{ID: "joined-2", Kind: TaskKindGC}}
	if _, err := f.svc.StartGC(context.Background(), runner, "alice", true); err != nil {
		t.Fatalf("nil audit seam = %v", err)
	}
}

// TestStartGCRunFailsAfterAJoinHint: Run returns an error while the table
// already reports a record — the record wins (the pass exists).
func TestStartGCRunFailsAfterAJoinHint(t *testing.T) {
	f := newFixture(t)
	runner := &hintErrRunner{rec: &wal.TaskRecord{ID: "joined-3", Kind: TaskKindGC}}
	task, err := f.svc.StartGC(context.Background(), runner, "alice", false)
	if err != nil || task.ID != "joined-3" || task.Joined {
		t.Fatalf("start = (%+v, %v)", task, err)
	}
}

// hintErrRunner fails Run immediately and reports no record on the pre-check,
// then the record (the join that raced the pre-check: the finish arm answers
// the hint instead of the error).
type hintErrRunner struct {
	rec   *wal.TaskRecord
	calls int
}

func (r *hintErrRunner) Run(context.Context, string, string, map[string]string, func(context.Context, *wal.Task) error) (*wal.TaskRecord, error) {
	return nil, errors.New("join lost")
}

func (r *hintErrRunner) Running(string, string) *wal.TaskRecord {
	r.calls++
	if r.calls == 1 {
		return nil
	}
	return r.rec
}

// TestGCStatusNamesTheLivePass: the page's status reports the running record.
func TestGCStatusNamesTheLivePass(t *testing.T) {
	f := newFixture(t)
	runner := &raceRunner{rec: &wal.TaskRecord{ID: "live-9", Kind: TaskKindGC, Started: "2026-11-22T10:00:00Z",
		Params: map[string]string{"dry_run": "1"}}}
	st, err := f.svc.GCStatus(context.Background(), runner)
	if err != nil || st.Running == nil || st.Running.ID != "live-9" {
		t.Fatalf("status = (%+v, %v)", st, err)
	}
}

// TestSweepAllPrunesStaleInstanceRows: the instance pass is the one writer that
// sees every registry, so it drops usage-index rows for registries that no
// longer exist (a moved or deleted repository) — a per-repository pass cannot
// know about them.
func TestSweepAllPrunesStaleInstanceRows(t *testing.T) {
	f, _ := gcInstanceFixture(t)
	ctx := context.Background()
	// A stale row plus a real one.
	raw, _ := json.Marshal(InstanceUsage{Version: 1, Repos: map[string]int64{
		"acme/other":    5,
		"gone/registry": 1 << 20,
	}})
	if _, err := store.PutBytes(ctx, f.st, InstanceUsageKey(), raw, store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	// A dry run must not prune.
	if _, err := f.svc.SweepAll(ctx, GCSweepRequest{Actor: "alice", DryRun: true}, nil); err != nil {
		t.Fatal(err)
	}
	u, err := f.svc.InstanceUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Repos["gone/registry"]; !ok {
		t.Fatal("a dry run pruned the index")
	}
	if _, err := f.svc.SweepAll(ctx, GCSweepRequest{Actor: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	u, err = f.svc.InstanceUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Repos["gone/registry"]; ok {
		t.Fatalf("stale row survived the instance pass: %+v", u)
	}
	if _, ok := u.Repos["acme/demo"]; !ok {
		t.Fatalf("a real registry row was pruned: %+v", u)
	}
	var sum int64
	for _, b := range u.Repos {
		sum += b
	}
	if u.TotalBytes != sum {
		t.Fatalf("total = %d, rows sum to %d", u.TotalBytes, sum)
	}
}
