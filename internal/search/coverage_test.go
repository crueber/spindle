// coverage_test.go — the remaining branches of the service's contract: the
// scope-id rendering, the two §8 gate orders, the request deadline inside the
// gating loop and the per-repository gate's cancellation path. They are
// separate from the matrix tests so each failure names its rule.
package search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// gateFunc adapts a function to the Access seam (the matrix tests use data).
type gateFunc func(ctx context.Context, owner, repo string) *auth.AuthError

func (f gateFunc) CheckRead(ctx context.Context, owner, repo string, _ auth.Principal) *auth.AuthError {
	return f(ctx, owner, repo)
}

func TestScopeID(t *testing.T) {
	if got := (Scope{Owner: "acme"}).ID(); got != "acme" {
		t.Fatalf("owner scope id = %q", got)
	}
	if got := (Scope{Owner: "acme", Repo: "app"}).ID(); got != "acme/app" {
		t.Fatalf("repo scope id = %q", got)
	}
}

func TestSearchRepoQueryRejectedAfterTheGate(t *testing.T) {
	// §8 step 3→4: an allowed caller with a bad query reaches Parse and gets
	// the 400 (the gate precedes validation, not the other way round).
	svc := New(Options{Git: &fakeGit{}, Repos: realRepos(Target{}), Access: allowAll()})
	_, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: ""}, anon)
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
}

func TestOwnerScopeIssueSections(t *testing.T) {
	// Owner-scope issue search uses the same candidate/read path as code and
	// calls the P4 searcher once per visible repository (no separate
	// visibility logic, no ungated owner-wide scan).
	iss := &fakeIssues{section: IssueSection{Complete: true, Hits: []IssueHit{{Repo: "acme/a", Num: 1, Labels: nil}}}}
	repos := &fakeRepos{names: []string{"a"}}
	svc := New(Options{Repos: repos, Access: allowAll(), Issues: iss})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "crash"}, user)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code != nil {
		t.Fatal("type=issues must not run code")
	}
	if got := iss.scopeOf(); got != (IssueScope{Owner: "acme", Repo: "a"}) {
		t.Fatalf("issue scope = %+v, want acme/a", got)
	}
	if len(ans.Issues.Repos) != 1 || ans.Issues.Repos[0].Status != StatusOK {
		t.Fatalf("issue repos = %+v", ans.Issues.Repos)
	}
	// The never-nil contract covers labels too.
	if ans.Issues.Hits[0].Labels == nil {
		t.Fatal("hit labels is nil")
	}
	// type=all runs both sections, and each issue candidate is enumerated once.
	iss = &fakeIssues{section: IssueSection{Complete: true, Hits: []IssueHit{}}}
	repos = &fakeRepos{names: []string{"a"}, warm: ownerWarm(true)}
	svc = New(Options{Git: ownerGit(nil), Repos: repos, Access: allowAll(), Issues: iss})
	ans, err = svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "all"}, user)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code == nil || ans.Issues == nil {
		t.Fatalf("type=all sections = %+v", ans)
	}
	// One broken repository is a per-repository status, never a whole-request
	// failure that hides the other answers (code decision 7).
	iss = &fakeIssues{err: errors.New("index down")}
	repos = &fakeRepos{names: []string{"a", "b"}}
	svc = New(Options{Repos: repos, Access: allowAll(), Issues: iss})
	ans, err = svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "issues"}, user)
	if err != nil {
		t.Fatalf("per-repo issue failure must not fail the request: %v", err)
	}
	if ans.Issues.Complete || len(ans.Issues.Repos) != 2 {
		t.Fatalf("failed issue section = %+v", ans.Issues)
	}
	for _, r := range ans.Issues.Repos {
		if r.Status != StatusFailed || r.Reason != IssueReasonFailed {
			t.Fatalf("repo = %+v, want failed", r)
		}
	}
}

func TestOwnerScopeUnwiredSeams(t *testing.T) {
	// A code request with no primitive is a 503, before any listing.
	svc := New(Options{Repos: &fakeRepos{}, Access: allowAll()})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil git: err = %v", err)
	}
	svc = New(Options{Git: &fakeGit{}, Access: allowAll()})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil repos: err = %v", err)
	}
}

