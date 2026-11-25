// service_test.go — repository-scope search: the real git primitive over a real
// bare fixture, the error classification of docs/features/20_search.md §10, the
// completeness flags (§6.4) and the pools (§11).
package search

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
)

// repoService builds a service whose repository source always resolves to t's
// target and whose git seam is the real primitive.
func repoService(t *testing.T, layer Git, repos *fakeRepos, access Access) *Service {
	t.Helper()
	return New(Options{Git: layer, Repos: repos, Access: access})
}

// realRepos resolves every repository to the fixture target.
func realRepos(tgt Target) *fakeRepos {
	return &fakeRepos{
		names: []string{"app"},
		resolve: func(_, ref string) (Target, error) {
			out := tgt
			if ref != "" {
				// The fake's "resolution" of a sha is itself; a branch name
				// would come back as its tip in production.
				out.Ref, out.SHA = ref, ref
			}
			return out, nil
		},
		warm: func(context.Context, string) (Target, bool, error) { return tgt, true, nil },
	}
}

func TestSearchRepoRealGit(t *testing.T) {
	f := newSearchFixture(t)
	svc := repoService(t, f.Layer, realRepos(f.Target), allowAll())

	ans, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "HelloUtil"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Query != "HelloUtil" || ans.Type != TypeCode || ans.Scope.Kind != ScopeRepo {
		t.Fatalf("answer envelope = %+v", ans)
	}
	if ans.Issues != nil {
		t.Fatal("unrequested issues section must be omitted")
	}
	if ans.Code == nil || !ans.Code.Complete || ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 {
		t.Fatalf("code section = %+v", ans.Code)
	}
	if len(ans.Code.Results) != 1 {
		t.Fatalf("results = %d", len(ans.Code.Results))
	}
	r := ans.Code.Results[0]
	if r.Repo != "acme/app" || r.Ref != "main" || r.SHA != f.Second || r.Status != StatusOK || !r.Complete {
		t.Fatalf("result = %+v", r)
	}
	// The case-insensitive default matches HelloUtil's comment as well.
	if r.MatchCount != 2 || r.FileCount != 1 {
		t.Fatalf("counts = %d matches / %d files, want 2/1", r.MatchCount, r.FileCount)
	}
	if r.Files[0].Path != "pkg/util.go" || len(r.Files[0].Matches) != 2 {
		t.Fatalf("files = %+v", r.Files)
	}
	if r.Files[0].Matches[0].Line != 2 || r.Files[0].Matches[0].Column != 4 {
		t.Fatalf("first match = %+v, want line 2 column 4", r.Files[0].Matches[0])
	}
	if r.Files[0].Matches[1].Text != "func HelloUtil() {}" {
		t.Fatalf("second match text = %q", r.Files[0].Matches[1].Text)
	}
}

