// issue_test.go — P4 issue/PR search (docs/features/20_search.md §6.3, §7,
// §10, §12 decision 4/12). The fixtures are real store objects in the issue
// family's JSON shapes: the searcher reads them through the same loaders the
// page/JSON lanes use, so the metadata/body split, the index fallback and the
// moderation fold are exercised end to end rather than through a mock read.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// ---- fixtures ---------------------------------------------------------------

type issueSpec struct {
	Num      int
	Kind     string
	Title    string
	State    string
	Author   string
	Labels   []string
	Updated  string
	Body     string
	Comments []string
}

func putJSON(t *testing.T, st store.ObjectStore, key string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	if _, err := store.PutBytes(context.Background(), st, key, raw, store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func putIssueThread(t *testing.T, st store.ObjectStore, owner, repo string, spec issueSpec) issues.Card {
	t.Helper()
	if spec.Kind == "" {
		spec.Kind = "issue"
	}
	if spec.State == "" {
		spec.State = issues.StateOpen
	}
	if spec.Updated == "" {
		spec.Updated = "2026-11-20T10:11:12Z"
	}
	th := issues.Thread{
		Num: spec.Num, Kind: spec.Kind, Title: spec.Title, State: spec.State,
		Author: spec.Author, CreatedAt: "2026-11-19T09:00:00Z", UpdatedAt: spec.Updated,
		Labels: spec.Labels, NextEventSeq: 1 + len(spec.Comments), CommentCount: len(spec.Comments),
	}
	putJSON(t, st, issues.ThreadKey(owner, repo, spec.Num), th)
	body := spec.Body
	putJSON(t, st, issues.EventKey(owner, repo, spec.Num, 0), issues.Event{
		Seq: 0, Type: issues.EventOpened, Actor: spec.Author, At: th.CreatedAt, Body: &body,
	})
	for i, text := range spec.Comments {
		body := text
		putJSON(t, st, issues.EventKey(owner, repo, spec.Num, i+1), issues.Event{
			Seq: i + 1, Type: issues.EventCommented, Actor: "bob", At: spec.Updated, Body: &body,
		})
	}
	return issues.Card{
		Num: spec.Num, Kind: spec.Kind, Title: spec.Title, State: spec.State,
		Author: spec.Author, CreatedAt: th.CreatedAt, UpdatedAt: th.UpdatedAt,
		Labels: spec.Labels, CommentCount: len(spec.Comments),
	}
}

// putIndex writes the shared index plus the P2 counter. complete=false writes
// an index without the current Card projection, forcing the header scan.
func putIndex(t *testing.T, st store.ObjectStore, owner, repo string, complete bool, cards ...issues.Card) {
	t.Helper()
	next := 1
	open := []issues.Card{}
	closed := []issues.Card{}
	for _, c := range cards {
		if c.Num >= next {
			next = c.Num + 1
		}
		if c.State == issues.StateClosed {
			closed = append(closed, c)
		} else {
			open = append(open, c)
		}
	}
	putJSON(t, st, issues.CounterKey(owner, repo), issues.Counter{Next: next})
	ix := issues.Index{Version: 1, Open: open, ClosedRecent: closed}
	if complete {
		ix.CardVersion = issues.CardProjectionVersion
	}
	putJSON(t, st, issues.IndexKey(owner, repo), ix)
}

func issueStore(t *testing.T) (*store.Memory, *issues.Service) {
	t.Helper()
	st := store.NewMemory()
	return st, issues.New(st, nil)
}

func issueQuery(q IssueQuery) IssueQuery {
	if q.Limit == 0 {
		q.Limit = DefaultLimit
	}
	return q
}

// metadataFixture is three cards: an open issue (bug/jane), a closed issue
// (documentation/bob) and an open PR (bug/jane). The numbers are dense so the
// index is provably complete.
func metadataFixture(t *testing.T) *issues.Service {
	t.Helper()
	st, iss := issueStore(t)
	i1 := putIssueThread(t, st, "acme", "app", issueSpec{Num: 1, Title: "Crash on empty ref", Author: "jane", Labels: []string{"bug"}, Body: "nothing here"})
	i2 := putIssueThread(t, st, "acme", "app", issueSpec{Num: 2, Title: "Docs update", Author: "bob", Labels: []string{"documentation"}, State: issues.StateClosed, Body: "docs body"})
	p3 := putIssueThread(t, st, "acme", "app", issueSpec{Num: 3, Kind: "pr", Title: "Fix empty ref", Author: "jane", Labels: []string{"bug"}, Body: "pr body"})
	putIndex(t, st, "acme", "app", true, i1, i2, p3)
	return iss
}

// ---- metadata matching ------------------------------------------------------

func TestIssueMetadataMatching(t *testing.T) {
	iss := metadataFixture(t)
	s := NewIssueSearcher(iss)

	cases := []struct {
		name    string
		q       IssueQuery
		wantNum []int // newest-number order
		wantMat string
	}{
		{"title", IssueQuery{Text: "crash"}, []int{1}, MatchTitle},
		{"label", IssueQuery{Text: "documentation"}, []int{2}, MatchLabel},
		{"author", IssueQuery{Text: "jane"}, []int{3, 1}, MatchAuthor},
		{"state", IssueQuery{Text: "closed"}, []int{2}, MatchState},
		{"kind", IssueQuery{Text: "pr"}, []int{3}, MatchKind},
		{"case insensitive default", IssueQuery{Text: "CRASH"}, []int{1}, MatchTitle},
		{"word whole", IssueQuery{Text: "ref", Word: true}, []int{3, 1}, MatchTitle},
		{"word partial", IssueQuery{Text: "cras", Word: true}, nil, ""},
		{"regex", IssueQuery{Text: "crash|docs", Regex: true}, []int{2, 1}, MatchTitle},
		{"regex word", IssueQuery{Text: "crash", Regex: true, Word: true}, []int{1}, MatchTitle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := issueQuery(tc.q)
			sec, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, q)
			if err != nil {
				t.Fatalf("SearchIssues: %v", err)
			}
			if !sec.Complete {
				t.Fatalf("metadata answer incomplete: %+v", sec)
			}
			if len(sec.Hits) != len(tc.wantNum) {
				t.Fatalf("hits = %+v, want %d", sec.Hits, len(tc.wantNum))
			}
			for i, want := range tc.wantNum {
				h := sec.Hits[i]
				if h.Num != want {
					t.Fatalf("hit %d = %+v, want num %d", i, h, want)
				}
				if h.Matched != tc.wantMat {
					t.Fatalf("matched = %q, want %q", h.Matched, tc.wantMat)
				}
				if h.Labels == nil || h.Repo != "acme/app" || h.UpdatedAt == "" {
					t.Fatalf("hit = %+v", h)
				}
				wantURL := fmt.Sprintf("/acme/app/issues/%d", want)
				if h.Kind == "pr" {
					wantURL = fmt.Sprintf("/acme/app/pulls/%d", want)
				}
				if h.URL != wantURL {
					t.Fatalf("url = %q, want %q", h.URL, wantURL)
				}
			}
		})
	}

	// Case-exact rejects the folded variant; the default accepts it.
	sec, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "CRASH", CaseExact: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 0 {
		t.Fatalf("case-exact hits = %+v", sec.Hits)
	}

	// Metadata-only mode never reads events or applies the body caps.
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "jane"}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if sec.Scanned.Threads != 0 || sec.Scanned.Events != 0 {
		t.Fatalf("metadata scan = %+v, want zero", sec.Scanned)
	}
}

