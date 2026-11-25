// query_test.go — docs/features/20_search.md §5.1/§10: the query grammar, the
// normalised options echo and the 400-class text, table-driven.
package search

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
)

func TestParseDefaultsRepoScope(t *testing.T) {
	q, err := Parse(repoScope("acme", "app"), Params{Text: "func main"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Type != TypeCode {
		t.Fatalf("type = %q, want code (repo scope default)", q.Type)
	}
	if q.Limit != DefaultLimit {
		t.Fatalf("limit = %d, want %d", q.Limit, DefaultLimit)
	}
	if q.Regex || q.CaseSensitive || q.Word {
		t.Fatalf("default flags = %+v, want fixed string, ASCII case-insensitive, no word", q)
	}
	if q.Paths == nil || len(q.Paths) != 0 {
		t.Fatalf("paths = %#v, want an empty non-nil list", q.Paths)
	}
	if q.Ref != "" {
		t.Fatalf("ref = %q, want the default branch", q.Ref)
	}
	if got := q.Scope.ID(); got != "acme/app" {
		t.Fatalf("scope id = %q", got)
	}
}

func TestParseDefaultsOwnerScopeIsIssues(t *testing.T) {
	// §5.1: the global (owner) surface defaults to issues; code is an explicit
	// type=code request.
	q, err := Parse(ownerScope("acme"), Params{Text: "crash"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Type != TypeIssues {
		t.Fatalf("type = %q, want issues", q.Type)
	}
}

func TestParseTable(t *testing.T) {
	owner := ownerScope("acme")
	repo := repoScope("acme", "app")
	tests := []struct {
		name  string
		scope Scope
		p     Params
		want  func(t *testing.T, q Query)
		field string // expected InvalidError.Field
		why   string // expected substring of the message
	}{
		{name: "type code", scope: repo, p: Params{Text: "x", Type: "code"},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if !q.wantsCode() || q.wantsIssues() {
					t.Fatal("type code")
				}
			}},
		{name: "type issues", scope: repo, p: Params{Text: "x", Type: "issues"},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if !q.wantsIssues() || q.wantsCode() {
					t.Fatal("type issues")
				}
			}},
		{name: "type all", scope: repo, p: Params{Text: "x", Type: "all"},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if !q.wantsCode() || !q.wantsIssues() {
					t.Fatal("type all")
				}
			}},
		{name: "type trimmed", scope: repo, p: Params{Text: "x", Type: " all "},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Type != TypeAll {
					t.Fatalf("type = %q", q.Type)
				}
			}},
		{name: "unknown type", scope: repo, p: Params{Text: "x", Type: "commits"}, field: "type", why: "type must be code, issues or all"},
		{name: "limit zero is default", scope: repo, p: Params{Text: "x", Limit: 0},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Limit != DefaultLimit {
					t.Fatalf("limit = %d", q.Limit)
				}
			}},
		{name: "limit negative clamps to one", scope: repo, p: Params{Text: "x", Limit: -5},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Limit != 1 {
					t.Fatalf("limit = %d", q.Limit)
				}
			}},
		{name: "limit above ceiling clamps", scope: repo, p: Params{Text: "x", Limit: 5000},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Limit != MaxLimit {
					t.Fatalf("limit = %d", q.Limit)
				}
			}},
		{name: "limit kept", scope: repo, p: Params{Text: "x", Limit: 37},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Limit != 37 {
					t.Fatalf("limit = %d", q.Limit)
				}
			}},
		{name: "regex accepted", scope: repo, p: Params{Text: "func [a-z]+", Regex: true},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if !q.Regex {
					t.Fatal("regex not set")
				}
			}},
		{name: "invalid regex", scope: repo, p: Params{Text: "func (", Regex: true}, field: "q", why: "invalid regular expression"},
		{name: "fixed string with regex metacharacters is data", scope: repo, p: Params{Text: "func ("},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Regex {
					t.Fatal("fixed string became a regex")
				}
			}},
		{name: "leading dash is data", scope: repo, p: Params{Text: "-rf"},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Text != "-rf" {
					t.Fatalf("text = %q", q.Text)
				}
			}},
		{name: "empty q", scope: repo, p: Params{}, field: "q", why: "q is required"},
		{name: "q too long", scope: repo, p: Params{Text: strings.Repeat("a", MaxPatternBytes+1)}, field: "q", why: "exceeds 256 bytes"},
		{name: "q with NUL", scope: repo, p: Params{Text: "a\x00b"}, field: "q", why: "NUL, CR or newline"},
		{name: "q with newline", scope: repo, p: Params{Text: "a\nb"}, field: "q", why: "NUL, CR or newline"},
		{name: "paths accepted", scope: repo, p: Params{Text: "x", Paths: []string{"cmd/", "*.go"}},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if len(q.Paths) != 2 || q.Paths[0] != "cmd/" {
					t.Fatalf("paths = %v", q.Paths)
				}
			}},
		{name: "too many paths", scope: repo, p: Params{Text: "x", Paths: make([]string, MaxPathPatterns+1)}, field: "path", why: "at most 8 path patterns"},
		{name: "empty path", scope: repo, p: Params{Text: "x", Paths: []string{"a", ""}}, field: "path", why: "is empty"},
		{name: "path too long", scope: repo, p: Params{Text: "x", Paths: []string{strings.Repeat("p", MaxPathBytes+1)}}, field: "path", why: "exceeds 256 bytes"},
		{name: "path with NUL", scope: repo, p: Params{Text: "x", Paths: []string{"a\x00b"}}, field: "path", why: "NUL, CR or newline"},
		{name: "path magic colon", scope: repo, p: Params{Text: "x", Paths: []string{":(glob)*.go"}}, field: "path", why: "pathspec magic"},
		{name: "path magic exclude", scope: repo, p: Params{Text: "x", Paths: []string{"!vendor/"}}, field: "path", why: "pathspec magic"},
		{name: "path magic caret", scope: repo, p: Params{Text: "x", Paths: []string{"^top"}}, field: "path", why: "pathspec magic"},
		{name: "ref accepted", scope: repo, p: Params{Text: "x", Ref: "refs/tags/v1.0"},
			want: func(t *testing.T, q Query) {
				t.Helper()
				if q.Ref != "refs/tags/v1.0" {
					t.Fatalf("ref = %q", q.Ref)
				}
			}},
		{name: "ref leading dash", scope: repo, p: Params{Text: "x", Ref: "-main"}, field: "ref", why: "must not start with '-'"},
		{name: "ref too long", scope: repo, p: Params{Text: "x", Ref: strings.Repeat("r", 257)}, field: "ref", why: "exceeds 256 bytes"},
		{name: "ref newline", scope: repo, p: Params{Text: "x", Ref: "ma\nin"}, field: "ref", why: "NUL, CR or newline"},
		{name: "ref control", scope: repo, p: Params{Text: "x", Ref: "ma\x1bin"}, field: "ref", why: "control character"},
		{name: "ref in owner scope", scope: owner, p: Params{Text: "x", Type: "code", Ref: "main"}, field: "ref", why: "repository scope only"},
		{name: "repo scope without a repo", scope: Scope{Kind: ScopeRepo, Owner: "acme"}, p: Params{Text: "x"}, field: "scope", why: "needs an owner and a repository"},
		{name: "owner scope without an owner", scope: Scope{Kind: ScopeOwner}, p: Params{Text: "x"}, field: "owner", why: "owner is required"},
		{name: "owner scope with a bad name", scope: Scope{Kind: ScopeOwner, Owner: "../etc"}, p: Params{Text: "x"}, field: "owner", why: "is not a valid name"},
		{name: "owner scope with a repo", scope: Scope{Kind: ScopeOwner, Owner: "acme", Repo: "app"}, p: Params{Text: "x"}, field: "repo", why: "not valid in owner scope"},
		{name: "instance scope", scope: Scope{Kind: ScopeInstance}, p: Params{Text: "x", Type: "code"}, field: "", why: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Parse(tc.scope, tc.p)
			if tc.name == "instance scope" {
				if !errors.Is(err, ErrUnsupportedScope) {
					t.Fatalf("err = %v, want ErrUnsupportedScope", err)
				}
				return
			}
			if tc.field != "" {
				var ie *InvalidError
				if !errors.As(err, &ie) {
					t.Fatalf("err = %v (%T), want *InvalidError", err, err)
				}
				if ie.Field != tc.field {
					t.Fatalf("field = %q, want %q (%v)", ie.Field, tc.field, err)
				}
				if !strings.Contains(ie.Why, tc.why) {
					t.Fatalf("why = %q, want %q", ie.Why, tc.why)
				}
				if !errors.Is(err, ErrInvalidQuery) {
					t.Fatal("InvalidError must unwrap to ErrInvalidQuery")
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tc.want != nil {
				tc.want(t, q)
			}
		})
	}
}