func TestSearchRepoRefSelectionAndOptions(t *testing.T) {
	f := newSearchFixture(t)
	svc := repoService(t, f.Layer, realRepos(f.Target), allowAll())

	// The first commit's README says "hello world"; the second says
	// "hello version two". Asking for the sha pins the older answer.
	ans, err := svc.Search(context.Background(), repoScope("acme", "app"),
		Params{Text: "hello", Case: true, Ref: f.Main}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	r := ans.Code.Results[0]
	if r.Ref != f.Main || r.SHA != f.Main {
		t.Fatalf("ref/sha = %q/%q, want the requested sha", r.Ref, r.SHA)
	}
	// The first commit's README says "hello world" and its main.go calls
	// hello(); the second commit's README says "hello version two" and must
	// not appear.
	if r.MatchCount != 2 || r.FileCount != 2 {
		t.Fatalf("result at the first commit = %+v", r)
	}
	if r.Files[0].Path != "README.md" || r.Files[0].Matches[0].Text != "hello world" {
		t.Fatalf("README match = %+v", r.Files[0])
	}
	for _, f := range r.Files {
		for _, m := range f.Matches {
			if strings.Contains(m.Text, "version two") {
				t.Fatalf("searched the wrong commit: %+v", m)
			}
		}
	}

	// Pathspec and word matching ride the primitive.
	ans, err = svc.Search(context.Background(), repoScope("acme", "app"),
		Params{Text: "main", Word: true, Paths: []string{"cmd/"}}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	r = ans.Code.Results[0]
	// "main" as a whole word matches the package clause and the func; the
	// pathspec keeps README.md (which has no "main") and pkg/ out of it.
	if r.MatchCount != 2 || r.FileCount != 1 || r.Files[0].Path != "cmd/app/main.go" {
		t.Fatalf("pathspec result = %+v", r)
	}

	// A regex query selects POSIX ERE.
	ans, err = svc.Search(context.Background(), repoScope("acme", "app"),
		Params{Text: "func [A-Za-z]+", Regex: true}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Code.Results[0].MatchCount != 2 {
		t.Fatalf("regex matches = %d, want 2", ans.Code.Results[0].MatchCount)
	}
}

func TestSearchRepoNoMatchesIsSuccess(t *testing.T) {
	f := newSearchFixture(t)
	svc := repoService(t, f.Layer, realRepos(f.Target), allowAll())
	ans, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "no-such-needle-anywhere"}, anon)
	if err != nil {
		t.Fatalf("a non-match must be success: %v", err)
	}
	r := ans.Code.Results[0]
	if r.Status != StatusOK || !r.Complete || r.MatchCount != 0 || r.FileCount != 0 {
		t.Fatalf("result = %+v", r)
	}
	if r.Files == nil || len(r.Files) != 0 {
		t.Fatalf("files = %#v, want []", r.Files)
	}
}

func TestSearchRepoTruncationFolds(t *testing.T) {
	f := newSearchFixture(t)
	svc := repoService(t, f.Layer, realRepos(f.Target), allowAll())

	// limit=1 caps the repository globally: one kept match, truncated.
	ans, err := svc.Search(context.Background(), repoScope("acme", "app"),
		Params{Text: "HelloUtil", Limit: 1}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	r := ans.Code.Results[0]
	if !r.Truncated || r.Complete || ans.Code.Complete {
		t.Fatalf("limit=1 result = %+v, complete=%v", r, ans.Code.Complete)
	}
	if r.MatchCount != 1 {
		t.Fatalf("match_count = %d, want 1", r.MatchCount)
	}

	// A per-file cap (5 kept, a 6th observed) folds into the repository flag
	// without a per-file boolean on the wire (decision 8).
	dir := t.TempDir()
	lines := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		lines = append(lines, "needle here")
	}
	work := dir + "/work"
	searchGit(t, dir, "init", "-b", "main", "-q", work)
	if err := writeFile(work+"/many.txt", strings.Join(lines, "\n")+"\n"); err != nil {
		t.Fatal(err)
	}
	searchGit(t, work, "add", "-A")
	searchGit(t, work, "commit", "-qm", "many")
	bare := dir + "/serving.git"
	searchGit(t, dir, "clone", "--bare", "-q", work, bare)
	sha := searchGit(t, work, "rev-parse", "HEAD")
	tgt := Target{Repo: &git.LocalRepo{Path: bare}, Ref: "main", SHA: sha}
	svc = repoService(t, git.NewLayer(), realRepos(tgt), allowAll())
	ans, err = svc.Search(context.Background(), repoScope("acme", "many"), Params{Text: "needle"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	r = ans.Code.Results[0]
	if !r.Truncated || r.Complete {
		t.Fatalf("per-file cap result = %+v", r)
	}
	if r.MatchCount != MatchesPerFile {
		t.Fatalf("match_count = %d, want %d", r.MatchCount, MatchesPerFile)
	}
}

func TestSearchRepoSourceErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
		why  string
	}{
		{"unknown ref", Wrap(ErrNotFound, `unknown revision "nope"`), ErrNotFound, "unknown revision"},
		{"unborn", Wrap(ErrNotFound, "empty repository: acme/app"), ErrNotFound, "empty repository"},
		{"warming", Wrap(ErrWarming, ErrWarming.Error()), ErrWarming, "warming"},
		{"remote", Wrap(ErrNotLocal, ErrNotLocal.Error()), ErrNotLocal, "local serving copy"},
		{"backend", Wrap(ErrUnavailable, ErrUnavailable.Error()), ErrUnavailable, "temporarily unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repos := &fakeRepos{resolve: func(string, string) (Target, error) { return Target{}, tc.err }}
			svc := New(Options{Git: &fakeGit{}, Repos: repos, Access: allowAll()})
			_, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x"}, anon)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("message = %q, want %q", err.Error(), tc.why)
			}
		})
	}
}

