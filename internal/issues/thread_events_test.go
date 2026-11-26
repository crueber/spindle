package issues

import (
	"context"
	"testing"
)

// TestThreadEventsCoverIssueEdges pins PAR-020's emitters: every documented
// issue action the triggers accept is actually delivered by the issues service,
// and an issue comment is its own edge.
func TestThreadEventsCoverIssueEdges(t *testing.T) {
	roles := newFakeRoles()
	roles.grant("o", "r", "alice@example.com", "write")
	s := testService(roles)
	var got []ThreadEvent
	s.ThreadEvents = func(_ context.Context, ev ThreadEvent) { got = append(got, ev) }
	ctx := context.Background()

	if _, err := s.CreateLabel(ctx, "o", "r", writerP, "bug", "ff0000", ""); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	th, _, err := s.CreateIssue(ctx, "o", "r", writerP, "It broke", "body", &IssueMeta{
		Labels: []string{"bug"}, Assignees: []string{"alice@example.com"},
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := s.AddComment(ctx, "o", "r", th.Num, writerP, "me too"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	title := "Renamed"
	labels := []string{}
	assignees := []string{}
	closed := StateClosed
	if _, err := s.PatchIssue(ctx, "o", "r", th.Num, writerP, IssuePatch{
		Title: &title, Labels: &labels, Assignees: &assignees, State: &closed,
	}); err != nil {
		t.Fatalf("PatchIssue: %v", err)
	}
	reopened := StateOpen
	if _, err := s.PatchIssue(ctx, "o", "r", th.Num, writerP, IssuePatch{State: &reopened}); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	want := []struct{ kind, action string }{
		{"issue", "opened"},
		{"issue_comment", "created"},
		{"issue", "edited"},
		{"issue", "unlabeled"},
		{"issue", "unassigned"},
		{"issue", "closed"},
		{"issue", "reopened"},
	}
	seen := map[string]bool{}
	for _, ev := range got {
		seen[ev.Kind+":"+ev.Action] = true
	}
	for _, w := range want {
		if !seen[w.kind+":"+w.action] {
			t.Errorf("no %s:%s edge (got %v)", w.kind, w.action, keysOf(got))
		}
	}
}

func keysOf(events []ThreadEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind+":"+ev.Action)
	}
	return out
}
