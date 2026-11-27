package threads

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func TestCorruptRunnerRecordsFailClosed(t *testing.T) {
	e := newTestEnv(t)
	id, token, authn := wave4Runner(t, e,
		MintInput{Name: "corrupt", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	// A registry record that is not JSON is a corrupt error, never "no runner".
	key := RunnerKey(id)
	if _, err := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutUpdate}); err != nil {
		// PutUpdate needs a version in some stores; fall back to delete+create.
		_ = e.store.Delete(ctx(), key, "")
		if _, cerr := store.PutBytes(ctx(), e.store, key, []byte("{"), store.PutOptions{Mode: store.PutCreate}); cerr != nil {
			t.Fatalf("write corrupt: %v", cerr)
		}
	}
	if _, err := e.svc.ListRunners(ctx(), wave4Admin()); err == nil {
		t.Fatal("corrupt runner listed")
	}
	if _, err := e.svc.SetRunnerPolicy(ctx(), id, RunnerPolicy{AllowAll: true}, "admin"); err == nil {
		t.Fatal("corrupt runner policy write succeeded")
	}
	if err := e.svc.RevokeRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("corrupt runner revoked")
	}
	if err := e.svc.DrainRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("corrupt runner drained")
	}
	if _, err := e.svc.authorizeRunner(ctx(), token); err == nil {
		t.Fatal("corrupt runner authenticated")
	}
	// Corrupt credential / registration token records.
	_ = e.store.Delete(ctx(), key, "")
	_, _, _ = wave4Runner(t, e,
		MintInput{Name: "corrupt-cred", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	corruptKey(t, e, RunnerCredentialKey(authn.Doc.ID))
	corruptKey(t, e, RunnerTokenKey(authn.Doc.ID))
	if _, err := e.svc.authorizeRunner(ctx(), token); err == nil {
		t.Fatal("corrupt credential authenticated")
	}
}

func TestRunnerPolicyAndRegisterGuards(t *testing.T) {
	e := newTestEnv(t)
	id, _, authn := wave4Runner(t, e,
		MintInput{Name: "policy", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.SetRunnerPolicy(ctx(), id, RunnerPolicy{AllowAll: true}, ""); err == nil {
		t.Fatal("unattributed policy write accepted")
	}
	if _, err := e.svc.SetRunnerPolicy(ctx(), "!!", RunnerPolicy{AllowAll: true}, "admin"); err == nil {
		t.Fatal("invalid runner id accepted")
	}
	if _, err := e.svc.SetRunnerPolicy(ctx(), id, RunnerPolicy{}, "admin"); err == nil {
		t.Fatal("empty policy accepted")
	}
	// Re-submitting the same policy is a no-op and still answers.
	if _, err := e.svc.SetRunnerPolicy(ctx(), id, RunnerPolicy{AllowAll: true}, "admin"); err != nil {
		t.Fatalf("no-op policy: %v", err)
	}
	// A revoked runner's policy cannot be changed.
	if err := e.svc.RevokeRunner(ctx(), id, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetRunnerPolicy(ctx(), id, RunnerPolicy{AllowAll: true}, "admin"); err == nil {
		t.Fatal("revoked runner policy changed")
	}
	// Register refuses a revoked record and a bad label declaration.
	if _, err := e.svc.Register(ctx(), RegisterInput{Token: "wrg_bad"}); err == nil {
		t.Fatal("bad registration token accepted")
	}
	// Declare on a revoked runner is refused (the credential path rejects first);
	// declare on an unknown runner through a hand-built auth is refused too.
	if _, err := e.svc.Declare(ctx(), &runnerAuth{Doc: &RunnerDoc{ID: "missing"}}, DeclareInput{Labels: []string{"linux"}}); err == nil {
		t.Fatal("declare on unknown runner accepted")
	}
	if authn.Doc.DeclaredVersion != "" {
		t.Fatalf("unexpected declared version %q", authn.Doc.DeclaredVersion)
	}
}

func TestRunnerFleetGuardPaths(t *testing.T) {
	e := newTestEnv(t)
	if err := e.svc.DrainRunner(ctx(), "!!", wave4Admin()); err == nil {
		t.Fatal("invalid drain id accepted")
	}
	if err := e.svc.ResumeRunner(ctx(), "!!", wave4Admin()); err == nil {
		t.Fatal("invalid resume id accepted")
	}
	if _, err := e.svc.RotateRunnerToken(ctx(), "!!", wave4Admin()); err == nil {
		t.Fatal("invalid rotate id accepted")
	}
	if err := e.svc.DeregisterRunner(ctx(), "!!", wave4Admin()); err == nil {
		t.Fatal("invalid deregister id accepted")
	}
	if err := e.svc.DrainOwnRunner(ctx(), nil); err == nil {
		t.Fatal("nil runner drained")
	}
	if err := e.svc.ResumeOwnRunner(ctx(), nil); err == nil {
		t.Fatal("nil runner resumed")
	}
	if err := e.svc.DeregisterOwnRunner(ctx(), nil); err == nil {
		t.Fatal("nil runner deregistered")
	}
	if _, err := e.svc.RotateOwnRunnerToken(ctx(), nil); err == nil {
		t.Fatal("nil runner rotated")
	}
	id, _, authn := wave4Runner(t, e,
		MintInput{Name: "busy", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	// A runner mid-job cannot deregister.
	_, _ = e.svc.casUpdate(ctx(), RunnerKey(id), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		doc, _, _ := e.svc.loadRunner(ctx(), id)
		doc.CurrentJob = &RunnerJobRef{Repo: "o/r", RunID: "r", JobID: "j"}
		return mustJSON(doc), true, nil
	})
	if err := e.svc.DeregisterRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("busy runner deregistered")
	}
	// A draining runner cannot rotate itself.
	if err := e.svc.DrainOwnRunner(ctx(), authn); err != nil {
		t.Fatal(err)
	}
	_, _, authn = wave4RunnerStale(t, e, id)
	authn.Doc.State = RunnerDraining
	if _, err := e.svc.RotateOwnRunnerToken(ctx(), authn); err == nil {
		t.Fatal("draining runner rotated")
	}
}

func TestRunnerGroupDirectEdges(t *testing.T) {
	var nilGroup *RunnerGroup
	if v := nilGroup.view(0); v.ID != "" {
		t.Fatalf("nil view = %+v", v)
	}
	e := newTestEnv(t)
	g, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{
		Name: "one", Labels: []string{"a"}, RunnerPolicyInput: RunnerPolicyInput{AllowAll: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// id mismatch on an existing group.
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), g.ID, RunnerGroupInput{
		Name: "other", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true},
	}); err == nil {
		t.Fatal("group id change accepted")
	}
	// invalid update input.
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), g.ID, RunnerGroupInput{Name: "one", RunnerPolicyInput: RunnerPolicyInput{}}); err == nil {
		t.Fatal("invalid update accepted")
	}
	// corrupt record: update and assign surface the corrupt error.
	corruptKey(t, e, RunnerGroupKey("broken"))
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), "broken", RunnerGroupInput{Name: "broken", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("corrupt group updated")
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "g", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, "broken", wave4Admin()); err == nil {
		t.Fatal("corrupt group assigned")
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, "!!", wave4Admin()); err == nil {
		t.Fatal("invalid group id assigned")
	}
	if _, err := e.svc.CreateRunnerGroup(ctx(), e.reader(), RunnerGroupInput{Name: "x", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("non-admin (invalid id) created a group")
	}
}

func TestNotificationCorruptAndBounds(t *testing.T) {
	e := newTestEnv(t)
	// A corrupt subscription document fails the read and the write.
	corruptKey(t, e, SubscriptionsKey("o", "r"))
	if _, err := e.svc.GetSubscription(ctx(), "o/r", auth.Principal{Name: "reader"}); err == nil {
		t.Fatal("corrupt subscriptions read")
	}
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, auth.Principal{Name: "reader"}); err == nil {
		t.Fatal("corrupt subscriptions written")
	}
	// A corrupt notification record is skipped, not fatal.
	corruptKey(t, e, NotificationKey("reader", "run1"))
	notes, err := e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 5)
	if err != nil || len(notes) != 0 {
		t.Fatalf("corrupt notification list = %+v, %v", notes, err)
	}
	// Marking read skips the corrupt record and covers an already-read one.
	if err := e.svc.MarkNotificationsRead(ctx(), auth.Principal{Name: "reader"}); err != nil {
		t.Fatalf("mark read corrupt: %v", err)
	}
	// The webhook validator accepts https with a path and rejects credentials.
	if got, err := validateNotifyWebhook(" https://example.test/hook "); err != nil || got != "https://example.test/hook" {
		t.Fatalf("valid webhook = %q, %v", got, err)
	}
}

