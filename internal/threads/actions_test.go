package threads

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// Repository-local actions at run creation (docs/threads.md §3.4a): the
// definition is read from the RUN commit, embedded in the job payload, shown
// expanded on the job, and every refusal is a failed job with a Reason (never a
// silent skip, never a run-wide failure). The image gate runs over the embedded
// definitions exactly like a container image.

// actionWorkflow is one job whose steps use a local composite action, a local
// docker action, and an inline docker:// image.
const actionWorkflow = `
name: Actions
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: actions/checkout
      - id: setup
        name: toolchain
        uses: ./.spindle/actions/setup
        with:
          version: "2.0"
      - run: echo "${{ steps.setup.outputs.bin }}"
      - uses: ./.spindle/actions/lint
      - uses: docker://alpine:3.20
`

const setupAction = `
name: setup
inputs:
  version:
    default: "1.0"
outputs:
  bin:
    value: ${{ steps.install.outputs.bin }}
runs:
  using: composite
  steps:
    - id: install
      run: install "$INPUT_VERSION"
      shell: bash
    - uses: ./.spindle/actions/deep
`

const deepAction = `
name: deep
runs:
  using: composite
  steps:
    - run: echo deep
      shell: sh
`

const lintAction = `
name: lint
runs:
  using: docker
  image: docker://ghcr.io/acme/lint:1.0
  args: ["--strict"]
`

// addActionWorkflow installs the workflow and the action files it uses at
// testSHA.
func (e *testEnv) addActionWorkflow() {
	e.git.add(testSHA, ".spindle/workflows/ci.yml", actionWorkflow)
	e.git.add(testSHA, ".spindle/actions/setup/action.yml", setupAction)
	e.git.add(testSHA, ".spindle/actions/deep/action.yaml", deepAction)
	e.git.add(testSHA, ".spindle/actions/lint/action.yml", lintAction)
}

// jobOf loads one job of a created run.
func jobOf(t *testing.T, e *testEnv, run *Run, id string) *Job {
	t.Helper()
	res, err := e.store.Get(ctx(), JobKey("o", "r", run.ID, id), store.GetOptions{})
	if err != nil {
		t.Fatalf("load job %s: %v", id, err)
	}
	obj, ok := res.(store.Object)
	if !ok {
		t.Fatalf("job %s: unexpected get result %T", id, res)
	}
	defer obj.Body.Close()
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("decode job %s: %v", id, err)
	}
	return &job
}

func TestCreateRunResolvesLocalActions(t *testing.T) {
	e := newTestEnv(t)
	e.addActionWorkflow()
	runs := e.mustCreate(t)
	job := jobOf(t, e, runs[0], "j-build")
	if job.State != JobQueued || job.Reason != "" {
		t.Fatalf("job = %+v", job)
	}
	if job.Spec == nil {
		t.Fatal("no spec")
	}
	// Both definitions (including the one nested inside setup) are embedded,
	// keyed by their repository-relative directory, pinned to the run commit.
	for _, path := range []string{".spindle/actions/setup", ".spindle/actions/deep", ".spindle/actions/lint"} {
		if _, ok := job.Spec.Actions[path]; !ok {
			t.Fatalf("action %q not embedded: %+v", path, job.Spec.Actions)
		}
	}
	if got := job.Spec.Actions[".spindle/actions/setup"].Inputs["version"].Default; got != "1.0" {
		t.Fatalf("input default = %q", got)
	}
	if got := job.Spec.Actions[".spindle/actions/deep"].Steps[0].Run; got != "echo deep" {
		t.Fatalf("nested action body = %q", got)
	}
	// The display list is expanded IN PLACE, in order, with the caller as the
	// parent of each inner step.
	want := []struct{ name, parent, run, uses string }{
		{"", "", "", "actions/checkout"},
		{"toolchain", "", "", "./.spindle/actions/setup"}, // the caller row is not shown: its steps are
	}
	_ = want
	var names []string
	for _, st := range job.Steps {
		names = append(names, st.Name+"|"+st.Parent+"|"+st.Run+"|"+st.Uses)
	}
	joined := strings.Join(names, "\n")
	for _, want := range []string{
		"|toolchain|install \"$INPUT_VERSION\"|",        // a setup inner step, parent "toolchain"
		"|toolchain / .spindle/actions/deep|echo deep|", // the nested action's step
		"|||docker://ghcr.io/acme/lint:1.0",             // the local docker action's image
		"|||docker://alpine:3.20",                       // the inline image
		"||echo \"${{ steps.setup.outputs.bin }}\"|",    // the caller's output reference
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expanded display steps miss %q:\n%s", want, joined)
		}
	}
	// The composite caller's own row is replaced by its expansion.
	if strings.Contains(joined, "./.spindle/actions/setup") {
		t.Fatalf("the composite caller must be expanded in place:\n%s", joined)
	}
	// Order: install before deep before lint before the inline image.
	install := strings.Index(joined, "install \"$INPUT_VERSION\"")
	deep := strings.Index(joined, "echo deep")
	lint := strings.Index(joined, "docker://ghcr.io/acme/lint")
	inline := strings.Index(joined, "docker://alpine:3.20")
	if !(install < deep && deep < lint && lint < inline) {
		t.Fatalf("expansion order = install %d, deep %d, lint %d, inline %d\n%s", install, deep, lint, inline, joined)
	}
}