// ---- body/comment matching and excerpts ------------------------------------

func TestIssueBodyAndCommentMatching(t *testing.T) {
	st, iss := issueStore(t)
	longBody := strings.Repeat("alpha ", 40) + "NEEDLE" + strings.Repeat(" omega", 40)
	i1 := putIssueThread(t, st, "acme", "app", issueSpec{
		Num: 1, Title: "A body thread", Author: "jane", Body: longBody,
		Comments: []string{"a comment with needle inside", "unrelated"},
	})
	putIndex(t, st, "acme", "app", true, i1)
	s := NewIssueSearcher(iss)

	sec, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Body: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 1 || sec.Hits[0].Matched != MatchBody {
		t.Fatalf("body hit = %+v", sec.Hits)
	}
	ex := sec.Hits[0].Excerpt
	if !strings.Contains(ex, "NEEDLE") {
		t.Fatalf("excerpt lost the match: %q", ex)
	}
	if !strings.HasPrefix(ex, "…") || !strings.HasSuffix(ex, "…") {
		t.Fatalf("excerpt was not clipped: %q", ex)
	}
	if n := utf8.RuneCountInString(ex); n > IssueExcerptRunes+2 {
		t.Fatalf("excerpt %d runes exceeds the window", n)
	}
	if ex == longBody {
		t.Fatal("excerpt returned the whole body")
	}
	if sec.Scanned.Threads != 1 || sec.Scanned.Events != 3 {
		t.Fatalf("scan = %+v, want one thread / three events", sec.Scanned)
	}

	// A comment-only match says so and carries the comment excerpt.
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "comment", Body: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 1 || sec.Hits[0].Matched != MatchComment || !strings.Contains(sec.Hits[0].Excerpt, "comment with needle") {
		t.Fatalf("comment hit = %+v", sec.Hits)
	}

	// Body=false is metadata-only: the body text is never consulted.
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle"}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 0 || sec.Scanned.Events != 0 {
		t.Fatalf("metadata-only answer = %+v", sec)
	}

	// Word matching applies to body text too.
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needl", Body: true, Word: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 0 {
		t.Fatalf("partial word hit = %+v", sec.Hits)
	}
}

