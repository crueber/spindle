package threads

import (
	"testing"
)

func TestClaimEntryGuardTable(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs()
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventDispatch, CreateOpts{Branch: "main", Ref: "refs/heads/main", Workflow: "ci.yml"}); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.svc.loadHead(ctx(), "o", "r")
	if head == nil || len(head.Pending) == 0 {
		t.Skip("no pending entry")
	}
	entry, ver, err := e.svc.loadEntry(ctx(), "o", "r", head.Pending[0])
	if err != nil || entry == nil {
		t.Fatalf("entry: %v", err)
	}
	cases := []struct {
		name string
		doc  *RunnerDoc
	}{
		{"tenancy", &RunnerDoc{ID: "t", Repos: []string{"other/*"}, Labels: []string{"linux"}}},
		{"group visibility", &RunnerDoc{ID: "g", AllowAll: true, Group: "grp", GroupRepos: []string{"other/*"}, Labels: []string{"linux"}}},
		{"draining", &RunnerDoc{ID: "d", AllowAll: true, State: RunnerDraining, Labels: []string{"linux"}}},
	}
	for _, tc := range cases {
		got, cerr := e.svc.claimEntry(ctx(), "o", "r", entry, ver, tc.doc)
		if cerr != nil || got != nil {
			t.Fatalf("%s: claim = %+v, %v", tc.name, got, cerr)
		}
	}
	var nilDoc *RunnerDoc
	if nilDoc.EffectiveLabels() != nil || nilDoc.IsDraining() || nilDoc.CanServeRepo("o/r") || nilDoc.AllowsRepo("o/r") {
		t.Fatal("nil runner doc answered a capability question")
	}
}
