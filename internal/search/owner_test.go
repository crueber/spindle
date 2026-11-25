// owner_test.go — bounded owner scope: the candidate source, the fan-out caps,
// completeness for every skip (§6.4), the request deadline and cancellation
// (§11).
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
)

// ownerWarm returns a WarmRefs that is ready for every repository and pins the
// sha to the repository name (so results are identifiable).
func ownerWarm(ready bool) func(context.Context, string) (Target, bool, error) {
	return func(_ context.Context, repo string) (Target, bool, error) {
		return Target{Repo: &git.LocalRepo{Path: "/serving/" + repo}, Ref: "main", SHA: "sha-" + repo}, ready, nil
	}
}

func ownerGit(matches map[string]int) *fakeGit {
	return &fakeGit{fn: func(_ context.Context, repo *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		n := matches[repo.Path]
		files := []git.GrepFile{}
		for i := 0; i < n; i++ {
			files = append(files, git.GrepFile{Path: fmt.Sprintf("f%d.go", i), Matches: []git.GrepMatch{{Line: 1, Column: 1, Text: "hit " + repo.Path}}})
		}
		return &git.GrepResult{GrepStats: git.GrepStats{FileCount: n, Matches: n}, Files: files}, nil
	}}
}

func TestOwnerScopeFansOutInNameOrder(t *testing.T) {
	repos := &fakeRepos{names: []string{"zeta", "Alpha", "beta"}, warm: ownerWarm(true)}
	g := ownerGit(map[string]int{"/serving/acme/beta": 2})
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/zeta": true, "acme/Alpha": true, "acme/beta": true}}})

	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "hit", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code == nil || !ans.Code.Complete {
		t.Fatalf("code section = %+v", ans.Code)
	}
	if ans.Code.ReposConsidered != 3 || ans.Code.ReposSearched != 3 {
		t.Fatalf("counts = %d/%d", ans.Code.ReposConsidered, ans.Code.ReposSearched)
	}
	want := []string{"acme/Alpha", "acme/beta", "acme/zeta"} // name ascending
	for i, r := range ans.Code.Results {
		if r.Repo != want[i] {
			t.Fatalf("result %d = %q, want %q (candidate order)", i, r.Repo, want[i])
		}
		if r.Status != StatusOK || !r.Complete || r.Ref != "main" || r.SHA != "sha-"+r.Repo {
			t.Fatalf("result = %+v", r)
		}
	}
	if ans.Code.Results[1].MatchCount != 2 || ans.Code.Results[1].FileCount != 2 {
		t.Fatalf("beta counts = %+v", ans.Code.Results[1])
	}
	if ans.Scope.Kind != ScopeOwner || ans.Scope.Repo != "" {
		t.Fatalf("scope = %+v", ans.Scope)
	}
}

func TestOwnerScopeBoundedCandidates(t *testing.T) {
	names := make([]string, MaxOwnerCandidates+7)
	public := map[string]bool{}
	for i := range names {
		names[i] = fmt.Sprintf("r%02d", i)
		public["acme/"+names[i]] = true
	}
	repos := &fakeRepos{names: names, warm: ownerWarm(true)}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: &matrixAccess{public: public}})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ans.Code.Results) != MaxOwnerCandidates {
		t.Fatalf("results = %d, want the %d-candidate bound", len(ans.Code.Results), MaxOwnerCandidates)
	}
	if ans.Code.Complete {
		t.Fatal("a capped candidate set must report complete:false")
	}
	if ans.Code.ReposConsidered != MaxOwnerCandidates || ans.Code.ReposSearched != MaxOwnerCandidates {
		t.Fatalf("counts = %d/%d", ans.Code.ReposConsidered, ans.Code.ReposSearched)
	}
	// The cap counts read gates too: the 51st name is never probed.
	if got := len(mustAccess(t, svc).calls); got != MaxOwnerCandidates {
		t.Fatalf("CheckRead calls = %d, want %d (bounded probes)", got, MaxOwnerCandidates)
	}
}

