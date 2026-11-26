package workflow

import (
	"strings"
	"testing"
	"time"
)

// TestTypedTriggerFires pins release/issues/issue_comment matching, including
// the absent-filter default and the bare-key spelling.
func TestTypedTriggerFires(t *testing.T) {
	cases := []struct {
		src    string
		event  string
		action string
		want   bool
	}{
		{"on: release\n", "release", "published", true},
		{"on:\n  release:\n    types: [published]\n", "release", "published", true},
		{"on:\n  release:\n    types: [published]\n", "release", "deleted", false},
		{"on: issues\n", "issues", "opened", true},
		{"on:\n  issue_comment:\n    types: [created]\n", "issue_comment", "created", true},
		{"on:\n  issue_comment:\n    types: [created]\n", "issue_comment", "edited", false},
		{"on: push\n", "issues", "opened", false},
	}
	for _, tc := range cases {
		w, err := Parse([]byte(tc.src + "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: echo hi\n"))
		if err != nil {
			t.Fatalf("%q: %v", tc.src, err)
		}
		if got := w.Fires(tc.event, RefBranch, "", tc.action); got != tc.want {
			t.Errorf("%q Fires(%s,%s) = %v, want %v", tc.src, tc.event, tc.action, got, tc.want)
		}
	}
}

// TestMatchPathsEdges pins paths-ignore and the fail-closed no-diff rule.
func TestMatchPathsEdges(t *testing.T) {
	if !matchPaths(nil, nil, nil, false) {
		t.Fatal("a trigger with no path filter must fire without a diff")
	}
	if matchPaths([]string{"src/**"}, nil, nil, false) {
		t.Fatal("a path-filtered trigger fired without a diff")
	}
	if !matchPaths(nil, []string{"docs/**"}, []string{"src/a.go"}, true) {
		t.Fatal("paths-ignore excluded a non-matching file")
	}
	if matchPaths(nil, []string{"docs/**"}, []string{"docs/a.md"}, true) {
		t.Fatal("paths-ignore did not exclude a matching file")
	}
	if !matchPaths(nil, nil, []string{"anything"}, true) {
		t.Fatal("a bare trigger did not fire")
	}
	// A nil receiver's ignore predicate is false (it is not an ignore list).
	var nilFilter *compiledFilter
	if nilFilter.isIgnore() || !nilFilter.Match("x") {
		t.Fatal("nil filter")
	}
	if !matchAnyList(nil, "x") || matchAnyList([]string{"a"}, "b") {
		t.Fatal("matchAnyList")
	}
	if matchAnyList([]string{"!"}, "x") {
		t.Fatal("an invalid hand-built pattern matched")
	}
}

// TestCronParsingEdges covers the special-form error arms and NextIn.
func TestCronParsingEdges(t *testing.T) {
	bad := []string{
		"0 0 L-40 * *", "0 0 0W * *", "0 0 * * FOO L", "0 0 * * 5#9",
		"0 0 * * XL", "0 0 * * FUNDAY#2", "0 0 @daily * *", "0 0 * * 1#2#3",
	}
	for _, expr := range bad {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("accepted %q", expr)
		}
	}
	c := mustCron(t, "0 3 * * *")
	loc, _ := time.LoadLocation("Europe/Berlin")
	next, ok := c.NextIn(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), loc)
	if !ok || next.Hour() != 3 {
		t.Fatalf("NextIn = %v (%v)", next, ok)
	}
	// nearestWeekday clamps at the month boundaries.
	if got := nearestWeekday(2026, time.August, 1); got != 3 { // 1 Aug 2026 is a Saturday
		t.Fatalf("1W = %d, want Monday 3", got)
	}
	if got := nearestWeekday(2026, time.May, 31); got != 29 { // 31 May 2026 is a Sunday
		t.Fatalf("31W = %d, want Friday 29", got)
	}
	// A month name in the day-of-week field is invalid; names are per field.
	if _, err := ParseCron("0 0 * JAN FRI"); err != nil {
		t.Fatalf("valid names rejected: %v", err)
	}
	if _, err := ParseCron("0 0 * FRI *"); err == nil || !strings.Contains(err.Error(), "month name") {
		t.Fatalf("month field hint = %v", err)
	}
}

// TestContextVocabularyEdges pins the new closed sets and their helpers.
func TestContextVocabularyEdges(t *testing.T) {
	if !ValidRunnerField("OS") || ValidRunnerField("nope") || ValidRunnerField("") {
		t.Fatal("ValidRunnerField")
	}
	if !ValidEventField("release.tag_name") || ValidEventField("release.no") {
		t.Fatal("ValidEventField")
	}
	if got := lookupFold(map[string]string{"REGION": "eu"}, "region"); got != "eu" {
		t.Fatalf("lookupFold = %q", got)
	}
	if got := lookupFold(nil, "x"); got != "" {
		t.Fatalf("nil lookupFold = %q", got)
	}
	// vars and runner refs validate; runner is refused at a job-level if.
	if err := ValidateTemplate("${{ vars.REGION }}", TemplateScope{}); err != nil {
		t.Fatalf("vars ref: %v", err)
	}
	if err := ValidateTemplate("${{ runner.os }}", TemplateScope{JobIf: true}); err == nil {
		t.Fatal("runner.os accepted in a job-level if")
	}
	if err := ValidateTemplate("${{ vars.X }}", TemplateScope{JobIf: true}); err == nil {
		t.Fatal("vars.X accepted in a job-level if")
	}
	if err := ValidateTemplate("${{ github.event.release.tag_name }}", TemplateScope{}); err != nil {
		t.Fatalf("event ref: %v", err)
	}
	if err := ValidateTemplate("${{ github.event.release.nope }}", TemplateScope{}); err == nil {
		t.Fatal("unknown event field accepted")
	}
}

