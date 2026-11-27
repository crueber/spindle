// cli_threads_ops.go — Wave-4 Threads CLI parity (PAR-154/PAR-155/PAR-157/
// PAR-196/PAR-198).
//
// These verbs are ordinary API clients (the repo-scoped JSON surface); each
// service call re-checks the role, so the CLI adds no authority. `explain` is
// operator-local (a file on this host, no server).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// runThreadsWatch follows one run to its conclusion. The exit code matches the
// run: 0 success, 1 failure, 130 cancelled, 3 unavailable/timeout (the API
// client's own mapping for an error answer).
func runThreadsWatch(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads watch")
	f.add(fs)
	var interval int
	fs.IntVar(&interval, "interval", 2, "poll interval in seconds")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, run, ok := repoAndWord(c, pos, "threads watch OWNER/REPO RUN")
	if !ok {
		return exitArg
	}
	if interval < 1 {
		interval = 1
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	last := ""
	for {
		raw, cerr := a.call(ctx, "GET", threadsPath(owner, repo, run), nil, nil)
		if cerr != nil {
			return a.fail(cerr)
		}
		var view struct {
			Run struct {
				State      string `json:"state"`
				Conclusion string `json:"conclusion"`
				Workflow   string `json:"workflow"`
			} `json:"run"`
		}
		if jerr := json.Unmarshal(raw, &view); jerr != nil {
			return a.emitJSON(raw)
		}
		state := view.Run.State
		if view.Run.Conclusion != "" {
			state = view.Run.Conclusion
		}
		if state != last && !f.json {
			fmt.Fprintf(c.stdout(), "%s: %s %s\n", time.Now().UTC().Format("15:04:05"), run, state)
			last = state
		}
		switch state {
		case "success":
			if f.json {
				return a.emitJSON(raw)
			}
			fmt.Fprintf(c.stdout(), "Run %s succeeded.\n", run)
			return exitOK
		case "failure":
			if f.json {
				_ = a.emitJSON(raw)
			}
			fmt.Fprintf(c.stdout(), "Run %s failed.\n", run)
			return exitErr
		case "cancelled":
			fmt.Fprintf(c.stdout(), "Run %s was cancelled.\n", run)
			return 130
		case "skipped":
			fmt.Fprintf(c.stdout(), "Run %s was skipped.\n", run)
			return exitOK
		}
		select {
		case <-ctx.Done():
			return exitErr
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

// runThreadsJobs lists one run's jobs (the API's jobs array).
func runThreadsJobs(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads jobs")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, run, ok := repoAndWord(c, pos, "threads jobs OWNER/REPO RUN")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "GET", threadsPath(owner, repo, run), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var view struct {
		Jobs []struct {
			ID       string `json:"id"`
			Key      string `json:"key"`
			State    string `json:"state"`
			RunsOn   string `json:"runs_on"`
			Runner   string `json:"runner"`
			Priority int    `json:"priority"`
			Reason   string `json:"reason"`
			Queued   string `json:"queue_reason"`
			Attempt  int    `json:"attempt"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "JOB", "STATE", "RUNS-ON", "PRIORITY", "ATTEMPT", "WHY")
	for _, j := range view.Jobs {
		why := j.Reason
		if why == "" {
			why = j.Queued
		}
		t.row(j.Key, j.State, j.RunsOn, strconv.Itoa(j.Priority), strconv.Itoa(j.Attempt), why)
	}
	t.flush()
	return exitOK
}

func runThreadsScheduleList(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads schedule list")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "threads schedule list OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "GET", threadsPath(owner, repo, "schedules"), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var list threads.ScheduleList
	if err := json.Unmarshal(raw, &list); err != nil {
		return a.emitJSON(raw)
	}
	t := newTable(c.stdout(), "FILE", "CRON", "TZ", "NEXT FIRE", "LAST FIRED", "MISSED", "STATE")
	for _, e := range list.Entries {
		state := "enabled"
		if e.Disabled {
			state = "disabled"
		}
		if !e.Valid {
			state = "invalid"
		}
		t.row(e.Path, e.Cron, e.Timezone, e.NextFire, e.LastFired, strconv.Itoa(e.MissedTicks), state)
	}
	t.flush()
	if list.WatermarkUpdatedAt != "" {
		fmt.Fprintf(c.stdout(), "\nwatermark updated: %s\n", list.WatermarkUpdatedAt)
	}
	return exitOK
}

func runThreadsScheduleEnable(enabled bool) func(context.Context, *cli, []string) int {
	return func(ctx context.Context, c *cli, args []string) int {
		verb := "schedule disable"
		if enabled {
			verb = "schedule enable"
		}
		f := &apiFlags{}
		fs := newAPIFlagSet(c, "threads "+verb)
		f.add(fs)
		var id threads.ScheduleEntryID
		fs.StringVar(&id.Path, "path", "", "workflow file path (.spindle/workflows/x.yml)")
		fs.StringVar(&id.Cron, "cron", "", "the cron expression as written")
		fs.StringVar(&id.Timezone, "timezone", "", "the entry timezone (empty = UTC)")
		pos, exit := parseAPI(fs, args)
		if exit != parseContinue {
			return exit
		}
		owner, repo, ok := oneRepo(c, pos, "threads "+verb+" OWNER/REPO")
		if !ok {
			return exitArg
		}
		if id.Path == "" || id.Cron == "" {
			fmt.Fprintln(c.stderr(), "spindle: --path and --cron are required (identify the entry)")
			return exitArg
		}
		a, err := newAPIClient(c, f)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
			return exitErr
		}
		body := map[string]any{"path": id.Path, "cron": id.Cron, "timezone": id.Timezone, "enabled": enabled}
		raw, err := a.call(ctx, "POST", threadsPath(owner, repo, "schedules", "enable"), nil, body)
		if err != nil {
			return a.fail(err)
		}
		if f.json {
			return a.emitJSON(raw)
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		fmt.Fprintf(c.stdout(), "%s: %s (%s)\n", id.Path, state, id.Cron)
		return exitOK
	}
}

func runThreadsScheduleRun(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads schedule run")
	f.add(fs)
	var id threads.ScheduleEntryID
	fs.StringVar(&id.Path, "path", "", "workflow file path (.spindle/workflows/x.yml)")
	fs.StringVar(&id.Cron, "cron", "", "the cron expression as written")
	fs.StringVar(&id.Timezone, "timezone", "", "the entry timezone (empty = UTC)")
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "threads schedule run OWNER/REPO")
	if !ok {
		return exitArg
	}
	if id.Path == "" || id.Cron == "" {
		fmt.Fprintln(c.stderr(), "spindle: --path and --cron are required (identify the entry)")
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "POST", threadsPath(owner, repo, "schedules", "run-now"), nil,
		map[string]any{"path": id.Path, "cron": id.Cron, "timezone": id.Timezone})
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var out struct {
		Runs []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"runs"`
	}
	if json.Unmarshal(raw, &out) == nil {
		for _, r := range out.Runs {
			fmt.Fprintf(c.stdout(), "Started %s (%s)\n", r.ID, r.State)
		}
	}
	return exitOK
}

func runThreadsPause(paused bool) func(context.Context, *cli, []string) int {
	return func(ctx context.Context, c *cli, args []string) int {
		verb := "pause"
		if !paused {
			verb = "resume"
		}
		f := &apiFlags{}
		fs := newAPIFlagSet(c, "threads "+verb)
		f.add(fs)
		pos, exit := parseAPI(fs, args)
		if exit != parseContinue {
			return exit
		}
		owner, repo, ok := oneRepo(c, pos, "threads "+verb+" OWNER/REPO")
		if !ok {
			return exitArg
		}
		a, err := newAPIClient(c, f)
		if err != nil {
			fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
			return exitErr
		}
		raw, err := a.call(ctx, "POST", threadsPath(owner, repo, "pause"), nil, map[string]any{"paused": paused})
		if err != nil {
			return a.fail(err)
		}
		if f.json {
			return a.emitJSON(raw)
		}
		if !f.quiet {
			if paused {
				fmt.Fprintln(c.stdout(), "CI paused: in-flight jobs finish, nothing new is claimed.")
			} else {
				fmt.Fprintln(c.stdout(), "CI resumed: queued jobs are claimable again.")
			}
		}
		return exitOK
	}
}

func runThreadsQueue(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads queue")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	owner, repo, ok := oneRepo(c, pos, "threads queue OWNER/REPO")
	if !ok {
		return exitArg
	}
	a, err := newAPIClient(c, f)
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	raw, err := a.call(ctx, "GET", threadsPath(owner, repo, "queue"), nil, nil)
	if err != nil {
		return a.fail(err)
	}
	if f.json {
		return a.emitJSON(raw)
	}
	var snap threads.QueueSnapshot
	if json.Unmarshal(raw, &snap) != nil {
		return a.emitJSON(raw)
	}
	if snap.Paused {
		fmt.Fprintln(c.stdout(), "CI is paused; nothing is claimed.")
	}
	t := newTable(c.stdout(), "ORDER", "RUN", "JOB", "PRIORITY", "LABELS", "STATE")
	for i, row := range snap.Rows {
		t.row(strconv.Itoa(i+1), row.RunID, row.JobID, strconv.Itoa(row.Priority), strings.Join(row.Labels, ","), row.State)
	}
	t.flush()
	return exitOK
}

// runThreadsExplain prints the local file's triggers/jobs (PAR-196). It reads
// no server and creates no run.
func runThreadsExplain(ctx context.Context, c *cli, args []string) int {
	f := &apiFlags{}
	fs := newAPIFlagSet(c, "threads explain")
	f.add(fs)
	pos, exit := parseAPI(fs, args)
	if exit != parseContinue {
		return exit
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr(), "usage: spindle threads explain FILE")
		return exitArg
	}
	raw, err := os.ReadFile(pos[0])
	if err != nil {
		fmt.Fprintf(c.stderr(), "spindle: %v\n", err)
		return exitErr
	}
	ex := threads.ExplainWorkflow(pos[0], raw)
	if f.json {
		out, _ := json.Marshal(ex)
		fmt.Fprintln(c.stdout(), string(out))
		if !ex.Valid {
			return exitErr
		}
		return exitOK
	}
	if !ex.Valid {
		fmt.Fprintf(c.stdout(), "%s: invalid\n", pos[0])
		if ex.IssueKey != "" {
			fmt.Fprintf(c.stdout(), "  %s: %s\n", ex.IssueKey, ex.Error)
		} else {
			fmt.Fprintf(c.stdout(), "  %s\n", ex.Error)
		}
		return exitErr
	}
	fmt.Fprintf(c.stdout(), "%s (%s)\n", ex.Name, pos[0])
	fmt.Fprintf(c.stdout(), "triggers: %s\n", strings.Join(ex.Triggers, ", "))
	t := newTable(c.stdout(), "JOB", "RUNS-ON", "NEEDS", "STEPS", "USES")
	for _, j := range ex.Jobs {
		t.row(j.Key, strings.Join(j.RunsOn, ","), strings.Join(j.Needs, ","), strconv.Itoa(j.Steps), j.Uses)
	}
	t.flush()
	_ = ctx
	return exitOK
}
