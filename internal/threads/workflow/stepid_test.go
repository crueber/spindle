package workflow

import (
	"errors"
	"strings"
	"testing"
)

// Step `id:` (15 §3.4/§4.1, 2026-11-21). `id:` is extremely common in real
// workflows and `steps.<id>.outcome` depends on it; the parser accepts it,
// validates the GitHub identifier shape and rejects a duplicate within a job.

func stepIDWorkflow(ids ...string) string {
	var b strings.Builder
	b.WriteString("name: ids\non: push\njobs:\n  build:\n    runs-on: linux\n    steps:\n")
	for _, id := range ids {
		b.WriteString("      - id: " + id + "\n        run: echo hi\n")
	}
	return b.String()
}

func TestParseStepIDs(t *testing.T) {
	w, err := Parse([]byte(stepIDWorkflow("checkout", "unit_test", "lint-2", "_private")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	steps := w.Jobs["build"].Steps
	want := []string{"checkout", "unit_test", "lint-2", "_private"}
	for i, id := range want {
		if steps[i].ID != id {
			t.Fatalf("step %d id = %q, want %q", i+1, steps[i].ID, id)
		}
	}
	// An anonymous step keeps an empty id.
	w, err = Parse([]byte(stepIDWorkflow("named")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plain, err := Parse([]byte("name: x\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if plain.Jobs["a"].Steps[0].ID != "" {
		t.Fatalf("anonymous step id = %q", plain.Jobs["a"].Steps[0].ID)
	}
}

func TestParseRejectsBadStepIDs(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		wantID string
		job    string
	}{
		{"leading digit", stepIDWorkflow("1build"), "1build", "build"},
		{"dot", stepIDWorkflow("unit.test"), "unit.test", "build"},
		{"space", stepIDWorkflow("\"a b\""), "a b", "build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if !errors.Is(err, ErrBadStepID) {
				t.Fatalf("err = %v, want ErrBadStepID", err)
			}
			for _, want := range []string{tc.job, tc.wantID} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
	// A duplicate names the job and the id; case-insensitively, because
	// `steps.<id>` lookups are case-insensitive and one of the two would be
	// unreachable.
	src := "name: ids\non: push\njobs:\n  build:\n    runs-on: linux\n    steps:\n" +
		"      - id: unit\n        run: echo one\n" +
		"      - id: UNIT\n        run: echo two\n"
	_, err := Parse([]byte(src))
	if !errors.Is(err, ErrBadStepID) {
		t.Fatalf("duplicate err = %v, want ErrBadStepID", err)
	}
	for _, want := range []string{"build", "UNIT", "duplicate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("duplicate error %q does not name %q", err, want)
		}
	}
	// The id check is per file (one bad file is that file's error) and per job
	// (the same id in two jobs is legal).
	sameInTwoJobs := "name: ids\non: push\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - id: unit\n        run: echo a\n" +
		"  b:\n    runs-on: linux\n    steps:\n      - id: unit\n        run: echo b\n"
	if _, err := Parse([]byte(sameInTwoJobs)); err != nil {
		t.Fatalf("the same id in two jobs is legal: %v", err)
	}
}
