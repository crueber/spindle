package issues

// PAR-020: the label and milestone DEFINITION triggers. Each is emitted by this
// service from a real transition (created/edited/deleted, and the milestone's
// state change), with the bounded definition on the event.

import (
	"context"
	"testing"
)

func TestLabelDefinitionTriggers(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	var events []ThreadEvent
	s.ThreadEvents = func(_ context.Context, ev ThreadEvent) { events = append(events, ev) }

	if _, err := s.CreateLabel(reqCtx(), "acme", "repo", aliceP, "bug", "ff0000", "broken"); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	if _, err := s.UpdateLabel(reqCtx(), "acme", "repo", aliceP, "bug", nil, strPtr("still broken")); err != nil {
		t.Fatalf("UpdateLabel: %v", err)
	}
	if _, err := s.DeleteLabel(reqCtx(), "acme", "repo", aliceP, "bug"); err != nil {
		t.Fatalf("DeleteLabel: %v", err)
	}
	actions := make([]string, 0, len(events))
	for _, ev := range events {
		actions = append(actions, ev.Action)
		if ev.Kind != "label" || ev.Label == nil {
			t.Fatalf("label event = %+v", ev)
		}
	}
	want := []string{"created", "edited", "deleted"}
	if len(actions) != len(want) {
		t.Fatalf("label actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("label actions = %v, want %v", actions, want)
		}
	}
	if events[0].Label.Name != "bug" || events[0].Label.Color != "ff0000" {
		t.Fatalf("created label payload = %+v", events[0].Label)
	}
	if events[2].Label.Description != "still broken" {
		t.Fatalf("deleted label payload = %+v", events[2].Label)
	}
}

func TestMilestoneDefinitionTriggers(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	var events []ThreadEvent
	s.ThreadEvents = func(_ context.Context, ev ThreadEvent) { events = append(events, ev) }

	m, err := s.CreateMilestone(reqCtx(), "acme", "repo", aliceP, "v1", "first", nil)
	if err != nil {
		t.Fatalf("CreateMilestone: %v", err)
	}
	closed := StateClosed
	if _, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, nil, nil, nil, &closed); err != nil {
		t.Fatalf("close milestone: %v", err)
	}
	open := StateOpen
	if _, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, nil, nil, nil, &open); err != nil {
		t.Fatalf("reopen milestone: %v", err)
	}
	title := "v1.0"
	if _, err := s.UpdateMilestone(reqCtx(), "acme", "repo", aliceP, m.ID, &title, nil, nil, nil); err != nil {
		t.Fatalf("edit milestone: %v", err)
	}
	if err := s.DeleteMilestone(reqCtx(), "acme", "repo", aliceP, m.ID); err != nil {
		t.Fatalf("DeleteMilestone: %v", err)
	}
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
		if ev.Kind != "milestone" || ev.Milestone == nil {
			t.Fatalf("milestone event = %+v", ev)
		}
	}
	want := []string{"created", "closed", "opened", "edited", "deleted"}
	if len(actions) != len(want) {
		t.Fatalf("milestone actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("milestone actions = %v, want %v", actions, want)
		}
	}
}
