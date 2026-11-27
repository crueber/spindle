package runner

// hashFiles (PAR-041): the glob dialect, the one-exec-per-step cache, the
// no-match rule and the named caps. The walk itself is a container-side script;
// these tests pin the boundary (one `docker exec`, an encoded pattern list, a
// deterministic value) and the evaluator wiring.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
)

func TestGlobToERE(t *testing.T) {
	cases := []struct{ pattern, want string }{
		{"go.sum", "^go\\.sum$"},
		{"**/go.sum", "^(.*/)?go\\.sum$"},
		{"*.go", "^[^/]*\\.go$"},
		{"**/*.go", "^(.*/)?[^/]*\\.go$"},
		{"dist/**", "^dist/.*$"},
		{"a?c", "^a[^/]c$"},
		{"[ab].txt", "^[ab]\\.txt$"},
		{"[!a]bc", "^[^a]bc$"},
		{"!vendor/**", "!^vendor/.*$"},
		{"./src/*.c", "^src/[^/]*\\.c$"},
		{"a+b(c)", "^a\\+b\\(c\\)$"},
	}
	for _, tc := range cases {
		got, err := globToERE(tc.pattern)
		if err != nil || got != tc.want {
			t.Errorf("globToERE(%q) = %q, %v; want %q", tc.pattern, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "!", "a[b"} {
		if _, err := globToERE(bad); err == nil {
			t.Errorf("globToERE(%q) accepted", bad)
		}
	}
}

func TestHashFilesOneWalkPerStep(t *testing.T) {
	d := &fakeDocker{runOutputs: []runOutput{{match: "spindle-hashfiles-", out: "deadbeef\n"}}}
	p := &Plan{Names: Names{Container: "c1"}, Workspace: "/spindle/workspace"}
	cache := newHashFilesCache()
	hash := (&Executor{Docker: d}).hashFilesFunc(context.Background(), p, cache)
	first, err := hash([]string{"**/go.sum"})
	if err != nil || first != "deadbeef" {
		t.Fatalf("first = %q, %v", first, err)
	}
	// The same pattern set in the same step is served from the cache.
	if second, _ := hash([]string{"**/go.sum"}); second != first {
		t.Fatalf("second = %q", second)
	}
	if calls := len(d.snapshot()); calls != 1 {
		t.Fatalf("docker exec calls = %d, want 1", calls)
	}
	// A completed step invalidates the cache (files may have changed).
	cache.invalidate()
	if _, err := hash([]string{"**/go.sum"}); err != nil {
		t.Fatal(err)
	}
	if calls := len(d.snapshot()); calls != 2 {
		t.Fatalf("after invalidate calls = %d, want 2", calls)
	}
}

func TestHashFilesNoMatchIsEmptyAndCapsAreNamed(t *testing.T) {
	p := &Plan{Names: Names{Container: "c"}, Workspace: "/w"}
	empty := &fakeDocker{runOutputs: []runOutput{{match: "spindle-hashfiles-", out: ""}}}
	hash := (&Executor{Docker: empty}).hashFilesFunc(context.Background(), p, nil)
	if got, err := hash([]string{"nothing/*"}); err != nil || got != "" {
		t.Fatalf("no match = %q, %v; want the empty string", got, err)
	}
	over := &fakeDocker{runOutputs: []runOutput{{
		match: "spindle-hashfiles-", errOut: "hashFiles: matched 1001 files, over the cap of 1000", code: 3,
	}}}
	hash = (&Executor{Docker: over}).hashFilesFunc(context.Background(), p, nil)
	if _, err := hash([]string{"**"}); err == nil || !strings.Contains(err.Error(), "cap of 1000") {
		t.Fatalf("cap error = %v", err)
	}
}

func TestHashFilesPatternsAreEncodedNotSpliced(t *testing.T) {
	d := &fakeDocker{}
	p := &Plan{Names: Names{Container: "c"}, Workspace: "/w"}
	if _, err := (&Executor{Docker: d}).hashFilesFunc(context.Background(), p, nil)([]string{"'; rm -rf / #"}); err != nil {
		t.Fatalf("hash: %v", err)
	}
	script := ""
	for _, call := range d.snapshot() {
		if strings.Contains(call, "spindle-hashfiles-") {
			script = call
		}
	}
	if script == "" {
		t.Fatal("no hashFiles exec recorded")
	}
	if strings.Contains(script, "rm -rf") {
		t.Fatalf("a pattern was spliced into shell text: %s", script)
	}
}

func TestHashFilesPreviewStaysAuthored(t *testing.T) {
	p := &Plan{}
	const field = "echo ${{ hashFiles('**/go.sum') }}"
	got, err := p.previewValue(field, workflow.TemplateValues{})
	if err != nil || got != field {
		t.Fatalf("preview = %q, %v; want the authored span", got, err)
	}
}

func TestHashFilesEvaluatesInConditions(t *testing.T) {
	ctx := ExprContext{Success: true, HashFiles: func(patterns []string) (string, error) {
		if len(patterns) != 2 || patterns[0] != "**/go.sum" || patterns[1] != "**/go.mod" {
			t.Fatalf("patterns = %v", patterns)
		}
		return "abc", nil
	}, Site: workflow.SiteStepIf}
	ok, err := EvalIf("hashFiles('**/go.sum', '**/go.mod') == 'abc'", ctx)
	if err != nil || !ok {
		t.Fatalf("condition = %v, %v", ok, err)
	}
	// No provider (a server-side condition) is a named error, never "".
	if _, err := EvalIf("hashFiles('**/go.sum') != ''", ExprContext{Site: workflow.SiteJobIf}); err == nil {
		t.Fatal("hashFiles without a provider was accepted")
	}
}

// TestJobTimeoutConclusionIsDistinct pins PAR-055's named result: a job context
// past its OWN deadline reads timed_out, while a parent cancel is cancelled.
func TestJobTimeoutConclusionIsDistinct(t *testing.T) {
	timed, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-timed.Done()
	if !jobTimedOut(timed, context.Background()) {
		t.Fatal("an expired job deadline did not read timed_out")
	}
	parent, cancelParent := context.WithCancel(context.Background())
	child := parent
	cancelParent()
	if jobTimedOut(child, parent) {
		t.Fatal("a parent cancel read timed_out")
	}
}

// TestHashFilesCancelledContextIsNotAHang pins that the exec uses the job
// context: a cancelled job returns promptly.
func TestHashFilesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &fakeDocker{blockOn: "spindle-hashfiles"}
	p := &Plan{Names: Names{Container: "c"}, Workspace: "/w"}
	hash := (&Executor{Docker: d}).hashFilesFunc(ctx, p, nil)
	done := make(chan error, 1)
	go func() { _, err := hash([]string{"**"}); done <- err }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hashFiles ignored the job context")
	}
}

