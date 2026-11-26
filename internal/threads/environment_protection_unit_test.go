package threads

// environment_protection_unit_test.go — the pure helpers behind the wave-2
// protection and retention rules. They are table tests because each is a small
// decision that the HTTP/service paths depend on; covering the empty, duplicate
// and malformed arms here keeps the enforcement paths readable.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeploymentBranchPatternValidation(t *testing.T) {
	got, err := normalizeDeploymentBranches([]string{" main ", "main", "", "release/*", "refs/tags/v*"})
	if err != nil {
		t.Fatalf("valid patterns: %v", err)
	}
	if strings.Join(got, ",") != "main,refs/tags/v*,release/*" {
		t.Fatalf("normalised = %v", got)
	}
	for _, bad := range []string{
		strings.Repeat("x", 257),
		"bad\x01pattern",
		"release/[ab]",
		"a{b}",
		"why?",
	} {
		if _, err := normalizeDeploymentBranches([]string{bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q accepted (%v)", bad, err)
		}
	}
	many := make([]string, MaxDeploymentBranchPatterns+1)
	for i := range many {
		many[i] = "b" + strings.Repeat("x", i%10) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)
	}
	if _, err := normalizeDeploymentBranches(many); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many patterns accepted: %v", err)
	}
}

func TestDeploymentRefMatching(t *testing.T) {
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"main", "main", true},
		{"main", "refs/heads/main", false},
		{"release/*", "release/v1", true},
		{"release/*", "release/", true},
		{"release/*", "releases/v1", false},
		{"v*", "v1.2.3", true},
		{"*", "anything", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxcyyb", false},
		{"", "main", false},
		{"main", "", false},
	}
	for _, tc := range cases {
		if got := globMatchRef(tc.pattern, tc.s); got != tc.want {
			t.Fatalf("globMatchRef(%q,%q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
	if got := refName("refs/heads/main", ""); got != "main" {
		t.Fatalf("branch ref = %q", got)
	}
	if got := refName("refs/tags/v1", ""); got != "v1" {
		t.Fatalf("tag ref = %q", got)
	}
	if got := refName("", "feature"); got != "feature" {
		t.Fatalf("branch fallback = %q", got)
	}
	if got := refName("weird/ref", "b"); got != "weird/ref" {
		t.Fatalf("unrecognised ref = %q", got)
	}
	// A policy with no patterns allows every ref; an empty ref cannot match a
	// non-empty policy.
	if ok, reason := deploymentRefAllowed(nil, &Run{}, "production"); !ok || reason != "" {
		t.Fatalf("no policy = %v %q", ok, reason)
	}
	if ok, reason := deploymentRefAllowed([]string{"main"}, &Run{}, "production"); ok || !strings.Contains(reason, "(no ref)") {
		t.Fatalf("empty ref = %v %q", ok, reason)
	}
	if ok, _ := deploymentRefAllowed([]string{"refs/heads/main"}, &Run{Ref: "refs/heads/main"}, "production"); !ok {
		t.Fatal("a full-ref pattern did not match")
	}
}

func TestApprovalBookkeeping(t *testing.T) {
	complete := &Deployment{RequiredApprovers: []string{"ada", "grace"}, Approvals: []string{"ada"}}
	if requiredApprovalComplete(complete) {
		t.Fatal("a partial approval read complete")
	}
	if got := awaitingApprovers(complete); strings.Join(got, ",") != "grace" {
		t.Fatalf("awaiting = %v", got)
	}
	recordApproval(complete, " Grace ")
	recordApproval(complete, "grace") // idempotent
	recordApproval(complete, "")
	recordApproval(nil, "ada")
	if !requiredApprovalComplete(complete) || len(complete.Approvals) != 2 {
		t.Fatalf("bookkeeping = %+v", complete)
	}
	if !requiredApprovalComplete(nil) || awaitingApprovers(nil) != nil {
		t.Fatal("a nil deployment is not a requirement")
	}
	if !requiredApprovalComplete(&Deployment{}) {
		t.Fatal("an empty requirement must be complete (a timer-only hold)")
	}
}

func TestDeploymentWaitRemaining(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(dateTimeFmt)
	past := now.Add(-time.Hour).Format(dateTimeFmt)
	for _, tc := range []struct {
		name string
		job  *Job
		want bool
	}{
		{"nil job", nil, false},
		{"no deployment", &Job{}, false},
		{"no timer", &Job{Deployment: &Deployment{}}, false},
		{"empty stamp", &Job{Deployment: &Deployment{WaitUntil: ptr(" ")}}, false},
		{"future", &Job{Deployment: &Deployment{WaitUntil: &future}}, true},
		{"past", &Job{Deployment: &Deployment{WaitUntil: &past}}, false},
		{"unparseable holds", &Job{Deployment: &Deployment{WaitUntil: ptr("soon")}}, true},
	} {
		if got, _ := deploymentWaitRemaining(tc.job, now); got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
	if waitUntilFor(nil, now) != nil {
		t.Fatal("a nil record has a timer")
	}
	if waitUntilFor(&EnvironmentDoc{}, now) != nil {
		t.Fatal("a zero timer has a deadline")
	}
	if got := waitUntilFor(&EnvironmentDoc{WaitTimerSeconds: 60}, now); got == nil || *got != now.Add(time.Minute).Format(dateTimeFmt) {
		t.Fatalf("timer deadline = %v", got)
	}
}

func TestNormalizeTeamReviewerArms(t *testing.T) {
	s := &Service{}
	if got, err := s.normalizeTeamReviewers(context.Background(), []string{"", "  "}); err != nil || got != nil {
		t.Fatalf("empty list = %v (%v)", got, err)
	}
	// No roster: a team is a named refusal (fail closed).
	if _, err := s.normalizeTeamReviewers(context.Background(), []string{"acme/x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unwired roster = %v", err)
	}
	s.TeamRoster = fakeTeamRoster{teams: map[string][]string{"acme/rel": {"ada"}, "acme/dup": {"ada"}}}
	got, err := s.normalizeTeamReviewers(context.Background(), []string{" Acme/Rel ", "acme/rel"})
	if err != nil || strings.Join(got, ",") != "acme/rel" {
		t.Fatalf("dedupe/lowercase = %v (%v)", got, err)
	}
	if _, err := s.normalizeTeamReviewers(context.Background(), []string{"acme/empty"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty team = %v", err)
	}
	s.TeamRoster = fakeTeamRoster{err: errors.New("down")}
	if _, err := s.normalizeTeamReviewers(context.Background(), []string{"acme/rel"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("roster error = %v", err)
	}
}

func TestRunGroupPoliciesArms(t *testing.T) {
	jobs := []*Job{
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "  "}}},
		{State: JobQueued, Spec: &Spec{}},
		{State: JobSkipped, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "skipped"}}},
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "g"}}},
		{State: JobQueued, Spec: &Spec{Concurrency: &ConcurrencySpec{Group: "g", CancelInProgress: true}}},
	}
	pol := runGroupPolicies(jobs)
	if len(pol) != 1 || pol[0].Group != "g" || !pol[0].CancelInProgress {
		t.Fatalf("policies = %+v", pol)
	}
	if len(runGroupPolicies(nil)) != 0 {
		t.Fatal("no jobs, policies")
	}
	e := newTestEnv(t)
	if got, err := e.svc.prepareRunGroups(ctx(), "o", "r", nil, nil); err != nil || got != "" {
		t.Fatalf("nil run = %q (%v)", got, err)
	}
	if got, err := e.svc.prepareGroup(ctx(), "o", "r", groupPolicy{}, "run"); err != nil || got != "" {
		t.Fatalf("empty group = %q (%v)", got, err)
	}
}