func TestSearchRepoGrepFailures(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		want  error
		check func(t *testing.T, err error)
	}{
		{
			name: "git rejects the pattern at run time",
			err:  &git.GrepPatternError{Detail: "Unmatched ( or \\("},
			want: ErrInvalidQuery,
			check: func(t *testing.T, err error) {
				var ie *InvalidError
				if !errors.As(err, &ie) || ie.Field != "q" || !strings.Contains(ie.Why, "invalid regular expression: Unmatched") {
					t.Fatalf("err = %v", err)
				}
			},
		},
		{
			name: "pattern sentinel without detail",
			err:  git.ErrGrepPattern,
			want: ErrInvalidQuery,
			check: func(t *testing.T, err error) {
				if got := err.Error(); got != "invalid regular expression" {
					t.Fatalf("err = %q", got)
				}
			},
		},
		{
			name: "subprocess failure",
			err:  errors.New("git grep: exit 128"),
			want: ErrUnavailable,
			check: func(t *testing.T, err error) {
				if got := err.Error(); got != ErrUnavailable.Error() {
					t.Fatalf("a backend failure must not leak the command: %q", got)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGit{fn: func(context.Context, *git.LocalRepo, string, git.GrepQuery) (*git.GrepResult, error) {
				return nil, tc.err
			}}
			svc := New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/tmp/x"}, Ref: "main", SHA: "abc"}), Access: allowAll()})
			_, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x"}, anon)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.check != nil {
				tc.check(t, err)
			}
		})
	}
}

func TestSearchRepoNilPrimitiveResult(t *testing.T) {
	// A nil result with a nil error is a seam bug, not an empty success: the
	// answer says incomplete rather than "no matches".
	g := &fakeGit{fn: func(context.Context, *git.LocalRepo, string, git.GrepQuery) (*git.GrepResult, error) {
		return nil, nil
	}}
	svc := New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/tmp/x"}, Ref: "main", SHA: "abc"}), Access: allowAll()})
	ans, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x"}, anon)
	if err != nil {
		t.Fatal(err)
	}
	if r := ans.Code.Results[0]; r.Status != StatusOK || r.Complete || r.Files == nil {
		t.Fatalf("nil primitive result projected as %+v", r)
	}
}

