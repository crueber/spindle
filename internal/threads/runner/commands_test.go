package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Command files (docs/features/21_parity_master.md PAR-090/PAR-091;
// docs/threads.md §3.10): `$GITHUB_ENV` extends the environment of LATER steps,
// `$GITHUB_PATH` prepends directories to their PATH, and neither ever mutates
// the runner process or a concurrent job.

func TestParseEnvFile(t *testing.T) {
	ok := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"one pair", "TAG=v1\n", map[string]string{"TAG": "v1"}},
		{"value keeps equals", "FLAGS=-a=b=c\n", map[string]string{"FLAGS": "-a=b=c"}},
		{"empty value", "EMPTY=\n", map[string]string{"EMPTY": ""}},
		{"blank lines ignored", "\nA=1\n\n\nB=2\n", map[string]string{"A": "1", "B": "2"}},
		{"crlf works", "A=1\r\nB=2\r\n", map[string]string{"A": "1", "B": "2"}},
		{"last write wins", "A=1\nA=2\n", map[string]string{"A": "2"}},
		{"space around the name is trimmed", "  A =x\n", map[string]string{"A": "x"}},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEnvFile([]byte(tc.in))
			if err != nil {
				t.Fatalf("parseEnvFile(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseEnvFile(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("parseEnvFile(%q)[%s] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
	bad := []struct {
		name string
		in   string
		want string
	}{
		{"no equals", "JUST_A_WORD\n", "expected NAME=VALUE"},
		{"heredoc is not documented for this file", "TAG<<EOF\nv1\nEOF\n", "expected NAME=VALUE"},
		{"empty name", "=value\n", "empty"},
		{"invalid name", "BAD-NAME=x\n", "not a shell identifier"},
		{"digit first", "1BAD=x\n", "starts with a digit"},
		{"protected namespace", "GITHUB_SHA=deadbeef\n", "GITHUB_*"},
		{"runner namespace", "SPINDLE_OUTPUT=/tmp/x\n", "SPINDLE_*"},
		{"path must use GITHUB_PATH", "PATH=/evil\n", "GITHUB_PATH"},
		{"loader hook", "LD_PRELOAD=/tmp/evil.so\n", "LD_PRELOAD"},
		{"node options", "NODE_OPTIONS=--require /tmp/evil.js\n", "process-startup hooks"},
		{"nul byte", "A=x\x00y\n", "control character"},
		{"carriage return in the value", "A=x\ry\n", "control character"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEnvFile([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseEnvFile(%q) = %v, want an error containing %q", tc.in, err, tc.want)
			}
			// The error never repeats a value (it may be a secret the step read).
			if strings.Contains(err.Error(), "deadbeef") || strings.Contains(err.Error(), "/tmp/evil") {
				t.Fatalf("the error echoed a value: %q", err)
			}
		})
	}
}

func TestParsePathFile(t *testing.T) {
	got, err := parsePathFile([]byte("/opt/bin\n\n  /usr/local/tool  \r\n"))
	if err != nil {
		t.Fatalf("parsePathFile: %v", err)
	}
	if strings.Join(got, "|") != "/opt/bin|/usr/local/tool" {
		t.Fatalf("entries = %v", got)
	}
	if _, err := parsePathFile([]byte("/a\n/a\n")); err != nil {
		t.Fatalf("a duplicate path is not an error: %v", err)
	}
	if _, err := parsePathFile([]byte(strings.Repeat("x", MaxCommandPathEntryLen+1) + "\n")); err == nil ||
		!strings.Contains(err.Error(), "longer than") {
		t.Fatalf("over-long entry = %v", err)
	}
}

// TestCommandFileCaps pins the per-job bounds (a runaway job cannot grow the
// exec argv without limit).
func TestCommandFileCaps(t *testing.T) {
	cmds := newCommandFiles()
	values := map[string]string{}
	for i := 0; i <= MaxCommandEnvVars; i++ {
		values["V"+strings.Repeat("A", i+1)] = "x"
	}
	if err := cmds.addEnv(values); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("count cap = %v", err)
	}
	big := newCommandFiles()
	if err := big.addEnv(map[string]string{"A": strings.Repeat("x", MaxCommandEnvValueLen+1)}); err == nil ||
		!strings.Contains(err.Error(), "over the cap") {
		t.Fatalf("value cap = %v", err)
	}
	// The byte cap is separate from the per-value cap.
	fill := newCommandFiles()
	if err := fill.addEnv(map[string]string{"A": strings.Repeat("x", MaxCommandEnvValueLen)}); err != nil {
		t.Fatalf("first value: %v", err)
	}
	more := map[string]string{}
	for i := 0; i < 20; i++ {
		more["B"+strings.Repeat("B", i+1)] = strings.Repeat("y", MaxCommandEnvValueLen)
	}
	if err := fill.addEnv(more); err == nil || !strings.Contains(err.Error(), "bytes of added environment") {
		t.Fatalf("byte cap = %v", err)
	}
	paths := newCommandFiles()
	var entries []string
	for i := 0; i <= MaxCommandPathEntries; i++ {
		entries = append(entries, "/p/"+strings.Repeat("d", i+1))
	}
	if err := paths.addPath(entries); err == nil || !strings.Contains(err.Error(), "prepend more than") {
		t.Fatalf("path cap = %v", err)
	}
}

// TestApplyCommandFiles pins the argv shape: env values are `--env NAME=VALUE`
// elements, the step's own `env:` wins, and PATH entries wrap the command
// (prepended, never replacing the image PATH).
func TestApplyCommandFiles(t *testing.T) {
	p := &Plan{Names: Names{Container: "sp-j-test"}, Workspace: DefaultWorkspace}
	step := StepPlan{
		Kind:    "run",
		StepEnv: []string{"AUTHORED"},
		Argv:    []string{"exec", "--workdir", DefaultWorkspace, "--env", "AUTHORED=keep", "sp-j-test", "sh", "-e", "-c", "run"},
	}
	cmds := newCommandFiles()
	if err := cmds.addEnv(map[string]string{"TAG": "v1", "AUTHORED": "from-file"}); err != nil {
		t.Fatal(err)
	}
	if err := cmds.addPath([]string{"/opt/bin", "/opt/other"}); err != nil {
		t.Fatal(err)
	}
	got := applyCommandFiles(p, step, cmds)
	argv := strings.Join(got.Argv, " ")
	if !strings.Contains(argv, "--env TAG=v1") {
		t.Fatalf("TAG not injected: %s", argv)
	}
	if strings.Contains(argv, "--env AUTHORED=from-file") {
		t.Fatalf("the step's own env was overridden: %s", argv)
	}
	if !strings.Contains(argv, "--env AUTHORED=keep") {
		t.Fatalf("the authored env disappeared: %s", argv)
	}
	// The wrapper prepends both dirs to the step's own PATH and execs the
	// original command unchanged.
	if !strings.Contains(argv, `PATH="$0:$PATH"; export PATH; exec "$@"`) ||
		!strings.Contains(argv, "/opt/bin:/opt/other") {
		t.Fatalf("PATH wrapper = %s", argv)
	}
	if !strings.HasSuffix(argv, "sh -e -c run") {
		t.Fatalf("the original command was changed: %s", argv)
	}
	// Injection happens before the container name (docker's own order).
	containerAt := indexOfString(got.Argv, "sp-j-test")
	envAt := indexOfString(got.Argv, "TAG=v1")
	if envAt < 0 || envAt > containerAt {
		t.Fatalf("TAG injected after the container: %v", got.Argv)
	}
	// A docker action's argv is the image's own and is never wrapped.
	docker := StepPlan{Kind: "docker", Argv: []string{"run", "--name", "sp-a-x", "sp-j-test", "alpine", "true"}}
	if unchanged := applyCommandFiles(p, docker, cmds); strings.Join(unchanged.Argv, " ") != strings.Join(docker.Argv, " ") {
		t.Fatalf("a docker action was modified: %v", unchanged.Argv)
	}
	// With no command-file state the argv is untouched.
	empty := applyCommandFiles(p, step, newCommandFiles())
	if strings.Join(empty.Argv, " ") != strings.Join(step.Argv, " ") {
		t.Fatalf("an empty state changed the argv: %v", empty.Argv)
	}
}

func indexOfString(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

// TestExecuteCommandFilesAffectLaterStepsOnly runs a real three-step job against
// the fake docker: step 1 writes an env var and a PATH entry, step 2 sees both,
// step 3 does not see step 2's addition before it runs.
func TestExecuteCommandFilesAffectLaterStepsOnly(t *testing.T) {
	docker := &fakeDocker{
		runOutputs: []runOutput{
			// Step 1 writes TAG and one PATH entry.
			{match: "/tmp/spindle-command-1-env", out: "TAG=v1\n"},
			{match: "/tmp/spindle-command-1-path", out: "/opt/tool/bin\n"},
			// Step 2 writes a second PATH entry.
			{match: "/tmp/spindle-command-2-path", out: "/opt/second/bin\n"},
		},
	}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "write", Run: "write"},
		{Name: "read", Run: "read"},
		{Name: "read-again", Run: "read-again"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%+v)", res.Conclusion, res.Steps)
	}
	calls := docker.snapshot()
	var first, second, third string
	for _, call := range calls {
		switch {
		case strings.HasSuffix(call, "bash --noprofile --norc -eo pipefail -c write"):
			first = call
		case strings.HasSuffix(call, "bash --noprofile --norc -eo pipefail -c read"):
			second = call
		case strings.HasSuffix(call, "bash --noprofile --norc -eo pipefail -c read-again"):
			third = call
		}
	}
	if first == "" || second == "" || third == "" {
		t.Fatalf("missing step calls: %v", calls)
	}
	if strings.Contains(first, "TAG=v1") {
		t.Fatalf("a step saw its own env-file write: %s", first)
	}
	if !strings.Contains(second, "--env TAG=v1") {
		t.Fatalf("the next step did not see TAG: %s", second)
	}
	if !strings.Contains(second, "/opt/tool/bin") || !strings.Contains(second, `PATH="$0:$PATH"`) {
		t.Fatalf("the next step's PATH was not prepended: %s", second)
	}
	if !strings.Contains(third, "/opt/tool/bin:/opt/second/bin") {
		t.Fatalf("PATH entries did not accumulate in write order: %s", third)
	}
}

