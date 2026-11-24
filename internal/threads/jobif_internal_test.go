package threads

// Unit coverage for the job-`if:` helpers and the queue-entry need projection
// (the server-side decisions jobif.go/needs.go make). The behaviour tests live
// in jobif_test.go; these pin the nil/edge branches those flows do not reach.

import (
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

func TestEvaluateJobIfNilAndEmptySpec(t *testing.T) {
	if verdict, reason, err := evaluateJobIf(nil, nil, needFacts{}); verdict != jobIfRun || reason != "" || err != nil {
		t.Fatalf("nil job = %v, %q, %v", verdict, reason, err)
	}
	// A record written before Spec existed has no condition: the default
	// (all dependencies green) applies.
	legacy := &Job{ID: "j-a"}
	if verdict, _, err := evaluateJobIf(legacy, nil, needFacts{}); verdict != jobIfRun || err != nil {
		t.Fatalf("legacy job = %v, %v", verdict, err)
	}
	if verdict, reason, _ := evaluateJobIf(legacy, nil, needFacts{Results: map[string]string{"b": JobFailure}}); verdict != jobIfSkip ||
		reason != "needs: b concluded failure" {
		t.Fatalf("legacy default over a failed need = %v, %q", verdict, reason)
	}
}

func TestJobIfContextContexts(t *testing.T) {
	run := &Run{
		ID: "r1", Repo: "o/r", Commit: testSHA, Event: EventPush,
		Branch: "main", Workflow: "CI", Ref: "",
	}
	job := &Job{
		ID: "j-build", Key: "build",
		Spec: &Spec{
			Workflow: "CI", Matrix: map[string]string{"go": "1.27"},
			Inputs: map[string]string{"env": "staging"},
			Env:    map[string]string{"FOO": "bar"},
			If:     "matrix.go == '1.27' && inputs.env == 'staging' && env.FOO == 'bar'",
		},
	}
	verdict, reason, err := evaluateJobIf(job, run, needFacts{})
	if verdict != jobIfRun || err != nil {
		t.Fatalf("context condition = %v, %q, %v", verdict, reason, err)
	}
	ctx := jobIfContext(job, run, needFacts{Results: map[string]string{"lint": JobSuccess, "old": JobSkipped}})
	if ctx.GitHub["ref"] != "refs/heads/main" || ctx.GitHub["job"] != "j-build" || ctx.GitHub["workflow"] != "CI" {
		t.Fatalf("github context = %v", ctx.GitHub)
	}
	// A skipped dependency makes success() false (GitHub's rule), but neither
	// failure() nor cancelled() fires.
	if ctx.Success || ctx.Failure || ctx.Cancelled {
		t.Fatalf("success/failure/cancelled = %v/%v/%v", ctx.Success, ctx.Failure, ctx.Cancelled)
	}
	// Nil-safe accessors: a job without a spec reads empty maps, never panics.
	if specMatrix(nil) != nil || specInputs(nil) != nil || specEnv(nil) != nil {
		t.Fatal("nil job spec maps")
	}
	if jobIdentifier(nil) != "" {
		t.Fatal("nil job identifier")
	}
	// A run with no ref derives one from the branch; a nil run has no github
	// context at all (a caller that lost the header must not panic).
	if got := jobIfContext(job, run, needFacts{}).GitHub["ref"]; got != "refs/heads/main" {
		t.Fatalf("derived ref = %q", got)
	}
	if got := jobIfContext(job, nil, needFacts{}).GitHub; len(got) != 0 {
		t.Fatalf("nil run github = %v", got)
	}
}

func TestNeedsSummaryStates(t *testing.T) {
	cases := []struct {
		results                     map[string]string
		success, failure, cancelled bool
	}{
		{nil, true, false, false},
		{map[string]string{"a": JobSuccess}, true, false, false},
		{map[string]string{"a": JobSkipped}, false, false, false},
		{map[string]string{"a": JobFailure}, false, true, false},
		{map[string]string{"a": JobCancelled}, false, false, true},
		{map[string]string{"a": JobFailure, "b": JobCancelled}, false, true, true},
	}
	for _, tc := range cases {
		s, f, c := needsSummary(tc.results)
		if s != tc.success || f != tc.failure || c != tc.cancelled {
			t.Errorf("needsSummary(%v) = %v/%v/%v", tc.results, s, f, c)
		}
	}
}

func TestNeedIDsDedupesAndSorts(t *testing.T) {
	if got := needIDs(nil); got != nil {
		t.Fatalf("no needs = %v", got)
	}
	if got := needIDs(map[string][]string{"a": {}}); got != nil {
		t.Fatalf("empty expansion = %v", got)
	}
	got := needIDs(map[string][]string{"a": {"j-b", "j-a"}, "b": {"j-a"}})
	if len(got) != 2 || got[0] != "j-a" || got[1] != "j-b" {
		t.Fatalf("needIDs = %v", got)
	}
}

func TestNeedResultsMissingAndWaiting(t *testing.T) {
	byID := map[string]*Job{
		"j-a": {ID: "j-a", State: JobSuccess},
		"j-b": {ID: "j-b", State: JobRunning},
	}
	facts := foldNeeds(map[string][]string{"a": {"j-a"}}, byID)
	if facts.Waiting || facts.Missing || facts.Results["a"] != JobSuccess {
		t.Fatalf("ready = %v %v %v", facts.Results, facts.Waiting, facts.Missing)
	}
	if facts := foldNeeds(map[string][]string{"b": {"j-b"}}, byID); !facts.Waiting {
		t.Fatal("a running dependency must read as waiting")
	}
	if facts := foldNeeds(map[string][]string{"ghost": {"j-ghost"}}, byID); !facts.Missing {
		t.Fatal("a missing dependency must read as missing")
	}
	// A matrix dependency folds to its worst cell.
	byID["j-c1"] = &Job{ID: "j-c1", State: JobSuccess}
	byID["j-c2"] = &Job{ID: "j-c2", State: JobFailure}
	facts = foldNeeds(map[string][]string{"build": {"j-c1", "j-c2"}}, byID)
	if facts.Results["build"] != JobFailure {
		t.Fatalf("matrix fold = %q", facts.Results["build"])
	}
	// A live cell ranks worst so the fold waits rather than reporting success.
	if got := worseResult(JobFailure, JobRunning); got != JobRunning {
		t.Fatalf("worseResult(live) = %q", got)
	}
}

func TestEntryNeedMapSkipsUnknownKeys(t *testing.T) {
	job := &Job{Needs: []string{"build", "ghost"}}
	got := entryNeedMap(map[string][]string{"build": {"j-build"}}, job)
	if len(got) != 1 || len(got["build"]) != 1 || got["build"][0] != "j-build" {
		t.Fatalf("entryNeedMap = %v", got)
	}
	if entryNeedMap(nil, nil) != nil || entryNeedMap(map[string][]string{}, &Job{}) != nil {
		t.Fatal("a job with no needs projects nothing")
	}
}

// TestDependentVerdictFaults pins the fail-closed reads of the dependent gate:
// a corrupt job, a corrupt run header or a corrupt dependency is an error, never
// a silent "run it".
func TestDependentVerdictFaults(t *testing.T) {
	e, runID := needsEnv(t, needsChainYAML)
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	var buildEntry *QueueEntry
	for _, seq := range head.Pending {
		entry, _, eerr := e.svc.loadEntry(ctx(), "o", "r", seq)
		if eerr != nil {
			t.Fatalf("entry: %v", eerr)
		}
		if entry.JobID == "j-build" {
			buildEntry = entry
		}
	}
	if buildEntry == nil || len(buildEntry.NeedKeys) == 0 {
		t.Fatalf("build entry = %+v", buildEntry)
	}
	// A corrupt dependency fails the verdict.
	lintKey := JobKey("o", "r", runID, "j-lint")
	lintBody, _, gerr := e.svc.getJSON(ctx(), lintKey)
	if gerr != nil || lintBody == nil {
		t.Fatalf("read dep: %v", gerr)
	}
	if _, perr := store.PutBytes(ctx(), e.store, lintKey, []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatalf("corrupt dep: %v", perr)
	}
	if _, _, verr := e.svc.dependentVerdict(ctx(), "o", "r", buildEntry, jobCache{}, runCache{}); !errors.Is(verr, ErrCorrupt) {
		t.Fatalf("corrupt dependency = %v", verr)
	}
	// A corrupt run header fails it too (the if context needs the header).
	if _, perr := store.PutBytes(ctx(), e.store, lintKey, lintBody, store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatalf("restore dep: %v", perr)
	}
	runKey := RunKey("o", "r", runID)
	if _, perr := store.PutBytes(ctx(), e.store, runKey, []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); perr != nil {
		t.Fatalf("corrupt run: %v", perr)
	}
	if _, _, verr := e.svc.dependentVerdict(ctx(), "o", "r", buildEntry, jobCache{}, runCache{}); !errors.Is(verr, ErrCorrupt) {
		t.Fatalf("corrupt run = %v", verr)
	}
}
