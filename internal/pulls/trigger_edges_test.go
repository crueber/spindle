package pulls

// PAR-020/PAR-021: the pulls service's trigger seams. The run creation itself
// is composition's; here the service must report the fork and PR-comment edges.

import (
	"context"
	"testing"
)

func TestPullCommentTriggerEdge(t *testing.T) {
	e := newTestEnv()
	var got []PullCommentEvent
	e.svc.PullCommentEvents = func(_ context.Context, ev PullCommentEvent) { got = append(got, ev) }
	th, _ := openBasic(t, e, "o", "r")
	if _, err := e.svc.AddComment(ctx(), "o", "r", th.Num, writer(), "conversation note"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("comment edges = %+v", got)
	}
	ev := got[0]
	if ev.Repo != "o/r" || ev.Num != th.Num || ev.Actor != "jane@example.com" || ev.Body != "conversation note" || ev.Title != th.Title {
		t.Fatalf("comment edge = %+v", ev)
	}
	// A nil sink is safe.
	e.svc.PullCommentEvents = nil
	if _, err := e.svc.AddComment(ctx(), "o", "r", th.Num, writer(), "again"); err != nil {
		t.Fatalf("AddComment with nil sink: %v", err)
	}
}

func TestForkTriggerEdgeShape(t *testing.T) {
	e := newTestEnv()
	var got []ForkEvent
	e.svc.ForkEvents = func(_ context.Context, ev ForkEvent) { got = append(got, ev) }
	e.svc.forkEvent(ctx(), ForkEvent{Source: "o/r", Forkee: "o/r-fork", Actor: "jane"})
	if len(got) != 1 || got[0].Source != "o/r" || got[0].Forkee != "o/r-fork" {
		t.Fatalf("fork edge = %+v", got)
	}
	e.svc.ForkEvents = nil
	e.svc.forkEvent(ctx(), ForkEvent{Source: "o/r"})
}