// ---- moderation redaction ---------------------------------------------------

type fakeIssueModeration struct {
	hidden map[string]issues.HiddenInfo
	err    error
}

func (f *fakeIssueModeration) BlockedFromRepo(context.Context, string, string, string) (bool, string, error) {
	return false, "", nil
}

func (f *fakeIssueModeration) Snapshot(_ context.Context, _, _ string, _ int, fn func(target, by, at, reason string)) (bool, string, string, int, error) {
	if f.err != nil {
		return false, "", "", 0, f.err
	}
	for target, h := range f.hidden {
		fn(target, h.By, h.At, h.Reason)
	}
	return false, "", "", 1, nil
}

func TestIssueModerationRedactionBeforeMatching(t *testing.T) {
	st, iss := issueStore(t)
	i1 := putIssueThread(t, st, "acme", "app", issueSpec{
		Num: 1, Title: "A redacted thread", Author: "jane",
		Body:     "secret needle in the description",
		Comments: []string{"public needle in a comment"},
	})
	putIndex(t, st, "acme", "app", true, i1)
	mod := &fakeIssueModeration{hidden: map[string]issues.HiddenInfo{
		"thread:000001:event:000000000000": {By: "mod", At: "2026-11-21T00:00:00Z", Reason: "spam"},
	}}
	iss.Moderation = mod
	s := NewIssueSearcher(iss)

	// The hidden description can never be matched or excerpted; the visible
	// comment still matches and is reported as a comment hit.
	sec, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Body: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 1 || sec.Hits[0].Matched != MatchComment {
		t.Fatalf("hits = %+v, want the visible comment only", sec.Hits)
	}
	if strings.Contains(sec.Hits[0].Excerpt, "secret") {
		t.Fatalf("hidden text leaked into the excerpt: %q", sec.Hits[0].Excerpt)
	}
	if _, ok := func() (IssueHit, bool) {
		for _, h := range sec.Hits {
			if h.Matched == MatchBody {
				return h, true
			}
		}
		return IssueHit{}, false
	}(); ok {
		t.Fatal("hidden description produced a body hit")
	}

	// Hide the comment too: neither text may match.
	mod.hidden["thread:000001:event:000000000001"] = issues.HiddenInfo{By: "mod", At: "2026-11-21T00:00:00Z", Reason: "abuse"}
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Body: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 0 {
		t.Fatalf("hidden bodies matched: %+v", sec.Hits)
	}

	// A moderation read failure fails the search closed (never serve text we
	// cannot prove visible).
	mod.err = errors.New("moderation store down")
	if _, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Body: true})); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("moderation failure = %v, want ErrUnavailable", err)
	}
}

