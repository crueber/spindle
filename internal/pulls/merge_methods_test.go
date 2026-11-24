package pulls

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/policy"
)

// PLAN-V2 §2.2/§2.5.2: merge_methods and require_up_to_date are enforced at
// the merge task (never the UI only).

// methodPolicy builds a one-rule policy restricting the methods on main.
func methodPolicy(t *testing.T, e *testEnv, body string) {
	t.Helper()
	putPolicy(t, e, `{"version":1,"rules":[{"name":"release-gate","match":{"refs":["refs/heads/main"]},"effect":{"protect":{`+body+`}}}]}`)
}

func mergeEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv()
	e.roles.Roles["jane@example.com"] = "write"
	e.roles.Roles["merger@example.com"] = "maintain"
	openBasic(t, e, "o", "r")
	seedMergeable(t, e, hexSHA(1), hexSHA(2))
	return e
}

func TestMergeMethodGate(t *testing.T) {
	t.Run("each forbidden method is refused on the request path", func(t *testing.T) {
		for _, tc := range []struct{ allowed, forbidden []string }{
			{[]string{"squash"}, []string{StrategyMerge, StrategyRebase}},
			{[]string{"merge"}, []string{StrategySquash, StrategyRebase}},
			{[]string{"rebase"}, []string{StrategyMerge, StrategySquash}},
			{[]string{"merge", "squash"}, []string{StrategyRebase}},
		} {
			for _, method := range tc.forbidden {
				e := mergeEnv(t)
				methodPolicy(t, e, `"merge_methods":["`+strings.Join(tc.allowed, `","`)+`"]`)
				_, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: method}, "")
				if !errors.Is(err, ErrMethodNotAllowed) || !errors.Is(err, ErrConflict) {
					t.Fatalf("allowed=%v method=%s: err = %v", tc.allowed, method, err)
				}
				// The refusal names the method, the rule and the allowed set.
				msg := err.Error()
				for _, want := range []string{method, "release-gate", strings.Join(tc.allowed, ", "), "allowed:"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("message %q misses %q", msg, want)
					}
				}
				if len(e.refs.updatesFor("refs/heads/main")) != 0 {
					t.Fatal("a refused method must never publish")
				}
				if e.svc.MergeTask("o", "r") != nil {
					t.Fatal("a refused method must not start a task")
				}
			}
		}
	})
	t.Run("HTTP maps the refusal to 409", func(t *testing.T) {
		err := checkMergeMethod(mustGate(t, `"merge_methods":["squash"]`), StrategyMerge, "refs/heads/main")
		if statusFor(err) != 409 {
			t.Fatalf("status = %d (%v)", statusFor(err), err)
		}
	})
	t.Run("each permitted method still merges", func(t *testing.T) {
		for _, method := range []string{StrategyMerge, StrategySquash, StrategyRebase} {
			e := mergeEnv(t)
			// Separate rules per method would only narrow the set, so the
			// permitted set is always one method here: the rule names
			// exactly the method under test.
			methodPolicy(t, e, `"merge_methods":["`+method+`"]`)
			rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: method}, "")
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			_ = rec
			done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
			if done == nil || done.State != TaskOK {
				t.Fatalf("%s: task = %+v", method, done)
			}
			if pr, _, _ := e.svc.loadPR(ctx(), "o", "r", 1); !pr.Merged || *pr.MergeStrategy != method {
				t.Fatalf("%s: pr = %+v", method, pr)
			}
		}
	})
	t.Run("no rule restricts the set", func(t *testing.T) {
		e := mergeEnv(t)
		methodPolicy(t, e, `"restricts":["update"]`)
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyRebase}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("task re-checks under itself (policy changed after the request)", func(t *testing.T) {
		e := mergeEnv(t)
		// StartMerge sees an unrestricted policy; the task then reads a
		// narrowed one — the task's own read is authoritative.
		putPolicy(t, e, `{"version":1,"rules":[{"name":"r","match":{"refs":["refs/heads/main"]},"effect":{"protect":{}}}]}`)
		rec := &TaskRecord{Progress: []string{}}
		methodPolicy(t, e, `"merge_methods":["squash"]`)
		_, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec)
		if !errors.Is(err, ErrMethodNotAllowed) {
			t.Fatalf("task err = %v", err)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatal("refused method published")
		}
	})
	t.Run("bypass does not exempt the method set", func(t *testing.T) {
		e := mergeEnv(t)
		// merge_methods is an offer, not a verdict: the plan declares the
		// intersection per matching rule, and bypass governs up-to-date.
		methodPolicy(t, e, `"merge_methods":["squash"],"bypass":["merger@example.com"]`)
		_, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if !errors.Is(err, ErrMethodNotAllowed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid strategy stays a 400", func(t *testing.T) {
		e := mergeEnv(t)
		_, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: "octopus"}, "")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown policy read fails the request closed", func(t *testing.T) {
		e := mergeEnv(t)
		e.failGet(PolicyKey("o", "r"), errors.New("bucket down"))
		if _, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, ""); err == nil {
			t.Fatal("a policy read failure must refuse the merge")
		}
	})
}

func mustGate(t *testing.T, body string) policy.MergeGate {
	t.Helper()
	e := newTestEnv()
	putPolicy(t, e, `{"version":1,"rules":[{"name":"r","match":{"refs":["refs/heads/main"]},"effect":{"protect":{`+body+`}}}]}`)
	gate, err := e.svc.loadMergeGate(ctx(), "o", "r", "refs/heads/main", "merger@example.com")
	if err != nil {
		t.Fatalf("loadMergeGate: %v", err)
	}
	return gate
}

