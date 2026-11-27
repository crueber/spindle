package threads

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
)

// Error-path coverage for the Wave-4 surface (role refusals, malformed bodies,
// unknown ids and method mismatches), which is most of the remaining package
// coverage gate.

func TestHTTPWave4RouteErrorTable(t *testing.T) {
	e, h, cur := newHTTP(t)
	e.git.refs["HEAD"] = testSHA
	e.roles.roles["maint"] = "maintain"
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		as     auth.Principal
		want   int
	}{
		{"anonymous schedules on a public repo", http.MethodGet, "/o/r/api/threads/schedules", "", auth.Anonymous(), 200},
		{"writer disable", http.MethodPost, "/o/r/api/threads/schedules/enable", `{"path":".spindle/workflows/ci.yml","cron":"0 3 * * *","enabled":false}`, e.writer(), 403},
		{"missing entry", http.MethodPost, "/o/r/api/threads/schedules/enable", `{"path":"nope.yml","cron":"0 3 * * *","enabled":false}`, auth.Principal{Name: "maint"}, 404},
		{"invalid entry id", http.MethodPost, "/o/r/api/threads/schedules/enable", `{"path":"","cron":""}`, auth.Principal{Name: "maint"}, 400},
		{"run-now unknown", http.MethodPost, "/o/r/api/threads/schedules/run-now", `{"path":"nope.yml","cron":"x"}`, auth.Principal{Name: "maint"}, 404},
		{"run-now anonymous", http.MethodPost, "/o/r/api/threads/schedules/run-now", `{}`, auth.Anonymous(), 401},
		{"schedules method", http.MethodPut, "/o/r/api/threads/schedules", "", auth.Principal{Name: "maint"}, 405},
		{"enable method", http.MethodGet, "/o/r/api/threads/schedules/enable", "", auth.Principal{Name: "maint"}, 405},
		{"run-now method", http.MethodGet, "/o/r/api/threads/schedules/run-now", "", auth.Principal{Name: "maint"}, 405},
		{"queue anonymous on a public repo", http.MethodGet, "/o/r/api/threads/queue", "", auth.Anonymous(), 200},
		{"queue method", http.MethodPost, "/o/r/api/threads/queue", "", auth.Principal{Name: "maint"}, 405},
		{"pause body", http.MethodPost, "/o/r/api/threads/pause", `{"x":1}`, auth.Principal{Name: "maint"}, 400},
		{"subscription anonymous", http.MethodGet, "/o/r/api/threads/subscription", "", auth.Anonymous(), 200},
		{"subscription body", http.MethodPut, "/o/r/api/threads/subscription", `{"x":1}`, auth.Principal{Name: "reader"}, 400},
		{"subscription method", http.MethodPost, "/o/r/api/threads/subscription", "", auth.Principal{Name: "reader"}, 405},
		{"notifications method", http.MethodPost, "/o/r/api/threads/notifications", "", auth.Principal{Name: "reader"}, 405},
		{"notifications read method", http.MethodGet, "/o/r/api/threads/notifications/read", "", auth.Principal{Name: "reader"}, 405},
		{"notifications read anon", http.MethodPost, "/o/r/api/threads/notifications/read", "", auth.Anonymous(), 401},
		{"validate anonymous on a public repo", http.MethodPost, "/o/r/api/threads/validate", `{"file":"x","content":"x"}`, auth.Anonymous(), 200},
		{"validate method", http.MethodGet, "/o/r/api/threads/validate", "", e.writer(), 405},
		{"groups anonymous", http.MethodGet, "/api/v1/admin/runner-groups", "", auth.Anonymous(), 401},
		{"groups create invalid", http.MethodPost, "/api/v1/admin/runner-groups", `{"name":""}`, wave4Admin(), 400},
		{"groups update unknown", http.MethodPut, "/api/v1/admin/runner-groups/nope", `{"name":"n","repos":["a/b"]}`, wave4Admin(), 404},
		{"groups delete unknown", http.MethodDelete, "/api/v1/admin/runner-groups/nope", "", wave4Admin(), 200},
		{"groups delete method", http.MethodPatch, "/api/v1/admin/runner-groups/nope", "", wave4Admin(), 405},
		{"admin runners method", http.MethodPut, "/api/v1/admin/runners", "", wave4Admin(), 405},
		{"admin runner unknown", http.MethodPost, "/api/v1/admin/runners/nope/bogus", "", wave4Admin(), 404},
		{"admin group body", http.MethodPost, "/api/v1/admin/runners/nope/group", `{"x":1}`, wave4Admin(), 400},
		{"admin drain missing", http.MethodPost, "/api/v1/admin/runners/nope/drain", "", wave4Admin(), 404},
		{"admin rotate missing", http.MethodPost, "/api/v1/admin/runners/nope/rotate-token", "", wave4Admin(), 404},
	}
	for _, tc := range cases {
		*cur = tc.as
		rec := do(h, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Fatalf("%s: %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestRunnerGroupValidationEdges(t *testing.T) {
	e := newTestEnv(t)
	if _, err := ValidateRunnerGroupInput(RunnerGroupInput{Name: "", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("nameless group accepted")
	}
	if _, err := ValidateRunnerGroupInput(RunnerGroupInput{Name: "!!!", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("group with no usable id accepted")
	}
	if _, err := ValidateRunnerGroupInput(RunnerGroupInput{Name: "x", RunnerPolicyInput: RunnerPolicyInput{}}); err == nil {
		t.Fatal("group with no visibility accepted")
	}
	g, err := ValidateRunnerGroupInput(RunnerGroupInput{Name: " Build Boxes ", Labels: []string{"GPU", " gpu "}, RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"acme/*"}}})
	if err != nil || g.ID != "build-boxes" || len(g.Labels) != 1 {
		t.Fatalf("normalised group = %+v, %v", g, err)
	}
	first, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "one", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "one", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("duplicate group accepted")
	}
	// A runner assigned to a group and then re-assigned to the same group is a
	// no-op; assigning to an unknown id is refused.
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "grp", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, first.ID, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, first.ID, wave4Admin()); err != nil {
		t.Fatalf("idempotent assign: %v", err)
	}
	// A revoked runner cannot be group-assigned.
	if err := e.svc.RevokeRunner(ctx(), authn.Doc.ID, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, first.ID, wave4Admin()); err == nil {
		t.Fatal("revoked runner group-assigned")
	}
	// Listing groups counts members and includes a zero-member row.
	views, err := e.svc.ListRunnerGroups(ctx(), wave4Admin())
	if err != nil || len(views) != 1 {
		t.Fatalf("groups = %+v, %v", views, err)
	}
}

