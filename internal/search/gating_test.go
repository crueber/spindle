// gating_test.go — the permission matrix of docs/features/20_search.md §8:
// every candidate passes the one read gate, a denied repository contributes no
// name / count / total / error text, and the API's 401/403 distinction
// survives the service boundary.
package search

import (
	"context"
	"errors"
	"testing"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// gatingService is a repo-scope service whose gate is the matrix double and
// whose grep always answers one match.
func gatingService(t *testing.T, access *matrixAccess) *Service {
	t.Helper()
	g := &fakeGit{fn: func(context.Context, *git.LocalRepo, string, git.GrepQuery) (*git.GrepResult, error) {
		return &git.GrepResult{Files: []git.GrepFile{{Path: "a.go", Matches: []git.GrepMatch{{Line: 1, Column: 1, Text: "hit"}}}},
			GrepStats: git.GrepStats{FileCount: 1, Matches: 1}}, nil
	}}
	return New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/serving/acme/app"}, Ref: "main", SHA: "abc"}), Access: access})
}

func TestGatingMatrixRepoScope(t *testing.T) {
	tests := []struct {
		name    string
		public  bool
		role    bool
		p       auth.Principal
		want    error // nil = allowed
		message string
	}{
		{name: "anonymous public", public: true, p: anon},
		{name: "anonymous private", p: anon, want: ErrUnauthorized, message: "authentication required"},
		{name: "signed-in with read", role: true, p: user},
		{name: "signed-in public", public: true, p: user},
		{name: "signed-in denied", p: user, want: ErrForbidden, message: "read access required"},
		{name: "admin private", p: adm},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			access := &matrixAccess{public: map[string]bool{"acme/app": tc.public}, role: map[string]bool{"acme/app": tc.role}}
			svc := gatingService(t, access)
			ans, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "hit"}, tc.p)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				if err.Error() != tc.message {
					t.Fatalf("message = %q, want %q", err.Error(), tc.message)
				}
				if ans != nil {
					t.Fatalf("denied request returned an answer: %+v", ans)
				}
				return
			}
			if err != nil {
				t.Fatalf("allowed request failed: %v", err)
			}
			if ans.Code.Results[0].MatchCount != 1 {
				t.Fatalf("result = %+v", ans.Code.Results[0])
			}
		})
	}
}

func TestGatingMatrixOwnerScope(t *testing.T) {
	// One public, one readable-but-private, one denied-private — the caller
	// must see exactly the readable set, and no count may mention the third.
	access := &matrixAccess{
		public: map[string]bool{"acme/pub": true},
		role:   map[string]bool{"acme/mine": true},
	}
	repos := &fakeRepos{names: []string{"pub", "mine", "secret"}, warm: ownerWarm(true)}
	svc := New(Options{Git: ownerGit(map[string]int{"/serving/acme/pub": 1}), Repos: repos, Access: access})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "hit", Type: "code"}, user)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ans.Code.Results) != 2 {
		t.Fatalf("results = %+v, want pub and mine only", ans.Code.Results)
	}
	for _, r := range ans.Code.Results {
		if r.Repo == "acme/secret" {
			t.Fatal("denied repository leaked")
		}
	}
	if ans.Code.ReposConsidered != 2 || ans.Code.ReposSearched != 2 {
		t.Fatalf("counts = %d/%d, want 2/2 (no denied repository anywhere)", ans.Code.ReposConsidered, ans.Code.ReposSearched)
	}
	if !ans.Code.Complete {
		t.Fatal("the visible set answered fully")
	}

	// The same owner as anonymous: only the public repository remains.
	repos = &fakeRepos{names: []string{"pub", "mine", "secret"}, warm: ownerWarm(true)}
	svc = New(Options{Git: ownerGit(nil), Repos: repos, Access: access})
	ans, err = svc.Search(context.Background(), ownerScope("acme"), Params{Text: "hit", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ans.Code.Results) != 1 || ans.Code.Results[0].Repo != "acme/pub" {
		t.Fatalf("anonymous owner results = %+v", ans.Code.Results)
	}
	if ans.Code.ReposConsidered != 1 {
		t.Fatalf("anonymous considered = %d, want 1", ans.Code.ReposConsidered)
	}
}

func TestGatingFailsClosedWithoutAGate(t *testing.T) {
	// A service built without the identity seam must refuse, not allow: an
	// unwired route is a 503, never an open repository.
	svc := New(Options{Git: &fakeGit{}, Repos: realRepos(Target{})})
	_, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x"}, anon)
	if !errors.Is(err, ErrUnavailable) || err.Error() != "access check unavailable" {
		t.Fatalf("err = %v, want the fail-closed 503", err)
	}
	// The seam returns *auth.AuthError, never error, so the identity
	// boundary's "typed nil is not a nil error" trap cannot reach this far:
	// a nil result is the allow path and nothing else.
}

func TestGatingOrderRepoScope(t *testing.T) {
	// §8: the read gate runs BEFORE query validation, so an unauthorized
	// caller learns nothing about the query grammar.
	access := &matrixAccess{public: map[string]bool{}}
	svc := gatingService(t, access)
	_, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: ""}, anon)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want the gate's 401 (not the query's 400)", err)
	}
	if len(access.calls) != 1 {
		t.Fatalf("gate calls = %v", access.calls)
	}
}

func TestGatingOrderOwnerScope(t *testing.T) {
	// §8: owner scope validates the parameters before paying a listing.
	repos := &fakeRepos{names: []string{"a"}, warm: ownerWarm(true)}
	access := &matrixAccess{public: map[string]bool{"acme/a": true}}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: access})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "bogus"}, anon); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want a 400", err)
	}
	if repos.listCount() != 0 {
		t.Fatalf("a bad query paid %d listings", repos.listCount())
	}
	if len(access.calls) != 0 {
		t.Fatalf("a bad query paid %d gates", len(access.calls))
	}
	// An unknown owner name is rejected before any work too.
	if _, err := svc.Search(context.Background(), ownerScope("../etc"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("bad owner: err = %v", err)
	}
	if repos.listCount() != 0 {
		t.Fatalf("a bad owner paid a listing")
	}
}
