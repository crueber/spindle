package threads

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func corruptKey(t *testing.T, e *testEnv, key string) {
	t.Helper()
	_ = e.store.Delete(ctx(), key, "")
	if _, err := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatalf("corrupt %s: %v", key, err)
	}
}

func TestHTTPWave4HandlerAuthErrors(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = wave4Admin()
	h.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad session"}
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/o/r/api/threads/schedules", ""},
		{http.MethodPost, "/o/r/api/threads/schedules/enable", `{"path":"a","cron":"b","enabled":false}`},
		{http.MethodPost, "/o/r/api/threads/schedules/run-now", `{"path":"a","cron":"b"}`},
		{http.MethodPost, "/o/r/api/threads/pause", `{"paused":true}`},
		{http.MethodGet, "/o/r/api/threads/queue", ""},
		{http.MethodGet, "/o/r/api/threads/notifications", ""},
		{http.MethodPost, "/o/r/api/threads/notifications/read", ""},
		{http.MethodGet, "/o/r/api/threads/subscription", ""},
		{http.MethodPut, "/o/r/api/threads/subscription", `{"subscribed":true}`},
		{http.MethodPost, "/o/r/api/threads/validate", `{"file":"x","content":"y"}`},
		{http.MethodGet, "/api/v1/admin/runner-groups", ""},
		{http.MethodPost, "/api/v1/admin/runners/abc/drain", ""},
	} {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != 401 {
			t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
	_ = e
}

func TestHTTPWave4ServiceErrorBranches(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint"}
	e.roles.roles["maint"] = "maintain"
	e.git.refs["HEAD"] = testSHA
	corruptKey(t, e, ScheduleOpsKey("o", "r"))
	if rec := do(h, http.MethodGet, "/o/r/api/threads/schedules", ""); rec.Code != 500 {
		t.Fatalf("corrupt schedules = %d", rec.Code)
	}
	// Subscription store failure.
	corruptKey(t, e, SubscriptionsKey("o", "r"))
	if rec := do(h, http.MethodGet, "/o/r/api/threads/subscription", ""); rec.Code != 500 {
		t.Fatalf("corrupt subscription = %d", rec.Code)
	}
	*cur = auth.Principal{Name: "reader"}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/subscription", `{"subscribed":true}`); rec.Code != 500 {
		t.Fatalf("corrupt subscription write = %d", rec.Code)
	}
	// Queue head corruption.
	corruptKey(t, e, QueueHeadKey("o", "r"))
	if rec := do(h, http.MethodGet, "/o/r/api/threads/queue", ""); rec.Code != 500 {
		t.Fatalf("corrupt queue = %d", rec.Code)
	}
	// Bad JSON bodies on the group and self routes.
	*cur = wave4Admin()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/admin/runner-groups", `{bad`},
		{http.MethodPut, "/api/v1/admin/runner-groups/x", `{bad`},
		{http.MethodPost, "/api/v1/admin/runners/x/group", `{bad`},
	} {
		if rec := do(h, tc.method, tc.path, tc.body); rec.Code != 400 {
			t.Fatalf("%s %s = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}
	// Fleet verb failures that are not the already-covered drain path.
	for _, verb := range []string{"resume", "rotate-token", "deregister"} {
		if rec := do(h, http.MethodPost, "/api/v1/admin/runners/nope/"+verb, ""); rec.Code != 404 {
			t.Fatalf("%s missing = %d", verb, rec.Code)
		}
	}
	// Group verbs with an unknown id and a duplicate create.
	if rec := do(h, http.MethodDelete, "/api/v1/admin/runner-groups/nope", ""); rec.Code != 200 {
		t.Fatalf("delete unknown = %d", rec.Code)
	}
	body := `{"name":"dupe","repos":["a/b"]}`
	if rec := do(h, http.MethodPost, "/api/v1/admin/runner-groups", body); rec.Code != 201 {
		t.Fatalf("create dupe = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runner-groups", body); rec.Code != 409 {
		t.Fatalf("duplicate create = %d", rec.Code)
	}
}

func TestRunnerGroupStoreEdges(t *testing.T) {
	e := newTestEnv(t)
	// Corrupt group record: load and list both surface a corrupt error.
	corruptKey(t, e, RunnerGroupKey("bad"))
	if _, err := e.svc.loadRunnerGroup(ctx(), "bad"); err == nil {
		t.Fatal("corrupt group loaded")
	}
	if _, err := e.svc.ListRunnerGroups(ctx(), wave4Admin()); err == nil {
		t.Fatal("corrupt group list succeeded")
	}
	if err := e.svc.DeleteRunnerGroup(ctx(), wave4Admin(), "bad"); err != nil {
		t.Fatalf("delete corrupt: %v", err)
	}
	// Non-admin guards on every mutation.
	if _, err := e.svc.UpdateRunnerGroup(ctx(), e.reader(), "x", RunnerGroupInput{}); err == nil {
		t.Fatal("reader updated a group")
	}
	if err := e.svc.DeleteRunnerGroup(ctx(), e.reader(), "x"); err == nil {
		t.Fatal("reader deleted a group")
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), "x", "", e.reader()); err == nil {
		t.Fatal("reader assigned a group")
	}
	// Invalid ids are not-found, never a silent success.
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), "!!", RunnerGroupInput{Name: "x", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("invalid update id accepted")
	}
	if err := e.svc.DeleteRunnerGroup(ctx(), wave4Admin(), "!!"); err == nil {
		t.Fatal("invalid delete id accepted")
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), "!!", "", wave4Admin()); err == nil {
		t.Fatal("invalid runner id accepted")
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), "abc", "!!", wave4Admin()); err == nil {
		t.Fatal("invalid group id accepted")
	}
	// The group cap.
	for i := 0; i < MaxRunnerGroups; i++ {
		_, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{
			Name: "g" + strconv.Itoa(i), RunnerPolicyInput: RunnerPolicyInput{AllowAll: true},
		})
		if err != nil {
			t.Fatalf("group %d: %v", i, err)
		}
	}
	if _, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{Name: "over", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("group cap not enforced")
	}
}