// TestExecuteCommandFileRefusalFailsTheStep pins the protected-name refusal and
// the fail-closed parse: a step that tries to set LD_PRELOAD fails, named, and
// the job fails (never a silent drop).
func TestExecuteCommandFileRefusalFailsTheStep(t *testing.T) {
	docker := &fakeDocker{
		runOutputs: []runOutput{{match: "/tmp/spindle-command-1-env", out: "LD_PRELOAD=/tmp/evil.so\n"}},
	}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "inject", Run: "inject"},
		{Name: "after", Run: "after"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	if res.Steps[0].Conclusion != threads.JobFailure || !strings.Contains(res.Steps[0].Error, "LD_PRELOAD") ||
		!strings.Contains(res.Steps[0].Error, "refusing") {
		t.Fatalf("step 1 = %+v", res.Steps[0])
	}
	if res.Steps[1].Conclusion != "skipped" {
		t.Fatalf("step 2 = %+v", res.Steps[1])
	}
}

// TestExecuteEnvFileOnAFailedStepStillApplies pins GitHub's rule: a step that
// writes its env file and then fails still changes the job environment, so a
// tolerated failure's later steps see the variable.
func TestExecuteEnvFileOnAFailedStepStillApplies(t *testing.T) {
	docker := &fakeDocker{
		streamByMatch: []runOutput{{match: "bash --noprofile --norc -eo pipefail -c fail", code: 1}},
		runOutputs:    []runOutput{{match: "/tmp/spindle-command-1-env", out: "AFTER=failure\n"}},
	}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "fail", Run: "fail", ContinueOnError: true},
		{Name: "after", Run: "after"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%+v)", res.Conclusion, res.Steps)
	}
	var after string
	for _, call := range docker.snapshot() {
		if strings.HasSuffix(call, "bash --noprofile --norc -eo pipefail -c after") {
			after = call
		}
	}
	if !strings.Contains(after, "--env AFTER=failure") {
		t.Fatalf("the failed step's env-file value did not reach the next step: %s", after)
	}
}