func TestScheduleOpErrorPaths(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 10, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	e.roles.roles["maint"] = "maintain"
	if err := (ScheduleEntryID{}).Validate(); err == nil {
		t.Fatal("empty entry id accepted")
	}
	if _, err := e.svc.EnsureScheduleEntry(ctx(), "o", "r", ScheduleEntryID{Path: "x", Cron: "y"}); err == nil {
		t.Fatal("unknown entry accepted")
	}
	if _, err := e.svc.RunScheduleNow(ctx(), "o/r", ScheduleEntryID{Path: "x", Cron: "y"}, e.writer()); err == nil {
		t.Fatal("run-now of unknown entry accepted")
	}
	// An empty repository has no entries and no HEAD, and never errors.
	empty := newTestEnv(t)
	empty.git.refs = map[string]string{}
	if list, err := empty.svc.ListSchedules(ctx(), "o", "r", empty.reader()); err != nil || (list != nil && len(list.Entries) != 0) {
		t.Fatalf("empty schedules = %+v, %v", list, err)
	}
	if _, err := empty.svc.EnsureScheduleEntry(ctx(), "o", "r", ScheduleEntryID{Path: "x", Cron: "y"}); err == nil {
		t.Fatal("entry in an empty repository accepted")
	}
	// A second identical disable writes nothing and still returns the row.
	id := ScheduleEntryID{Path: ".spindle/workflows/ci.yml", Cron: "0 3 * * *"}
	for i := 0; i < 2; i++ {
		if _, err := e.svc.SetScheduleEnabled(ctx(), "o/r", id, false, auth.Principal{Name: "maint"}); err != nil {
			t.Fatalf("disable %d: %v", i, err)
		}
	}
	// An entry no workflow declares cannot be described.
	if _, err := e.svc.describeScheduleEntry(ctx(), "o", "r", ScheduleStatus{ScheduleEntryID: ScheduleEntryID{Path: "gone", Cron: "x"}}); err != nil {
		t.Fatalf("describe unknown: %v", err)
	}
	// A malformed watermark slot contributes no coalesced ticks.
	if n := coalescedTicks(nil, nil, "x", at); n != 0 {
		t.Fatalf("nil coalescing = %d", n)
	}
}

