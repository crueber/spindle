package pulls

import (
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/checks"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/server/auth"
)

// The standard team workflow acceptance matrix: protect main from direct
// pushes AND require a green check for merges. It had no valid
// configuration — the `protect` update restriction refused the merge
// itself, and a bypass-listed merger skipped the checks gate. The split
// (03 §5 step 4): `restricts`/`bypass` are the push half; `require_checks`
// is the merge half and always applies to the merging actor.

const (
	noBypassPolicy = `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update"],"require_checks":["threads/ci/lint"]}}}]}`
	bypassPolicy   = `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update"],"require_checks":["threads/ci/lint"],"bypass":["bypasser@example.com"]}}}]}`
)

func protectionEnv(t *testing.T, doc string) (*testEnv, *checks.Service) {
	t.Helper()
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	e.roles.Roles["bypasser@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	putPolicy(t, e, doc)
	cs := checks.New(e.store, e.roles)
	cs.Commits = fakeCommits{}
	e.svc.Checks = cs
	return e, cs
}

func reportCI(t *testing.T, cs *checks.Service, sha, state string) {
	t.Helper()
	if _, err := cs.ReportStatus(ctx(), "o", "r", sha, auth.Principal{Name: "jane@example.com", Write: true}, "",
		checks.ReportInput{Context: "threads/ci/lint", State: state}); err != nil {
		t.Fatalf("report %s: %v", state, err)
	}
}

func runMergeTask(t *testing.T, e *testEnv, actor auth.Principal) *TaskRecord {
	t.Helper()
	if _, err := e.svc.StartMerge(ctx(), "o", "r", 1, actor, MergeInput{Strategy: StrategyMerge}, "corr-matrix"); err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	rec := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
	// Wait for the task table to release the (repo, kind) slot: state flips
	// before the deferred end(), and a second StartMerge inside that window
	// would JOIN the finished run instead of starting a new one.
	deadline := time.Now().Add(5 * time.Second)
	for rec != nil && rec.Finished == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		rec = e.svc.MergeTask("o", "r")
	}
	return rec
}

func TestProtectionWorkflowMatrix(t *testing.T) {
	t.Run("protected main + required check green + empty bypass merges", func(t *testing.T) {
		e, cs := protectionEnv(t, noBypassPolicy)
		reportCI(t, cs, hexSHA(2), checks.StateSuccess)
		done := runMergeTask(t, e, maintainer())
		if done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 1 {
			t.Fatalf("base not published: %+v", e.refs.updatesFor("refs/heads/main"))
		}
	})
	t.Run("protected main + required check pending refuses with the context", func(t *testing.T) {
		e, cs := protectionEnv(t, noBypassPolicy)
		reportCI(t, cs, hexSHA(2), checks.StatePending)
		done := runMergeTask(t, e, maintainer())
		if done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
		want := "merge refused: required checks not green for " + hexSHA(2) + ": threads/ci/lint (pending)"
		if !strings.Contains(done.Error, want) {
			t.Fatalf("refusal %q misses %q", done.Error, want)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatalf("base published despite pending checks: %+v", e.refs.updatesFor("refs/heads/main"))
		}
	})
	t.Run("bypass actor pushes directly but the merge is still check-gated", func(t *testing.T) {
		e, cs := protectionEnv(t, bypassPolicy)
		doc, err := policy.Parse([]byte(bypassPolicy))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		// Direct push: the bypass list admits the actor (push half).
		if v := policy.Evaluate(ctx(), doc, policy.Request{Principal: "bypasser@example.com", Ref: "refs/heads/main", Op: policy.OpUpdate}); !v.Allow {
			t.Fatalf("bypass actor's direct push refused: %+v", v)
		}
		// Merge: the same actor is still gated (merge half).
		reportCI(t, cs, hexSHA(2), checks.StatePending)
		done := runMergeTask(t, e, auth.Principal{Name: "bypasser@example.com"})
		if done == nil || done.State != TaskError || !strings.Contains(done.Error, "threads/ci/lint (pending)") {
			t.Fatalf("bypass actor merged past a pending check: %+v", done)
		}
		reportCI(t, cs, hexSHA(2), checks.StateSuccess)
		done = runMergeTask(t, e, auth.Principal{Name: "bypasser@example.com"})
		if done == nil || done.State != TaskOK {
			t.Fatalf("green check must merge: %+v", done)
		}
	})
	t.Run("non-bypass actor's direct push is refused by the protect rule", func(t *testing.T) {
		doc, err := policy.Parse([]byte(bypassPolicy))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		v := policy.Evaluate(ctx(), doc, policy.Request{Principal: "merger@example.com", Ref: "refs/heads/main", Op: policy.OpUpdate})
		if v.Allow || v.Rule != "main-needs-ci" {
			t.Fatalf("non-bypass push verdict = %+v", v)
		}
	})
	t.Run("a rule without require_checks behaves as before", func(t *testing.T) {
		e, _ := protectionEnv(t, `{"version":1,"rules":[{"name":"no-direct-main","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}]}`)
		done := runMergeTask(t, e, maintainer())
		if done == nil || done.State != TaskOK {
			t.Fatalf("plain protect rule must not block the merge: %+v", done)
		}
	})
	t.Run("the check gate reads the live head sha, not pr.json", func(t *testing.T) {
		e, cs := protectionEnv(t, noBypassPolicy)
		// The stored PR head still points at hexSHA(2); the branch moved.
		live := hexSHA(9)
		e.seedRefs("o/r", map[string]string{
			"refs/heads/main":  hexSHA(1),
			"refs/heads/topic": live,
			"refs/pull/1/head": live,
		})
		reportCI(t, cs, hexSHA(2), checks.StateSuccess) // stale head: green
		reportCI(t, cs, live, checks.StatePending)      // live head: pending
		done := runMergeTask(t, e, maintainer())
		if done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
		if !strings.Contains(done.Error, "required checks not green for "+live) {
			t.Fatalf("gate used a cached head: %q", done.Error)
		}
	})
}
