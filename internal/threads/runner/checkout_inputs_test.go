package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// actions/checkout inputs (docs/features/21_parity_master.md PAR-073;
// docs/threads.md §4.4). Every honoured input has one documented argv, and an
// unsupported input is refused BY NAME — never ignored.

// fetchLine returns the generated script's `git … fetch …` line.
func fetchLine(script string) string {
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, " fetch ") {
			return line
		}
	}
	return ""
}

// TestCheckoutInputArgv pins the exact fetch argv for each honoured input.
func TestCheckoutInputArgv(t *testing.T) {
	const sha = "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	base := &Plan{Workspace: "/spindle/workspace", CheckoutURL: "https://git.example.com/o/r.git", CheckoutSHA: sha}
	cases := []struct {
		name string
		opts checkoutOpts
		want []string
		deny []string
	}{
		{
			name: "default is shallow and explicit about tags",
			opts: checkoutOpts{Depth: "1"},
			want: []string{"fetch -q --depth=1 --no-tags origin '" + sha + "'"},
		},
		{
			name: "fetch-depth 0 is a full clone",
			opts: checkoutOpts{Depth: ""},
			want: []string{"fetch -q --no-tags origin '" + sha + "'"},
			deny: []string{"--depth"},
		},
		{
			name: "fetch-depth N",
			opts: checkoutOpts{Depth: "25"},
			want: []string{"--depth=25 --no-tags"},
		},
		{
			name: "fetch-tags asks for every tag",
			opts: checkoutOpts{Depth: "1", Tags: true},
			want: []string{"fetch -q --depth=1 --tags origin '" + sha + "'"},
			deny: []string{"--no-tags"},
		},
		{
			name: "ref replaces the run commit as the fetch target",
			opts: checkoutOpts{Depth: "1", Ref: "refs/heads/release"},
			want: []string{"origin 'refs/heads/release'", "checked out " + sha + " (ref refs/heads/release)"},
			deny: []string{"origin '" + sha + "'"},
		},
		{
			name: "path checks out into a workspace subdirectory",
			opts: checkoutOpts{Depth: "1", Path: "src/repo"},
			want: []string{"mkdir -p '/spindle/workspace/src/repo'", "cd '/spindle/workspace/src/repo'"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := checkoutScript(base, tc.opts)
			fetch := fetchLine(script)
			for _, want := range tc.want {
				target := fetch
				if strings.HasPrefix(want, "mkdir") || strings.HasPrefix(want, "cd ") || strings.HasPrefix(want, "checked out") {
					target = script
				}
				if !strings.Contains(target, want) {
					t.Errorf("script misses %q\nfetch: %s\nscript:\n%s", want, fetch, script)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(fetch, deny) {
					t.Errorf("fetch carries %q: %s", deny, fetch)
				}
			}
		})
	}
}

// TestCheckoutOptionsParse pins the `with:` parser and its named refusals.
func TestCheckoutOptionsParse(t *testing.T) {
	opts, err := checkoutOptions(map[string]string{"fetch-depth": "0", "fetch-tags": "true", "ref": "main", "path": "sub/dir"})
	if err != nil {
		t.Fatalf("checkoutOptions: %v", err)
	}
	if opts.Depth != "" || !opts.Tags || opts.Ref != "main" || opts.Path != "sub/dir" {
		t.Fatalf("opts = %+v", opts)
	}
	// A bare checkout is shallow with tags off.
	if opts, err := checkoutOptions(nil); err != nil || opts.Depth != "1" || opts.Tags || opts.Ref != "" || opts.Path != "" {
		t.Fatalf("default opts = %+v, %v", opts, err)
	}
	// An empty path and an all-zero depth are the workspace root / a full clone.
	if opts, err := checkoutOptions(map[string]string{"path": "", "fetch-depth": "00"}); err != nil ||
		opts.Path != "" || opts.Depth != "" {
		t.Fatalf("empty path/zero depth = %+v, %v", opts, err)
	}
	// An unsupported key with no documented hint still names itself and the set.
	if _, err := checkoutOptions(map[string]string{"wat": "1"}); err == nil ||
		!strings.Contains(err.Error(), `"wat" is not supported`) ||
		!strings.Contains(err.Error(), "fetch-depth, fetch-tags, path, ref") {
		t.Fatalf("unknown key = %v", err)
	}
	// A path with a redundant '.' segment is accepted and normalised by the
	// script's containerDir.
	if opts, err := checkoutOptions(map[string]string{"path": "a/./b"}); err != nil || opts.Path != "a/./b" {
		t.Fatalf("dot segment = %+v, %v", opts, err)
	}
	// A NUL in the path is refused.
	if _, err := checkoutOptions(map[string]string{"path": "a\x00b"}); err == nil ||
		!strings.Contains(err.Error(), "path") {
		t.Fatalf("NUL path = %v", err)
	}
	// fetch-tags accepts GitHub's spelling of false too.
	if opts, err := checkoutOptions(map[string]string{"fetch-tags": "false"}); err != nil || opts.Tags {
		t.Fatalf("fetch-tags false = %+v, %v", opts, err)
	}
	refusals := []struct {
		with map[string]string
		want string
	}{
		{map[string]string{"submodules": "true"}, "submodules"},
		{map[string]string{"token": "${{ secrets.X }}"}, "token"},
		{map[string]string{"clean": "true"}, "clean"},
		{map[string]string{"fetch-depth": "-1"}, "fetch-depth"},
		{map[string]string{"fetch-depth": "all"}, "fetch-depth"},
		{map[string]string{"fetch-tags": "maybe"}, "fetch-tags"},
		{map[string]string{"ref": "--upload-pack=touch"}, "ref"},
		{map[string]string{"ref": "main branch"}, "ref"},
		{map[string]string{"path": "/abs"}, "path"},
		{map[string]string{"path": "../escape"}, "path"},
		{map[string]string{"path": "~/home"}, "path"},
		{map[string]string{"path": strings.Repeat("a", 513)}, "path"},
		{map[string]string{"ref": strings.Repeat("a", 257)}, "ref"},
		{map[string]string{"ref": "   "}, "ref"},
	}
	for _, tc := range refusals {
		_, err := checkoutOptions(tc.with)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkoutOptions(%v) = %v, want an error naming %q", tc.with, err, tc.want)
		}
	}
}

