// cli_search_test.go — `spindle search` (docs/features/20_search.md §6;
// docs/CLI.md §5.2): exact request path/query, --json passthrough, target
// selection, the flag conflict, and the WP11 exit mapping for 400/404/401.
package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/search"
)

const searchBody = `{
  "query": "func main",
  "type": "code",
  "options": {"regex": false, "case_sensitive": false, "word": false, "paths": [], "limit": 100},
  "scope": {"kind": "repo", "owner": "demo", "repo": "hello"},
  "code": {
    "complete": true,
    "repos_considered": 1,
    "repos_searched": 1,
    "results": [{
      "repo": "demo/hello",
      "ref": "main",
      "sha": "cb38da1b23e56a2b3c4d5e6f708192a3b4c5d6e7",
      "status": "ok",
      "complete": true,
      "truncated": false,
      "timed_out": false,
      "match_count": 2,
      "file_count": 1,
      "files": [{"path": "cmd/app/main.go",
                 "matches": [{"line": 12, "column": 6, "text": "func main() {"}]}]
    }]
  }
}`

func TestSearchRepoRequest(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/demo/hello/api/search", 200, searchBody)
	code, out, errOut := runCmd(t, srv, "wgt_test", runSearch,
		"func main", "--repo", "demo/hello", "--ref", "dev",
		"--regex", "--case", "--word", "--path", "cmd/", "--path", "web/", "--limit", "5")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr=%s", code, errOut)
	}
	rec := f.last()
	if rec.Method != "GET" || rec.Path != "/demo/hello/api/search" {
		t.Fatalf("request = %s %s", rec.Method, rec.Path)
	}
	if got := rec.Header.Get("Authorization"); got != "Bearer wgt_test" {
		t.Fatalf("authorization = %q", got)
	}
	q := rec.Query
	want := map[string]string{
		"q": "func main", "type": "code", "ref": "dev",
		"regex": "1", "case": "1", "word": "1", "limit": "5",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := q["path"]; len(got) != 2 || got[0] != "cmd/" || got[1] != "web/" {
		t.Errorf("path = %v", got)
	}
	for _, wantLine := range []string{
		"demo/hello @ cb38da1b23e5 (main) — ok: 2 match(es) in 1 file(s)",
		"cmd/app/main.go:12:6  func main() {",
		"1 repositories searched (1 considered)",
	} {
		if !strings.Contains(out, wantLine) {
			t.Errorf("output missing %q:\n%s", wantLine, out)
		}
	}
}

func TestSearchRepoSelectorGlobal(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/demo/hello/api/search", 200, searchBody)
	c := isolateCLI(t, srv.URL)
	c.repo = "demo/hello"
	var out, errOut strings.Builder
	c.out, c.errOut = &out, &errOut
	if code := runSearch(t.Context(), c, []string{"func"}); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, errOut.String())
	}
	if rec := f.last(); rec.Path != "/demo/hello/api/search" {
		t.Fatalf("path = %s", rec.Path)
	}
}

func TestSearchDispatchPeelsRepoSelector(t *testing.T) {
	// The real dispatcher peels -R from anywhere in argv before runSearch
	// sees the arguments (docs/CLI.md §2).
	f, srv := newFakeAPI(t)
	f.on("GET", "/demo/hello/api/search", 200, searchBody)
	c := isolateCLI(t, srv.URL)
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	if code := dispatch(context.Background(), []string{"search", "func", "-R", "demo/hello"}, c); code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, errOut.String())
	}
	if rec := f.last(); rec.Path != "/demo/hello/api/search" || rec.Query.Get("q") != "func" {
		t.Fatalf("request = %s %v", rec.Path, rec.Query)
	}
	if !strings.Contains(out.String(), "demo/hello @") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestSearchOwnerRequest(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/api/v1/search", 200, `{"query":"func","type":"code","options":{"regex":false,"case_sensitive":false,"word":false,"paths":[],"limit":100},"scope":{"kind":"owner","owner":"demo"},"code":{"complete":false,"repos_considered":2,"repos_searched":1,"results":[{"repo":"demo/hello","ref":"main","sha":"abc","status":"not_warm","reason":"not cached on this instance","complete":false,"truncated":false,"timed_out":false,"match_count":0,"file_count":0,"files":[]}]}}`)
	code, out, errOut := runCmd(t, srv, "wgt_test", runSearch, "func", "--owner", "demo")
	if code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, errOut)
	}
	rec := f.last()
	if rec.Path != "/api/v1/search" {
		t.Fatalf("path = %s", rec.Path)
	}
	if got := rec.Query.Get("owner"); got != "demo" {
		t.Fatalf("owner = %q", got)
	}
	if got := rec.Query.Get("type"); got != "code" {
		t.Fatalf("type = %q (the CLI is the code-search surface)", got)
	}
	if got := rec.Query.Get("limit"); got != "" {
		t.Fatalf("limit = %q, want omitted when 0", got)
	}
	for _, want := range []string{"not cached on this instance", "partial results"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSearchJSONPassthrough(t *testing.T) {
	f, srv := newFakeAPI(t)
	f.on("GET", "/demo/hello/api/search", 200, searchBody)
	code, out, errOut := runCmd(t, srv, "", runSearch, "func main", "--repo", "demo/hello", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d stderr=%s", code, errOut)
	}
	// The body is the API's own schema, re-indented — no CLI envelope.
	for _, want := range []string{`"repos_considered": 1`, `"timed_out": false`, `"matched"`[:0] + `"files"`} {
		if want != "" && !strings.Contains(out, want) {
			t.Errorf("--json output missing %q:\n%s", want, out)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(out), "demo/hello @") {
		t.Fatalf("--json printed the human table:\n%s", out)
	}
}

func TestSearchUsageErrors(t *testing.T) {
	_, srv := newFakeAPI(t)
	cases := []struct {
		name string
		args []string
	}{
		{"no query", nil},
		{"extra positional", []string{"a", "b", "--owner", "demo"}},
		{"no target", []string{"func"}},
		{"bad repo", []string{"func", "--repo", "not-a-repo"}},
		{"owner wins conflict", []string{"func", "--owner", "demo", "--repo", "demo/hello"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runCmd(t, srv, "", runSearch, tc.args...)
			if code != exitArg {
				t.Fatalf("exit = %d, want %d (stderr=%s)", code, exitArg, errOut)
			}
		})
	}
}

func TestSearchBadServer(t *testing.T) {
	c := isolateCLI(t, "ftp://not-http.example")
	var out, errOut bytes.Buffer
	c.out, c.errOut = &out, &errOut
	if code := runSearch(t.Context(), c, []string{"func", "--owner", "demo"}); code != exitErr {
		t.Fatalf("exit = %d, want %d (%s)", code, exitErr, errOut.String())
	}
}

func TestSearchHelp(t *testing.T) {
	_, srv := newFakeAPI(t)
	code, _, errOut := runCmd(t, srv, "", runSearch, "-h")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"usage: spindle search QUERY", "--owner", "-repo", "-ref", "-regex", "-case", "-word", "-path", "-limit", "-json"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("help missing %q:\n%s", want, errOut)
		}
	}
}

func TestSearchExitMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusBadRequest, "q is required", exitErr},
		{http.StatusNotFound, "unknown revision \"nope\"", exitNotFound},
		{http.StatusUnauthorized, "authentication required", exitAuth},
		{http.StatusForbidden, "read access required", exitAuth},
		{http.StatusServiceUnavailable, "search is warming this repository; retry in a few seconds", exitErr},
	}
	for _, tc := range cases {
		f, srv := newFakeAPI(t)
		f.on("GET", "/demo/hello/api/search", tc.status, tc.body)
		code, _, errOut := runCmd(t, srv, "wgt_test", runSearch, "func", "--repo", "demo/hello")
		if code != tc.want {
			t.Errorf("%d: exit = %d, want %d (%s)", tc.status, code, tc.want, errOut)
		}
		if !strings.Contains(errOut, tc.body) {
			t.Errorf("%d: stderr missing the server body:\n%s", tc.status, errOut)
		}
	}
}

func TestPrintSearchAnswerCompleteness(t *testing.T) {
	var out bytes.Buffer
	c := &cli{out: &out}
	printSearchAnswer(c, &search.Answer{
		Query: "func",
		Type:  search.TypeAll,
		Scope: search.Scope{Kind: search.ScopeOwner, Owner: "demo"},
		Code: &search.CodeSection{
			Complete:        false,
			ReposConsidered: 2,
			ReposSearched:   1,
			Results: []search.Result{{
				Repo: "demo/hello", Ref: "main", SHA: strings.Repeat("c", 40),
				Status: search.StatusFailed, Reason: "search failed",
				Truncated: true, TimedOut: true,
				Files: []search.File{{Path: "a.go", Matches: []search.Match{{Line: 1, Column: 1, Text: "func x"}}}},
			}},
		},
		Issues: &search.IssueSection{
			Complete: false,
			Scanned:  search.IssueScan{Threads: 3, Events: 5},
			Hits: []search.IssueHit{{
				Repo: "demo/hello", Num: 7, Kind: "issue", Title: "Crash", Matched: "title",
			}},
		},
	})
	got := out.String()
	for _, want := range []string{
		"failed, truncated, timed out", "search failed", "a.go:1:1  func x",
		"1 repositories searched (2 considered)", "partial results",
		"demo/hello #7 (issue) Crash — matched title", "3 thread(s) scanned; events 5",
		"partial issue results",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if shortSHA("short") != "short" || shortSHA(strings.Repeat("c", 40)) != strings.Repeat("c", 12) {
		t.Fatal("shortSHA")
	}
}

// TestSearchDispatchRegistration pins the dispatch/help surface: the row
// exists, it is not a group, and `spindle help search` reaches the FlagSet path.
func TestSearchDispatchRegistration(t *testing.T) {
	cmd := lookup("search")
	if cmd == nil {
		t.Fatal("search is not in the dispatch table")
	}
	if groupByName("search") != nil {
		t.Fatal("search must not be an apiGroup")
	}
	if !ownsFlagHelp("search") {
		t.Fatal("search owns its FlagSet help")
	}
	if !strings.Contains(helpText, "search QUERY") {
		t.Fatal("helpText does not list search")
	}
}
