package threads

// The server-side image policy (threads.image_allow_prefixes /
// threads.image_require_digest): the acceptance rows of
// docs/PLAN-ARTIFACT-KEEPER.md §7 / M2.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// gatedWorkflowYAML names a container and a service, so one refusal matrix
// covers both fields.
const gatedWorkflowYAML = `name: CI
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
jobs:
  build:
    runs-on: linux
    container: golang:1.27
    services:
      redis:
        image: redis:7
    steps:
      - run: go build ./...
`

// gatedEnv installs the container+service workflow and disables the approval
// gate (the subject is the image policy, not the fork gate).
func gatedEnv(t *testing.T, allow []string, requireDigest bool) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", gatedWorkflowYAML)
	e.svc.ImageAllowPrefixes = allow
	e.svc.ImageRequireDigest = requireDigest
	return e
}

// runClass is one trigger class the policy must cover.
type runClass struct {
	name  string
	event string
	opts  CreateOpts
}

func runClasses() []runClass {
	return []runClass{
		{"push", EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"}},
		{"same repo PR", EventPullReq, CreateOpts{Action: "opened", Branch: "main", Ref: "refs/pull/1/head", BaseBranch: "main", Actor: "writer"}},
		{"fork PR", EventPullReq, CreateOpts{Action: "opened", Branch: "main", Ref: "refs/pull/1/head", BaseBranch: "main", Fork: true, Actor: "guest"}},
		{"dispatch", EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer", Workflow: "ci.yml"}},
	}
}

// createForClass creates one run for a trigger class and returns its single
// job.
func createForClass(t *testing.T, e *testEnv, class runClass) (*Run, *Job) {
	t.Helper()
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, class.event, class.opts)
	if err != nil {
		t.Fatalf("%s: CreateRun: %v", class.name, err)
	}
	if len(runs) != 1 {
		t.Fatalf("%s: runs = %d", class.name, len(runs))
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%s: jobs = %d (%v)", class.name, len(jobs), err)
	}
	return runs[0], jobs[0]
}

func TestImagePolicyEmptyAllowlistIsUnrestricted(t *testing.T) {
	for _, class := range runClasses() {
		t.Run(class.name, func(t *testing.T) {
			e := gatedEnv(t, nil, false)
			run, job := createForClass(t, e, class)
			if job.State != JobQueued || job.Reason != "" {
				t.Fatalf("job = %s (%q), want queued with no reason", job.State, job.Reason)
			}
			if run.State != RunQueued {
				t.Fatalf("run = %s", run.State)
			}
		})
	}
}

func TestImagePolicyAllowlistPerRunClass(t *testing.T) {
	// "docker.io/library" allows the host-less golang:1.27 and redis:7 in
	// their canonical Docker Hub spelling; ghcr.io/acme is a second, distinct
	// registry. A host-only entry is not a prefix (the schema requires
	// host[:port]/path), so the library path is what names official images.
	allow := []string{"docker.io/library", "ghcr.io/acme"}
	for _, class := range runClasses() {
		for _, tc := range []struct {
			name       string
			container  string
			service    string
			wantState  string
			wantReason string // fragment the Reason must carry; "" = no reason
		}{
			{"allowed host-less", "golang:1.27", "redis:7", JobQueued, ""},
			{"allowed explicit hub", "docker.io/library/golang:1.27", "docker.io/library/redis:7", JobQueued, ""},
			{"allowed canonical library path", "docker.io/library/postgres:17", "redis:7", JobQueued, ""},
			{"allowed second registry", "ghcr.io/acme/build:1", "redis:7", JobQueued, ""},
			{"refused container", "evil.example/collector:1", "redis:7", JobFailure, `image "evil.example/collector:1" (container.image) is not allowed`},
			{"refused service", "golang:1.27", "evil.example/collector:1", JobFailure, `image "evil.example/collector:1" (services.redis.image) is not allowed`},
			// A prefix boundary is required: ghcr.io/acme must not allow a
			// lookalike host, and docker.io must not allow a lookalike path.
			{"host lookalike", "ghcr.io/acmeevil/x:1", "redis:7", JobFailure, "not allowed"},
			{"path lookalike", "docker.io/library-evil/x:1", "redis:7", JobFailure, "not allowed"},
			{"port required", "localhost:5000/x:1", "redis:7", JobFailure, "not allowed"},
		} {
			t.Run(class.name+"/"+tc.name, func(t *testing.T) {
				e := gatedEnv(t, allow, false)
				e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(
					strings.Replace(gatedWorkflowYAML, "container: golang:1.27", "container: "+tc.container, 1),
					"image: redis:7", "image: "+tc.service, 1))
				run, job := createForClass(t, e, class)
				if job.State != tc.wantState {
					t.Fatalf("job state = %s (%q), want %s", job.State, job.Reason, tc.wantState)
				}
				if tc.wantReason == "" {
					if job.Reason != "" {
						t.Fatalf("allowed job carries reason %q", job.Reason)
					}
					if run.State != RunQueued {
						t.Fatalf("run = %s", run.State)
					}
					return
				}
				if !strings.Contains(job.Reason, tc.wantReason) {
					t.Fatalf("reason = %q, want %q", job.Reason, tc.wantReason)
				}
				if !strings.Contains(job.Reason, "docker.io/library") || !strings.Contains(job.Reason, "ghcr.io/acme") {
					t.Fatalf("reason does not name the allowlist: %q", job.Reason)
				}
				// A refusal is a failed run, not a silently dropped job.
				if run.State != RunFailure {
					t.Fatalf("run = %s, want failure", run.State)
				}
				// The check mirror is red for exactly this job.
				if len(e.checks.reports) != 1 || e.checks.reports[0].State != "failure" {
					t.Fatalf("checks = %+v, want one failure", e.checks.reports)
				}
			})
		}
	}
}

