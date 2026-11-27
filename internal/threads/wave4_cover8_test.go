package threads

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

func TestLastCoverageBranches(t *testing.T) {
	e := newTestEnv(t)
	// sortQueuePending with no head.
	sortQueuePending(nil)
	// Subscription helpers with an empty principal name (anonymous records with
	// no name are refused rather than keyed by "").
	if _, err := e.svc.GetSubscription(ctx(), "o/r", auth.Principal{}); err != nil {
		t.Fatalf("empty-user read: %v", err)
	}
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, auth.Principal{}); err == nil {
		t.Fatal("empty-user subscription accepted")
	}
	// Notification list limit clamp.
	if _, err := e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 1000); err != nil {
		t.Fatalf("clamped list: %v", err)
	}
	// Rotate a runner whose RegisteredBy is empty (falls back to "runner") and
	// whose credential is missing (not-found), then deregister a deleted id.
	id, _, _ := wave4Runner(t, e,
		MintInput{Name: "last", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	doc, _, _ := e.svc.loadRunner(ctx(), id)
	doc.RegisteredBy = ""
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(id), mustJSON(doc), store.PutOptions{Mode: store.PutUpdate, IfVersion: "0"}); err != nil {
		// Force via delete+create when the version hint is refused.
		_ = e.store.Delete(ctx(), RunnerKey(id), "")
		if _, cerr := store.PutBytes(ctx(), e.store, RunnerKey(id), mustJSON(doc), store.PutOptions{Mode: store.PutCreate}); cerr != nil {
			t.Skipf("cannot rewrite the runner: %v", cerr)
		}
	}
	if rot, err := e.svc.RotateRunnerToken(ctx(), id, wave4Admin()); err != nil || !strings.HasPrefix(rot.Token, runnerTokenPrefix) {
		t.Fatalf("rotate unregistered-by: %+v, %v", rot, err)
	}
	// A deregister of a record that vanished is not-found.
	_ = e.store.Delete(ctx(), RunnerKey(id), "")
	if err := e.svc.DeregisterRunner(ctx(), id, wave4Admin()); err == nil {
		t.Fatal("deregister of a vanished runner succeeded")
	}
	// setRunnerState on a corrupt record fails closed.
	id2, _, _ := wave4Runner(t, e,
		MintInput{Name: "corrupt-state", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	corruptKey(t, e, RunnerKey(id2))
	if err := e.svc.DrainRunner(ctx(), id2, wave4Admin()); err == nil {
		t.Fatal("corrupt record drained")
	}
}