// ---- caps and completeness --------------------------------------------------

func TestIssueCapsAndCompleteAccounting(t *testing.T) {
	// Thread cap: 55 matching threads, only the newest 50 are body-scanned.
	st, iss := issueStore(t)
	cards := make([]issues.Card, 0, 55)
	for n := 1; n <= 55; n++ {
		cards = append(cards, putIssueThread(t, st, "acme", "app", issueSpec{Num: n, Title: fmt.Sprintf("needle %d", n), Author: "a", Body: "x"}))
	}
	putIndex(t, st, "acme", "app", true, cards...)
	s := NewIssueSearcher(iss)
	sec, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Body: true}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if sec.Complete || sec.Reason != IssueReasonThreads {
		t.Fatalf("thread-cap section = %+v", sec)
	}
	if sec.Scanned.Threads != MaxIssueThreads {
		t.Fatalf("threads scanned = %d, want %d", sec.Scanned.Threads, MaxIssueThreads)
	}

	// Event cap: one thread with 600 comments, only 500 events read.
	st, iss = issueStore(t)
	comments := make([]string, 600)
	for i := range comments {
		comments[i] = fmt.Sprintf("needle comment %d", i)
	}
	i1 := putIssueThread(t, st, "acme", "app", issueSpec{Num: 1, Title: "many comments", Author: "a", Body: "nothing", Comments: comments})
	putIndex(t, st, "acme", "app", true, i1)
	s = NewIssueSearcher(iss)
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Body: true, Limit: 200}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if sec.Complete || sec.Reason != IssueReasonEvents {
		t.Fatalf("event-cap section = %+v", sec)
	}
	if sec.Scanned.Events != MaxIssueEvents {
		t.Fatalf("events scanned = %d, want %d", sec.Scanned.Events, MaxIssueEvents)
	}

	// Limit cap: the next matching thread after the kept hits makes the
	// answer incomplete (the caller asked for fewer).
	st, iss = issueStore(t)
	cards = cards[:0]
	for n := 1; n <= 5; n++ {
		cards = append(cards, putIssueThread(t, st, "acme", "app", issueSpec{Num: n, Title: "needle", Author: "a"}))
	}
	putIndex(t, st, "acme", "app", true, cards...)
	s = NewIssueSearcher(iss)
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Limit: 2}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if sec.Complete || sec.Reason != IssueReasonLimit || len(sec.Hits) != 2 {
		t.Fatalf("limit-cap section = %+v", sec)
	}

	// Exactly limit hits is not a cap.
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle", Limit: 5}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if !sec.Complete || len(sec.Hits) != 5 {
		t.Fatalf("exact-limit section = %+v", sec)
	}
}