func TestImagePolicyRequireDigest(t *testing.T) {
	pinned := "golang@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, class := range runClasses() {
		t.Run(class.name, func(t *testing.T) {
			// Off: a tag runs.
			e := gatedEnv(t, nil, false)
			if _, job := createForClass(t, e, class); job.State != JobQueued {
				t.Fatalf("digest rule off: job = %s (%q)", job.State, job.Reason)
			}
			// On: the tag is refused, naming the image and the key.
			e = gatedEnv(t, nil, true)
			_, job := createForClass(t, e, class)
			if job.State != JobFailure || !strings.Contains(job.Reason, "threads.image_require_digest") || !strings.Contains(job.Reason, `"golang:1.27"`) {
				t.Fatalf("digest rule on: job = %s (%q)", job.State, job.Reason)
			}
			// A pinned container still refuses the tag-named service.
			e = gatedEnv(t, nil, true)
			e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(gatedWorkflowYAML, "container: golang:1.27", "container: "+pinned, 1))
			_, job = createForClass(t, e, class)
			if job.State != JobFailure || !strings.Contains(job.Reason, "services.redis.image") {
				t.Fatalf("unpinned service: job = %s (%q)", job.State, job.Reason)
			}
			// Both pinned: the job runs.
			e = gatedEnv(t, nil, true)
			e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(
				strings.Replace(gatedWorkflowYAML, "container: golang:1.27", "container: "+pinned, 1),
				"image: redis:7", "image: redis@"+strings.TrimPrefix(pinned, "golang@"), 1))
			if _, job := createForClass(t, e, class); job.State != JobQueued {
				t.Fatalf("pinned job = %s (%q)", job.State, job.Reason)
			}
		})
	}
}

func TestImagePolicyFailsClosedOnAnInvalidEntry(t *testing.T) {
	for _, bad := range []string{"https://ak.example/dockerhub", "ak.example/dockerhub/", "", "AK.example/x", "docker.io/library:latest"} {
		e := gatedEnv(t, []string{bad}, false)
		_, job := createForClass(t, e, runClasses()[0])
		if job.State != JobFailure || !strings.Contains(job.Reason, "fail closed") || !strings.Contains(job.Reason, "threads.image_allow_prefixes") {
			t.Fatalf("entry %q: job = %s (%q)", bad, job.State, job.Reason)
		}
	}
}

func TestRefusedForkRunCarriesReasonAndNoTokenMaterial(t *testing.T) {
	e := gatedEnv(t, []string{"docker.io/library"}, false)
	// A fork PR reads the BASE branch's file, so the hostile image lives there
	// (the untrusted head cannot rewrite it — and would be refused anyway).
	e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(gatedWorkflowYAML,
		"container: golang:1.27", "container: evil.example/collector:1", 1))
	class := runClasses()[2] // fork PR
	run, job := createForClass(t, e, class)
	if job.State != JobFailure || !strings.Contains(job.Reason, `"evil.example/collector:1"`) || !strings.Contains(job.Reason, "docker.io/library") {
		t.Fatalf("fork job = %s (%q)", job.State, job.Reason)
	}
	if job.Trusted {
		t.Fatal("fork job claims trusted")
	}
	// The records themselves carry no credential material: the refusal path
	// never reads a docker config, and the reason names only the image and the
	// allowlist.
	for _, key := range []string{RunKey("o", "r", run.ID), JobKey("o", "r", run.ID, job.ID)} {
		raw, _, err := store.GetBytes(ctx(), e.store, key, store.GetOptions{})
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		text := string(raw)
		for _, forbidden := range []string{"wjt_", "wrt_", "wrg_", "password", "auths", "DOCKER_CONFIG", "registry_password"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s carries %q: %s", key, forbidden, text)
			}
		}
	}
}

