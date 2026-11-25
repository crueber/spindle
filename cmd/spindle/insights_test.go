// insights_test.go — composition seams for the repository pulse: the
// maintainer sweeper's kind/due gate and the manual op registration. The
// projection math and the page render are tested in their own packages.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/insights"
	"github.com/crueber/spindle/internal/maintain"
	"github.com/crueber/spindle/internal/store"
)

type noScan struct{}

func (noScan) InsightsCommits(context.Context, *git.LocalRepo, string, time.Time, int) ([]git.InsightsCommit, bool, error) {
	return nil, false, nil
}

func (noScan) InsightsLanguages(context.Context, *git.LocalRepo, string, int) ([]git.InsightsLanguage, bool, error) {
	return nil, false, nil
}

func testInsightsService() *insights.Service {
	return insights.New(insights.Deps{
		Store: store.NewMemory(),
		Git:   noScan{},
		Head:  func(context.Context, string, string) (string, error) { return "", nil },
		Serve: func(context.Context, string, string) (*git.LocalRepo, string, error) { return nil, "", nil },
	})
}

func TestInsightsSweeperDueGate(t *testing.T) {
	clock := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	w := &insightsSweeper{svc: testInsightsService(), now: func() time.Time { return clock }}
	if w.Kind() != insights.TaskKind {
		t.Fatalf("kind = %q", w.Kind())
	}
	var _ maintain.Sweeper = w
	if !w.Due("acme/api") {
		t.Fatal("first pass must be due")
	}
	_ = w.Sweep(context.Background(), "acme/api", nopTask{})
	if w.Due("acme/api") {
		t.Fatal("second pass inside the interval must not be due")
	}
	clock = clock.Add(insightsSweepInterval)
	if !w.Due("acme/api") {
		t.Fatal("pass after the interval must be due")
	}
	if w.Due("acme/other") != true {
		t.Fatal("a different repo has its own gate")
	}
	// A malformed id or nil service is a no-op, never a panic.
	if err := w.Sweep(context.Background(), "bad", nopTask{}); err != nil {
		t.Fatalf("malformed repo: %v", err)
	}
	if (&insightsSweeper{}).Due("acme/api") {
		t.Fatal("nil service must not be due")
	}
	if err := (&insightsSweeper{}).Sweep(context.Background(), "acme/api", nopTask{}); err != nil {
		t.Fatalf("nil service sweep: %v", err)
	}
}

type nopTask struct{}

func (nopTask) Notice(string)                           {}
func (nopTask) Progress(string, uint64, uint64, string) {}

func TestMaintainSweepersIncludesInsights(t *testing.T) {
	c := &collabWiring{insightsSvc: testInsightsService()}
	var found bool
	for _, sw := range maintainSweepers(c) {
		if sw.Kind() == insights.TaskKind {
			found = true
		}
	}
	if !found {
		t.Fatal("insights sweeper not registered")
	}
	if got := maintainSweepers(nil); got != nil {
		t.Fatalf("nil wiring = %v", got)
	}
}

func TestOpsListsInsightsRefresh(t *testing.T) {
	svc := testInsightsService()
	ot := &opsTasks{insights: svc}
	var found bool
	for _, op := range ot.Ops() {
		if op.Op == insights.TaskKind {
			found = true
		}
	}
	if !found {
		t.Fatal("insights-refresh op not listed")
	}
	if (&opsTasks{}).Ops() != nil {
		// Sanity: an unwired opsTasks still lists the core ops, but not the
		// insights kind.
		for _, op := range (&opsTasks{}).Ops() {
			if op.Op == insights.TaskKind {
				t.Fatal("insights op listed without a service")
			}
		}
	}
	// opFn maps the kind only when the service is wired.
	if fn := ot.opFn(git.RepoId{Owner: "acme", Name: "api"}, insights.TaskKind, nil); fn == nil {
		t.Fatal("wired op did not return a body")
	}
	if fn := (&opsTasks{}).opFn(git.RepoId{Owner: "acme", Name: "api"}, insights.TaskKind, nil); fn != nil {
		t.Fatal("unwired op returned a body")
	}
}
