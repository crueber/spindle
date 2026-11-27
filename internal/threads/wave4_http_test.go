package threads

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// Wave-4 HTTP surface: schedules, pause, queue order, notifications,
// subscriptions, validation, runner groups and the fleet verbs (admin and
// runner-authenticated).

func w4DoAuth(h *Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestHTTPWave4SchedulesPauseAndQueue(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "maint"}
	e.roles.roles["maint"] = "maintain"
	e.git.refs["HEAD"] = testSHA

	if rec := do(h, http.MethodGet, "/o/r/api/threads/schedules", ""); rec.Code != 200 {
		t.Fatalf("schedules = %d: %s", rec.Code, rec.Body.String())
	}
	body := `{"path":".spindle/workflows/ci.yml","cron":"0 3 * * *","timezone":"","enabled":false}`
	if rec := do(h, http.MethodPost, "/o/r/api/threads/schedules/enable", body); rec.Code != 200 {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/schedules/run-now", `{"path":".spindle/workflows/ci.yml","cron":"0 3 * * *"}`); rec.Code != 201 {
		t.Fatalf("run-now = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/pause", `{"paused":true}`); rec.Code != 200 {
		t.Fatalf("pause = %d: %s", rec.Code, rec.Body.String())
	}
	rec := do(h, http.MethodGet, "/o/r/api/threads/queue", "")
	if rec.Code != 200 {
		t.Fatalf("queue = %d: %s", rec.Code, rec.Body.String())
	}
	var snap QueueSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || !snap.Paused {
		t.Fatalf("queue = %+v, %v", snap, err)
	}
	*cur = e.writer()
	if rec := do(h, http.MethodPost, "/o/r/api/threads/pause", `{"paused":false}`); rec.Code != 403 {
		t.Fatalf("writer pause = %d, want 403", rec.Code)
	}
	// Method/body errors.
	if rec := do(h, http.MethodGet, "/o/r/api/threads/pause", ""); rec.Code != 405 {
		t.Fatalf("GET pause = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/schedules/enable", `{"nope":1}`); rec.Code != 400 {
		t.Fatalf("bad enable body = %d", rec.Code)
	}
}

func TestHTTPWave4NotificationsAndSubscription(t *testing.T) {
	_, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "reader"}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/subscription", `{"subscribed":true}`); rec.Code != 200 {
		t.Fatalf("subscribe = %d: %s", rec.Code, rec.Body.String())
	}
	rec := do(h, http.MethodGet, "/o/r/api/threads/subscription", "")
	if rec.Code != 200 {
		t.Fatalf("get subscription = %d", rec.Code)
	}
	var sub SubscriptionView
	if json.Unmarshal(rec.Body.Bytes(), &sub) != nil || !sub.Subscribed {
		t.Fatalf("subscription = %+v", sub)
	}
	if rec := do(h, http.MethodGet, "/o/r/api/threads/notifications?n=10", ""); rec.Code != 200 {
		t.Fatalf("notifications = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/notifications/read", ""); rec.Code != 200 {
		t.Fatalf("mark read = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/o/r/api/threads/subscription", `{"subscribed":false}`); rec.Code != 200 {
		t.Fatalf("unsubscribe = %d", rec.Code)
	}
	// Anonymous has no inbox.
	*cur = auth.Anonymous()
	if rec := do(h, http.MethodGet, "/o/r/api/threads/notifications", ""); rec.Code != 401 {
		t.Fatalf("anonymous notifications = %d", rec.Code)
	}
}

func TestHTTPWave4Validate(t *testing.T) {
	_, h, cur := newHTTP(t)
	*cur = auth.Principal{Name: "writer"}
	body := `{"file":"ci.yml","content":"name: ci\non: [push]\njobs:\n  build:\n    bogus: 1\n"}`
	rec := do(h, http.MethodPost, "/o/r/api/threads/validate", body)
	if rec.Code != 200 {
		t.Fatalf("validate = %d: %s", rec.Code, rec.Body.String())
	}
	var res WorkflowValidation
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.OK || len(res.Issues) == 0 {
		t.Fatalf("validation = %+v, %v", res, err)
	}
	if !strings.Contains(res.Issues[0].Key, "jobs.build") {
		t.Fatalf("key = %q", res.Issues[0].Key)
	}
	// Too large.
	big := strings.Repeat("x", 1<<20+1)
	if rec := do(h, http.MethodPost, "/o/r/api/threads/validate",
		`{"file":"ci.yml","content":"`+big+`"}`); rec.Code != 422 {
		t.Fatalf("oversized validate = %d", rec.Code)
	}
}

func TestHTTPWave4RunnerGroupsAndFleet(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = wave4Admin()
	// Create, list, update, assign, delete.
	rec := do(h, http.MethodPost, "/api/v1/admin/runner-groups",
		`{"name":"build boxes","labels":["gpu"],"repos":["acme/*"]}`)
	if rec.Code != 201 {
		t.Fatalf("create group = %d: %s", rec.Code, rec.Body.String())
	}
	var group RunnerGroupView
	if err := json.Unmarshal(rec.Body.Bytes(), &group); err != nil || group.ID != "build-boxes" {
		t.Fatalf("group = %+v, %v", group, err)
	}
	if rec := do(h, http.MethodGet, "/api/v1/admin/runner-groups", ""); rec.Code != 200 {
		t.Fatalf("list groups = %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/api/v1/admin/runner-groups/"+group.ID,
		`{"name":"build boxes","labels":["gpu","cuda"],"repos":["acme/*"]}`); rec.Code != 200 {
		t.Fatalf("update group = %d: %s", rec.Code, rec.Body.String())
	}

	id, _, _ := wave4Runner(t, e,
		MintInput{Name: "fleet-1", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/"+id+"/group", `{"group":"`+group.ID+`"}`); rec.Code != 200 {
		t.Fatalf("assign group = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/"+id+"/drain", ""); rec.Code != 200 {
		t.Fatalf("admin drain = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/"+id+"/resume", ""); rec.Code != 200 {
		t.Fatalf("admin resume = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/"+id+"/rotate-token", ""); rec.Code != 200 {
		t.Fatalf("admin rotate = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/"+id+"/deregister", ""); rec.Code != 200 {
		t.Fatalf("admin deregister = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, http.MethodDelete, "/api/v1/admin/runner-groups/"+group.ID, ""); rec.Code != 200 {
		t.Fatalf("delete group = %d", rec.Code)
	}
	// Non-admin is refused before any write.
	*cur = e.writer()
	if rec := do(h, http.MethodGet, "/api/v1/admin/runner-groups", ""); rec.Code != 403 {
		t.Fatalf("writer groups = %d", rec.Code)
	}
}

func TestHTTPWave4RunnerSelfVerbs(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = wave4Admin()
	_, token, _ := wave4Runner(t, e,
		MintInput{Name: "self-1", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	// The runner-authenticated path ignores the request principal and resolves
	// the bearer itself.
	*cur = auth.Anonymous()
	for _, path := range []string{"drain", "resume", "rotate", "deregister"} {
		rec := w4DoAuth(h, http.MethodPost, "/api/v1/threads/runners/"+path, "", token)
		if rec.Code != 200 {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := w4DoAuth(h, http.MethodPost, "/api/v1/threads/runners/drain", "", token); rec.Code != 401 {
		t.Fatalf("retired credential = %d, want 401", rec.Code)
	}
	if rec := w4DoAuth(h, http.MethodGet, "/api/v1/threads/runners/drain", "", token); rec.Code != 405 {
		t.Fatalf("GET drain = %d", rec.Code)
	}
}
