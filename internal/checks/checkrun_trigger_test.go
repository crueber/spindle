package checks

// PAR-021: an EXTERNAL CI report emits the check_run trigger edge. Threads'
// own mirror writes the same records but never emits (the run is the check; a
// second edge would let a workflow trigger a copy of itself).

import (
	"context"
	"testing"
)

func TestCheckRunTriggerEdges(t *testing.T) {
	e := newTestEnv()
	var got []CheckRunEvent
	e.svc.CheckRunEvents = func(_ context.Context, ev CheckRunEvent) { got = append(got, ev) }
	sha := hexSHA(1)
	e.knowSHA(sha)

	// First report: created.
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci/build", State: StateInProgress,
	}); err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if len(got) != 1 || got[0].Action != "created" || got[0].Context != "ci/build" || got[0].Creator != "jane@example.com" {
		t.Fatalf("created edge = %+v", got)
	}
	// A live update is not an edge.
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci/build", State: StateInProgress,
	}); err != nil {
		t.Fatalf("ReportStatus(update): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a live update emitted an edge: %+v", got)
	}
	// Terminal report: completed.
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci/build", State: StateSuccess,
	}); err != nil {
		t.Fatalf("ReportStatus(terminal): %v", err)
	}
	if len(got) != 2 || got[1].Action != "completed" || got[1].State != StateSuccess {
		t.Fatalf("completed edge = %+v", got)
	}
	// A repeated terminal report is not a second completed edge.
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci/build", State: StateSuccess,
	}); err != nil {
		t.Fatalf("ReportStatus(repeat): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("repeat terminal emitted: %+v", got)
	}

	// The Threads mirror never emits, even on its first report.
	got = nil
	if _, err := e.svc.RecordStatus(ctx(), "o", "r", sha, "threads", ReportInput{
		Context: "threads/ci/build", State: StateFailure,
	}); err != nil {
		t.Fatalf("RecordStatus: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the Threads mirror emitted a check_run edge: %+v", got)
	}
	// Nil sink is safe.
	e.svc.CheckRunEvents = nil
	if _, err := e.svc.RecordStatus(ctx(), "o", "r", sha, "external", ReportInput{
		Context: "x", State: StateSuccess,
	}); err != nil {
		t.Fatalf("RecordStatus with nil sink: %v", err)
	}
}