func TestSearchRepoWarmingAndCancellation(t *testing.T) {
	// A caller cancellation while the grep runs is an error, never a partial
	// success (§11).
	ctx, cancel := context.WithCancel(context.Background())
	g := &fakeGit{fn: func(ctx context.Context, _ *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/tmp/x"}, Ref: "main", SHA: "abc"}), Access: allowAll()})
	if _, err := svc.Search(ctx, repoScope("acme", "app"), Params{Text: "x"}, anon); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSearchDisabledAndUnwired(t *testing.T) {
	scope := repoScope("acme", "app")
	params := Params{Text: "x"}
	disabled := New(Options{Disabled: true, Git: &fakeGit{}, Repos: realRepos(Target{}), Access: allowAll()})
	if _, err := disabled.Search(context.Background(), scope, params, anon); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: err = %v", err)
	}
	var nilSvc *Service
	if _, err := nilSvc.Search(context.Background(), scope, params, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil service: err = %v", err)
	}
	// A nil seam answers 503 rather than searching without a gate (law 9).
	noGit := New(Options{Repos: realRepos(Target{}), Access: allowAll()})
	if _, err := noGit.Search(context.Background(), scope, params, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil git: err = %v", err)
	}
	noRepos := New(Options{Git: &fakeGit{}, Access: allowAll()})
	if _, err := noRepos.Search(context.Background(), scope, params, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil repos: err = %v", err)
	}
	// No access gate is fail-closed too.
	noGate := New(Options{Git: &fakeGit{}, Repos: realRepos(Target{})})
	if _, err := noGate.Search(context.Background(), scope, params, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil access: err = %v", err)
	}
}

func TestSearchUnsupportedScopes(t *testing.T) {
	svc := New(Options{Git: &fakeGit{}, Repos: realRepos(Target{}), Access: allowAll()})
	if _, err := svc.Search(context.Background(), Scope{Kind: ScopeInstance}, Params{Text: "x", Type: "code"}, anon); !errors.Is(err, ErrUnsupportedScope) {
		t.Fatalf("instance scope: err = %v", err)
	}
	if _, err := svc.Search(context.Background(), Scope{}, Params{Text: "x"}, anon); !errors.Is(err, ErrUnsupportedScope) {
		t.Fatalf("empty scope: err = %v", err)
	}
	// A repo scope with no owner never reaches the gate.
	if _, err := svc.Search(context.Background(), Scope{Kind: ScopeRepo}, Params{Text: "x"}, anon); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("empty repo scope: err = %v", err)
	}
}

func TestSearchIssueSectionSeam(t *testing.T) {
	scope := repoScope("acme", "app")
	// Without a searcher the request is refused, never an empty section.
	plain := New(Options{Git: &fakeGit{}, Repos: realRepos(Target{}), Access: allowAll()})
	if _, err := plain.Search(context.Background(), scope, Params{Text: "x", Type: "issues"}, anon); !errors.Is(err, ErrIssuesUnavailable) {
		t.Fatalf("unwired issues: err = %v", err)
	}
	// With one, the section is projected and the frozen never-nil contract is
	// enforced even when the implementation returns nil slices.
	iss := &fakeIssues{section: IssueSection{Complete: true, Hits: nil}}
	svc := New(Options{Git: &fakeGit{}, Repos: realRepos(Target{}), Access: allowAll(), Issues: iss})
	ans, err := svc.Search(context.Background(), scope, Params{Text: "crash", Type: "issues", Case: true, Limit: 3}, user)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if ans.Issues == nil || ans.Issues.Hits == nil {
		t.Fatal("issues section / hits must never be nil")
	}
	if iss.scope != (IssueScope{Owner: "acme", Repo: "app"}) {
		t.Fatalf("issue scope = %+v", iss.scope)
	}
	if iss.got.Text != "crash" || !iss.got.CaseExact || !iss.got.Body || iss.got.Limit != 3 {
		t.Fatalf("issue query = %+v", iss.got)
	}
	if ans.Code != nil {
		t.Fatal("type=issues must not run a code search")
	}
	// A searcher error is the answer (P4 maps its own failures).
	iss = &fakeIssues{err: errors.New("issue index unavailable")}
	svc = New(Options{Git: &fakeGit{}, Repos: realRepos(Target{}), Access: allowAll(), Issues: iss})
	if _, err := svc.Search(context.Background(), scope, Params{Text: "x", Type: "issues"}, user); err == nil {
		t.Fatal("searcher error swallowed")
	}
}

func TestSearchRequestPoolBoundsConcurrency(t *testing.T) {
	var started sync.WaitGroup
	started.Add(3)
	release := make(chan struct{})
	var once sync.Once
	gate := make(chan struct{})
	g := &fakeGit{fn: func(_ context.Context, repo *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		started.Done()
		once.Do(func() { close(gate) }) // at least one grep is running
		<-release
		return &git.GrepResult{Files: []git.GrepFile{}}, nil
	}}
	svc := New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/tmp/x"}, Ref: "main", SHA: "abc"}),
		Access: allowAll(), MaxConcurrent: 2})
	if cap(svc.sem) != 2 {
		t.Fatalf("request pool = %d slots, want 2", cap(svc.sem))
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x"}, anon); err != nil {
				t.Errorf("Search: %v", err)
			}
		}()
	}
	<-gate
	// Give the other goroutines time to block on the pool (they cannot enter
	// the fake until a slot frees).
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	started.Wait()
	if got := g.maxConcurrent(); got > 2 {
		t.Fatalf("max concurrent greps = %d, want <= 2", got)
	}
}

