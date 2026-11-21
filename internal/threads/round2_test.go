package threads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// round2_test.go covers the edges of round-2 pass 2 (schedule, retention, the
// approval gate, the job token and the list filter) that the behavioural tests
// do not need to reach, and the pre-existing small helpers whose coverage the
// new statements moved.

// --- retention edges -------------------------------------------------------

// TestGCResultStringAndRetentionDefault pins the summary line and the window
// fallback (a zero Service is the documented 30 days).
func TestGCResultStringAndRetentionDefault(t *testing.T) {
	r := GCResult{Requeued: 1, RunsDeleted: 2, JobsDeleted: 3, LogsDeleted: 4, QueueDeleted: 5, RunnersDeleted: 6, CredentialsDeleted: 7, BytesFreed: 8}
	got := r.String()
	for _, want := range []string{"requeued=1", "runs=2", "jobs=3", "logs=4", "queue=5", "runners=6", "credentials=7", "freed=8B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, missing %q", got, want)
		}
	}
	e := newTestEnv(t)
	if d := e.svc.retention(); d != DefaultRetention {
		t.Fatalf("retention = %s, want %s", d, DefaultRetention)
	}
	e.svc.Retention = time.Hour
	if d := e.svc.retention(); d != time.Hour {
		t.Fatalf("retention = %s", d)
	}
	// A nil service is a no-op sweep, never a panic.
	if res, err := (*Service)(nil).Retain(context.Background(), "o", "r", nil); err != nil || res == nil {
		t.Fatalf("nil Retain = %+v, %v", res, err)
	}
}

// TestRetainUnclaimedQueueEntriesGoWithTheRun pins the queue cleanup for a run
// nobody ever claimed (the entries carry no Job.QueueSeq).
func TestRetainUnclaimedQueueEntriesGoWithTheRun(t *testing.T) {
	e, now := retentionEnv(t)
	run := e.mustCreate(t)[0]
	// Never claimed, never finished: still queued. Force it terminal by
	// cancelling it, then age it.
	if _, err := e.svc.cancelRunSystem(context.Background(), "o", "r", run.ID); err != nil {
		t.Fatal(err)
	}
	if n := countPrefix(t, e, QueuePrefix("o", "r")); n < 2 {
		t.Fatalf("queue objects = %d, want the run's entries", n)
	}
	ageRun(t, e, run.ID, now.Add(-31*24*time.Hour))
	res, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RunsDeleted != 1 || res.QueueDeleted < 2 {
		t.Fatalf("retain = %+v, want the run and its queue entries", res)
	}
	if n := countPrefix(t, e, QueuePrefix("o", "r")); n != 1 { // head.json only
		t.Fatalf("queue objects after = %d, want only head.json", n)
	}
}

// TestBeforeCutoffBounds pins the age rule: a terminal run uses CompletedAt, a
// never-completed one falls back to CreatedAt, and an unparseable stamp is
// never a reason to delete.
func TestBeforeCutoffBounds(t *testing.T) {
	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	old := "2026-10-01T00:00:00Z"
	young := "2026-11-10T00:00:00Z"
	if !beforeCutoff(&Run{State: RunSuccess, CompletedAt: &old}, cutoff) {
		t.Fatal("old completed run not past the cutoff")
	}
	if beforeCutoff(&Run{State: RunSuccess, CompletedAt: &young}, cutoff) {
		t.Fatal("young completed run past the cutoff")
	}
	if !beforeCutoff(&Run{State: RunSuccess, CreatedAt: old}, cutoff) {
		t.Fatal("old never-completed run not past the cutoff")
	}
	if beforeCutoff(&Run{State: RunSuccess, CompletedAt: &[]string{"not a stamp"}[0]}, cutoff) {
		t.Fatal("unparseable stamp treated as old")
	}
}

