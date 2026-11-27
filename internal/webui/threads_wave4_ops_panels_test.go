package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// Wave-4 Threads panels: schedules, claim order, pause/notification switches,
// the watch toggle and the validate editor render from the service seam.
func TestThreadsWave4PanelsRender(t *testing.T) {
	bindChromeProjections(t)
	run := threadsFixtureRun()
	BindThreads(ThreadsDeps{
		List: func(context.Context, string, string, auth.Principal, string, int, threads.RunFilter) (*threads.RunPage, error) {
			return &threads.RunPage{Runs: []*threads.Run{run}}, nil
		},
		Workflows: func(context.Context, string, string, string, auth.Principal) (*threads.WorkflowList, error) {
			return &threads.WorkflowList{Commit: run.WorkflowCommit, Workflows: []*threads.WorkflowState{{
				Path: run.WorkflowPath, Name: run.Workflow, Digest: run.WorkflowDigest, Triggers: []string{"push"},
			}}}, nil
		},
		Settings: func(context.Context, string, string, auth.Principal) (*threads.SettingsView, error) {
			return &threads.SettingsView{CIPaused: true}, nil
		},
		ListSchedules: func(context.Context, string, string, auth.Principal) (*threads.ScheduleList, error) {
			return &threads.ScheduleList{Entries: []threads.ScheduleStatus{{
				ScheduleEntryID: threads.ScheduleEntryID{Path: run.WorkflowPath, Cron: "0 3 * * *"},
				NextFire:        "2026-11-21T03:00:00Z", LastFired: "2026-11-20T03:00:00Z", MissedTicks: 2, Valid: true,
			}}, WatermarkUpdatedAt: "2026-11-20T03:00:00Z"}, nil
		},
		Queue: func(context.Context, string, auth.Principal) (*threads.QueueSnapshot, error) {
			return &threads.QueueSnapshot{Paused: true, Rows: []threads.QueueRow{{
				RunID: run.ID, JobID: "j-build", Priority: 5, Labels: []string{"linux"}, State: threads.QueueQueued,
			}}}, nil
		},
		Subscription: func(context.Context, string, auth.Principal) (*threads.SubscriptionView, error) {
			return &threads.SubscriptionView{Repo: "acme/widgets", Subscribed: true, Workflows: []string{"*"}}, nil
		},
		Notifications: func(context.Context, auth.Principal, int) ([]threads.Notification, error) {
			return []threads.Notification{{ID: run.ID, Title: "CI failed on main", Link: "/acme/widgets/threads/" + run.ID, State: threads.RunFailure}}, nil
		},
	})
	t.Cleanup(func() { BindThreads(ThreadsDeps{}) })

	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "ada", Write: true}
	blob, _ := json.Marshal(&threads.WorkflowValidation{
		File: "ci.yml", OK: false,
		Issues: []threads.WorkflowIssue{{File: "ci.yml", Key: "jobs.build.bogus", Line: 5, Message: "field bogus not found"}},
	})
	rec := f.get("/acme/widgets/threads?validate=" + url.QueryEscape(string(blob)))
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec,
		"Schedules", "0 3 * * *", "2026-11-21T03:00:00Z", "2 missed",
		"Claim order", "priority descending", "CI paused",
		"unwatch this repository", "CI failed on main", "/acme/widgets/threads/"+run.ID,
		"Validate a workflow", "jobs.build.bogus", "field bogus not found",
		"pause CI", "Failure notification webhook",
	)
}
