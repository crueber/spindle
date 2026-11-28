package threads

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// The merge_group trigger's ref contract and run shape (PAR-016).

func TestParseMergeQueueRef(t *testing.T) {
	head := strings.Repeat("a", 40)
	sha256Head := strings.Repeat("b", 64)
	cases := []struct {
		name string
		ref  string
		ok   bool
		base string
		num  int
	}{
		{"main", "refs/heads/gh-readonly-queue/main/pr-7-" + head, true, "main", 7},
		{"base with a slash", "refs/heads/gh-readonly-queue/release/1.0/pr-123-" + head, true, "release/1.0", 123},
		{"sha256 repository", "refs/heads/gh-readonly-queue/main/pr-1-" + sha256Head, true, "main", 1},
		{"not a ref", "main", false, "", 0},
		{"ordinary branch", "refs/heads/someone/gh-readonly-queue/main/pr-7-" + head, false, "", 0},
		{"missing pr", "refs/heads/gh-readonly-queue/main/pr-" + head, false, "", 0},
		{"zero number", "refs/heads/gh-readonly-queue/main/pr-0-" + head, false, "", 0},
		{"leading zero", "refs/heads/gh-readonly-queue/main/pr-07-" + head, false, "", 0},
		{"short sha", "refs/heads/gh-readonly-queue/main/pr-7-abc123", false, "", 0},
		{"uppercase sha", "refs/heads/gh-readonly-queue/main/pr-7-" + strings.ToUpper(head), false, "", 0},
		{"empty base", "refs/heads/gh-readonly-queue//pr-7-" + head, false, "", 0},
		{"dotdot base", "refs/heads/gh-readonly-queue/../pr-7-" + head, false, "", 0},
		{"leading dot component", "refs/heads/gh-readonly-queue/.hidden/pr-7-" + head, false, "", 0},
		{"lock suffix", "refs/heads/gh-readonly-queue/main.lock/pr-7-" + head, false, "", 0},
		{"space in base", "refs/heads/gh-readonly-queue/ma in/pr-7-" + head, false, "", 0},
	}
	for _, tc := range cases {
		mq, ok := ParseMergeQueueRef(tc.ref)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if mq.BaseBranch != tc.base || mq.Number != tc.num || mq.HeadSHA == "" ||
			mq.BaseRef != "refs/heads/"+tc.base || mq.Ref != tc.ref ||
			mq.Name != strings.TrimPrefix(tc.ref, "refs/heads/") {
			t.Errorf("%s: parsed %+v", tc.name, mq)
		}
	}
	// The prefix check is deliberately broader than the parser: the hook uses
	// it to refuse a malformed queue ref instead of treating it as a branch.
	if !IsMergeQueueRef("refs/heads/gh-readonly-queue/main/pr-0-x") {
		t.Fatal("a malformed queue ref must still claim the prefix")
	}
	if IsMergeQueueRef("refs/heads/main") {
		t.Fatal("an ordinary branch does not claim the queue prefix")
	}
}

const mergeGroupWorkflowYAML = `name: Queue
on:
  merge_group:
    types: [checks_requested]
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo queued
  test:
    runs-on: linux
    needs: [build]
    steps:
      - run: echo tested
`