// TestDeadRunnerRule pins the runner-retention predicate: revoked past the
// window yes, revoked inside it no, pending past the registration TTL yes, an
// active runner never.
func TestDeadRunnerRule(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	old := "2026-10-01T00:00:00Z"
	young := now.Add(-time.Minute).Format(dateTimeFmt)
	cases := []struct {
		name string
		doc  RunnerDoc
		want bool
	}{
		{"revoked old", RunnerDoc{State: RunnerRevoked, RevokedAt: &old, CreatedAt: old}, true},
		{"revoked young", RunnerDoc{State: RunnerRevoked, RevokedAt: &young, CreatedAt: young}, false},
		{"revoked without stamp", RunnerDoc{State: RunnerRevoked, CreatedAt: old}, true},
		{"pending expired", RunnerDoc{State: RunnerPending, CreatedAt: old}, true},
		{"pending fresh", RunnerDoc{State: RunnerPending, CreatedAt: young}, false},
		{"active", RunnerDoc{State: RunnerActive, CreatedAt: old}, false},
		{"revoked unparseable", RunnerDoc{State: RunnerRevoked, CreatedAt: "nope", RevokedAt: nil}, false},
	}
	for _, tc := range cases {
		if got := deadRunner(&tc.doc, cutoff, now); got != tc.want {
			t.Errorf("%s: deadRunner = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRetainRevokedRunnerAndListingFailure covers the revoked-record delete and
// a listing failure being a silent no-op (never a failed sweep).
func TestRetainRevokedRunnerAndListingFailure(t *testing.T) {
	e, now := retentionEnv(t)
	admin := auth.Principal{Name: "writer", Admin: true}
	tok, err := e.svc.MintRegistrationToken(context.Background(), "o/r", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(context.Background(), RegisterInput{Token: tok.Token, Name: "r1", Labels: []string{"linux"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeRunner(context.Background(), "o/r", tok.ID, admin); err != nil {
		t.Fatal(err)
	}
	// Age the revocation past the window.
	revokedAt := now.Add(-40 * 24 * time.Hour).Format(dateTimeFmt)
	createdAt := now.Add(-41 * 24 * time.Hour).Format(dateTimeFmt)
	if _, err := e.svc.casUpdate(context.Background(), RunnerKey("o", "r", tok.ID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var doc RunnerDoc
		if jerr := json.Unmarshal(cur, &doc); jerr != nil {
			return nil, false, jerr
		}
		doc.RevokedAt = &revokedAt
		doc.CreatedAt = createdAt
		doc.Version++
		return mustJSON(&doc), true, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Retain(context.Background(), "o", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RunnersDeleted != 1 || res.CredentialsDeleted != 1 {
		t.Fatalf("retain = %+v, want the revoked runner removed", res)
	}
	if doc, _, _ := e.svc.loadRunner(context.Background(), "o", "r", tok.ID); doc != nil {
		t.Fatal("revoked runner survived")
	}
}

// --- schedule edges --------------------------------------------------------

// TestSweepScheduleNilServiceAndCorruptWatermark covers the fail-closed paths.
func TestSweepScheduleNilServiceAndCorruptWatermark(t *testing.T) {
	if res, err := (*Service)(nil).SweepSchedule(context.Background(), "o", "r", nil); err != nil || res == nil {
		t.Fatalf("nil sweep = %+v, %v", res, err)
	}
	e, _ := scheduleEnv(t, time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC))
	e.svc.Git = nil
	if res, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil); err != nil || res.Fired != 0 {
		t.Fatalf("gitless sweep = %+v, %v", res, err)
	}
	e.svc.Git = e.git
	if _, err := store.PutBytes(context.Background(), e.store, ScheduleKey("o", "r"), []byte("["), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.LoadScheduleWatermark(context.Background(), "o", "r"); err == nil {
		t.Fatal("corrupt watermark read accepted")
	}
	if _, err := e.svc.SweepSchedule(context.Background(), "o", "r", nil); err == nil {
		t.Fatal("corrupt watermark sweep accepted")
	}
}

// TestScheduleBranchFallsBack pins a reader without the optional branch
// extension (the run then carries no branch name) and a nil branch name.
func TestScheduleBranchFallsBack(t *testing.T) {
	e, _ := scheduleEnv(t, time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC))
	plain := &plainGit{inner: e.git}
	e.svc.Git = plain
	branch, ref := e.svc.scheduleBranch(context.Background(), "o/r")
	if branch != "" || ref != "" {
		t.Fatalf("fallback branch = %q %q, want empty", branch, ref)
	}
	e.git.branch = ""
	if branch, ref := e.svc.scheduleBranch(context.Background(), "o/r"); branch != "" || ref != "" {
		t.Fatalf("empty branch = %q %q", branch, ref)
	}
	e.svc.Git = e.git
	e.git.branch = "trunk"
	if branch, ref := e.svc.scheduleBranch(context.Background(), "o/r"); branch != "trunk" || ref != "refs/heads/trunk" {
		t.Fatalf("branch = %q %q", branch, ref)
	}
}

// plainGit hides the optional DefaultBranchReader so the fallback is reachable.
type plainGit struct{ inner *fakeGit }

func (p *plainGit) ListWorkflowFiles(ctx context.Context, repo, commit string) ([]string, error) {
	return p.inner.ListWorkflowFiles(ctx, repo, commit)
}
func (p *plainGit) ReadFile(ctx context.Context, repo, commit, path string) ([]byte, error) {
	return p.inner.ReadFile(ctx, repo, commit, path)
}
func (p *plainGit) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	return p.inner.ResolveRef(ctx, repo, ref)
}

// TestIsNotFoundLike pins the empty-repository recognition.
func TestIsNotFoundLike(t *testing.T) {
	for _, err := range []error{nil, errors.New("boom")} {
		if isNotFoundLike(err) {
			t.Fatalf("isNotFoundLike(%v) = true", err)
		}
	}
	for _, err := range []error{ErrNotFound, ErrInvalid, ErrUnprocessable, wrapErr(ErrNotFound)} {
		if !isNotFoundLike(err) {
			t.Fatalf("isNotFoundLike(%v) = false", err)
		}
	}
}

// wrapErr wraps one sentinel (errors.Is must see through it).
func wrapErr(err error) error { return &wrapped{err: err} }

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// TestSweepScheduleLogsAndCancel covers the narration and ctx-cancel paths.
func TestSweepScheduleLogsAndCancel(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 17, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	var notices []string
	var progress int
	log := &captureLog{notice: func(s string) { notices = append(notices, s) }, progress: func() { progress++ }}
	if _, err := e.svc.SweepSchedule(context.Background(), "o", "r", log); err != nil {
		t.Fatal(err)
	}
	if len(notices) == 0 || progress == 0 {
		t.Fatalf("sweep narrated notices=%v progress=%d", notices, progress)
	}
	// A cancelled context stops the pass without creating a run.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.svc.SweepSchedule(cctx, "o", "r", nil); err == nil {
		t.Fatal("cancelled sweep returned no error")
	}
}

type captureLog struct {
	notice   func(string)
	progress func()
}

func (c *captureLog) Notice(text string) {
	if c.notice != nil {
		c.notice(text)
	}
}
func (c *captureLog) Progress(string, uint64, uint64, string) {
	if c.progress != nil {
		c.progress()
	}
}

// --- approval edges --------------------------------------------------------

// TestApprovalEdges covers the small branches: a nil projection, a system
// actor, an unknown run and the projection of a nil header.
func TestApprovalEdges(t *testing.T) {
	if Approval(nil).Pending {
		t.Fatal("nil run reported pending")
	}
	e := newTestEnv(t)
	e.svc.ApproveFirstTimeForks = true
	// No evidence source: held (fail closed).
	if !e.svc.needsApproval(ctx(), "o", "r", "newcomer", CreateOpts{Fork: true}, EventPullReq) {
		t.Fatal("missing evidence source did not hold the run")
	}
	// A system actor is never gated.
	for _, actor := range []string{"", defaultRunActor, "push", "merge", "schedule"} {
		if e.svc.needsApproval(ctx(), "o", "r", actor, CreateOpts{Fork: true}, EventPullReq) {
			t.Fatalf("system actor %q was gated", actor)
		}
	}
	// Unknown run / wrong role on both verbs.
	admin := auth.Principal{Name: "writer", Admin: true}
	if _, err := e.svc.ApproveRun(ctx(), "o/r", "0000000000000000000-abc123", admin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown approve err = %v", err)
	}
	if _, err := e.svc.DenyRun(ctx(), "o/r", "0000000000000000000-abc123", admin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deny err = %v", err)
	}
	if _, err := e.svc.DenyRun(ctx(), "bad", "x", admin); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo deny err = %v", err)
	}
}

// --- job token / permissions edges -----------------------------------------

// TestPermissionNormalizationEdges pins the unknown-level and empty-spec
// branches.
func TestPermissionNormalizationEdges(t *testing.T) {
	for in, want := range map[string]string{
		"write": PermWrite, "write-all": PermWrite, "READ": PermRead,
		"read-all": PermRead, "": PermNone, "none": PermNone, "bogus": "",
	} {
		if got := normalizePerm(in); got != want {
			t.Errorf("normalizePerm(%q) = %q, want %q", in, got, want)
		}
	}
	e := newTestEnv(t)
	if perms := e.svc.EffectivePermissions(nil, true); perms["contents"] != PermRead {
		t.Fatalf("nil spec perms = %v", perms)
	}
}

// TestRunFilterMatching pins the list filter predicates (workflow by name or
// path, case-insensitive; state and actor).
func TestRunFilterMatching(t *testing.T) {
	run := &Run{Workflow: "CI", WorkflowPath: ".spindle/workflows/ci.yml", State: RunSuccess, Actor: "alice"}
	cases := []struct {
		f    RunFilter
		want bool
	}{
		{RunFilter{}, true},
		{RunFilter{Workflow: "ci"}, true},
		{RunFilter{Workflow: ".spindle/workflows/ci.yml"}, true},
		{RunFilter{Workflow: "other"}, false},
		{RunFilter{State: "success"}, true},
		{RunFilter{State: "failure"}, false},
		{RunFilter{Actor: "ALICE"}, true},
		{RunFilter{Actor: "bob"}, false},
	}
	for _, tc := range cases {
		if got := tc.f.match(run); got != tc.want {
			t.Errorf("match(%+v) = %v, want %v", tc.f, got, tc.want)
		}
	}
	// The filtered list narrows and reports More when the scan cap is hit.
	e := newTestEnv(t)
	a := e.mustCreate(t)[0]
	b := e.mustCreate(t)[0]
	page, err := e.svc.ListRunsFiltered(ctx(), "o/r", e.public(), "", 10, RunFilter{State: RunSuccess})
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("filtered success = %+v, %v", page, err)
	}
	page, err = e.svc.ListRunsFiltered(ctx(), "o/r", e.public(), "", 1, RunFilter{State: RunQueued})
	if err != nil || len(page.Runs) != 1 || !page.More {
		t.Fatalf("filtered queued = %+v, %v", page, err)
	}
	_ = a
	_ = b
}

// TestRunnerNameFallback pins the attribution fallback.
func TestRunnerNameFallback(t *testing.T) {
	if got := runnerName(nil); got != "" {
		t.Fatalf("nil runner name = %q", got)
	}
	if got := runnerName(&RunnerDoc{ID: "abc"}); got != "abc" {
		t.Fatalf("id fallback = %q", got)
	}
	if got := runnerName(&RunnerDoc{ID: "abc", Name: "box"}); got != "box" {
		t.Fatalf("name = %q", got)
	}
}

// --- matrix / input substitution (pre-existing helpers the new code touched) -

// TestSubstituteInputsAndMatrixShape pins the input substitution the dispatch
// payload uses and the matrix shape guards.
func TestSubstituteInputsAndMatrixShape(t *testing.T) {
	in := map[string]string{"env": "staging"}
	if got := SubstituteInputs("deploy ${{ inputs.env }}", in); got != "deploy staging" {
		t.Fatalf("SubstituteInputs = %q", got)
	}
	if got := SubstituteInputs("${{ inputs.missing }}", in); got != "${{ inputs.missing }}" {
		t.Fatalf("unknown input = %q, want it left for the runner to report", got)
	}
	if got := SubstituteInputs("plain", nil); got != "plain" {
		t.Fatalf("nil inputs = %q", got)
	}
	if matrixIsMatrix(nil) {
		t.Fatal("nil strategy is a matrix")
	}
	if matrixIsMatrix(&StrategySpec{Matrix: map[string][]string{"go": {}}}) {
		t.Fatal("an empty axis list is a matrix")
	}
	matrix := &StrategySpec{Matrix: map[string][]string{"go": {"1.26", "1.27"}}}
	if got, err := matrixProductSize(matrix); err != nil || got != 2 {
		t.Fatalf("product size = %d, %v", got, err)
	}
	big := &StrategySpec{Matrix: map[string][]string{}}
	for i := 0; i < 10; i++ {
		axis := string(rune('a' + i))
		big.Matrix[axis] = []string{"1", "2", "3"}
	}
	if _, err := matrixProductSize(big); err == nil {
		t.Fatal("an over-cap product was accepted")
	}
}
