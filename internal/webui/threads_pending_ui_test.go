package webui

// threads_pending_ui_test.go — the pending-deployment and concurrency-wait
// surfaces (PAR-135/PAR-152) and the environment protection form (PAR-131), as
// rendered. The states are text labels (never colour-only), the cancel control
// is a real form, and the settings tab posts every protection rule.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// TestThreadsListPendingBadges pins the run-list badges: a held deployment names
// its environment, a concurrency wait names its group, and a writer gets a cancel
// control on a live run.
func TestThreadsListPendingBadges(t *testing.T) {
	bindChromeProjections(t)
	held := threadsFixtureRun()
	held.ID = "0000000000000000001-aaaaaa"
	held.State = threads.RunPendingApproval
	held.ApprovalKind = threads.ApprovalEnvironment
	held.Environments = []string{"production"}
	waiting := threadsFixtureRun()
	waiting.ID = "0000000000000000002-bbbbbb"
	waiting.State = threads.RunQueued
	waiting.PendingGroup = "deploy-main"
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{held, waiting}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	for _, want := range []string{
		"awaiting deployment", "production",
		"group", "deploy-main", "waiting",
		`action="/acme/widgets/threads/` + held.ID + `/cancel"`,
		"cancel",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run list lacks %q\n%s", want, body)
		}
	}
	// A reader gets no cancel form (the service would 403 anyway).
	f.principal = auth.Principal{Name: "reader"}
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{held}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "read", nil },
	})
	rec = f.get("/acme/widgets/threads")
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(bodyText(rec), "/cancel") {
		t.Fatal("a read principal was offered a cancel form")
	}
}

// TestThreadRunPageWaitTimerAndApprovals pins the held deployment's detail: who
// approved so far, who is still awaited, the wait-timer deadline, the
// self-review rule and a concurrency wait's named group.
func TestThreadRunPageWaitTimerAndApprovals(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	run.State = threads.RunPendingApproval
	run.ApprovalKind = threads.ApprovalEnvironment
	run.Environments = []string{"production"}
	run.Actor = "ada"
	until := "2026-11-20T12:00:00Z"
	job := threadsFixtureJob(run)
	job.State = threads.JobQueued
	job.Deployment = &threads.Deployment{
		Environment: "production", State: threads.DeploymentPending,
		RequiredApprovers: []string{"ada", "grace"}, Approvals: []string{"ada"},
		WaitUntil: &until, PreventSelfReview: true, TriggeredBy: "ada",
	}
	approval := threads.ApprovalView{
		Pending: true, Kind: threads.ApprovalEnvironment, Environments: []string{"production"},
		Reviewers: []string{"ada", "grace"}, Approvals: []string{"ada"}, WaitingFor: []string{"grace"},
		WaitUntil: &until, PreventSelfReview: true, CanDecide: true,
	}
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run, Jobs: []*threads.Job{job}, Approval: &approval}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	f.view.summary = busySummary()
	rec := f.get("/acme/widgets/threads/" + run.ID)
	wantStatus(t, rec, http.StatusOK)
	body := bodyText(rec)
	for _, want := range []string{
		"Approved so far", "ada", "Still awaiting", "grace",
		"wait timer", "2026-11-20T12:00:00Z",
		"prevents self-review", "ada",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("run page lacks %q\n%s", want, body)
		}
	}

	// A cross-run hold names its own rule, never the fork wording (PAR-098).
	cross := threadsFixtureRun()
	cross.State = threads.RunPendingApproval
	cross.ApprovalKind = threads.ApprovalCrossRun
	crossApproval := threads.ApprovalView{Pending: true, Kind: threads.ApprovalCrossRun, CanDecide: true}
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: cross, Jobs: []*threads.Job{threadsFixtureJob(cross)}, Approval: &crossApproval}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	rec = f.get("/acme/widgets/threads/" + cross.ID)
	wantStatus(t, rec, http.StatusOK)
	crossBody := bodyText(rec)
	for _, want := range []string{"downloads another run's artifacts", "run-id", "audited"} {
		if !strings.Contains(crossBody, want) {
			t.Fatalf("cross-run hold page lacks %q\n%s", want, crossBody)
		}
	}
	if strings.Contains(crossBody, "first-time contributor") {
		t.Fatalf("a cross-run hold rendered the fork wording\n%s", crossBody)
	}

	// A concurrency wait renders the group banner instead of an approval one.
	run2 := threadsFixtureRun()
	run2.State = threads.RunQueued
	run2.PendingGroup = "deploy-main"
	run2.Environments = nil
	run2.ApprovalKind = ""
	BindThreads(ThreadsDeps{
		Get: func(context.Context, string, string, string, auth.Principal) (*threads.RunView, error) {
			return &threads.RunView{Run: run2, Jobs: []*threads.Job{threadsFixtureJob(run2)}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Workflows: []*threads.WorkflowState{}}, nil
		},
		Role: func(context.Context, string, string, auth.Principal) (string, error) { return "write", nil },
	})
	rec = f.get("/acme/widgets/threads/" + run2.ID)
	wantStatus(t, rec, http.StatusOK)
	if body := bodyText(rec); !strings.Contains(body, "waiting for concurrency group") || !strings.Contains(body, "deploy-main") {
		t.Fatalf("a concurrency wait shows no named group\n%s", body)
	}
}

// TestEnvironmentTabRendersProtectionRules pins the settings form and rows: the
// four rule controls render, a stored record shows its rules in text, and a save
// carries all of them to the service.
func TestEnvironmentTabRendersProtectionRules(t *testing.T) {
	fake := newFakeEnvThreads()
	fake.envs = []threads.EnvironmentView{{
		Name: "production", RequiredReviewers: []string{"ada"},
		TeamReviewers: []string{"acme/releasers"}, WaitTimerSeconds: 600,
		PreventSelfReview: true, DeploymentBranches: []string{"main", "release/*"},
		UpdatedAt: "2026-11-23T01:00:00Z",
	}}
	s := envSettingsServer(t, "maintain", fake)

	rec := settingsDo(t, s, http.MethodGet, "/acme/monorepo/settings/environments", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="environment_teams"`,
		`name="environment_wait_minutes"`,
		`name="environment_prevent_self_review"`,
		`name="environment_branches"`,
		"10 min wait timer", "self-review blocked", "acme/releasers", "main, release/*",
		"team reviewers",
	} {
		settingsMustContain(t, body, want, "protection rules tab")
	}

	// A save carries every rule through the seam.
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments", url.Values{
		"environment_name":                {"staging"},
		"environment_reviewers":           {"ada, grace"},
		"environment_teams":               {"acme/releasers"},
		"environment_wait_minutes":        {"15"},
		"environment_prevent_self_review": {"on"},
		"environment_branches":            {"main, release/*"},
	}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.last.WaitTimerSeconds != 15*60 || !fake.last.PreventSelfReview ||
		strings.Join(fake.last.TeamReviewers, ",") != "acme/releasers" ||
		strings.Join(fake.last.DeploymentBranches, ",") != "main,release/*" {
		t.Fatalf("posted protection rules = %+v", fake.last)
	}
	// A bad timer is a form error, not a silent zero.
	rec = settingsDo(t, s, http.MethodPost, "/acme/monorepo/settings/environments", url.Values{
		"environment_name": {"staging"}, "environment_wait_minutes": {"soon"},
	}, nil)
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("a bad wait timer was accepted: %d", rec.Code)
	}
}