func TestCanonicalImageRef(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"golang:1.27", "docker.io/library/golang:1.27"},
		{"golang", "docker.io/library/golang"},
		{"alpine/git:latest", "docker.io/alpine/git:latest"},
		{"golang@sha256:abc", "docker.io/library/golang@sha256:abc"},
		{"docker.io/library/golang:1.27", "docker.io/library/golang:1.27"},
		{"ghcr.io/a/b:t", "ghcr.io/a/b:t"},
		{"localhost:5000/a/b:t", "localhost:5000/a/b:t"},
		{"[::1]:5000/a", "[::1]:5000/a"},
		{"", ""},
	} {
		if got := canonicalImageRef(tc.in); got != tc.want {
			t.Fatalf("canonicalImageRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestImageHasPrefixBoundaries(t *testing.T) {
	for _, tc := range []struct {
		image, prefix string
		want          bool
	}{
		{"docker.io/library/golang:1.27", "docker.io", true},
		{"docker.io/library/golang:1.27", "docker.io/library", true},
		{"docker.io/library/golang:1.27", "docker.io/library/golang", true},
		{"docker.io/library/golang:1.27", "docker.io/library/golang:1.27", true},
		{"docker.io/library/golang:1.27", "docker.io/library-evil", false},
		{"docker.io/library/golang:1.27", "docker.io/libr", false},
		{"ghcr.io/acme/x:t", "ghcr.io/acme", true},
		{"ghcr.io/acmeevil/x:t", "ghcr.io/acme", false},
		{"localhost:5000/a:t", "localhost:5000", true},
		{"localhost:6000/a:t", "localhost:5000", false},
		{"a/b@sha256:x", "a/b", true},
	} {
		if got := imageHasPrefix(tc.image, tc.prefix); got != tc.want {
			t.Fatalf("imageHasPrefix(%q, %q) = %v, want %v", tc.image, tc.prefix, got, tc.want)
		}
	}
}

// The policy is evaluated per job, after the matrix/if decisions: one job
// may run while its sibling is refused, and the run folds to failure.
func TestImagePolicyAppliesPerJob(t *testing.T) {
	e := gatedEnv(t, []string{"docker.io/library"}, false)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on:
  push:
    branches: [main]
jobs:
  good:
    runs-on: linux
    container: golang:1.27
    steps:
      - run: true
  bad:
    runs-on: linux
    container: evil.example/collector:1
    steps:
      - run: true
`)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", runs[0].ID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %d (%v)", len(jobs), err)
	}
	states := map[string]string{}
	for _, j := range jobs {
		states[j.Key] = j.State
	}
	if states["good"] != JobQueued || states["bad"] != JobFailure {
		t.Fatalf("job states = %+v", states)
	}
	if runs[0].State != RunQueued {
		// One job is still queued, so the run is not terminal.
		t.Fatalf("run = %s", runs[0].State)
	}
}

// A job a condition already skipped pulls no image, so the gate does not turn
// a skip into a failure (the record keeps its `if:` reason).
func TestImagePolicyLeavesSkippedJobsAlone(t *testing.T) {
	e := gatedEnv(t, []string{"docker.io"}, false)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", `name: CI
on:
  push:
    branches: [main]
jobs:
  skipped:
    runs-on: linux
    if: github.ref == 'refs/heads/other'
    container: evil.example/collector:1
    steps:
      - run: true
`)
	run, job := createForClass(t, e, runClasses()[0])
	if job.State != JobSkipped || !strings.Contains(job.Reason, "github.ref") {
		t.Fatalf("job = %s (%q)", job.State, job.Reason)
	}
	if run.State != RunSuccess {
		t.Fatalf("run = %s", run.State)
	}
	if len(e.checks.reports) != 1 || e.checks.reports[0].State != "skipped" {
		t.Fatalf("checks = %+v", e.checks.reports)
	}
}

// ensure the unused-import guard stays honest if the tests above change
var _ = context.Background

// TestImagePolicyHelpers pins the small pieces the matrix goes through:
// refusal rendering, the allowlist entry spellings, the digest grammar and
// the empty/nil cases (a spec without an image names nothing to gate).
func TestImagePolicyHelpers(t *testing.T) {
	refusal := ImageRefusal{Image: "x:1", Source: "container.image", Reason: "image \"x:1\" is not allowed"}
	if refusal.String() != refusal.Reason {
		t.Fatalf("String() = %q", refusal.String())
	}

	for _, tc := range []struct {
		entry string
		ok    bool
	}{
		{"docker.io/library", true},
		{"localhost:5000/a/b", true},
		{"192.168.1.5:5000/a", true},
		{" docker.io/library", false}, // surrounding whitespace
		{"DOCKER.io/library", false},  // uppercase
		{"docker.io//library", false}, // empty segment
		{"docker.io/library/", false}, // trailing slash
		{"https://docker.io/library", false},
		{"", false},
	} {
		if _, err := normalizeImagePrefix(tc.entry); (err == nil) != tc.ok {
			t.Fatalf("normalizeImagePrefix(%q) err = %v, want ok=%v", tc.entry, err, tc.ok)
		}
	}

	policy := ImagePolicy{AllowPrefixes: []string{"docker.io/library"}}
	if refusal, err := policy.checkJobImages(nil); refusal != nil || err != nil {
		t.Fatalf("nil spec: %v / %v", refusal, err)
	}
	if refusal, err := policy.checkJobImages(&Spec{Services: map[string]ServiceSpec{"db": {}}}); refusal != nil || err != nil {
		t.Fatalf("empty images: %v / %v", refusal, err)
	}
	if refusal, err := policy.checkImage("  ", "container.image"); refusal != nil || err != nil {
		t.Fatalf("blank image: %v / %v", refusal, err)
	}

	// The digest grammar: length and hex case, and no digest without '@'.
	for _, tc := range []struct {
		image string
		ok    bool
	}{
		{"golang@sha256:" + strings.Repeat("a", 64), true},
		{"golang@sha256:" + strings.Repeat("a", 63), false},
		{"golang@sha256:" + strings.Repeat("A", 64), false},
		{"golang@sha256:" + strings.Repeat("g", 64), false},
		{"golang:1.27", false},
	} {
		if got := hasImageDigest(tc.image); got != tc.ok {
			t.Fatalf("hasImageDigest(%q) = %v, want %v", tc.image, got, tc.ok)
		}
	}
}

// The gate is a server-side decision, not a payload field: no allowlist text
// may appear in the job spec a runner receives (L3 is rejected in the plan),
// and a refused job's record stays self-describing.
func TestImagePolicyNeverEntersTheJobSpec(t *testing.T) {
	e := gatedEnv(t, []string{"docker.io/library", "ak.example/dockerhub"}, true)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(gatedWorkflowYAML,
		"container: golang:1.27", "container: evil.example/collector:1", 1))
	run, job := createForClass(t, e, runClasses()[0])
	raw, _, err := store.GetBytes(ctx(), e.store, JobKey("o", "r", run.ID, job.ID), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	specJSON, err := json.Marshal(rec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"image_allow_prefixes", "AllowPrefixes", "image_require_digest", "ak.example/dockerhub"} {
		if strings.Contains(string(specJSON), forbidden) {
			t.Fatalf("job spec carries %q: %s", forbidden, specJSON)
		}
	}
	// The refusal is a real job failure, not a queued job with a note.
	if job.State != JobFailure || job.Reason == "" {
		t.Fatalf("job = %s (%q)", job.State, job.Reason)
	}
}

// A policy tightened after a run was created is enforced on the next attempt:
// rerun re-applies the gate, so "rerun" cannot bypass a newly-enabled
// allowlist (a refusal is a failed job with the same named reason, and a run
// with nothing left to run folds to failure).
func TestImagePolicyReappliedOnRerun(t *testing.T) {
	e := gatedEnv(t, nil, false)
	e.git.add(testSHA, ".spindle/workflows/ci.yml", strings.Replace(gatedWorkflowYAML,
		"container: golang:1.27", "container: evil.example/collector:1", 1))
	run, job := createForClass(t, e, runClasses()[0])
	if job.State != JobQueued {
		t.Fatalf("pre-policy job = %s (%q)", job.State, job.Reason)
	}
	e.svc.ImageAllowPrefixes = []string{"docker.io/library"}

	rerun, err := e.svc.RerunRun(ctx(), "o/r", run.ID, e.writer())
	if err != nil {
		t.Fatalf("RerunRun: %v", err)
	}
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d (%v)", len(jobs), err)
	}
	if jobs[0].State != JobFailure || !strings.Contains(jobs[0].Reason, "evil.example/collector:1") {
		t.Fatalf("rerun job = %s (%q)", jobs[0].State, jobs[0].Reason)
	}
	if rerun.State != RunFailure {
		t.Fatalf("rerun run = %s", rerun.State)
	}
}
