package threads

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// PAR-092/PAR-093 transport: complete carries the step summary and the
// workflow-command annotations; the server bounds them again, stores them on the
// job, and states the annotation count on the commit's check.

func TestCompleteStoresAnnotationsAndSummary(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin, "linux")
	job, _ := fetchJob(t, h, runnerToken)
	if job == nil {
		t.Fatal("no job")
	}
	startPath := "/api/v1/threads/jobs/" + job.JobRef + "/attempt/1/start"
	rec := doAuth(h, http.MethodPost, startPath, "", runnerToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("start = %d", rec.Code)
	}
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"conclusion": "failure",
		"steps":      []map[string]any{{"name": "test", "conclusion": "failure"}},
		"annotations": []map[string]any{
			{"level": "error", "message": "boom", "file": "main.go", "line": 3, "col": 2, "title": "Build", "step": "test"},
			{"level": "notice", "message": "fyi", "step": "test"},
		},
		// A large summary must fit the complete endpoint's own (larger) body
		// bound: the generic protocol bound is a log-frame pair.
		"summary": "## Result\n\n```mermaid\ngraph TD; A-->B;\n```\n" + strings.Repeat("x", 1<<20),
	})
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/complete", string(body), start.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete = %d: %s", rec.Code, rec.Body.String())
	}
	stored := e.jobRecord(t, run.ID, job.JobID)
	if len(stored.Annotations) != 2 || stored.Annotations[0].File != "main.go" || stored.Annotations[1].Level != "notice" {
		t.Fatalf("annotations = %+v", stored.Annotations)
	}
	if !strings.Contains(stored.Summary, "## Result") || len(stored.Summary) < 1<<20 {
		t.Fatalf("summary length = %d", len(stored.Summary))
	}
	var last checkReport
	found := false
	for _, rep := range e.checks.snapshot() {
		if rep.Context == job.Context {
			last, found = rep, true
		}
	}
	if !found || !strings.Contains(last.Description, "2 annotation(s)") {
		t.Fatalf("check description = %q (the commit must state the annotation count)", last.Description)
	}
}

func TestCompleteRejectsAnInvalidAnnotationLevel(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	e.mustCreate(t)
	_, runnerToken := registerRunner(t, e, h, admin, "linux")
	job, _ := fetchJob(t, h, runnerToken)
	startPath := "/api/v1/threads/jobs/" + job.JobRef + "/attempt/1/start"
	rec := doAuth(h, http.MethodPost, startPath, "", runnerToken)
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	body := `{"conclusion":"success","annotations":[{"level":"fatal","message":"x"}]}`
	rec = doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+job.JobRef+"/attempt/1/complete", body, start.Token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid level = %d: %s", rec.Code, rec.Body.String())
	}
}