func TestIssueIndexFallbackAndFailures(t *testing.T) {
	// An index without the current Card projection is incomplete; the header
	// scan still finds the cards, and scanned.index_complete says so.
	st, iss := issueStore(t)
	i1 := putIssueThread(t, st, "acme", "app", issueSpec{Num: 1, Title: "needle one", Author: "jane"})
	putIndex(t, st, "acme", "app", false, i1)
	s := NewIssueSearcher(iss)
	sec, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle"}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 1 || sec.Hits[0].Num != 1 {
		t.Fatalf("fallback hits = %+v", sec.Hits)
	}
	if sec.Scanned.IndexComplete || sec.Complete || sec.Reason != IssueReasonIndex {
		t.Fatalf("fallback section = %+v", sec)
	}

	// An empty repository (no index, no counter, no headers) is a complete
	// empty answer, not an incomplete index forever.
	st, iss = issueStore(t)
	s = NewIssueSearcher(iss)
	sec, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "empty"}, issueQuery(IssueQuery{Text: "needle"}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if !sec.Complete || !sec.Scanned.IndexComplete || len(sec.Hits) != 0 {
		t.Fatalf("empty repo section = %+v", sec)
	}

	// Corrupt index: fail closed as unavailable, never an empty success.
	st, iss = issueStore(t)
	if _, err := store.PutBytes(context.Background(), st, issues.IndexKey("acme", "bad"), []byte("{bad"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	s = NewIssueSearcher(iss)
	_, err = s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "bad"}, issueQuery(IssueQuery{Text: "needle"}))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt index = %v, want ErrUnavailable", err)
	}

	// Corrupt header with an incomplete index is the same fail-closed answer.
	st, iss = issueStore(t)
	putJSON(t, st, issues.CounterKey("acme", "badhdr"), issues.Counter{Next: 2})
	putJSON(t, st, issues.IndexKey("acme", "badhdr"), issues.Index{Version: 1})
	if _, err := store.PutBytes(context.Background(), st, issues.ThreadKey("acme", "badhdr", 1), []byte("{bad"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	s = NewIssueSearcher(iss)
	if _, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "badhdr"}, issueQuery(IssueQuery{Text: "x"})); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt header = %v, want ErrUnavailable", err)
	}
}

// ---- seam error paths and helpers ------------------------------------------

func TestIssueSearcherSeamErrors(t *testing.T) {
	// A nil service is the named refusal, never an empty "nothing matched".
	s := NewIssueSearcher(nil)
	if _, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, IssueQuery{Text: "x"}); !errors.Is(err, ErrIssuesUnavailable) {
		t.Fatalf("nil service = %v", err)
	}
	// An owner-wide call is refused rather than searched without a gate.
	st, iss := issueStore(t)
	_ = st
	s = NewIssueSearcher(iss)
	if _, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme"}, IssueQuery{Text: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("owner-wide = %v", err)
	}
	// Direct-call grammar validation.
	for _, q := range []IssueQuery{
		{},
		{Text: strings.Repeat("x", MaxPatternBytes+1)},
		{Text: "x\ny"},
		{Text: "[", Regex: true},
	} {
		if _, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(q)); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid query %+v = %v", q, err)
		}
	}
	// Limit defaults and clamps even on a direct seam call.
	for _, q := range []IssueQuery{
		{Text: "x"},
		{Text: "x", Limit: MaxLimit + 20},
	} {
		if _, err := s.SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "empty"}, q); err != nil {
			t.Fatalf("limit %d = %v", q.Limit, err)
		}
	}
	// newIssueMatcher's own regex error path (the service validation normally
	// catches it first).
	if _, err := newIssueMatcher(IssueQuery{Text: "[", Regex: true}); err == nil {
		t.Fatal("invalid matcher regex accepted")
	}
}

func TestIssueNilLabelsAndExcerptEdges(t *testing.T) {
	st, iss := issueStore(t)
	// A card with no labels still produces a well-formed hit ([] not null).
	i1 := putIssueThread(t, st, "acme", "app", issueSpec{Num: 1, Title: "needle without labels", Author: "jane"})
	putIndex(t, st, "acme", "app", true, i1)
	sec, err := NewIssueSearcher(iss).SearchIssues(context.Background(), IssueScope{Owner: "acme", Repo: "app"}, issueQuery(IssueQuery{Text: "needle"}))
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(sec.Hits) != 1 || sec.Hits[0].Labels == nil {
		t.Fatalf("hit = %+v", sec.Hits)
	}

	// A match at the very start keeps the head and clips the tail; a match at
	// the very end keeps the tail and clips the head.
	head := "needle" + strings.Repeat(" tail", 100)
	if got := clipIssueExcerpt(head, 0, len("needle")); !strings.HasPrefix(got, "needle") || !strings.HasSuffix(got, "…") {
		t.Fatalf("head clip = %q", got)
	}
	tail := strings.Repeat("lead ", 100) + "needle"
	if got := clipIssueExcerpt(tail, strings.Index(tail, "needle"), strings.Index(tail, "needle")+len("needle")); !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "needle") {
		t.Fatalf("tail clip = %q", got)
	}
}

