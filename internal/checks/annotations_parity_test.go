package checks

// annotations_parity_test.go — PAR-192: the check-run API accepts annotations,
// stores them with the report, returns them on every read, and the merge gate
// still reads the same state (annotations never change a state).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReportStoresCheckAnnotations(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(11)
	e.knowSHA(sha)
	doc, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci/build", State: StateFailure, Description: "2 errors",
		Annotations: []Annotation{
			{Level: "error", Message: "undefined: x", File: "main.go", Line: 12, Col: 3, Title: "compile"},
			{Level: "warning", Message: "unused import", File: "main.go", Line: 4},
			{Level: "notice", Message: "a notice"},
		},
	})
	if err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if len(doc.Annotations) != 3 {
		t.Fatalf("annotations = %+v", doc.Annotations)
	}
	got := doc.Annotations[0]
	if got.Level != "error" || got.File != "main.go" || got.Line != 12 || got.Col != 3 || got.Title != "compile" {
		t.Fatalf("annotation[0] = %+v", got)
	}
	// Every read path carries them (the PR panel and the commit page read the
	// combined view; the checks page reads GetStatuses).
	view, err := e.svc.GetStatuses(ctx(), "o", "r", sha, reader())
	if err != nil {
		t.Fatalf("GetStatuses: %v", err)
	}
	if len(view.Statuses) != 1 || len(view.Statuses[0].Annotations) != 3 {
		t.Fatalf("status view = %+v", view)
	}
	combined, err := e.svc.Combined(ctx(), "o", "r", sha, reader())
	if err != nil {
		t.Fatalf("Combined: %v", err)
	}
	if combined.State != StateFailure {
		t.Fatalf("combined state = %q (annotations changed the gate)", combined.State)
	}
	if len(combined.Statuses[0].Annotations) != 3 {
		t.Fatalf("combined annotations = %+v", combined.Statuses[0].Annotations)
	}
	// The JSON wire shape carries level/file/line (the documented check-run
	// annotations fields).
	raw, _ := json.Marshal(combined.Statuses[0])
	for _, want := range []string{`"annotations"`, `"level":"error"`, `"file":"main.go"`, `"line":12`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("wire JSON missing %s: %s", want, raw)
		}
	}
}

func TestReportReplacesAnnotationsAndValidatesLevels(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(12)
	e.knowSHA(sha)
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci", State: StateSuccess,
		Annotations: []Annotation{{Level: "error", Message: "first"}},
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	doc, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci", State: StateSuccess,
		Annotations: []Annotation{{Level: "notice", Message: "replacement"}},
	})
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	if len(doc.Annotations) != 1 || doc.Annotations[0].Message != "replacement" {
		t.Fatalf("annotations = %+v", doc.Annotations)
	}
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci", State: StateSuccess,
		Annotations: []Annotation{{Level: "fatal", Message: "x"}},
	}); err == nil {
		t.Fatal("unknown annotation level accepted")
	}
	if _, err := e.svc.ReportStatus(ctx(), "o", "r", sha, writer(), "", ReportInput{
		Context: "ci", State: StateSuccess,
		Annotations: []Annotation{{Level: "error", Message: strings.Repeat("x", MaxAnnotationMessage+50)}},
	}); err != nil {
		t.Fatalf("long annotation must truncate, not fail: %v", err)
	}
}
