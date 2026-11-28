package checks

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/store"
)

// require_checks merge-time gate (05 §6): union across matching rules,
// verbatim refusal, bypass semantics, fail-closed parse.

func TestGatePassesWithoutRules(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(40)
	e.knowSHA(sha)
	e.mustReport(t, sha, "ci/build", StateSuccess)
	// No policy ⇒ pass.
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("absent policy: %v", err)
	}
	// Plain protect ⇒ pass.
	e.putPolicy(t, `{"version":1,"rules":[{"name":"plain","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"]}}}]}`)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("plain: %v", err)
	}
}

func TestGateVerbatimRefusal(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(41)
	e.knowSHA(sha)
	e.putPolicy(t, `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","delete","force-push"],"require_checks":["ci/build","lint"]}}}]}`)
	e.mustReport(t, sha, "ci/build", StateFailure)
	// lint missing, ci/build failing — verbatim message, sorted offenders.
	err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil {
		t.Fatal("gate passed")
	}
	want := "merge refused: required checks not green for " + sha + ": ci/build (failure), lint (missing)"
	if err.Error() != want {
		t.Fatalf("got  %q\nwant %q", err.Error(), want)
	}
	// Green both ⇒ pass.
	e.mustReport(t, sha, "ci/build", StateSuccess)
	e.mustReport(t, sha, "lint", StateSuccess)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("green: %v", err)
	}
	// Pending counts as not-green (named state, not missing).
	e.mustReport(t, sha, "lint", StatePending)
	err = e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "lint (pending)") {
		t.Fatalf("pending: %v", err)
	}
}

func TestGateUnionAndBypass(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(42)
	e.knowSHA(sha)
	e.mustReport(t, sha, "a", StateSuccess)
	e.mustReport(t, sha, "b", StateSuccess)
	// Union across matching rules: both a and b required.
	e.putPolicy(t, `{"version":1,"rules":[
	  {"name":"r1","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["a"]}}},
	  {"name":"r2","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["b"]}}}]}`)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("union green: %v", err)
	}
	// A rule on another ref does not contribute.
	e.putPolicy(t, `{"version":1,"rules":[
	  {"name":"r1","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["a"]}}},
	  {"name":"other","match":{"refs":["refs/heads/dev"]},"effect":{"protect":{"restricts":["update"],"require_checks":["zzz"]}}}]}`)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err != nil {
		t.Fatalf("scoped: %v", err)
	}
	// Bypass lists govern direct pushes only: a merger the rule's bypass
	// admits is STILL gated at merge time (03 §5 step 4).
	e.putPolicy(t, `{"version":1,"rules":[
	  {"name":"r1","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["zzz"],"bypass":["merger@example.com"]}}}]}`)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err == nil || !strings.Contains(err.Error(), "zzz (missing)") {
		t.Fatalf("bypassed merger must still be gated: %v", err)
	}
	// Partial bypass: the non-bypassed rule gates as well (union of all).
	e.putPolicy(t, `{"version":1,"rules":[
	  {"name":"r1","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["a"],"bypass":["merger@example.com"]}}},
	  {"name":"r2","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["zzz"]}}}]}`)
	err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err == nil || !strings.Contains(err.Error(), "zzz (missing)") {
		t.Fatalf("partial bypass: %v", err)
	}
}

func TestGateFailClosed(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(43)
	// Unparseable policy fails closed.
	e.putPolicy(t, `{oops`)
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", sha, "refs/heads/main", "m"); err == nil {
		t.Fatal("corrupt policy passed")
	}
	// Bad sha fails before any read.
	if err := e.svc.CheckRequiredChecks(ctx(), "o", "r", "abc", "refs/heads/main", "m"); err == nil {
		t.Fatal("bad sha passed")
	}
}

// TestRequiredChecksProgress is the merge queue's polling read (the Merge
// queue card): the same gate, with the pending/decided split a queue needs to
// wait rather than eject.
func TestRequiredChecksProgress(t *testing.T) {
	e := newTestEnv()
	sha := hexSHA(48)
	e.knowSHA(sha)
	// No policy: OK, no required contexts, not pending.
	p, err := e.svc.RequiredChecksProgress(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err != nil || !p.OK || len(p.Required) != 0 || p.Pending {
		t.Fatalf("no policy = %+v, %v", p, err)
	}
	e.putPolicy(t, `{"version":1,"rules":[{"name":"main-needs-ci","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["update"],"require_checks":["ci/build","lint"]}}}]}`)
	// Missing contexts are pending (wait, do not eject).
	p, err = e.svc.RequiredChecksProgress(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if err != nil || p.OK || !p.Pending || len(p.Offenders) != 2 {
		t.Fatalf("missing = %+v, %v", p, err)
	}
	// A pending state is pending.
	e.mustReport(t, sha, "ci/build", StatePending)
	p, _ = e.svc.RequiredChecksProgress(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if !p.Pending || p.OK {
		t.Fatalf("pending = %+v", p)
	}
	// A decided failure is not pending (eject now).
	e.mustReport(t, sha, "ci/build", StateFailure)
	e.mustReport(t, sha, "lint", StateSuccess)
	p, _ = e.svc.RequiredChecksProgress(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if p.Pending || p.OK || len(p.Offenders) != 1 || !strings.Contains(p.Offenders[0], "ci/build (failure)") {
		t.Fatalf("failure = %+v", p)
	}
	// All green is OK.
	e.mustReport(t, sha, "ci/build", StateSuccess)
	p, _ = e.svc.RequiredChecksProgress(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com")
	if !p.OK || p.Pending || len(p.Offenders) != 0 {
		t.Fatalf("green = %+v", p)
	}
	// An unparseable policy fails closed.
	if _, err := store.PutBytes(ctx(), e.store, "repos/o/r/policy.json", []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	if _, err := e.svc.RequiredChecksProgress(ctx(), "o", "r", sha, "refs/heads/main", "merger@example.com"); err == nil {
		t.Fatal("corrupt policy must fail closed")
	}
	// A bad sha is invalid.
	if _, err := e.svc.RequiredChecksProgress(ctx(), "o", "r", "nope", "refs/heads/main", "m"); err == nil {
		t.Fatal("bad sha")
	}
}