func TestHashFilesErrorsAndCacheReset(t *testing.T) {
	e := &Executor{Docker: &fakeDocker{}}
	if _, err := e.runHashFiles(context.Background(), nil, []string{"x"}); err == nil {
		t.Fatal("nil plan accepted")
	}
	if _, err := e.runHashFiles(context.Background(), &Plan{}, []string{"x"}); err == nil {
		t.Fatal("a plan with no container accepted")
	}
	p := &Plan{Names: Names{Container: "c"}, Workspace: "/w"}
	if _, err := e.runHashFiles(context.Background(), p, nil); err == nil {
		t.Fatal("no patterns accepted")
	}
	if _, err := e.runHashFiles(context.Background(), p, []string{"a[b"}); err == nil {
		t.Fatal("a malformed pattern accepted")
	}
	// A failed exec with no output names the exit code, not an empty error.
	e.Docker = &fakeDocker{runOutputs: []runOutput{{match: "spindle-hashfiles-", code: 9}}}
	if _, err := e.runHashFiles(context.Background(), p, []string{"x"}); err == nil || !strings.Contains(err.Error(), "exit 9") {
		t.Fatalf("empty failure = %v", err)
	}
	// The cache drops its map rather than growing without bound.
	cache := newHashFilesCache()
	for i := 0; i < 40; i++ {
		cache.put([]string{strings.Repeat("x", i+1)}, "v")
	}
	if len(cache.cache) > 32 {
		t.Fatalf("cache grew to %d", len(cache.cache))
	}
}

func TestFailOrCancelClassification(t *testing.T) {
	e := &Executor{}
	timed, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-timed.Done()
	if got := e.failOrCancel(timed, context.Background(), quietStream(t), nil); got != threads.JobTimedOut {
		t.Fatalf("deadline = %q", got)
	}
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if got := e.failOrCancel(cancelled, context.Background(), quietStream(t), nil); got != threads.JobCancelled {
		t.Fatalf("cancel = %q", got)
	}
	if got := e.failOrCancel(context.Background(), context.Background(), quietStream(t), nil); got != threads.JobFailure {
		t.Fatalf("failure = %q", got)
	}
}
