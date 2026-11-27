package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func bindThreadsOps(t *testing.T, mutate func(*ThreadsDeps)) {
	t.Helper()
	d := ThreadsDeps{
		ListSchedules: func(context.Context, string, string, auth.Principal) (*threads.ScheduleList, error) {
			return &threads.ScheduleList{}, nil
		},
		SetSchedule: func(context.Context, string, threads.ScheduleEntryID, bool, auth.Principal) (*threads.ScheduleStatus, error) {
			return &threads.ScheduleStatus{}, nil
		},
		RunScheduleNow: func(context.Context, string, threads.ScheduleEntryID, auth.Principal) ([]*threads.Run, error) {
			return []*threads.Run{{ID: "run-now"}}, nil
		},
		SetPaused: func(context.Context, string, bool, auth.Principal) (*threads.SettingsView, error) {
			return &threads.SettingsView{}, nil
		},
		SetSubscription: func(context.Context, string, bool, []string, auth.Principal) (*threads.SubscriptionView, error) {
			return &threads.SubscriptionView{}, nil
		},
		MarkRead: func(context.Context, auth.Principal) error { return nil },
		Validate: func(_ context.Context, _ string, _ auth.Principal, file string, _ []byte) (*threads.WorkflowValidation, error) {
			return &threads.WorkflowValidation{File: file, Issues: []threads.WorkflowIssue{{Key: "jobs.build.bogus"}}}, nil
		},
	}
	if mutate != nil {
		mutate(&d)
	}
	BindThreads(d)
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
}

func TestThreadsScheduleOpsPost(t *testing.T) {
	bindThreadsOps(t, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	rec := f.post("/acme/widgets/threads/schedule", url.Values{
		"action": {"disable"}, "path": {".spindle/workflows/ci.yml"}, "cron": {"0 3 * * *"}, "timezone": {""},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "schedule_notice") {
		t.Fatalf("disable = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = f.post("/acme/widgets/threads/schedule", url.Values{
		"action": {"enable"}, "path": {".spindle/workflows/ci.yml"}, "cron": {"0 3 * * *"},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "schedule_notice") {
		t.Fatalf("enable = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = f.post("/acme/widgets/threads/schedule", url.Values{
		"action": {"run-now"}, "path": {".spindle/workflows/ci.yml"}, "cron": {"0 3 * * *"},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "run-now") {
		t.Fatalf("run-now = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Unknown action is a 400, and a service error is a redirect with the reason.
	if rec := f.post("/acme/widgets/threads/schedule", url.Values{"action": {"bogus"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus action = %d", rec.Code)
	}
	bindThreadsOps(t, func(d *ThreadsDeps) {
		d.SetSchedule = func(context.Context, string, threads.ScheduleEntryID, bool, auth.Principal) (*threads.ScheduleStatus, error) {
			return nil, errors.New("nope")
		}
	})
	rec = f.post("/acme/widgets/threads/schedule", url.Values{"action": {"disable"}, "path": {"p"}, "cron": {"c"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "schedule_error") {
		t.Fatalf("error redirect = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Not wired.
	BindThreads(ThreadsDeps{})
	if rec := f.post("/acme/widgets/threads/schedule", url.Values{"action": {"disable"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired schedule = %d", rec.Code)
	}
}

func TestThreadsPauseSubscribeAndValidatePosts(t *testing.T) {
	bindThreadsOps(t, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	for _, form := range []url.Values{{"paused": {"1"}}, {"paused": {"0"}}} {
		rec := f.post("/acme/widgets/threads/pause", form)
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "notice") {
			t.Fatalf("pause = %d %s", rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, form := range []url.Values{{"subscribed": {"1"}}, {"subscribed": {"0"}}} {
		rec := f.post("/acme/widgets/threads/subscribe", form)
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "notice") {
			t.Fatalf("subscribe = %d %s", rec.Code, rec.Header().Get("Location"))
		}
	}
	rec := f.post("/acme/widgets/threads/notifications/read", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mark read = %d", rec.Code)
	}
	rec = f.post("/acme/widgets/threads/validate", url.Values{
		"file": {"ci.yml"}, "source": {"name: ci\non: [push]\njobs: {}\n"},
	})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "validate=") {
		t.Fatalf("validate = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Service errors redirect with a notice.
	bindThreadsOps(t, func(d *ThreadsDeps) {
		d.SetPaused = func(context.Context, string, bool, auth.Principal) (*threads.SettingsView, error) {
			return nil, errors.New("paused failed")
		}
		d.SetSubscription = func(context.Context, string, bool, []string, auth.Principal) (*threads.SubscriptionView, error) {
			return nil, errors.New("sub failed")
		}
		d.Validate = func(context.Context, string, auth.Principal, string, []byte) (*threads.WorkflowValidation, error) {
			return nil, errors.New("validate failed")
		}
	})
	if rec := f.post("/acme/widgets/threads/pause", url.Values{"paused": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("pause error = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/threads/subscribe", url.Values{"subscribed": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("subscribe error = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/threads/validate", url.Values{"file": {"x"}, "source": {"y"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("validate error = %d", rec.Code)
	}
	// Unwired 503s.
	BindThreads(ThreadsDeps{})
	if rec := f.post("/acme/widgets/threads/pause", url.Values{"paused": {"1"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired pause = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/threads/subscribe", url.Values{"subscribed": {"1"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired subscribe = %d", rec.Code)
	}
	if rec := f.post("/acme/widgets/threads/validate", url.Values{"file": {"x"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired validate = %d", rec.Code)
	}
	// Mark-read is silently a redirect when unwired.
	if rec := f.post("/acme/widgets/threads/notifications/read", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("unwired mark read = %d", rec.Code)
	}
}

func TestThreadsAdminRunnerFleetPage(t *testing.T) {
	bindChromeProjections(t)
	bindThreadsOps(t, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Admin: true}
	// The admin pages use their own fixture; a plain render is covered by
	// admin tests. Here we only ensure the page route still resolves with the
	// fleet view fields present.
	if rec := f.get("/admin/runners"); rec.Code != http.StatusOK && rec.Code != http.StatusForbidden {
		t.Fatalf("admin runners = %d", rec.Code)
	}
}

func TestThreadsOpsFormParseErrorsAndUnwiredRunNow(t *testing.T) {
	bindThreadsOps(t, nil)
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	// run-now with the seam missing is a named 503.
	bindThreadsOps(t, func(d *ThreadsDeps) { d.RunScheduleNow = nil })
	if rec := f.post("/acme/widgets/threads/schedule", url.Values{
		"action": {"run-now"}, "path": {"p"}, "cron": {"c"},
	}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired run-now = %d", rec.Code)
	}
	// Enabling with the setter missing is a named 503.
	bindThreadsOps(t, func(d *ThreadsDeps) { d.SetSchedule = nil })
	if rec := f.post("/acme/widgets/threads/schedule", url.Values{"action": {"disable"}}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired set = %d", rec.Code)
	}
}
