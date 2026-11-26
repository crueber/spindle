package threads

// runner_labels_test.go — the label→image mapping (PAR-171): Forgejo's
// `name:docker://image` declaration is accepted and honoured for a job whose
// runs-on names the label and whose workflow named no container image; the D-15
// backends are refused by name; a mapped image never overrides the workflow's
// own container.image.

import (
	"errors"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

const mappedImageWorkflow = `name: Mapped
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  pinned:
    runs-on: [self-hosted, linux]
    container:
      image: debian:12-slim
    steps:
      - run: echo pinned
`

// TestParseRunnerLabels pins the documented forms and the refusals.
func TestParseRunnerLabels(t *testing.T) {
	got, err := ParseRunnerLabels([]string{"self-hosted", "linux", "ubuntu-latest:docker://node:lts", " ubuntu-latest:docker://node:lts "})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(got.Labels, ",") != "linux,self-hosted,ubuntu-latest" {
		t.Fatalf("labels = %v", got.Labels)
	}
	if got.Images["ubuntu-latest"] != "node:lts" {
		t.Fatalf("images = %v", got.Images)
	}
	// Empty entries are ignored (a trailing comma in a flag).
	if got, err := ParseRunnerLabels([]string{"", "  ", "linux"}); err != nil || strings.Join(got.Labels, ",") != "linux" {
		t.Fatalf("empty entries = %+v (%v)", got, err)
	}
	// A plain set has no mapping and keeps the default label when empty.
	plain, err := ParseRunnerLabels([]string{"linux"})
	if err != nil || plain.Images != nil || len(plain.Labels) != 1 {
		t.Fatalf("plain = %+v (%v)", plain, err)
	}
	empty, err := ParseRunnerLabels(nil)
	if err != nil || strings.Join(empty.Labels, ",") != "self-hosted" {
		t.Fatalf("empty = %+v (%v)", empty, err)
	}
	// The D-15 backends are named refusals, never a silent docker fallback.
	for _, entry := range []string{"linux:lxc://debian", "linux:host", "linux:host:///usr/bin", "linux:podman://x", "linux:docker://", ":docker://node:lts", "linux:docker://node lts"} {
		if _, perr := ParseRunnerLabels([]string{entry}); !errors.Is(perr, ErrInvalid) {
			t.Fatalf("%q err = %v, want invalid", entry, perr)
		}
	}
	// The refusal text names the non-goal so an operator knows what to write.
	_, err = ParseRunnerLabels([]string{"linux:lxc://debian"})
	if err == nil || !strings.Contains(err.Error(), "D-15") || !strings.Contains(err.Error(), "docker://") {
		t.Fatalf("lxc refusal = %v", err)
	}
}

// TestRunnerLabelImageMappingReachesThePayload pins the server half end to end:
// register/declare store the mapping, the run page still matches the label, and
// the delivered job carries the mapped image unless the workflow named one.
func TestRunnerLabelImageMappingReachesThePayload(t *testing.T) {
	e := newTestEnv(t)
	e.git.files = map[string]map[string][]byte{}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", mappedImageWorkflow)
	e.git.refs["refs/heads/main"] = testSHA
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main", Ref: "refs/heads/main", Actor: "w"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Register through the protocol (register/declare share the parser).
	admin := auth.Principal{Name: "root", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, MintInput{
		Name: "mapped", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{
		Token: mint.Token, Labels: []string{"self-hosted", "linux", "ubuntu-latest:docker://node:lts"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	runner := &runnerAuth{Doc: &RunnerDoc{ID: reg.RunnerID, AllowAll: true, State: RunnerActive,
		Labels: reg.Labels, LabelImages: reg.LabelImages}}
	declared, err := e.svc.Declare(ctx(), runner, DeclareInput{
		Version: "test", Labels: []string{"self-hosted", "linux", "ubuntu-latest:docker://node:lts"},
	})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if declared.LabelImages["ubuntu-latest"] != "node:lts" {
		t.Fatalf("declare echo = %+v", declared)
	}
	if _, err := e.svc.Declare(ctx(), runner, DeclareInput{Labels: []string{"x:lxc://y"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("declare lxc err = %v", err)
	}
	// The registry record carries it for /admin/runners.
	list, err := e.svc.ListRunners(ctx(), admin)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	if len(list) != 1 || list[0].LabelImages["ubuntu-latest"] != "node:lts" {
		t.Fatalf("runner view = %+v", list)
	}

	// Claim both jobs and inspect the delivered images.
	seen := map[string]string{}
	for i := 0; i < 2; i++ {
		claim, cerr := e.svc.claimLoop(ctx(), "o", "r", runner.Doc, runner.Doc.Labels)
		if cerr != nil || claim == nil {
			t.Fatalf("claim %d = %+v (%v)", i, claim, cerr)
		}
		res, perr := e.svc.fetchPayload(ctx(), "o", "r", runner, claim, claim.Job, "")
		if perr != nil {
			t.Fatalf("fetch %s: %v", claim.Job.Name, perr)
		}
		image := ""
		if res.Spec.Container != nil {
			image = res.Spec.Container.Image
		}
		seen[claim.Job.Name] = image
		if claim.Job.Name == "build" {
			if res.LabelImages["ubuntu-latest"] != "node:lts" {
				t.Fatalf("payload did not echo the mapping: %+v", res.LabelImages)
			}
		}
	}
	if seen["build"] != "node:lts" {
		t.Fatalf("mapped image = %q, want node:lts (seen %v)", seen["build"], seen)
	}
	// The workflow's own container.image always wins over the mapping.
	if seen["pinned"] != "debian:12-slim" {
		t.Fatalf("explicit container image = %q, want debian:12-slim", seen["pinned"])
	}
}

// TestRegisterStoresLabelImages pins the registration path too: a runner that
// registers (not declares) with the mapping stores it immediately.
func TestRegisterStoresLabelImages(t *testing.T) {
	e := newTestEnv(t)
	admin := auth.Principal{Name: "root", Admin: true}
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, MintInput{Name: "r1", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	reg, err := e.svc.Register(ctx(), RegisterInput{
		Token: mint.Token, Labels: []string{"self-hosted", "linux", "ubuntu-latest:docker://node:lts"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.LabelImages["ubuntu-latest"] != "node:lts" {
		t.Fatalf("register echo = %+v", reg)
	}
	raw, _, err := e.svc.getJSON(ctx(), RunnerKey(reg.RunnerID))
	if err != nil || raw == nil {
		t.Fatalf("registry record: %v", err)
	}
	var doc RunnerDoc
	if jerr := jsonUnmarshalTest(raw, &doc); jerr != nil {
		t.Fatal(jerr)
	}
	if doc.LabelImages["ubuntu-latest"] != "node:lts" {
		t.Fatalf("stored runner = %+v", doc)
	}
	// A refused backend is a register error, not a stored label.
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: mint.Token, Labels: []string{"x:host"}}); err == nil {
		t.Fatal("a used token registered; want an auth error")
	}
}