func TestSearchPerRepoSerialization(t *testing.T) {
	release := make(chan struct{})
	g := &fakeGit{fn: func(_ context.Context, _ *git.LocalRepo, _ string, _ git.GrepQuery) (*git.GrepResult, error) {
		<-release
		return &git.GrepResult{Files: []git.GrepFile{}}, nil
	}}
	svc := New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/tmp/same"}, Ref: "main", SHA: "abc"}),
		Access: allowAll(), MaxConcurrent: 4})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x"}, anon); err != nil {
				t.Errorf("Search: %v", err)
			}
		}()
	}
	// Wait until one grep is definitely in flight; the other two must queue on
	// the repository gate, never run beside it.
	deadline := time.Now().Add(2 * time.Second)
	for g.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := g.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent greps of one repository = %d, want 1", got)
	}
	close(release)
	wg.Wait()
	if got := g.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent greps after completion = %d, want 1", got)
	}
	// The gate map is empty again (idle entries are reference counted away).
	if len(svc.gates.gates) != 0 {
		t.Fatalf("gate map retained %d idle entries", len(svc.gates.gates))
	}
}

func TestSearchAnswersWithSaturatedControlPlanePool(t *testing.T) {
	f := newSearchFixture(t)
	// A layer whose control-plane pool is fully occupied: the grep must still
	// answer, because search only ever takes the derived scan pool (§11,
	// decision 10).
	layer := &git.Layer{Binary: "git", Pool: git.NewPool(1), ScanPool: git.NewPool(1)}
	ctx := context.Background()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = layer.Pool.Run(ctx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	svc := repoService(t, layer, realRepos(f.Target), allowAll())
	ans, err := svc.Search(ctx, repoScope("acme", "app"), Params{Text: "HelloUtil"}, anon)
	if err != nil {
		close(release)
		<-done
		t.Fatalf("search stalled behind the control-plane pool: %v", err)
	}
	if ans.Code.Results[0].MatchCount != 2 {
		close(release)
		<-done
		t.Fatalf("matches = %d", ans.Code.Results[0].MatchCount)
	}
	close(release)
	<-done
}

func TestServicePoolSizing(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, DefaultMaxConcurrent}, {-3, DefaultMaxConcurrent}, {1, 1}, {99, MaxConcurrentCeiling}} {
		svc := New(Options{MaxConcurrent: tc.in})
		if cap(svc.sem) != tc.want {
			t.Errorf("MaxConcurrent %d → %d slots, want %d", tc.in, cap(svc.sem), tc.want)
		}
	}
}

func TestSearchCanceledWhileQueuedForAPoolSlot(t *testing.T) {
	g := &fakeGit{}
	svc := New(Options{Git: g, Repos: realRepos(Target{Repo: &git.LocalRepo{Path: "/tmp/x"}, Ref: "main", SHA: "abc"}),
		Access: allowAll(), MaxConcurrent: 1})
	svc.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := svc.Search(ctx, repoScope("acme", "app"), Params{Text: "x"}, anon); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if g.callCount() != 0 {
		t.Fatal("a request that never got a slot must not grep")
	}
}