// TestFilterAndTriggerValidationArms covers the remaining validation arms the
// documented surface exposes.
func TestFilterAndTriggerValidationArms(t *testing.T) {
	if !ValidPullRequestType("Ready_For_Review") || ValidPullRequestType("deleted") {
		t.Fatal("ValidPullRequestType")
	}
	if !matchFilter([]string{"main"}, nil, "main") || !matchFilter(nil, []string{"wip"}, "main") {
		t.Fatal("matchFilter include/exclude")
	}
	if matchFilter([]string{"!"}, nil, "x") {
		t.Fatal("an invalid hand-built include pattern matched")
	}
	bad := []string{
		"on:\n  push:\n    branches: [main]\n    branches-ignore: [wip]\n",
		"on:\n  push:\n    tags: [v*]\n    tags-ignore: [v0]\n",
		"on:\n  push:\n    paths: [a]\n    paths-ignore: [b]\n",
		"on:\n  pull_request:\n    branches: [main]\n    branches-ignore: [wip]\n",
		"on:\n  pull_request:\n    paths: [a]\n    paths-ignore: [b]\n",
		"on:\n  pull_request:\n    types: []\n",
	}
	for _, src := range bad {
		if _, err := Parse([]byte(src + "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n")); err == nil {
			t.Errorf("accepted:\n%s", src)
		}
	}
	// The Events() projection lists the new triggers too.
	w, err := Parse([]byte("on: [release, issues, issue_comment]\njobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(w.Events(), ",")
	for _, want := range []string{"release", "issues", "issue_comment"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Events = %v", w.Events())
		}
	}
}

// TestCronDayValueArms covers the day-of-week token parser directly (numbers,
// names, 7 as Sunday, out-of-range and unknown names).
func TestCronDayValueArms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		err  bool
	}{
		{"0", 0, false}, {"7", 0, false}, {"5", 5, false}, {"MON", 1, false},
		{"sun", 0, false}, {"8", 0, true}, {"-1", 0, true}, {"FUNDAY", 0, true},
	} {
		got, err := cronDayValue(tc.in)
		if (err != nil) != tc.err || (!tc.err && got != tc.want) {
			t.Errorf("cronDayValue(%q) = %d, %v", tc.in, got, err)
		}
	}
}

func TestCronNilLocationFallback(t *testing.T) {
	c := mustCron(t, "0 3 * * *")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if _, ok := c.NextIn(now, nil); !ok {
		t.Fatal("NextIn(nil) returned no tick")
	}
	if _, ok := c.PrevIn(now, nil); !ok {
		t.Fatal("PrevIn(nil) returned no tick")
	}
	if _, err := (&CronEntry{Cron: "0 3 * * *", Timezone: "UTC"}).Location(); err != nil {
		t.Fatalf("UTC timezone: %v", err)
	}
}

// TestServiceCmdSpelling pins Forgejo's `services.<id>.cmd` alias.
func TestServiceCmdSpelling(t *testing.T) {
	w, err := Parse([]byte(`on: push
jobs:
  a:
    runs-on: linux
    services:
      db:
        image: postgres:16
        cmd: ["postgres", "-c", "fsync=off"]
        command: ["postgres"]
    steps:
      - run: echo hi
`))
	if err != nil {
		t.Fatalf("service cmd: %v", err)
	}
	svc := w.Jobs["a"].Services["db"]
	if len(svc.Cmd) != 3 || len(svc.Command) != 1 {
		t.Fatalf("service = %+v", svc)
	}
	if got := serviceCommandForTest(svc.Command, svc.Cmd); len(got) != 1 {
		t.Fatalf("command should win: %v", got)
	}
}

// serviceCommandForTest mirrors the spec projection's precedence without
// importing the threads package (the workflow package is a leaf).
func serviceCommandForTest(command, cmd []string) []string {
	if len(command) > 0 {
		return command
	}
	return cmd
}

// TestTriggerIgnoreNegationRefused pins the error arm for a negated ignore
// pattern.
func TestTriggerIgnoreNegationRefused(t *testing.T) {
	for _, src := range []string{
		"on:\n  push:\n    branches-ignore: ['!main']\n",
		"on:\n  push:\n    tags-ignore: ['!v*']\n",
		"on:\n  pull_request:\n    branches-ignore: ['!main']\n",
	} {
		if _, err := Parse([]byte(src + "jobs:\n  a:\n    runs-on: linux\n    steps:\n      - run: x\n")); err == nil {
			t.Errorf("accepted %q", src)
		}
	}
}
