// Wave 3 trigger parsing (PAR-016/PAR-018/PAR-019/PAR-021/PAR-024): the
// GitHub-only trigger vocabulary, its filters, and the named refusals that keep
// an unemittable trigger from being accepted inertly.
package workflow

import (
	"strings"
	"testing"
)

func parseWorkflow(t *testing.T, triggers string) *Workflow {
	t.Helper()
	w, err := Parse([]byte("name: W\n" + triggers + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: echo x\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return w
}

func TestRepositoryDispatchParseAndFires(t *testing.T) {
	w := parseWorkflow(t, "on:\n  repository_dispatch:\n    types: [deploy, rollback]")
	if !w.On.RepositoryDispatchSet || w.On.RepositoryDispatch == nil {
		t.Fatal("repository_dispatch did not parse")
	}
	if !w.Fires("repository_dispatch", RefBranch, "main", "deploy") {
		t.Fatal("a listed event_type must fire")
	}
	if w.Fires("repository_dispatch", RefBranch, "main", "other") {
		t.Fatal("an unlisted event_type must not fire")
	}
	// The bare spelling fires on every dispatch.
	bare := parseWorkflow(t, "on: repository_dispatch")
	if !bare.Fires("repository_dispatch", RefBranch, "", "anything") {
		t.Fatal("a bare repository_dispatch must fire on any event_type")
	}
	for _, bad := range []string{
		"on:\n  repository_dispatch:\n    types: [\"bad type\"]",
		"on:\n  repository_dispatch:\n    types: []",
		"on:\n  repository_dispatch:\n    bogus: [x]",
	} {
		if _, err := Parse([]byte("name: W\n" + bad + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: x\n")); err == nil {
			t.Fatalf("bad dispatch trigger parsed: %q", bad)
		}
	}
}

func TestWorkflowRunParseAndFilters(t *testing.T) {
	w := parseWorkflow(t, `on:
  workflow_run:
    workflows: [CI, Release]
    types: [completed]
    branches: [main]`)
	if !w.On.WorkflowRunSet {
		t.Fatal("workflow_run did not parse")
	}
	match := func(name, typ, branch string) bool {
		return w.FiresMatching(Match{Event: "workflow_run", Action: typ, Ref: branch, UpstreamWorkflow: name})
	}
	if !match("CI", "completed", "main") || !match("release", "completed", "main") {
		t.Fatal("a listed upstream workflow must fire (case-insensitive)")
	}
	if match("Other", "completed", "main") {
		t.Fatal("an unlisted upstream workflow must not fire")
	}
	if match("CI", "requested", "main") {
		t.Fatal("an unlisted type must not fire")
	}
	if match("CI", "completed", "dev") {
		t.Fatal("a branch outside the filter must not fire")
	}

	// `types: [in_progress]` is documented by GitHub but not emitted here:
	// a named parse error, never inert.
	if _, err := Parse([]byte(`on:
  workflow_run:
    types: [in_progress]
jobs:
  j:
    runs-on: linux
    steps:
      - run: x
`)); err == nil || !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("in_progress must be a named refusal, got %v", err)
	}
	// An empty workflow name is a parse error.
	if _, err := Parse([]byte(`on:
  workflow_run:
    workflows: [""]
jobs:
  j:
    runs-on: linux
    steps:
      - run: x
`)); err == nil {
		t.Fatal("an empty workflow name must be refused")
	}
}

func TestPullRequestTargetFires(t *testing.T) {
	w := parseWorkflow(t, "on:\n  pull_request_target:\n    types: [opened]\n    branches: [main]")
	if !w.Fires("pull_request_target", RefBranch, "main", "opened") {
		t.Fatal("pull_request_target must fire on its filters")
	}
	if w.Fires("pull_request_target", RefBranch, "dev", "opened") {
		t.Fatal("branch filter ignored")
	}
	if w.Fires("pull_request", RefBranch, "main", "opened") {
		t.Fatal("pull_request_target must not fire as pull_request")
	}
}

func TestRefTriggerFamily(t *testing.T) {
	w := parseWorkflow(t, "on: [create, delete, deployment, deployment_status]")
	for _, event := range []string{"create", "delete", "deployment", "deployment_status"} {
		if !w.Fires(event, RefBranch, "main", "") {
			t.Fatalf("%s must fire", event)
		}
	}
	events := w.Events()
	for _, event := range []string{"create", "delete", "deployment", "deployment_status"} {
		found := false
		for _, e := range events {
			if e == event {
				found = true
			}
		}
		if !found {
			t.Fatalf("Events() = %v, missing %s", events, event)
		}
	}
	// A mapping form with filters is refused (these events have none).
	if _, err := Parse([]byte("on:\n  create:\n    types: [x]\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: x\n")); err == nil {
		t.Fatal("create takes no filters")
	}
}

func TestMergeGroupRefusedByName(t *testing.T) {
	_, err := Parse([]byte("on: merge_group\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: x\n"))
	if err == nil {
		t.Fatal("merge_group must not be accepted without a merge queue")
	}
	if !strings.Contains(err.Error(), "merge queue") {
		t.Fatalf("merge_group refusal must name the missing queue, got %v", err)
	}
}

func TestWorkflowRunValidateEdges(t *testing.T) {
	if !ValidDispatchType("deploy-1.2") || ValidDispatchType("bad type") || ValidDispatchType("") {
		t.Fatal("ValidDispatchType charset")
	}
	for _, bad := range []string{
		`on:
  workflow_run:
    branches: [main]
    branches-ignore: [dev]`,
		`on:
  workflow_run:
    workflows: [` + strings.Repeat(`"x",`, 21) + `"y"]`,
	} {
		if _, err := Parse([]byte("name: W\n" + bad + "\njobs:\n  j:\n    runs-on: linux\n    steps:\n      - run: x\n")); err == nil {
			t.Fatalf("bad workflow_run filter parsed: %q", bad)
		}
	}
}

func TestWorkflowRunDefaultTypesMapping(t *testing.T) {
	// A mapping with filters but no `types:` defaults to completed, the same
	// as the bare spelling.
	w := parseWorkflow(t, `on:
  workflow_run:
    workflows: [CI]`)
	if !w.FiresMatching(Match{Event: "workflow_run", Action: "completed", UpstreamWorkflow: "CI"}) {
		t.Fatal("completed must fire with the default types")
	}
	if w.FiresMatching(Match{Event: "workflow_run", Action: "requested", UpstreamWorkflow: "CI"}) {
		t.Fatal("requested must not fire with the default types")
	}
}

func TestEventsListsEveryTrigger(t *testing.T) {
	w := parseWorkflow(t, `on:
  schedule:
    - cron: "0 3 * * *"
  push:
  pull_request:
  pull_request_target:
  workflow_dispatch:
  release:
  issues:
  issue_comment:
  repository_dispatch:
  workflow_run:
  create:
  delete:
  deployment:
  deployment_status:`)
	got := map[string]bool{}
	for _, e := range w.Events() {
		got[e] = true
	}
	for _, want := range []string{
		"push", "pull_request", "pull_request_target", "workflow_dispatch", "release",
		"issues", "issue_comment", "repository_dispatch", "workflow_run",
		"create", "delete", "deployment", "deployment_status", "schedule",
	} {
		if !got[want] {
			t.Fatalf("Events() = %v, missing %s", w.Events(), want)
		}
	}
	// The declared schedule fires (the maintainer selects the entries).
	if !w.FiresMatching(Match{Event: "schedule"}) {
		t.Fatal("schedule was declared")
	}
	if w.FiresMatching(Match{Event: "nonsense"}) {
		t.Fatal("an unknown event cannot fire")
	}
}

func TestClientPayloadEventField(t *testing.T) {
	if !ValidEventField("client_payload.environment") {
		t.Fatal("client_payload.<key> must be a supported event field")
	}
	if ValidEventField("client_payload.") || ValidEventField("client_payload.a.b") {
		t.Fatal("a malformed client_payload path must not validate")
	}
	// The closed vocabulary still refuses an arbitrary event path.
	if ValidEventField("client_payloadx.y") {
		t.Fatal("a non-client path must stay closed")
	}
}

func TestIsZeroPermissionKey(t *testing.T) {
	if !IsZeroPermissionKey(KeyAll) || IsZeroPermissionKey(ScopeContents) {
		t.Fatal("IsZeroPermissionKey must distinguish the baseline key from a scope")
	}
}