func TestCreateRunMergeGroup(t *testing.T) {
	base := strings.Repeat("1", 40)
	head := strings.Repeat("2", 40)
	g := newFakeGit()
	g.refs["refs/heads/main"] = base
	g.add(base, ".spindle/workflows/queue.yml", mergeGroupWorkflowYAML)
	// The queue head's own file is different and must NOT be read: a merge
	// group runs the base branch's repository-owned workflow.
	g.add(head, ".spindle/workflows/queue.yml", "name: Hijacked\non: merge_group\njobs:\n  x:\n    runs-on: linux\n    steps:\n      - run: rm -rf /\n")

	svc := New(store.NewMemory(), &fakeRoles{public: true})
	svc.Git = g
	svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	ref := "refs/heads/gh-readonly-queue/main/pr-7-" + head
	mq, ok := ParseMergeQueueRef(ref)
	if !ok {
		t.Fatalf("the fixture ref %q does not parse", ref)
	}
	// Action is omitted deliberately: checks_requested is the one emitted type
	// and the default for the trigger.
	runs, err := svc.CreateRun(ctx(), "o/r", head, EventMergeGroup, CreateOpts{
		Ref: ref, RefName: mq.BaseBranch,
		EventPayload: MergeGroupEventPayload("o/r", ref, head, mq.BaseRef, "", mq.Number, "push"),
	})
	if err != nil || len(runs) != 1 {
		t.Fatalf("create: %v, %d runs", err, len(runs))
	}
	run := runs[0]
	if run.Event != EventMergeGroup || run.Commit != head || run.Ref != ref || run.Branch != "" {
		t.Fatalf("run header = %+v", run)
	}
	if run.Workflow != "Queue" || run.WorkflowPath != ".spindle/workflows/queue.yml" || run.WorkflowCommit != base {
		t.Fatalf("workflow selection = %+v, want the base branch's file", run)
	}
	if !run.Trusted {
		t.Fatal("a merge-group run is repository-owned and must be trusted")
	}
	fields := EventFieldsOf(run.EventPayload)
	want := map[string]string{
		"event.action":               "checks_requested",
		"event.number":               "7",
		"event.merge_group.head_ref": ref,
		"event.merge_group.head_sha": head,
		"event.merge_group.base_ref": "refs/heads/main",
		"event.merge_group.base_sha": base,
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%s = %q, want %q (fields %+v)", k, fields[k], v, fields)
		}
	}
	// github.ref/sha/ref_name are the queue entry's identity, never a branch.
	ctxFields := githubContext(run, "Queue")
	if ctxFields["ref"] != ref || ctxFields["sha"] != head {
		t.Fatalf("github.ref/sha = %q/%q, want %q/%q", ctxFields["ref"], ctxFields["sha"], ref, head)
	}
	if got := runRefName(run); got != mq.Name || ctxFields["ref_name"] != mq.Name {
		t.Fatalf("github.ref_name = %q/%q, want %q", got, ctxFields["ref_name"], mq.Name)
	}
}

func TestCreateRunMergeGroupRefusals(t *testing.T) {
	base := strings.Repeat("1", 40)
	head := strings.Repeat("2", 40)
	g := newFakeGit()
	g.refs["refs/heads/main"] = base
	g.add(base, ".spindle/workflows/queue.yml", mergeGroupWorkflowYAML)
	svc := New(store.NewMemory(), &fakeRoles{public: true})
	svc.Git = g

	mkOpts := func() CreateOpts {
		ref := "refs/heads/gh-readonly-queue/main/pr-3-" + head
		return CreateOpts{
			Ref: ref, RefName: "main", Action: "checks_requested",
			EventPayload: MergeGroupEventPayload("o/r", ref, head, "refs/heads/main", "", 3, "push"),
		}
	}
	if _, err := svc.CreateRun(ctx(), "o/r", head, EventMergeGroup, CreateOpts{
		Ref: "refs/heads/main", Action: "checks_requested",
	}); err == nil || !strings.Contains(err.Error(), "gh-readonly-queue") {
		t.Fatalf("a non-queue ref must be refused by shape, got %v", err)
	}
	opts := mkOpts()
	opts.Action = "destroyed"
	if _, err := svc.CreateRun(ctx(), "o/r", head, EventMergeGroup, opts); err == nil {
		t.Fatal("an unemitted merge_group type must be refused")
	}
	opts = mkOpts()
	opts.BaseBranch = "dev"
	if _, err := svc.CreateRun(ctx(), "o/r", head, EventMergeGroup, opts); err == nil {
		t.Fatal("a base branch that disagrees with the ref must be refused")
	}
	// A queue ref for a base the git reader cannot resolve fails closed.
	g.refs = map[string]string{}
	if _, err := svc.CreateRun(ctx(), "o/r", head, EventMergeGroup, mkOpts()); err == nil {
		t.Fatal("an unresolvable base branch must be refused")
	}
}