func TestOwnerScopeCanceledWhileQueuedForARequestSlot(t *testing.T) {
	svc := New(Options{Git: &fakeGit{}, Repos: &fakeRepos{}, Access: allowAll(), MaxConcurrent: 1})
	svc.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := svc.Search(ctx, ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestRepoScopeCanceledBeforeTheGate(t *testing.T) {
	// Resolution succeeded but the caller went away before the grep gate: the
	// request is cancelled, never a partial success.
	ctx, cancel := context.WithCancel(context.Background())
	repos := &fakeRepos{resolve: func(string, string) (Target, error) {
		cancel()
		return Target{Repo: &git.LocalRepo{Path: "/tmp/x"}, Ref: "main", SHA: "abc"}, nil
	}}
	svc := New(Options{Git: &fakeGit{}, Repos: repos, Access: allowAll()})
	if _, err := svc.Search(ctx, repoScope("acme", "app"), Params{Text: "x"}, anon); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestOwnerScopeDeadlineDuringGating(t *testing.T) {
	// The deadline can fire while the per-candidate read gates run; the
	// section then reports complete:false and no candidate is searched.
	names := []string{"a", "b", "c"}
	gate := gateFunc(func(ctx context.Context, _, _ string) *auth.AuthError {
		<-ctx.Done() // the 30ms budget is spent in the first probe
		return &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "authentication required"}
	})
	svc := New(Options{Git: ownerGit(nil), Repos: &fakeRepos{names: names}, Access: gate})
	svc.ownerDeadline = 30 * time.Millisecond
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code.Complete || len(ans.Code.Results) != 0 || ans.Code.ReposConsidered != 0 {
		t.Fatalf("deadline-during-gating section = %+v", ans.Code)
	}
}

func TestOwnerScopeCanceledBeforeTheRepoGate(t *testing.T) {
	// A warm repository whose caller disappears before the gate: the branch
	// answers not_searched and the request is the cancellation (never a
	// silent partial list).
	ctx, cancel := context.WithCancel(context.Background())
	repos := &fakeRepos{names: []string{"a"}, warm: func(context.Context, string) (Target, bool, error) {
		cancel()
		return Target{Repo: &git.LocalRepo{Path: "/serving/a"}, Ref: "main", SHA: "abc"}, true, nil
	}}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: allowAll()})
	if _, err := svc.Search(ctx, ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRepoGatesCancellation(t *testing.T) {
	var g repoGates
	release, err := g.acquire(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.acquire(ctx, "acme/app"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	release()
	release() // idempotent: the once guard keeps the refcount sane
	if len(g.gates) != 0 {
		t.Fatalf("gate map = %d entries, want 0", len(g.gates))
	}
	// A fresh acquire after the gate was dropped works.
	release2, err := g.acquire(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	release2()
}

func TestDeadlineFallback(t *testing.T) {
	var svc Service
	if got := svc.deadline(); got != OwnerDeadline {
		t.Fatalf("deadline = %s, want %s", got, OwnerDeadline)
	}
}

func TestQueryOptionsWithoutPaths(t *testing.T) {
	// A hand-built query (not from Parse) may have nil paths; the wire echo
	// must still be [].
	if paths := (Query{}).options().Paths; paths == nil {
		t.Fatal("options.paths is nil")
	}
}

func TestErrorTextsAreTheWireBodies(t *testing.T) {
	// §10 names these sentences; a transport may send err.Error() verbatim.
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrUnauthorized, "authentication required"},
		{ErrForbidden, "read access required"},
		{ErrWarming, "search is warming this repository; retry in a few seconds"},
		{ErrNotLocal, "code search needs a local serving copy; this repository's pack set exceeds the cache budget"},
		{ErrDisabled, "search is disabled on this instance"},
		{ErrUnsupportedScope, "instance-wide code search is not available; choose an owner"},
	} {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Fatalf("%v does not carry %q", tc.err, tc.want)
		}
	}
}