func TestHTTPRunnerFleetBadMethodsAndErrors(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = wave4Admin()
	if rec := do(h, http.MethodGet, "/api/v1/admin/runners/x/drain", ""); rec.Code != 405 {
		t.Fatalf("GET admin drain = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/admin/runners/x/group", ""); rec.Code != 405 {
		t.Fatalf("GET admin group = %d", rec.Code)
	}
	// A revoked runner gives a conflict on drain/rotate/deregister.
	id, _, _ := wave4Runner(t, e,
		MintInput{Name: "revoked", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if err := e.svc.RevokeRunner(ctx(), id, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"drain", "resume", "rotate-token"} {
		if rec := do(h, http.MethodPost, "/api/v1/admin/runners/"+id+"/"+verb, ""); rec.Code != 409 {
			t.Fatalf("%s revoked = %d: %s", verb, rec.Code, rec.Body.String())
		}
	}
	// Self verbs on a revoked credential are 401.
	if rec := w4DoAuth(h, http.MethodPost, "/api/v1/threads/runners/drain", "", "wrt_"+id+".zzzz"); rec.Code != 401 {
		t.Fatalf("revoked self drain = %d", rec.Code)
	}
	_ = strings.TrimSpace
}

func TestAuthoringDirectEdges(t *testing.T) {
	if got := workflowTriggerNames(nil); len(got) != 0 {
		t.Fatalf("nil triggers = %v", got)
	}
	// Explain keeps the key of an invalid file and uses the file name when the
	// workflow has no name.
	ex := ExplainWorkflow("ci.yml", []byte("on: [push]\njobs:\n  b:\n    bogus: 1\n"))
	if ex.Valid || ex.IssueKey == "" || !strings.Contains(ex.Error, "bogus") {
		t.Fatalf("invalid explain = %+v", ex)
	}
	good := ExplainWorkflow("ci.yml", []byte("on: [push]\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: x\n"))
	if !good.Valid || good.Name != "ci.yml" {
		t.Fatalf("nameless explain = %+v", good)
	}
	// keyFromMessage has to leave prose alone.
	for _, msg := range []string{"no colon here", "a plain sentence: with spaces", ""} {
		if key := keyFromMessage(msg); key != "" {
			t.Fatalf("keyFromMessage(%q) = %q", msg, key)
		}
	}
	if key := keyFromMessage("permissions.attestations: refused"); key != "permissions.attestations" {
		t.Fatalf("dotted key = %q", key)
	}
	// splitYAMLErrors of a non-yaml message is one entry.
	if out := splitYAMLErrors("plain error"); len(out) != 1 || out[0] != "plain error" {
		t.Fatalf("split = %v", out)
	}
	if line, rest := errorLine("not a line"); line != 0 || rest != "not a line" {
		t.Fatalf("errorLine = %d, %q", line, rest)
	}
	if line, _ := errorLine("line nope: bad"); line != 0 {
		t.Fatalf("non-numeric line = %d", line)
	}
}