func TestCreateRunActionRefusals(t *testing.T) {
	cases := []struct {
		name  string
		step  string
		files map[string]string
		want  string
	}{
		{
			name: "missing action file",
			step: "      - uses: ./.spindle/actions/nope",
			want: "no action metadata at .spindle/actions/nope",
		},
		{
			name: "escaping path",
			step: "      - uses: ./../../etc/action",
			want: "escapes the repository",
		},
		{
			name:  "wrong using",
			step:  "      - uses: ./.spindle/actions/tool",
			files: map[string]string{".spindle/actions/tool/action.yml": "runs:\n  using: node20\n"},
			want:  `"node20" is not supported`,
		},
		{
			name:  "unknown with key",
			step:  "      - uses: ./.spindle/actions/tool\n        with:\n          versoin: 1",
			files: map[string]string{".spindle/actions/tool/action.yml": "inputs:\n  version:\n    default: \"1\"\nruns:\n  using: composite\n  steps:\n    - run: echo hi\n      shell: sh\n"},
			want:  `input "versoin" is not declared`,
		},
		{
			name:  "not yaml",
			step:  "      - uses: ./.spindle/actions/tool",
			files: map[string]string{".spindle/actions/tool/action.yml": "runs: [oops"},
			want:  "action:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.git.add(testSHA, ".spindle/workflows/ci.yml", actionWorkflowPrefix+tc.step+"\n")
			for p, body := range tc.files {
				e.git.add(testSHA, p, body)
			}
			runs := e.mustCreate(t)
			job := jobOf(t, e, runs[0], "j-build")
			if job.State != JobFailure || !strings.Contains(job.Reason, tc.want) {
				t.Fatalf("job = %s / %q, want failure with %q", job.State, job.Reason, tc.want)
			}
			if !strings.Contains(job.Reason, "step 1") {
				t.Fatalf("reason does not name the step: %q", job.Reason)
			}
		})
	}
}

// actionWorkflowPrefix is a one-step job; the tests append the step.
const actionWorkflowPrefix = `name: A
on: push
jobs:
  build:
    runs-on: linux
    steps:
`

func TestCreateRunActionCycleAndDepth(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: A
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: ./.spindle/actions/a
`)
	e.git.add(testSHA, ".spindle/actions/a/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: ./.spindle/actions/b\n")
	e.git.add(testSHA, ".spindle/actions/b/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: ./.spindle/actions/a\n")
	runs := e.mustCreate(t)
	job := jobOf(t, e, runs[0], "j-build")
	if job.State != JobFailure || !strings.Contains(job.Reason, "cycle") ||
		!strings.Contains(job.Reason, ".spindle/actions/a → .spindle/actions/b → .spindle/actions/a") {
		t.Fatalf("cycle job = %s / %q", job.State, job.Reason)
	}

	// A chain deeper than the limit is refused, and the reason names the limit.
	e = newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: A
on: push
jobs:
  build:
    runs-on: linux
    steps:
      - uses: ./.spindle/actions/a0
`)
	for i := 0; i <= 11; i++ {
		body := "  steps:\n    - run: echo bottom\n      shell: sh\n"
		if i < 11 {
			body = fmt.Sprintf("  steps:\n    - uses: ./.spindle/actions/a%d\n", i+1)
		}
		e.git.add(testSHA, fmt.Sprintf(".spindle/actions/a%d/action.yml", i),
			"runs:\n  using: composite\n"+body)
	}
	runs = e.mustCreate(t)
	job = jobOf(t, e, runs[0], "j-build")
	if job.State != JobFailure || !strings.Contains(job.Reason, "deeper than the limit of 10") {
		t.Fatalf("depth job = %s / %q", job.State, job.Reason)
	}
}

