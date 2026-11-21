package checks

import (
	"strings"
	"testing"
)

// The in_progress / cancelled states (05 §2, Threads round 2): a claimed job is
// distinguishable from a queued one, a cancelled job is not called a failure,
// and neither can satisfy a required check.

func TestStatesAcceptInProgressCancelledAndSkipped(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(51)
	e.knowSHA(sha)
	e.mustReport(t, sha, "threads/ci/build", StateInProgress)
	view, err := e.svc.GetStatuses(ctx(), "o", "r", sha, reader())
	if err != nil {
		t.Fatalf("GetStatuses: %v", err)
	}
	if len(view.Statuses) != 1 || view.Statuses[0].State != StateInProgress {
		t.Fatalf("statuses = %+v", view.Statuses)
	}
	if view.Statuses[0].CompletedAt != nil {
		t.Fatal("in_progress must not carry completed_at")
	}
	e.mustReport(t, sha, "threads/ci/build", StateCancelled)
	view, _ = e.svc.GetStatuses(ctx(), "o", "r", sha, reader())
	if view.Statuses[0].State != StateCancelled {
		t.Fatalf("cancelled status = %+v", view.Statuses[0])
	}
	e.mustReport(t, sha, "threads/ci/build", StateSkipped)
	view, _ = e.svc.GetStatuses(ctx(), "o", "r", sha, reader())
	if view.Statuses[0].State != StateSkipped {
		t.Fatalf("skipped status = %+v", view.Statuses[0])
	}
	// The states are valid on the trusted in-process reporter too (the exact
	// path the Threads check mirror uses), so one validation rule covers both.
	if _, err := e.svc.RecordStatus(ctx(), "o", "r", sha, "spindle", ReportInput{
		Context: "threads/ci/other", State: StateInProgress,
	}); err != nil {
		t.Fatalf("trusted in_progress: %v", err)
	}
}

func TestCombinedStatePrecedence(t *testing.T) {
	cases := []struct {
		states []string
		want   string
	}{
		{nil, StatePending},
		{[]string{StateSuccess}, StateSuccess},
		{[]string{StateSuccess, StateCancelled}, StateCancelled},
		{[]string{StateCancelled, StateSuccess}, StateCancelled},
		{[]string{StateFailure, StateCancelled}, StateFailure},
		{[]string{StateCancelled, StateInProgress}, StateInProgress},
		{[]string{StatePending, StateInProgress}, StateInProgress},
		{[]string{StateError, StateFailure}, StateError},
		{[]string{StateSuccess, StateSkipped}, StateSkipped},
		{[]string{StateSkipped, StateSuccess}, StateSkipped},
		{[]string{StateSkipped, StateCancelled}, StateCancelled},
		{[]string{StateSkipped, StateFailure}, StateFailure},
	}
	for _, tc := range cases {
		if got := combinedState(tc.states); got != tc.want {
			t.Errorf("combinedState(%v) = %q, want %q", tc.states, got, tc.want)
		}
	}
}

func TestGateRefusesCancelledAndInProgress(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(52)
	e.knowSHA(sha)
	e.putPolicy(t, `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["threads/ci/build"]}}}]}`)
	e.mustReport(t, sha, "threads/ci/build", StateInProgress)
	err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "(in_progress)") {
		t.Fatalf("in_progress gate = %v", err)
	}
	e.mustReport(t, sha, "threads/ci/build", StateCancelled)
	err = e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "(cancelled)") {
		t.Fatalf("cancelled gate = %v", err)
	}
	e.mustReport(t, sha, "threads/ci/build", StateSkipped)
	err = e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "(skipped)") {
		t.Fatalf("skipped gate = %v", err)
	}
	e.mustReport(t, sha, "threads/ci/build", StateSuccess)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("success gate: %v", err)
	}
}
