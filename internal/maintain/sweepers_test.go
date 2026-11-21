package maintain

// sweepers_test.go — the out-of-band per-repo sweep plug (docs/features/14
// §6, kind `packages-gc`): it runs after the §4 ladder, through the same
// (repo,kind) TaskRunner, is bounded by the unit cap, and a failure never
// fails the pass.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store/proto"
)

type fakeSweeper struct {
	kind  string
	mu    sync.Mutex
	calls []string
	err   error
	block bool
}

func (s *fakeSweeper) Kind() string { return s.kind }

func (s *fakeSweeper) Sweep(ctx context.Context, repo string, t TaskLogger) error {
	s.mu.Lock()
	s.calls = append(s.calls, repo)
	s.mu.Unlock()
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.err
}

func (s *fakeSweeper) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// TestSweepersRunAfterTheLadder: a sweep runs once per assigned repo, after
// the priority loop returned idle, through the task runner (so it appears in
// the task table and joins a manual start).
func TestSweepersRunAfterTheLadder(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.FsckInterval = 0
	eff.Maintenance.Checkpoints = false
	eff.Compaction.Enabled = false
	r1 := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	r2 := &fakeRepo{id: "acme/legacy", m: &proto.Manifest{Repo: "acme/legacy"}}
	eng := newFakeEngine(eff, r1, r2)
	sw := &fakeSweeper{kind: "packages-gc"}
	m := New(eng, Options{Leaser: &fakeLeaser{}, Sweepers: []Sweeper{sw}})

	m.RunPass(context.Background())

	if got := sw.called(); len(got) != 2 || got[0] != "acme/widget" || got[1] != "acme/legacy" {
		t.Fatalf("swept = %v", got)
	}
	eng.tasks.mu.Lock()
	starts := append([]string(nil), eng.tasks.starts...)
	eng.tasks.mu.Unlock()
	saw := 0
	for _, s := range starts {
		if s == "acme/widget packages-gc" || s == "acme/legacy packages-gc" {
			saw++
		}
	}
	if saw != 2 {
		t.Fatalf("sweep did not ride the task path: %v", starts)
	}
	if n := m.Metrics().Units["packages-gc"][OutcomeOK]; n != 2 {
		t.Fatalf("ok outcomes = %d, want 2", n)
	}
}

// TestSweeperFailureAndTimeout: a failing sweep is recorded, never fatal; a
// sweep that outlives the unit cap is cancelled through its context.
func TestSweeperFailureAndTimeout(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.FsckInterval = 0
	eff.Maintenance.Checkpoints = false
	eff.Compaction.Enabled = false
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	failing := &fakeSweeper{kind: "packages-gc", err: errors.New("boom")}
	m := New(eng, Options{Leaser: &fakeLeaser{}, Sweepers: []Sweeper{failing}})
	m.RunPass(context.Background())
	if n := m.Metrics().Units["packages-gc"][OutcomeError]; n != 1 {
		t.Fatalf("error outcomes = %d, want 1", n)
	}

	eng2 := newFakeEngine(eff, repo)
	blocked := &fakeSweeper{kind: "packages-gc", block: true}
	m2 := New(eng2, Options{Leaser: &fakeLeaser{}, Sweepers: []Sweeper{blocked}, UnitCap: time.Nanosecond})
	m2.RunPass(context.Background())
	if n := m2.Metrics().Units["packages-gc"][OutcomeError]; n != 1 {
		t.Fatalf("timeout outcomes = %d, want 1", n)
	}
}

// dueSweeper is a DueSweeper: a not-due repo must not even open a task.
type dueSweeper struct {
	fakeSweeper
	due bool
}

func (d *dueSweeper) Due(string) bool { return d.due }

func TestSweeperDueGateSkipsTheTask(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.FsckInterval = 0
	eff.Maintenance.Checkpoints = false
	eff.Compaction.Enabled = false
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}

	// Not due: no task appears in the table and Sweep never runs.
	eng := newFakeEngine(eff, repo)
	sw := &dueSweeper{fakeSweeper: fakeSweeper{kind: "packages-gc"}}
	m := New(eng, Options{Leaser: &fakeLeaser{}, Sweepers: []Sweeper{sw}})
	m.RunPass(context.Background())
	eng.tasks.mu.Lock()
	starts := append([]string(nil), eng.tasks.starts...)
	eng.tasks.mu.Unlock()
	for _, s := range starts {
		if s == "acme/widget packages-gc" {
			t.Fatalf("not-due sweep opened a task: %v", starts)
		}
	}
	if len(sw.called()) != 0 {
		t.Fatal("not-due sweep ran")
	}

	// Due: it runs once.
	eng2 := newFakeEngine(eff, repo)
	sw2 := &dueSweeper{fakeSweeper: fakeSweeper{kind: "packages-gc"}, due: true}
	m2 := New(eng2, Options{Leaser: &fakeLeaser{}, Sweepers: []Sweeper{sw2}})
	m2.RunPass(context.Background())
	if got := sw2.called(); len(got) != 1 {
		t.Fatalf("due sweep = %v", got)
	}
}