func TestIssueMatcherHelpers(t *testing.T) {
	if got := foldASCII("AbC_Z"); got != "abc_z" {
		t.Fatalf("foldASCII = %q", got)
	}
	if !isWordByte('_') || !isWordByte('9') || isWordByte('-') || isWordByte(0xc3) {
		t.Fatal("word-byte classification drifted from git -w")
	}
	if wordAt("a b", 2, 3) != true || wordAt("ab", 0, 1) != false || wordAt("ab", 1, 2) != false {
		t.Fatal("word boundaries")
	}
	// A zero-width regex match is not a hit (no excerpt, no false word).
	m, err := newIssueMatcher(IssueQuery{Text: "^", Regex: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.first("anything"); ok {
		t.Fatal("zero-width regex matched")
	}
	// UTF-8 clipping never splits a rune and keeps the match.
	text := strings.Repeat("é", 300) + "needle" + strings.Repeat("é", 300)
	clipped := clipIssueExcerpt(text, strings.Index(text, "needle"), strings.Index(text, "needle")+len("needle"))
	if !strings.Contains(clipped, "needle") || !utf8.ValidString(clipped) {
		t.Fatalf("utf8 clip = %q", clipped)
	}
	if short := "needle"; clipIssueExcerpt(short, 0, len("needle")) != short {
		t.Fatal("short text was clipped")
	}
	// The owner reason ranking is deterministic.
	cases := []struct {
		capped  bool
		reasons []IssueReason
		want    IssueReason
	}{
		{false, nil, IssueReasonIncomplete},
		{true, nil, IssueReasonCandidates},
		{false, []IssueReason{IssueReasonIndex, IssueReasonEvents}, IssueReasonEvents},
		{false, []IssueReason{IssueReasonLimit, IssueReasonFailed}, IssueReasonFailed},
		{false, []IssueReason{IssueReasonIndex, IssueReasonLimit}, IssueReasonLimit},
		{false, []IssueReason{IssueReasonIndex, IssueReasonThreads}, IssueReasonThreads},
		{false, []IssueReason{IssueReasonIndex}, IssueReasonIndex},
		{false, []IssueReason{IssueReasonDeadline, IssueReasonFailed}, IssueReasonDeadline},
		{false, []IssueReason{"something else"}, "something else"},
	}
	for _, tc := range cases {
		if got := issueOwnerReason(tc.capped, tc.reasons); got != tc.want {
			t.Fatalf("reason(%v, %v) = %q, want %q", tc.capped, tc.reasons, got, tc.want)
		}
	}
	sec := IssueSection{}
	normalizeIssueSection(&sec)
	if sec.Hits == nil || sec.Repos == nil {
		t.Fatalf("normalize = %+v", sec)
	}
}

// ---- service-level gating and owner scope ----------------------------------

func TestIssueGatingMatrixParity(t *testing.T) {
	// The same §8 matrix as the code section: a denied issue search returns
	// the 401/403/503 class before any query work.
	tests := []struct {
		name    string
		public  bool
		role    bool
		p       auth.Principal
		want    error
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
			iss := &fakeIssues{section: IssueSection{Complete: true, Hits: []IssueHit{{Repo: "acme/app", Num: 1}}}}
			svc := New(Options{Repos: &fakeRepos{}, Access: access, Issues: iss})
			ans, err := svc.Search(context.Background(), repoScope("acme", "app"), Params{Text: "x", Type: "issues"}, tc.p)
			if tc.want != nil {
				if !errors.Is(err, tc.want) || err.Error() != tc.message {
					t.Fatalf("err = %v, want %v (%s)", err, tc.want, tc.message)
				}
				if ans != nil {
					t.Fatalf("denied issue request returned an answer: %+v", ans)
				}
				return
			}
			if err != nil {
				t.Fatalf("allowed issue request: %v", err)
			}
			if ans.Issues == nil || ans.Issues.Hits == nil {
				t.Fatalf("issues section = %+v", ans)
			}
		})
	}
}