// TestMergeGroupRunChecksGate is PAR-016's gating half: the run's mirrored
// checks land on the queue head sha, and the existing require_checks merge-time
// gate reads that sha — so the queue entry's checks are what decide the merge.
func TestMergeGroupRunChecksGate(t *testing.T) {
	base := strings.Repeat("1", 40)
	queue := strings.Repeat("2", 40)
	mem := store.NewMemory()
	ck := checksTestService(t, mem, queue)
	gitReader := newFakeGit()
	gitReader.refs["refs/heads/main"] = base
	gitReader.add(base, ".spindle/workflows/queue.yml", mergeGroupWorkflowYAML)
	svc := New(mem, &fakeRoles{public: true})
	svc.Git = gitReader
	svc.Checks = realCheckSink{svc: ck}
	svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

	ref := "refs/heads/gh-readonly-queue/main/pr-9-" + queue
	mq, _ := ParseMergeQueueRef(ref)
	created, err := svc.CreateRun(ctx(), "o/r", queue, EventMergeGroup, CreateOpts{
		Ref: ref, RefName: mq.BaseBranch, Action: "checks_requested",
		EventPayload: MergeGroupEventPayload("o/r", ref, queue, mq.BaseRef, "", mq.Number, "push"),
	})
	if err != nil || len(created) != 1 {
		t.Fatalf("create: %v, %d runs", err, len(created))
	}
	view, err := svc.GetRun(ctx(), "o/r", created[0].ID, auth.Anonymous())
	if err != nil {
		t.Fatal(err)
	}
	build, test := view.Jobs[0], view.Jobs[1]

	if err := putPolicy(mem, `{"version":1,"rules":[{"name":"queue-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["threads/queue/build","threads/queue/test"]}}}]}`); err != nil {
		t.Fatal(err)
	}
	err = ck.CheckRequiredChecks(ctx(), "o", "r", queue, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "threads/queue/build (pending)") {
		t.Fatalf("queued merge-group gate = %v", err)
	}
	for _, j := range []*Job{build, test} {
		if _, err := svc.UpdateJob(ctx(), "o/r", created[0].ID, j.ID, JobUpdate{State: JobSuccess}); err != nil {
			t.Fatalf("green %s: %v", j.ID, err)
		}
	}
	if err := ck.CheckRequiredChecks(ctx(), "o", "r", queue, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("green merge-group gate: %v", err)
	}
	if _, err := svc.UpdateJob(ctx(), "o/r", created[0].ID, test.ID, JobUpdate{State: JobFailure}); err != nil {
		t.Fatal(err)
	}
	err = ck.CheckRequiredChecks(ctx(), "o", "r", queue, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "threads/queue/test (failure)") {
		t.Fatalf("failing merge-group gate = %v", err)
	}
}

// TestFillMergeGroupPayloadShapes keeps the payload normalization total: a nil
// payload and a payload whose merge_group value is not an object both end as
// the documented projection.
func TestFillMergeGroupPayloadShapes(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	mq, ok := ParseMergeQueueRef("refs/heads/gh-readonly-queue/main/pr-5-" + head)
	if !ok {
		t.Fatal("fixture ref does not parse")
	}
	for name, payload := range map[string]map[string]any{
		"nil":    nil,
		"scalar": {"merge_group": "not-an-object"},
	} {
		got := fillMergeGroupPayload(payload, "o/r", mq, head, base, "push")
		group, ok := got["merge_group"].(map[string]any)
		if !ok {
			t.Fatalf("%s: merge_group = %T", name, got["merge_group"])
		}
		if group["head_sha"] != head || group["base_sha"] != base || group["base_ref"] != "refs/heads/main" {
			t.Fatalf("%s: group = %+v", name, group)
		}
	}
}

