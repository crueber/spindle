package webui

import (
	"context"
	"strings"
	"testing"

	git "github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/policy"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/review"
	"github.com/crueber/spindle/internal/server/auth"
)

// PLAN-V2 §2.2 page half: the PR merge box offers only the methods the base
// ref's ruleset permits, and a behind head under require_up_to_date is a
// blocker with the update-branch affordance — while the merge POST still
// goes to the server verb (the page is never the gate; the task refuses).

func TestPullMergeBoxOffersPermittedMethods(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}
	d := pullDeps()
	// The fixture's PR predates full ref names; a stored PR always carries
	// refs/heads/... (validateRefName), which is what the policy matches.
	fullRefs(&d, "refs/heads/main", "refs/heads/feat")
	d.PolicyOf = func(context.Context, git.RepoId) (*policy.Document, error) {
		doc, _ := policy.Parse([]byte(`{"version":1,"rules":[
			{"name":"protection:main-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update"],"merge_methods":["squash"]}}}]}`))
		return doc, nil
	}
	f.deps(d)
	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, 200)
	body := rec.Body.String()
	// Only squash is offered, and the page says why.
	if !strings.Contains(body, `<option value="squash" selected>squash</option>`) {
		t.Fatalf("squash option missing:\n%s", body)
	}
	for _, forbidden := range []string{`<option value="merge"`, `<option value="rebase"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("page offered a forbidden method: %s", forbidden)
		}
	}
	if !strings.Contains(body, "protection ruleset allows only squash") {
		t.Fatal("page does not explain the narrowed method set")
	}
}

func TestPullMergeBoxBehindRequiresUpdate(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "alice", Admin: true}
	d := pullDeps()
	fullRefs(&d, "refs/heads/main", "refs/heads/feat")
	base := d.PullGet
	d.PullGet = func(ctx context.Context, owner, repo string, num int, p auth.Principal, after, n int) (*pulls.PullView, error) {
		v, err := base(ctx, owner, repo, num, p, after, n)
		if err != nil {
			return v, err
		}
		v.Mergeable = &pulls.MergeableDoc{State: pulls.MergeableBehind, Conflicts: []string{}}
		return v, nil
	}
	d.PolicyOf = func(context.Context, git.RepoId) (*policy.Document, error) {
		doc, _ := policy.Parse([]byte(`{"version":1,"rules":[
			{"name":"protection:main-push","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"restricts":["create","update"],"require_up_to_date":true}}}]}`))
		return doc, nil
	}
	f.deps(d)
	rec := f.get("/acme/widgets/pull/5")
	wantStatus(t, rec, 200)
	body := rec.Body.String()
	// The merge button is disabled, the refusal is named, and the fix (the
	// update-branch control) is rendered.
	if !strings.Contains(body, "head is behind base — update the branch first") {
		t.Fatal("behind refusal not rendered")
	}
	if !strings.Contains(body, `formaction="/acme/widgets/pull/5/update-branch"`) {
		t.Fatal("update-branch affordance missing")
	}
	if !strings.Contains(body, `disabled title="blocked: head is behind base`) {
		t.Fatalf("merge button not blocked:\n%s", body)
	}
	if !strings.Contains(body, "Update the branch") {
		t.Fatal("mergeability summary does not name the update step")
	}
}

func TestPullMergeBoxBehindWithoutRequirementStillMerges(t *testing.T) {
	t.Parallel()
	// The GitHub-parity half: a behind head is mergeable when no rule
	// demands an up-to-date head.
	view := pullMergeState(nil, &pulls.MergeableDoc{State: pulls.MergeableBehind}, nil, "", nil,
		identity.RoleMaintain, nil, policy.MergeGate{})
	if !view.CanMerge || view.UpdateHint != "" || !view.Behind {
		t.Fatalf("view = %+v", view)
	}
	// With the requirement it blocks, and the viewer keeps the update right.
	gate, _ := policy.Parse([]byte(`{"version":1,"rules":[{"name":"r","match":{"refs":["refs/heads/main"]},"effect":{"protect":{"require_up_to_date":true}}}]}`))
	view = pullMergeState(nil, &pulls.MergeableDoc{State: pulls.MergeableBehind}, nil, "", nil,
		identity.RoleMaintain, nil, policy.MergeGateFor(gate, policy.Request{Principal: "alice", Ref: "refs/heads/main", Op: policy.OpUpdate}))
	if view.CanMerge || !view.CanUpdate || view.State != "blocked" {
		t.Fatalf("view = %+v", view)
	}
	if !strings.Contains(view.BlockersText, "update the branch first") {
		t.Fatalf("blockers = %q", view.BlockersText)
	}
	// A clean head under the same gate merges.
	view = pullMergeState(nil, &pulls.MergeableDoc{State: pulls.MergeableClean}, nil, "", nil,
		identity.RoleMaintain, nil, policy.MergeGateFor(gate, policy.Request{Principal: "alice", Ref: "refs/heads/main", Op: policy.OpUpdate}))
	if !view.CanMerge || view.UpdateHint != "" {
		t.Fatalf("clean view = %+v", view)
	}
	// Changes-requested still wins over the behind hint.
	view = pullMergeState(nil, &pulls.MergeableDoc{State: pulls.MergeableBehind}, nil, review.DecisionChangesRequested, nil,
		identity.RoleMaintain, nil, policy.MergeGate{RequireUpToDate: true})
	if view.State != "blocked" || !strings.Contains(view.BlockersText, "changes requested") {
		t.Fatalf("view = %+v", view)
	}
}

// fullRefs rewrites the fixture's PR endpoints to full ref names (the shape
// a stored PR always has).
func fullRefs(d *CollabDeps, base, head string) {
	get := d.PullGet
	d.PullGet = func(ctx context.Context, owner, repo string, num int, p auth.Principal, after, n int) (*pulls.PullView, error) {
		v, err := get(ctx, owner, repo, num, p, after, n)
		if err != nil || v == nil || v.PR == nil {
			return v, err
		}
		v.PR.Base.Ref, v.PR.Head.Ref = base, head
		return v, nil
	}
}

func TestMergeMethodsFor(t *testing.T) {
	t.Parallel()
	all := mergeMethodsFor(policy.MergeGate{})
	if len(all) != 3 || all[0].Value != "merge" || all[2].Value != "rebase" {
		t.Fatalf("all = %+v", all)
	}
	narrow := mergeMethodsFor(policy.MergeGate{Methods: []string{"squash", "rebase"}})
	if len(narrow) != 2 || narrow[0].Value != "squash" || narrow[1].Label != "rebase" {
		t.Fatalf("narrow = %+v", narrow)
	}
	if mergeMethodLabel("squash") != "squash" || mergeMethodLabel("other") != "merge" {
		t.Fatal("labels")
	}
}