func TestIssueOwnerScopeGatingAndStatuses(t *testing.T) {
	st, iss := issueStore(t)
	for _, repo := range []string{"pub", "secret"} {
		i := putIssueThread(t, st, "acme", repo, issueSpec{Num: 1, Title: "needle " + repo, Author: "jane"})
		putIndex(t, st, "acme", repo, true, i)
	}
	repos := &fakeRepos{names: []string{"pub", "secret"}}
	access := &matrixAccess{public: map[string]bool{"acme/pub": true}}
	svc := New(Options{Repos: repos, Access: access, Issues: NewIssueSearcher(iss)})

	// Anonymous sees only the public repository; the denied one contributes
	// no hit and no repo entry.
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "needle", Type: "issues"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ans.Issues.Hits) != 1 || ans.Issues.Hits[0].Repo != "acme/pub" {
		t.Fatalf("anonymous hits = %+v", ans.Issues.Hits)
	}
	if len(ans.Issues.Repos) != 1 || ans.Issues.Repos[0].Repo != "acme/pub" || ans.Issues.Repos[0].Status != StatusOK {
		t.Fatalf("anonymous repos = %+v", ans.Issues.Repos)
	}
	if !ans.Issues.Complete {
		t.Fatalf("visible set answered fully: %+v", ans.Issues)
	}

	// A signed-in reader of both sees both, in name order.
	access.role = map[string]bool{"acme/secret": true}
	ans, err = svc.Search(context.Background(), ownerScope("acme"), Params{Text: "needle", Type: "issues"}, user)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ans.Issues.Hits) != 2 || ans.Issues.Hits[0].Repo != "acme/pub" || ans.Issues.Hits[1].Repo != "acme/secret" {
		t.Fatalf("reader hits = %+v", ans.Issues.Hits)
	}

	// One broken repository is a per-repository failed status; the other
	// answer survives and completeness says so.
	if _, err := store.PutBytes(context.Background(), st, issues.IndexKey("acme", "secret"), []byte("{bad"), store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	ans, err = svc.Search(context.Background(), ownerScope("acme"), Params{Text: "needle", Type: "issues"}, user)
	if err != nil {
		t.Fatalf("per-repo failure must not fail the request: %v", err)
	}
	if ans.Issues.Complete || ans.Issues.Reason != IssueReasonFailed {
		t.Fatalf("failed-repo section = %+v", ans.Issues)
	}
	byRepo := map[string]IssueRepo{}
	for _, r := range ans.Issues.Repos {
		byRepo[r.Repo] = r
	}
	if byRepo["acme/pub"].Status != StatusOK || byRepo["acme/secret"].Status != StatusFailed {
		t.Fatalf("repo statuses = %+v", ans.Issues.Repos)
	}
	if len(ans.Issues.Hits) != 1 || ans.Issues.Hits[0].Repo != "acme/pub" {
		t.Fatalf("surviving hits = %+v", ans.Issues.Hits)
	}
}

