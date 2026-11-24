package workflow

import (
	"strings"
	"testing"
)

// Edge coverage for the hand-written unmarshalers (yaml.v3 loses KnownFields
// inside a custom unmarshaler, so every shape branch is validated here).

func parseJob(t *testing.T, jobBody string) (*Workflow, error) {
	t.Helper()
	return Parse([]byte("on: push\njobs:\n  a:\n" + jobBody))
}

func TestParseUnmarshalEdges(t *testing.T) {
	// Null trigger block and null permission/container/concurrency blocks.
	if w, err := Parse([]byte("on: ~\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err != nil || len(w.Events()) != 0 {
		t.Fatalf("null on: %+v, %v", w, err)
	}
	if w, err := parseJob(t, "    runs-on: l\n    permissions: ~\n    concurrency: ~\n    container: ~\n    steps:\n      - run: x\n"); err != nil {
		t.Fatalf("null blocks: %v", err)
	} else if w.Permissions != nil || w.Jobs["a"].Permissions != nil || w.Jobs["a"].Concurrency != nil || w.Jobs["a"].Container != nil {
		t.Fatalf("null blocks = %+v / %+v", w.Permissions, w.Jobs["a"])
	}

	// A dispatch input must itself be a mapping.
	if _, err := Parse([]byte("on:\n  workflow_dispatch:\n    inputs:\n      x: string\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("scalar dispatch input accepted")
	}

	// Each typed input field surfaces a decode error.
	for _, frag := range []string{
		"description: [a]",
		"type: [a]",
		"required: [a]",
		"options: {a: b}",
	} {
		src := "on:\n  workflow_dispatch:\n    inputs:\n      x:\n        " + frag + "\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%q: accepted", frag)
		}
	}
	// A null default is simply absent.
	if w, err := Parse([]byte("on:\n  workflow_dispatch:\n    inputs:\n      x:\n        type: string\n        default: ~\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err != nil {
		t.Fatalf("null default: %v", err)
	} else if d := w.On.WorkflowDispatch.Inputs["x"].Default; d != nil {
		t.Fatalf("null default = %q", *d)
	}

	// Concurrency mapping with a non-scalar group fails in Decode.
	if _, err := Parse([]byte("on: push\nconcurrency: {group: [a]}\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("list concurrency group accepted")
	}
	// Permissions must be a mapping or the read-all/write-all shorthand.
	if _, err := Parse([]byte("on: push\npermissions: [a]\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("sequence permissions accepted")
	}

	// runs-on shape edges.
	if _, err := parseJob(t, "    runs-on: [a, [b]]\n    steps:\n      - run: x\n"); err == nil {
		t.Fatal("nested runs-on label accepted")
	}
	if _, err := parseJob(t, "    runs-on: {a: b}\n    steps:\n      - run: x\n"); err == nil {
		t.Fatal("mapping runs-on accepted")
	}
	// A null runs-on is empty, so a plain job fails the required field.
	if _, err := parseJob(t, "    runs-on: ~\n    steps:\n      - run: x\n"); err == nil {
		t.Fatal("null runs-on accepted for a plain job")
	}

	// Container mapping with a non-scalar image fails in Decode.
	if _, err := parseJob(t, "    runs-on: l\n    container: {image: [x]}\n    steps:\n      - run: x\n"); err == nil {
		t.Fatal("list container image accepted")
	}

	// Matrix must be a mapping; axis values are scalars or scalar lists.
	if _, err := parseJob(t, "    runs-on: l\n    strategy:\n      matrix: [a]\n    steps:\n      - run: x\n"); err == nil {
		t.Fatal("sequence matrix accepted")
	}
	w, err := parseJob(t, "    runs-on: l\n    strategy:\n      matrix:\n        go: 1.26\n        empty: ~\n    steps:\n      - run: x\n")
	if err != nil {
		t.Fatalf("scalar matrix axis: %v", err)
	}
	if got := w.Jobs["a"].Strategy.Matrix["go"]; len(got) != 1 || got[0] != "1.26" {
		t.Fatalf("scalar axis = %v", got)
	}
	if got := w.Jobs["a"].Strategy.Matrix["empty"]; len(got) != 0 {
		t.Fatalf("null axis = %v", got)
	}
	if _, err := parseJob(t, "    runs-on: l\n    strategy:\n      matrix:\n        go: [a, [b]]\n    steps:\n      - run: x\n"); err == nil {
		t.Fatal("nested matrix value accepted")
	}

	// checkKeys tolerates a non-mapping node (the following Decode reports it).
	if _, err := Parse([]byte("on:\n  push: x\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("scalar push trigger accepted")
	}

	// The scalar trigger branch reports an unknown event.
	if _, err := Parse([]byte("on: teleport\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("unknown scalar event accepted")
	}
}

func TestFiresPullRequestAbsent(t *testing.T) {
	w, err := Parse([]byte("on:\n  push:\njobs:\n  a:\n    runs-on: l\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Fires("pull_request", "main", "opened") {
		t.Fatal("push-only workflow fired on pull_request")
	}
}

func TestLabelsString(t *testing.T) {
	if got := (Labels{"linux", "docker"}).String(); got != "linux, docker" {
		t.Fatalf("Labels.String = %q", got)
	}
	if got := (Labels(nil)).String(); got != "" {
		t.Fatalf("empty Labels.String = %q", got)
	}
	_ = strings.TrimSpace("")
}