// mustAccess returns the matrix gate behind svc (every test builds it inline).
func mustAccess(t *testing.T, svc *Service) *matrixAccess {
	t.Helper()
	m, ok := svc.access.(*matrixAccess)
	if !ok {
		t.Fatalf("access = %T, want *matrixAccess", svc.access)
	}
	return m
}

func TestOwnerScopeDeniedRepositoriesAreAbsent(t *testing.T) {
	repos := &fakeRepos{names: []string{"public", "private"}, warm: ownerWarm(true)}
	access := &matrixAccess{public: map[string]bool{"acme/public": true}}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: access})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 {
		t.Fatalf("denied repository leaked into the counts: %+v", ans.Code)
	}
	for _, r := range ans.Code.Results {
		if r.Repo == "acme/private" {
			t.Fatal("denied repository appears in results")
		}
	}
	if !ans.Code.Complete {
		t.Fatal("a fully searched visible set is complete")
	}
}

func TestOwnerScopeWarmOnly(t *testing.T) {
	repos := &fakeRepos{names: []string{"cold", "empty"}, warm: func(_ context.Context, repo string) (Target, bool, error) {
		switch repo {
		case "acme/cold":
			return Target{Ref: "main"}, false, nil
		case "acme/empty":
			return Target{Ref: "trunk", SHA: ""}, true, nil
		}
		return Target{}, false, nil
	}}
	g := ownerGit(nil)
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/cold": true, "acme/empty": true}}})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if g.callCount() != 0 {
		t.Fatal("a not-warm repository must never be grepped")
	}
	byRepo := map[string]Result{}
	for _, r := range ans.Code.Results {
		byRepo[r.Repo] = r
	}
	cold := byRepo["acme/cold"]
	if cold.Status != StatusNotWarm || cold.Reason != reasonNotCached || cold.Complete || cold.SHA != "" {
		t.Fatalf("cold result = %+v", cold)
	}
	empty := byRepo["acme/empty"]
	if empty.Status != StatusNotWarm || empty.Reason != reasonEmptyRepo || empty.Ref != "trunk" {
		t.Fatalf("empty result = %+v", empty)
	}
	if ans.Code.Complete || ans.Code.ReposSearched != 0 || ans.Code.ReposConsidered != 2 {
		t.Fatalf("section = %+v", ans.Code)
	}
}

func TestOwnerScopePerRepoFailureIsAStatus(t *testing.T) {
	repos := &fakeRepos{names: []string{"broken", "fine"}, warm: func(_ context.Context, repo string) (Target, bool, error) {
		if repo == "acme/broken" {
			return Target{}, false, Wrap(ErrUnavailable, ErrUnavailable.Error())
		}
		return Target{Repo: &git.LocalRepo{Path: "/serving/fine"}, Ref: "main", SHA: "sha"}, true, nil
	}}
	g := &fakeGit{fn: func(_ context.Context, repo *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		if repo.Path == "/serving/fine" {
			return &git.GrepResult{Files: []git.GrepFile{{Path: "a.go", Matches: []git.GrepMatch{{Line: 1, Column: 1, Text: "x"}}}}}, nil
		}
		return nil, errors.New("boom")
	}}
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/broken": true, "acme/fine": true}}})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("a per-repository failure must not fail the request: %v", err)
	}
	if ans.Code.Complete || ans.Code.ReposSearched != 1 {
		t.Fatalf("section = %+v", ans.Code)
	}
	byRepo := map[string]Result{}
	for _, r := range ans.Code.Results {
		byRepo[r.Repo] = r
	}
	if r := byRepo["acme/broken"]; r.Status != StatusFailed || r.Reason != reasonUnavailable || r.Files == nil {
		t.Fatalf("broken = %+v", r)
	}
	if r := byRepo["acme/fine"]; r.Status != StatusOK || !r.Complete {
		t.Fatalf("fine = %+v", r)
	}
}