func TestQueryOptionsEcho(t *testing.T) {
	q, err := Parse(repoScope("acme", "app"), Params{
		Text: "x", Regex: true, Case: true, Word: true,
		Paths: []string{"cmd/"}, Limit: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := q.options()
	if !opts.Regex || !opts.CaseSensitive || !opts.Word || opts.Limit != 7 {
		t.Fatalf("options = %+v", opts)
	}
	if len(opts.Paths) != 1 || opts.Paths[0] != "cmd/" {
		t.Fatalf("paths = %#v", opts.Paths)
	}
	// The no-path query still echoes [] (never null).
	plain, err := Parse(repoScope("acme", "app"), Params{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if p := plain.options().Paths; p == nil {
		t.Fatal("options.paths is nil, want []")
	}
}

func TestGrepQueryPinsTheCaps(t *testing.T) {
	q, err := Parse(repoScope("acme", "app"), Params{Text: "needle", Paths: []string{"cmd/"}})
	if err != nil {
		t.Fatal(err)
	}
	gq := q.grepQuery()
	if gq.Pattern != "needle" || gq.PerFile != MatchesPerFile || gq.Limit != DefaultLimit ||
		gq.MaxBytes != MaxOutputBytes || gq.MaxText != MaxTextBytes || gq.Timeout != GrepTimeout {
		t.Fatalf("grep query = %+v", gq)
	}
	if len(gq.Paths) != 1 || gq.Paths[0] != "cmd/" {
		t.Fatalf("paths = %v", gq.Paths)
	}
	// The copy is defensive: mutating the query's paths must not leak into the
	// primitive request (or vice versa).
	q.Paths[0] = "pkg/"
	if gq.Paths[0] != "cmd/" {
		t.Fatal("grepQuery shares the query's path slice")
	}
	// A query without paths passes nil, which the primitive accepts.
	plain, _ := Parse(repoScope("acme", "app"), Params{Text: "x"})
	if paths := plain.grepQuery().Paths; paths != nil {
		t.Fatalf("paths = %v, want nil", paths)
	}
}

func TestValidOwnerName(t *testing.T) {
	for _, ok := range []string{"acme", "A.b_c-9", strings.Repeat("a", 100)} {
		if !validOwnerName(ok) {
			t.Errorf("validOwnerName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "a b", "acme!", strings.Repeat("a", 101)} {
		if validOwnerName(bad) {
			t.Errorf("validOwnerName(%q) = true", bad)
		}
	}
}

func TestGrepTimeoutBudgetIsBelowThePrimitiveCeiling(t *testing.T) {
	// The service's caps must be inside the primitive's hard bounds, or the
	// primitive would silently clamp them and the wire would lie.
	if GrepTimeout > git.GrepMaxTimeout {
		t.Fatalf("GrepTimeout %s exceeds the primitive ceiling %s", GrepTimeout, git.GrepMaxTimeout)
	}
	if time.Duration(OwnerDeadline) >= time.Duration(GrepTimeout) {
		t.Fatalf("owner deadline %s must be below the per-grep timeout %s (a 50-repo query cannot wait out 50 greps)", OwnerDeadline, GrepTimeout)
	}
	if MaxLimit > git.GrepMaxLimit || MaxOutputBytes > git.GrepMaxOutputBytes || MaxTextBytes > git.GrepMaxTextBytes || MatchesPerFile > git.GrepMaxPerFile {
		t.Fatal("a service cap exceeds the primitive ceiling")
	}
	if MaxPatternBytes > git.GrepMaxPatternBytes || MaxPathPatterns > git.GrepMaxPathPatterns || MaxPathBytes > git.GrepMaxPathBytes {
		t.Fatal("a query grammar cap exceeds the primitive ceiling")
	}
}