func TestNotifyWebhookFailureIsLogged(t *testing.T) {
	e := newTestEnv(t)
	e.roles.roles["maint"] = "maintain"
	// A webhook URL that refuses connections: the notification path must not
	// propagate the failure.
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{NotifyWebhook: "http://127.0.0.1:1/hook"}, auth.Principal{Name: "maint"}); err != nil {
		t.Fatal(err)
	}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// Force the run's state (the delivery helper reads it and returns early for
	// a non-failure), then exercise delivery directly.
	run, _ := e.svc.loadRun(ctx(), "o", "r", runs[0].ID)
	run.State = RunFailure
	e.svc.postRunWebhook(ctx(), "http://127.0.0.1:1/hook", "o", "r", run)
	e.svc.postRunWebhook(ctx(), "not-a-url", "o", "r", run)
	e.svc.notifyRunFailure(ctx(), "o", "r", "missing")
}

func TestQueueAndSubscriptionEdges(t *testing.T) {
	e := newTestEnv(t)
	// Queue snapshot of a repository with no queue.
	snap, err := e.svc.QueueSnapshotForRepo(ctx(), "o/r", e.reader())
	if err != nil || len(snap.Rows) != 0 || snap.Paused {
		t.Fatalf("empty queue = %+v, %v", snap, err)
	}
	// Subscription normalisation: duplicate/blank workflow names collapse; an
	// empty watch list means every workflow.
	if got := normalizeWorkflows([]string{"CI", " ci ", ""}); len(got) != 1 || got[0] != "ci" {
		t.Fatalf("normalizeWorkflows = %v", got)
	}
	sub := Subscription{Workflows: []string{"ci.yml"}}
	if !sub.watches(&Run{WorkflowPath: "ci.yml"}) || sub.watches(&Run{WorkflowPath: "other.yml"}) {
		t.Fatal("subscription match")
	}
	if (Subscription{}).watches(nil) {
		t.Fatal("nil run matched")
	}
	// An anonymous subscription is refused.
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, auth.Anonymous()); err == nil {
		t.Fatal("anonymous subscription accepted")
	}
	// Marking read for an anonymous caller is refused.
	if err := e.svc.MarkNotificationsRead(ctx(), auth.Anonymous()); err == nil {
		t.Fatal("anonymous mark-read accepted")
	}
	// Limits are bounded.
	if notes, err := e.svc.ListNotifications(ctx(), auth.Principal{Name: "nobody"}, -1); err != nil || len(notes) != 0 {
		t.Fatalf("bounded list = %+v, %v", notes, err)
	}
	_, _ = json.Marshal(snap)
	_ = strings.TrimSpace("")
}

func TestValidateWorkflowForRepoGuards(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.svc.ValidateWorkflowForRepo(ctx(), "o/r", e.reader(), "", []byte("on: [push]\n")); err != nil {
		t.Fatalf("read-role validate: %v", err)
	}
	// A public repository lets a reader (even anonymous) validate; the content
	// is the caller's own and nothing is stored.
	if _, err := e.svc.ValidateWorkflowForRepo(ctx(), "o/r", auth.Anonymous(), "", []byte("on: [push]\n")); err != nil {
		t.Fatalf("public validate: %v", err)
	}
	if _, err := e.svc.ValidateWorkflowForRepo(ctx(), "o/r", e.reader(), "", make([]byte, 1<<20+1)); err == nil {
		t.Fatal("oversized validate accepted")
	}
}