func TestIssueOwnerScopeBoundedCandidates(t *testing.T) {
	// The 50-candidate bound is the code section's; repositories beyond it are
	// not considered and the section says complete:false.
	st, iss := issueStore(t)
	_ = st
	names := make([]string, MaxOwnerCandidates+7)
	public := map[string]bool{}
	for i := range names {
		names[i] = fmt.Sprintf("r%02d", i)
		public["acme/"+names[i]] = true
	}
	svc := New(Options{Repos: &fakeRepos{names: names}, Access: &matrixAccess{public: public}, Issues: NewIssueSearcher(iss)})
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "issues"}, anon)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ans.Issues.Repos) != MaxOwnerCandidates {
		t.Fatalf("repos = %d, want %d", len(ans.Issues.Repos), MaxOwnerCandidates)
	}
	if ans.Issues.Complete || ans.Issues.Reason != IssueReasonCandidates {
		t.Fatalf("capped section = %+v", ans.Issues)
	}
	// Every considered repo is an empty repository and therefore complete;
	// the cap alone makes the section incomplete.
	for _, r := range ans.Issues.Repos {
		if r.Status != StatusOK || !r.Complete {
			t.Fatalf("empty repo = %+v", r)
		}
	}
}

func TestIssueOwnerScopeListingFailureAndCanceledCall(t *testing.T) {
	st, iss := issueStore(t)
	_ = st
	// A non-identity listing failure is the unavailable class.
	svc := New(Options{Repos: &fakeRepos{listErr: errors.New("listing exploded")}, Access: allowAll(), Issues: NewIssueSearcher(iss)})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "issues"}, anon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("listing failure = %v, want ErrUnavailable", err)
	}

	// A caller cancellation during the owner listing is returned as the
	// context error, never an empty answer.
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	repos := &fakeRepos{listFn: func(ctx context.Context) ([]string, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc = New(Options{Repos: repos, Access: allowAll(), Issues: NewIssueSearcher(iss)})
	done := make(chan error, 1)
	go func() {
		_, err := svc.Search(ctx, ownerScope("acme"), Params{Text: "x", Type: "issues"}, anon)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled owner listing = %v, want context.Canceled", err)
	}

	// A caller that disconnects before the issue request is admitted never
	// starts work: the request pool answers the context error.
	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	svc = New(Options{Repos: &fakeRepos{}, Access: allowAll(), Issues: NewIssueSearcher(iss)})
	if _, err := svc.Search(canceled, repoScope("acme", "app"), Params{Text: "x", Type: "issues"}, user); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission = %v, want context.Canceled", err)
	}
}

func TestIssueOwnerScopeIdentityFailureAndDeadline(t *testing.T) {
	st, iss := issueStore(t)
	_ = st
	// An identity outage is a whole-request 503, never a per-repo status.
	svc := New(Options{Repos: &fakeRepos{names: []string{"a"}}, Access: &matrixAccess{down: true}, Issues: NewIssueSearcher(iss)})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "issues"}, anon); !errors.Is(err, ErrUnavailable) || err.Error() != "access check unavailable" {
		t.Fatalf("identity failure = %v", err)
	}

	// A deadline inside the listing is an incomplete answer, not an outage.
	svc = New(Options{Repos: &fakeRepos{listFn: func(ctx context.Context) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}, Access: allowAll(), Issues: NewIssueSearcher(iss)})
	svc.ownerDeadline = 20 * time.Millisecond
	ans, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "issues"}, anon)
	if err != nil {
		t.Fatalf("deadline answer: %v", err)
	}
	if ans.Issues.Complete || ans.Issues.Reason != IssueReasonDeadline || ans.Issues.Hits == nil {
		t.Fatalf("deadline section = %+v", ans.Issues)
	}

	// An unwired issue seam is refused before any listing.
	svc = New(Options{Repos: &fakeRepos{names: []string{"a"}}, Access: allowAll()})
	if _, err := svc.Search(context.Background(), ownerScope("acme"), Params{Text: "x", Type: "issues"}, anon); !errors.Is(err, ErrIssuesUnavailable) {
		t.Fatalf("unwired issue seam = %v", err)
	}
}