func TestOwnerScopeGrepFailureIsARepoStatus(t *testing.T) {
	repos := &fakeRepos{names: []string{"a"}, warm: ownerWarm(true)}
	g := &fakeGit{fn: func(context.Context, *git.LocalRepo, string, git.GrepQuery) (*git.GrepResult, error) {
		return nil, errors.New("git grep failed")
	}}
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/a": true}}})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if r := ans.Code.Results[0]; r.Status != StatusFailed || r.Reason != reasonSearch {
		t.Fatalf("grep failure result = %+v", r)
	}
}

func TestOwnerScopePatternRejectionIsAWholeRequest400(t *testing.T) {
	repos := &fakeRepos{names: []string{"a", "b"}, warm: ownerWarm(true)}
	g := &fakeGit{fn: func(context.Context, *git.LocalRepo, string, git.GrepQuery) (*git.GrepResult, error) {
		return nil, &git.GrepPatternError{Detail: "Unmatched ["}
	}}
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/a": true, "acme/b": true}}})
	// "a|b" passes the pre-flight CompilePOSIX check, so the rejection can
	// only come from git at run time — the dialect corner §10 maps to a 400.
	_, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "a|b", Regex: true, Type: "code"}, anon)
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	var ie *InvalidError
	if !errors.As(err, &ie) || !strings.Contains(ie.Why, "invalid regular expression") {
		t.Fatalf("err = %v, want the pattern text", err)
	}
}

func TestOwnerScopeDeadline(t *testing.T) {
	names := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	public := map[string]bool{}
	for _, name := range names {
		public["acme/"+name] = true
	}
	repos := &fakeRepos{names: names, warm: func(ctx context.Context, repo string) (Target, bool, error) {
		// Every started candidate holds its worker until the request deadline,
		// so the four workers stall and the remaining candidates are never fed.
		<-ctx.Done()
		return Target{}, false, ctx.Err()
	}}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: &matrixAccess{public: public}})
	svc.ownerDeadline = 30 * time.Millisecond
	start := time.Now()
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("a deadline is an answer, not an error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("deadline not honoured: %s", elapsed)
	}
	if ans.Code.Complete {
		t.Fatal("a deadline-stopped fan-out is not complete")
	}
	if ans.Code.ReposConsidered != len(names) {
		t.Fatalf("considered = %d, want %d", ans.Code.ReposConsidered, len(names))
	}
	searched, notSearched := 0, 0
	for _, r := range ans.Code.Results {
		switch r.Status {
		case StatusNotSearched:
			notSearched++
			if r.Reason != reasonDeadline {
				t.Fatalf("%s reason = %q", r.Repo, r.Reason)
			}
		case StatusOK:
			searched++
		default:
			t.Fatalf("%s status = %q", r.Repo, r.Status)
		}
	}
	if searched != 0 {
		t.Fatalf("searched = %d, want 0 (every started candidate hit the deadline)", searched)
	}
	if notSearched != len(names) {
		t.Fatalf("not_searched = %d, want %d", notSearched, len(names))
	}
}

func TestOwnerScopeTimedOutGrepKeepsPartialAnswer(t *testing.T) {
	repos := &fakeRepos{names: []string{"a"}, warm: ownerWarm(true)}
	g := &fakeGit{fn: func(ctx context.Context, _ *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		<-ctx.Done()
		// The primitive reports the partial scan with a nil error on ITS
		// deadline; our request deadline cancels the context, so the seam sees
		// ctx.Err() and the partial result.
		return &git.GrepResult{
			Files:     []git.GrepFile{{Path: "a.go", Matches: []git.GrepMatch{{Line: 3, Column: 2, Text: "hit"}}}},
			GrepStats: git.GrepStats{FileCount: 1, Matches: 1},
		}, ctx.Err()
	}}
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/a": true}}})
	svc.ownerDeadline = 30 * time.Millisecond
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	r := ans.Code.Results[0]
	if !r.TimedOut || r.Complete || r.Status != StatusOK {
		t.Fatalf("timed-out result = %+v", r)
	}
	if r.MatchCount != 1 || len(r.Files) != 1 || r.Files[0].Matches[0].Line != 3 {
		t.Fatalf("partial matches lost: %+v", r)
	}
	if ans.Code.Complete || ans.Code.ReposSearched != 1 {
		t.Fatalf("section = %+v", ans.Code)
	}
}

func TestOwnerScopeCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repos := &fakeRepos{names: []string{"a", "b"}, warm: func(ctx context.Context, repo string) (Target, bool, error) {
		cancel()
		<-ctx.Done()
		return Target{}, false, ctx.Err()
	}}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/a": true, "acme/b": true}}})
	_, err := svc.Search(ctx, ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestOwnerScopeListingErrors(t *testing.T) {
	public := &matrixAccess{public: map[string]bool{}}
	svc := New(Options{Git: ownerGit(nil), Repos: &fakeRepos{listErr: errors.New("store down")}, Access: public})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("listing failure: err = %v", err)
	}
	// A caller cancellation during the listing is the context error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc = New(Options{Git: ownerGit(nil), Repos: &fakeRepos{listErr: context.Canceled}, Access: public})
	if _, err := svc.Search(ctx, ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled listing: err = %v", err)
	}
}

func TestOwnerScopeDeadlineDuringListing(t *testing.T) {
	// A listing that runs into the request deadline is an empty, incomplete
	// answer (§10's deadline row), never a 503 for a request that simply ran
	// out of time.
	repos := &fakeRepos{listFn: func(ctx context.Context) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: allowAll()})
	svc.ownerDeadline = 20 * time.Millisecond
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code == nil || ans.Code.Complete || len(ans.Code.Results) != 0 || ans.Code.Results == nil {
		t.Fatalf("deadline-during-listing section = %+v", ans.Code)
	}
}

func TestOwnerScopeIdentityFailureIsWholeRequest(t *testing.T) {
	repos := &fakeRepos{names: []string{"a"}, warm: ownerWarm(true)}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: &matrixAccess{down: true}})
	_, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got := err.Error(); got != "access check unavailable" {
		t.Fatalf("message = %q", got)
	}
}

func TestOwnerScopeNoCandidates(t *testing.T) {
	repos := &fakeRepos{names: nil, warm: ownerWarm(true)}
	svc := New(Options{Git: ownerGit(nil), Repos: repos, Access: &matrixAccess{}})
	ans, err := svc.Search(context.Background(), ownerScope("nobody"), Params{Text: "x", Type: "code"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !ans.Code.Complete || len(ans.Code.Results) != 0 || ans.Code.Results == nil {
		t.Fatalf("empty owner = %+v", ans.Code)
	}
}

func TestOwnerScopePerRepoSerialization(t *testing.T) {
	// Two owner queries over the same repository serialize on the 1-slot gate;
	// the request pool allows both to run.
	var mu sync.Mutex
	active := map[string]int{}
	maxActive := map[string]int{}
	release := make(chan struct{})
	g := &fakeGit{fn: func(_ context.Context, repo *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		mu.Lock()
		active[repo.Path]++
		if active[repo.Path] > maxActive[repo.Path] {
			maxActive[repo.Path] = active[repo.Path]
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active[repo.Path]--
		mu.Unlock()
		return &git.GrepResult{Files: []git.GrepFile{}}, nil
	}}
	repos := &fakeRepos{names: []string{"a", "b"}, warm: ownerWarm(true)}
	svc := New(Options{Git: g, Repos: repos, Access: &matrixAccess{public: map[string]bool{"acme/a": true, "acme/b": true}}, MaxConcurrent: 4})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "code"}, anon); err != nil {
				t.Errorf("Search: %v", err)
			}
		}()
	}
	// Wait until both searches are running (two greps, one per repository).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		total := len(active)
		started := 0
		for _, n := range active {
			started += n
		}
		mu.Unlock()
		if total == 2 && started == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for path, n := range maxActive {
		if n != 1 {
			t.Fatalf("%s had %d concurrent greps, want 1", path, n)
		}
	}
	if len(maxActive) != 2 {
		t.Fatalf("gripped repositories = %v, want both", maxActive)
	}
	if len(svc.gates.gates) != 0 {
		t.Fatalf("gate map retained %d idle entries", len(svc.gates.gates))
	}
}