// TestBuildPlanCheckoutRefusesAnUnsupportedInput pins that the refusal reaches
// the step as a named failure (the job fails before any container starts) and
// that the honoured inputs reach the plan.
func TestBuildPlanCheckoutRefusesAnUnsupportedInput(t *testing.T) {
	job := fixtureJob()
	job.Spec.Steps = []threads.StepSpec{{
		Name: "submodules", Uses: "actions/checkout@v4",
		With: map[string]string{"submodules": "true"},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Steps[0].Kind != "unsupported" || !strings.Contains(p.Steps[0].SkipReason, `"submodules" is not supported`) {
		t.Fatalf("step = %+v", p.Steps[0])
	}
	// The hint names what to do instead.
	if !strings.Contains(p.Steps[0].SkipReason, "git submodule update") {
		t.Fatalf("refusal has no hint: %q", p.Steps[0].SkipReason)
	}

	job = fixtureJob()
	job.Spec.Steps = []threads.StepSpec{{
		Name: "tags", Uses: "actions/checkout",
		With: map[string]string{"fetch-tags": "true", "ref": "v1.2.3", "path": "checkout"},
	}}
	p, err = BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if step.Kind != "checkout" {
		t.Fatalf("step = %+v", step)
	}
	for _, want := range []string{"--tags", "origin 'v1.2.3'", "mkdir -p '/spindle/workspace/checkout'"} {
		if !strings.Contains(step.Run, want) {
			t.Errorf("checkout script misses %q:\n%s", want, step.Run)
		}
	}
	if !strings.Contains(strings.Join(step.Argv, " "), "--env SPINDLE_JOB_TOKEN") {
		t.Fatalf("checkout lost the job token: %v", step.Argv)
	}
}

// TestCheckoutScriptRunsThePinnedArgvUnderARealShell drives the generated script
// with a recording `git` so the argv the container receives is asserted, not
// just the script text: fetch-tags, ref and path together.
func TestCheckoutScriptRunsThePinnedArgvUnderARealShell(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "git.log")
	stub := "#!/bin/sh\ncase \" $* \" in *\" init \"*) mkdir -p .git ;; esac\nprintf '%s\\n' \"$*\" >> \"$GIT_LOG\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub git: %v", err)
	}
	ws := t.TempDir()
	p := &Plan{
		Workspace:   ws,
		CheckoutURL: "https://git.example.com/o/r.git",
		CheckoutSHA: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	script := checkoutScript(p, checkoutOpts{Depth: "3", Tags: true, Ref: "refs/tags/v1", Path: "nested/repo"})
	cmd := exec.Command("sh", "-e", "-c", script)
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"GIT_LOG="+logPath,
		"SPINDLE_JOB_TOKEN=wjt_test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	want := []string{
		"init -q",
		"remote remove origin",
		"remote add origin https://git.example.com/o/r.git",
		" fetch -q --depth=3 --tags origin refs/tags/v1",
		"checkout -q FETCH_HEAD",
	}
	for i, wantLine := range want {
		if i >= len(lines) || !strings.Contains(lines[i], wantLine) {
			t.Fatalf("git call %d = %q, want %q\nall: %v", i, lineAt(lines, i), wantLine, lines)
		}
	}
	// The checkout landed in the requested subdirectory.
	if info, serr := os.Stat(filepath.Join(ws, "nested", "repo", ".git")); serr != nil || !info.IsDir() {
		t.Fatalf("the checkout directory was not created: %v", serr)
	}
	if !strings.Contains(string(out), "checked out cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa (ref refs/tags/v1)") {
		t.Fatalf("success line = %s", out)
	}
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