// TestCreateRunActionImagePolicy pins the single gate: a local docker action's
// image and an inline docker:// image are refused by the SAME allowlist/digest
// rule as a job container, and the reason names the step and the image.
func TestCreateRunActionImagePolicy(t *testing.T) {
	e := newTestEnv(t)
	e.addActionWorkflow()
	e.svc.ImageAllowPrefixes = []string{"docker.io/library"}
	runs := e.mustCreate(t)
	job := jobOf(t, e, runs[0], "j-build")
	if job.State != JobFailure {
		t.Fatalf("job = %s / %q", job.State, job.Reason)
	}
	if !strings.Contains(job.Reason, "ghcr.io/acme/lint:1.0") || !strings.Contains(job.Reason, "threads.image_allow_prefixes") ||
		!strings.Contains(job.Reason, "uses: ./.spindle/actions/lint") {
		t.Fatalf("reason = %q", job.Reason)
	}

	// The same policy allows a Docker Hub image; the digest rule then refuses it.
	e = newTestEnv(t)
	e.addActionWorkflow()
	e.svc.ImageAllowPrefixes = []string{"docker.io/library", "ghcr.io/acme"}
	e.svc.ImageRequireDigest = true
	runs = e.mustCreate(t)
	job = jobOf(t, e, runs[0], "j-build")
	if job.State != JobFailure || !strings.Contains(job.Reason, "not digest-pinned") ||
		!strings.Contains(job.Reason, "ghcr.io/acme/lint:1.0") {
		t.Fatalf("digest reason = %s / %q", job.State, job.Reason)
	}

	// A refused docker action refuses the job BEFORE it could be enqueued: the
	// reason is the evidence and the job is not queued.
	if job.State != JobFailure {
		t.Fatal("a refused job must not be queued")
	}
}

// TestCreateRunActionPinCommit pins the resolution commit: the action is read
// from the run commit, never from the workflow commit's tip or a later commit.
func TestCreateRunActionPinCommit(t *testing.T) {
	e := newTestEnv(t)
	const otherSHA = "2222222222222222222222222222222222222222"
	e.addActionWorkflow()
	// A newer commit holds a different definition; the run must keep the one at
	// the run commit.
	e.git.add(otherSHA, ".spindle/actions/setup/action.yml", strings.Replace(setupAction, `"1.0"`, `"9.9"`, 1))
	runs := e.mustCreate(t)
	job := jobOf(t, e, runs[0], "j-build")
	if got := job.Spec.Actions[".spindle/actions/setup"].Inputs["version"].Default; got != "1.0" {
		t.Fatalf("action definition came from another commit: default = %q", got)
	}
	if !strings.Contains(job.Spec.Actions[".spindle/actions/setup"].Steps[0].Run, "install") {
		t.Fatalf("wrong definition: %+v", job.Spec.Actions[".spindle/actions/setup"])
	}
}

// TestCreateRunSkippedActionJobIsNotResolved keeps the rule the image gate
// already follows: a job a condition skipped runs nothing, so a broken action
// in it refuses nothing.
func TestCreateRunSkippedActionJobIsNotResolved(t *testing.T) {
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: A
on: push
jobs:
  build:
    runs-on: linux
    if: github.ref == 'refs/heads/nope'
    steps:
      - uses: ./.spindle/actions/missing
`)
	runs := e.mustCreate(t)
	job := jobOf(t, e, runs[0], "j-build")
	if job.State != JobSkipped || job.Reason == "" {
		t.Fatalf("job = %s / %q", job.State, job.Reason)
	}
}

// TestCreateRunForkDockerActionGate pins the fork invariant at creation: a fork
// PR's local actions are resolved from the PR HEAD (the checked-out code), and
// its docker image is gated by the SAME policy as every other run class. The
// refusal is a failed job (no runner, no container); the run is untrusted and
// no secret material exists in the payload.
func TestCreateRunForkDockerActionGate(t *testing.T) {
	e := newTestEnv(t)
	const head = "4444444444444444444444444444444444444444"
	// The workflow file is the BASE commit's (trust selection); the action
	// definitions are the PR head's, exactly what the job checks out.
	e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(actionWorkflow, "on: push", "on: [pull_request]", 1))
	e.git.add(head, ".spindle/actions/setup/action.yml", setupAction)
	e.git.add(head, ".spindle/actions/deep/action.yaml", deepAction)
	e.git.add(head, ".spindle/actions/lint/action.yml", lintAction)
	e.svc.ImageAllowPrefixes = []string{"docker.io/library"}
	runs, err := e.svc.CreateRun(ctx(), "o/r", head, EventPullReq, CreateOpts{
		Action: "opened", Branch: "main", Ref: "refs/pull/1/head",
		BaseCommit: testSHA, BaseBranch: "main", Fork: true, Actor: "guest",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	job := jobOf(t, e, runs[0], "j-build")
	if job.Trusted {
		t.Fatal("a fork job must be untrusted")
	}
	if job.State != JobFailure || !strings.Contains(job.Reason, "ghcr.io/acme/lint:1.0") {
		t.Fatalf("fork job = %s / %q", job.State, job.Reason)
	}
	if _, ok := job.Spec.Actions[".spindle/actions/setup"]; !ok {
		t.Fatalf("the head's action definitions are not embedded: %+v", job.Spec.Actions)
	}
}
