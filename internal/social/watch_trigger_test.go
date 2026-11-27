package social

// PAR-021: a first star emits the `watch` (started) trigger edge; a repeat star
// and an unstar do not.

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestWatchTriggerOnFirstStar(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	var got []WatchEvent
	x.svc.WatchEvents = func(_ context.Context, ev WatchEvent) { got = append(got, ev) }
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatalf("Star: %v", err)
	}
	if len(got) != 1 || got[0].Repo != "o/r" || got[0].Actor != "jane" || got[0].Action != "started" {
		t.Fatalf("watch edges = %+v", got)
	}
	// A repeat star is the same state, not a new edge.
	if _, err := x.svc.Star(ctx(), jane(), "o", "r"); err != nil {
		t.Fatalf("restar: %v", err)
	}
	// An unstar has no watch action.
	if _, err := x.svc.Unstar(ctx(), jane(), "o", "r"); err != nil {
		t.Fatalf("Unstar: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("repeat/unstar emitted watch edges: %+v", got)
	}
	// Nil sink is safe.
	x.svc.WatchEvents = nil
	if _, err := x.svc.Star(ctx(), auth.Principal{Name: "bob"}, "o", "r"); err != nil {
		t.Fatalf("star with nil sink: %v", err)
	}
}