func TestScheduleOpMoreEdges(t *testing.T) {
	at := time.Date(2026, 11, 21, 3, 0, 10, 0, time.UTC)
	e, _ := scheduleEnv(t, at)
	e.roles.roles["maint"] = "maintain"
	e.roles.public = false
	// A private repository refuses an anonymous read.
	if _, err := e.svc.ListSchedules(ctx(), "o", "r", auth.Anonymous()); err == nil {
		t.Fatal("anonymous schedule read on a private repository")
	}
	e.roles.public = true
	// A repository with no git backend lists nothing and cannot resolve entries.
	noGit := newTestEnv(t)
	noGit.svc.Git = nil
	if list, err := noGit.svc.ListSchedules(ctx(), "o", "r", noGit.reader()); err != nil || list == nil {
		t.Fatalf("no-git schedules = %+v, %v", list, err)
	}
	if _, err := noGit.svc.EnsureScheduleEntry(ctx(), "o", "r", ScheduleEntryID{Path: "a", Cron: "b"}); err == nil {
		t.Fatal("no-git ensure succeeded")
	}
	// Two entries exercise the sort comparator (different path and different cron).
	e.git.add(testSHA, ".spindle/workflows/ci2.yml", strings.Replace(scheduleFixtureYAML, "0 3 * * *", "30 4 * * *", 1))
	if list, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err != nil || len(list.Entries) != 2 {
		t.Fatalf("two schedules = %+v, %v", list, err)
	}
	// Discovery and ref failures surface.
	e.git.listErr = errors.New("boom")
	if _, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err == nil {
		t.Fatal("discovery failure swallowed")
	}
	e.git.listErr = nil
	// Corrupt override and watermark documents.
	corruptKey(t, e, ScheduleOpsKey("o", "r"))
	if _, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err == nil {
		t.Fatal("corrupt schedule overrides swallowed")
	}
	e.store.Delete(ctx(), ScheduleOpsKey("o", "r"), "")
	corruptKey(t, e, ScheduleKey("o", "r"))
	if _, err := e.svc.ListSchedules(ctx(), "o", "r", e.reader()); err == nil {
		t.Fatal("corrupt watermark swallowed")
	}
	// A malformed slot is not a coalesced tick.
	if n := coalescedTicks(nil, nil, "2026-13-99T99:99", at); n != 0 {
		t.Fatalf("malformed slot = %d", n)
	}
}

const scheduleFixtureYAML = `
name: Scheduled
on:
  schedule:
    - cron: "0 3 * * *"
jobs:
  build:
    runs-on: linux
    steps:
      - run: echo hi
`