// TestExecuteDockerActionGetsNoCommandFiles pins the third-party rule: a
// docker:// step's argv carries no command-file variable and is not wrapped.
func TestExecuteDockerActionGetsNoCommandFiles(t *testing.T) {
	docker := &fakeDocker{
		runOutputs: []runOutput{{match: "/tmp/spindle-command-1-env", out: "TAG=v1\n"}},
	}
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "write", Run: "write"},
		{Name: "image", Uses: "docker://alpine:3.20"},
	}
	res := executorWith(docker).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%+v)", res.Conclusion, res.Steps)
	}
	var image string
	for _, call := range docker.snapshot() {
		if strings.Contains(call, "alpine:3.20") {
			image = call
		}
	}
	if image == "" {
		t.Fatalf("no docker action ran: %v", docker.snapshot())
	}
	if strings.Contains(image, "TAG=v1") || strings.Contains(image, "GITHUB_ENV=") || strings.Contains(image, "GITHUB_PATH=") {
		t.Fatalf("a docker action received command-file state: %s", image)
	}
}

// TestExecuteWritesEventFile pins PAR-033 at execution: the job's event JSON is
// written into the container before the first step, at the path every step
// reads from GITHUB_EVENT_PATH.
func TestExecuteWritesEventFile(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Checkout = nil
	job.EventPayload = map[string]any{"action": "opened", "pull_request": map[string]any{"number": float64(7)}}
	job.Spec.Steps = []threads.StepSpec{{Name: "noop", Run: "true"}}
	d := &fakeDocker{}
	res := executorWith(d).Execute(context.Background(), ExecInput{Job: job, AttemptToken: "t"}, quietStream(t))
	if res.Conclusion != threads.JobSuccess {
		t.Fatalf("conclusion = %s (%+v) err=%s calls=%v", res.Conclusion, res.Steps, res.Error, d.snapshot())
	}
	found := false
	for _, call := range d.snapshot() {
		if strings.Contains(call, EventPath) && strings.Contains(call, `"action":"opened"`) &&
			strings.Contains(call, `"number":7`) && strings.Contains(call, "printf") &&
			strings.Contains(call, RunnerTemp) && strings.Contains(call, "chmod 0777") {
			found = true
		}
	}
	if !found {
		t.Fatalf("event file write not observed: %v", d.snapshot())
	}
}