func TestMergeUpToDateGate(t *testing.T) {
	behind := func(t *testing.T) *testEnv {
		t.Helper()
		e := mergeEnv(t)
		// A base tip the head does not contain: the stamp says "behind",
		// and the ancestry answer is false (FakeGit's default).
		e.git.Behind = 3
		methodPolicy(t, e, `"require_up_to_date":true`)
		return e
	}
	t.Run("behind head is refused with the named error", func(t *testing.T) {
		e := behind(t)
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") })
		if done == nil || done.State != TaskError {
			t.Fatalf("task = %+v", done)
		}
		if !strings.Contains(done.Error, "head is behind base — update the branch first") {
			t.Fatalf("task error = %q", done.Error)
		}
		if !strings.Contains(done.Error, "release-gate") || !strings.Contains(done.Error, "refs/heads/main") {
			t.Fatalf("task error must name the rule and the ref: %q", done.Error)
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatal("behind head must not publish")
		}
		// The refusal is narrated on the task stream (law 7).
		found := false
		for _, p := range done.Progress {
			if strings.Contains(p, "up-to-date gate:") {
				found = true
			}
		}
		if !found {
			t.Fatalf("progress = %v", done.Progress)
		}
	})
	t.Run("update branch then merge lands", func(t *testing.T) {
		e := behind(t)
		// The update-branch task succeeds (base ancestor of head)*; then the
		// head contains the base and the merge is allowed.
		e.git.Ancestors[hexSHA(1)+"\x00"+hexSHA(2)] = true
		if _, err := e.svc.UpdateBranch(ctx(), "o", "r", 1, writer(), ""); err != nil {
			t.Fatalf("UpdateBranch: %v", err)
		}
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done != nil {
			t.Fatalf("a no-op update-branch must not start a merge task: %+v", done)
		}
		// The head now contains the base: the gate opens.
		e.git.Behind = 0
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("StartMerge after update: %v", err)
		}
		_ = rec
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("a bypassed rule does not demand up-to-date", func(t *testing.T) {
		e := mergeEnv(t)
		e.git.Behind = 3
		methodPolicy(t, e, `"require_up_to_date":true,"bypass":["merger@example.com"]`)
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done == nil || done.State != TaskOK {
			t.Fatalf("bypassed up-to-date must merge: %+v", done)
		}
	})
	t.Run("require_up_to_date with a current head merges", func(t *testing.T) {
		e := mergeEnv(t)
		methodPolicy(t, e, `"require_up_to_date":true`)
		// The head contains the base tip (the normal clean-PR shape).
		e.git.Ancestors[hexSHA(1)+"\x00"+hexSHA(2)] = true
		rec, err := e.svc.StartMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategySquash}, "")
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		_ = rec
		if done := waitTask(5*time.Second, func() *TaskRecord { return e.svc.MergeTask("o", "r") }); done == nil || done.State != TaskOK {
			t.Fatalf("task = %+v", done)
		}
	})
	t.Run("ancestry outage fails the task, never publishes", func(t *testing.T) {
		e := mergeEnv(t)
		methodPolicy(t, e, `"require_up_to_date":true`)
		e.git.IsAncestorErr = errors.New("git pool down")
		rec := &TaskRecord{Progress: []string{}}
		_, err := e.svc.runMerge(ctx(), "o", "r", 1, maintainer(), MergeInput{Strategy: StrategyMerge}, "", rec)
		if err == nil {
			t.Fatal("an ancestry outage must fail the merge")
		}
		if len(e.refs.updatesFor("refs/heads/main")) != 0 {
			t.Fatal("outage published")
		}
	})
	t.Run("checkMergeUpToDate helper", func(t *testing.T) {
		if err := checkMergeUpToDate(mustGate(t, `"require_up_to_date":true`), true, "refs/heads/main"); err != nil {
			t.Fatalf("contained: %v", err)
		}
		if err := checkMergeUpToDate(mustGate(t, `"restricts":["update"]`), false, "refs/heads/main"); err != nil {
			t.Fatalf("not required: %v", err)
		}
		err := checkMergeUpToDate(mustGate(t, `"require_up_to_date":true`), false, "refs/heads/main")
		if !errors.Is(err, ErrHeadBehindBase) || !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("quoteRules renders names", func(t *testing.T) {
		if got := quoteRules(nil); got != "policy" {
			t.Fatalf("empty = %q", got)
		}
		if got := quoteRules([]string{"a", "b"}); got != "'a', 'b'" {
			t.Fatalf("two = %q", got)
		}
	})
}

func TestMergeGateForPrincipalScopingTask(t *testing.T) {
	// A rule scoped to another principal never narrows this merger's set.
	e := mergeEnv(t)
	putPolicy(t, e, `{"version":1,"rules":[{"name":"bots","match":{"refs":["refs/heads/main"],"principals":["bot"]},"effect":{"protect":{"merge_methods":["squash"]}}}]}`)
	gate, err := e.svc.loadMergeGate(ctx(), "o", "r", "refs/heads/main", "merger@example.com")
	if err != nil {
		t.Fatalf("loadMergeGate: %v", err)
	}
	if len(gate.Methods) != 0 {
		t.Fatalf("gate = %+v", gate)
	}
	if err := checkMergeMethod(gate, StrategyRebase, "refs/heads/main"); err != nil {
		t.Fatalf("scoped rule must not narrow: %v", err)
	}
}