// TestMergeGroupRunsRead is the merge queue's narrow run read (the Merge
// queue card): the runs for a queue head, newest first, and nothing else.
func TestMergeGroupRunsRead(t *testing.T) {
	base := strings.Repeat("1", 40)
	queue := strings.Repeat("2", 40)
	mem := store.NewMemory()
	gitReader := newFakeGit()
	gitReader.refs["refs/heads/main"] = base
	gitReader.add(base, ".spindle/workflows/queue.yml", mergeGroupWorkflowYAML)
	svc := New(mem, &fakeRoles{public: true})
	svc.Git = gitReader
	var ticks int64
	svc.Now = func() time.Time { return time.Unix(1_700_000_000, atomic.AddInt64(&ticks, 1)).UTC() }

	ref := "refs/heads/gh-readonly-queue/main/pr-9-" + queue
	mq, _ := ParseMergeQueueRef(ref)
	mk := func() []*Run {
		runs, err := svc.CreateRun(ctx(), "o/r", queue, EventMergeGroup, CreateOpts{
			Ref: ref, RefName: mq.BaseBranch, Action: "checks_requested",
			EventPayload: MergeGroupEventPayload("o/r", ref, queue, mq.BaseRef, "", mq.Number, "push"),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return runs
	}
	first := mk()
	second := mk()
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("runs = %d/%d", len(first), len(second))
	}
	// A push run on the same commit must not appear.
	if _, err := svc.CreateRun(ctx(), "o/r", queue, EventPush, CreateOpts{
		Branch: "main", Ref: "refs/heads/main", Actor: "push",
	}); err != nil {
		t.Fatalf("push run: %v", err)
	}
	got, err := svc.MergeGroupRuns(ctx(), "o/r", queue)
	if err != nil {
		t.Fatalf("MergeGroupRuns: %v", err)
	}
	if len(got) != 2 || got[0].ID != second[0].ID || got[1].ID != first[0].ID {
		t.Fatalf("runs = %+v, want newest first", got)
	}
	for _, r := range got {
		if r.Event != EventMergeGroup || r.Commit != queue {
			t.Fatalf("run = %+v", r)
		}
	}
	// The watermark scopes the list to runs created at/after it: a since in
	// the future sees none, a since before the first creation sees both.
	if got, err := svc.MergeGroupRunsAfter(ctx(), "o/r", queue, time.Now().Add(time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("future since = %+v, %v", got, err)
	}
	if got, err := svc.MergeGroupRunsAfter(ctx(), "o/r", queue, time.Unix(1_600_000_000, 0)); err != nil || len(got) != 2 {
		t.Fatalf("past since = %+v, %v", got, err)
	}
	// Another commit's runs are not returned.
	if other, err := svc.MergeGroupRuns(ctx(), "o/r", strings.Repeat("9", 40)); err != nil || len(other) != 0 {
		t.Fatalf("other sha = %+v, %v", other, err)
	}
	// An empty sha is an empty read; a bad repo is invalid.
	if none, err := svc.MergeGroupRuns(ctx(), "o/r", ""); err != nil || len(none) != 0 {
		t.Fatalf("empty sha = %+v, %v", none, err)
	}
	if _, err := svc.MergeGroupRuns(ctx(), "bad", queue); err == nil {
		t.Fatal("bad repo")
	}
}

// failListStore fails List (the run index read) while delegating everything
// else, to cover MergeGroupRuns' index-outage arm.
type failListStore struct{ store.ObjectStore }

func (failListStore) List(context.Context, string, string, func(store.ObjectMeta) error) error {
	return errors.New("list down")
}

func TestMergeGroupRunsErrorArms(t *testing.T) {
	base := strings.Repeat("1", 40)
	queue := strings.Repeat("2", 40)
	mem := store.NewMemory()
	gitReader := newFakeGit()
	gitReader.refs["refs/heads/main"] = base
	gitReader.add(base, ".spindle/workflows/queue.yml", mergeGroupWorkflowYAML)
	svc := New(mem, &fakeRoles{public: true})
	svc.Git = gitReader
	ref := "refs/heads/gh-readonly-queue/main/pr-9-" + queue
	mq, _ := ParseMergeQueueRef(ref)
	if _, err := svc.CreateRun(ctx(), "o/r", queue, EventMergeGroup, CreateOpts{
		Ref: ref, RefName: mq.BaseBranch, Action: "checks_requested",
		EventPayload: MergeGroupEventPayload("o/r", ref, queue, mq.BaseRef, "", mq.Number, "push"),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A corrupt run header fails the read loud (the driver then ejects with
	// the reason rather than reading a phantom green).
	bad := "repos/o/r/threads/runs/0000000000000000001-abcdef.json"
	if _, err := mem.Put(ctx(), bad, store.PutBody{Bytes: []byte("{")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("seed bad run: %v", err)
	}
	if _, err := svc.MergeGroupRuns(ctx(), "o/r", queue); err == nil {
		t.Fatal("corrupt run must fail")
	}
	// An index (List) outage fails too.
	failing := New(failListStore{ObjectStore: mem}, &fakeRoles{public: true})
	failing.Git = gitReader
	if _, err := failing.MergeGroupRuns(ctx(), "o/r", queue); err == nil {
		t.Fatal("list outage must fail")
	}
}
