// state_test.go — the id contract and the page-shell observer.
//
// The wire id is the import/service id (the key GET .../imports/{id} attaches
// with); the WAL table mints its own id for the record and that one stays
// internal. taskView/taskView-nested (progressView) stamp the accepted id over
// the record, and Service.State is the same-key read a page shell uses to skip
// a progress card for an import that is already over.
package repoimport

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/wal"
)

func TestWireIDsAreServiceIDs(t *testing.T) {
	ok := true
	rec := wal.TaskRecord{ID: "t-table-id", Kind: KindRepoImport, Repo: "acme/widget", OK: &ok}
	if got := taskView(rec, "i-service")["id"]; got != "i-service" {
		t.Fatalf("taskView id = %v, want the service id", got)
	}
	if got := taskView(rec, "")["id"]; got != "t-table-id" {
		t.Fatalf("taskView with no key = %v, want the record id (pruned-record case)", got)
	}
	total := uint64(2)
	view := progressView(wal.Progress{Kind: "progress", Label: "clone", Done: 1, Total: &total, Task: &rec}, "i-service")
	nested, _ := view["task"].(map[string]any)
	if nested == nil || nested["id"] != "i-service" {
		t.Fatalf("progressView nested task = %v", view["task"])
	}
	// No key: the nested record keeps its own id, and a task-less progress
	// frame is untouched (the nil-guard branch).
	if v := progressView(wal.Progress{Kind: "progress", Label: "clone"}, "i-service"); v["task"] != nil {
		t.Fatalf("task-less progress gained a task: %v", v)
	}
	if nested, _ := progressView(wal.Progress{Kind: "progress", Task: &rec}, "")["task"].(map[string]any); nested["id"] != "t-table-id" {
		t.Fatalf("progressView with no key = %v", nested)
	}
}

func TestStateObserver(t *testing.T) {
	svc, _ := testService(t, nil, nil)
	ctx := context.Background()

	if _, ok := svc.State("i-nope"); ok {
		t.Fatal("unknown id must not be known")
	}

	// A live ring with no narration yet: known, running, no record.
	st := newStream()
	st.target = "acme/widget"
	svc.mu.Lock()
	svc.streams["i-live"] = st
	svc.mu.Unlock()
	ts, ok := svc.State("i-live")
	if !ok || ts.Finished || ts.Record != nil || ts.Outcome != nil {
		t.Fatalf("live state = %+v ok=%v", ts, ok)
	}

	// Narration lands, then the terminal outcome: both are visible.
	st.setRecord(wal.TaskRecord{ID: "t-1", Kind: KindRepoImport, Repo: "acme/widget", LogTail: []string{"clone done"}})
	st.finish(&Outcome{Repo: "acme/widget", SourceURL: "https://github.com/acme/widget.git"})
	ts, ok = svc.State("i-live")
	if !ok || !ts.Finished || ts.Record == nil || ts.Record.ID != "t-1" {
		t.Fatalf("finished state = %+v ok=%v", ts, ok)
	}
	if ts.Outcome == nil || ts.Outcome.Repo != "acme/widget" {
		t.Fatalf("outcome = %+v", ts.Outcome)
	}

	// A task the ring never saw (pruned past retention): the core table is
	// the witness, the outcome is not retained.
	rec, err := svc.reg.Tasks().Run(ctx, "acme/widget", KindRepoImport, nil, func(_ context.Context, task *wal.Task) error {
		task.Notice("done")
		return nil
	})
	if err != nil || rec == nil {
		t.Fatalf("table run: %v %v", rec, err)
	}
	ts, ok = svc.State(rec.ID)
	if !ok || !ts.Finished || ts.Record == nil || ts.Outcome != nil {
		t.Fatalf("table-only state = %+v ok=%v", ts, ok)
	}
}
