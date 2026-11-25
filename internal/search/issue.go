// issue.go — the P4 implementation of the frozen IssueSearcher seam
// (docs/features/20_search.md §7, §12 decisions 4 and 12). It matches the
// existing shared issue/PR index cards (title, labels, author, state, kind)
// and, in body mode, the bounded redacted body/comment projection
// internal/issues exposes (SearchProjection). It owns no bucket read itself:
// the index cards, the event scan and the moderation fold all live in the
// owning package, so P4 adds no object family and no second index (§1, §2).
//
// Matching is the same grammar the code section uses: ASCII case-insensitive
// by default, fixed strings by default, POSIX ERE behind regex=1 and whole
// word behind word=1. Excerpts are clipped here, deterministically, from the
// already-redacted text; the transport escapes them (the server renderer is
// the XSS boundary, §8).
package search

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crueber/spindle/internal/issues"
)

// Issue scan caps (§5.2). Body/comment mode is bounded per repository: the
// newest 50 threads and at most 500 events across them. Metadata-only reads
// are not subject to these caps because they do not scan events.
const (
	// MaxIssueThreads is the number of threads body-scanned per repository.
	MaxIssueThreads = 50
	// MaxIssueEvents is the total event reads per repository in body mode.
	MaxIssueEvents = 500
	// IssueExcerptRunes is the excerpt window kept around a body/comment
	// match (96 runes before and after). It is deterministic and rune-safe;
	// the matched span itself is always inside the window.
	IssueExcerptRunes = 192
)

// Match locations on IssueHit.Matched. The frozen wire vocabulary is
// title|body|comment|label|author; P4 adds state and kind because the card
// carries both and a search must be able to say that is where it hit (the P4
// card's metadata requirement). A hit is still never a body excerpt unless
// Matched is body or comment.
const (
	MatchTitle   = "title"
	MatchBody    = "body"
	MatchComment = "comment"
	MatchLabel   = "label"
	MatchAuthor  = "author"
	MatchState   = "state"
	MatchKind    = "kind"
)

// IssueSearcher returns the concrete IssueSearcher over the issues service.
// A nil service is accepted: the returned searcher answers
// ErrIssuesUnavailable on every call, so composition never has to special-case
// a boot without the collaboration layer.
func NewIssueSearcher(svc *issues.Service) IssueSearcher { return &issueSearcher{svc: svc} }

type issueSearcher struct {
	svc *issues.Service
}

var _ IssueSearcher = (*issueSearcher)(nil)

// SearchIssues implements the frozen seam. scope.Repo must be set: P4's
// service enumerates the visible owner set and calls this once per repository
// (owner-wide fan-out needs the caller's principal and read gate, which the
// frozen seam does not carry). An unscoped owner call is refused rather than
// searching fenceless.
func (s *issueSearcher) SearchIssues(ctx context.Context, scope IssueScope, q IssueQuery) (IssueSection, error) {
	if s == nil || s.svc == nil {
		return IssueSection{}, Wrap(ErrIssuesUnavailable, ErrIssuesUnavailable.Error())
	}
	if scope.Owner == "" || scope.Repo == "" {
		return IssueSection{}, Wrap(ErrUnavailable, "issue search is scoped per repository")
	}
	if err := validateIssueQuery(&q); err != nil {
		return IssueSection{}, err
	}
	m, err := newIssueMatcher(q)
	if err != nil {
		return IssueSection{}, err
	}

	maxThreads, maxEvents := 0, 0
	if q.Body {
		maxThreads, maxEvents = MaxIssueThreads, MaxIssueEvents
	}
	proj, err := s.svc.SearchProjection(ctx, scope.Owner, scope.Repo, maxThreads, maxEvents)
	if err != nil {
		if ctx.Err() != nil {
			return IssueSection{}, ctx.Err()
		}
		return IssueSection{}, Wrap(ErrUnavailable, ErrUnavailable.Error())
	}

	bodies := make(map[int][]issues.SearchText, len(proj.Threads))
	for _, th := range proj.Threads {
		bodies[th.Card.Num] = th.Texts
	}

	hits := make([]IssueHit, 0, len(proj.Cards))
	limitHit := false
	for _, c := range proj.Cards {
		matched, excerpt, ok := m.card(c, bodies[c.Num])
		if !ok {
			continue
		}
		if len(hits) >= q.Limit {
			limitHit = true
			break
		}
		hits = append(hits, issueHit(scope.Owner, scope.Repo, c, matched, excerpt))
	}

	reason := IssueReason("")
	switch {
	case proj.EventsTruncated:
		reason = IssueReasonEvents
	case proj.ThreadsTruncated:
		reason = IssueReasonThreads
	case !proj.IndexComplete:
		reason = IssueReasonIndex
	}
	if limitHit {
		reason = IssueReasonLimit
	}

	complete := proj.IndexComplete && !proj.ThreadsTruncated && !proj.EventsTruncated && !limitHit
	return IssueSection{
		Complete: complete,
		Reason:   reason,
		Scanned: IssueScan{
			Threads:       proj.ThreadsScanned,
			Events:        proj.EventsScanned,
			IndexComplete: proj.IndexComplete,
		},
		Hits: hits,
	}, nil
}

// validateIssueQuery re-applies the §5.1 grammar to the seam's inputs. The
// service has already validated them; a direct caller (a test, a future
// embedding) gets the same 400-class answer rather than an unbounded scan.
func validateIssueQuery(q *IssueQuery) error {
	switch {
	case q.Text == "":
		return &InvalidError{Field: "q", Why: "q is required"}
	case len(q.Text) > MaxPatternBytes:
		return &InvalidError{Field: "q", Why: fmt.Sprintf("q exceeds %d bytes", MaxPatternBytes)}
	case strings.ContainsAny(q.Text, "\x00\n\r"):
		return &InvalidError{Field: "q", Why: "q contains a NUL, CR or newline"}
	}
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	if q.Regex {
		if _, err := regexp.CompilePOSIX(q.Text); err != nil {
			return &InvalidError{Field: "q", Why: "invalid regular expression: " + err.Error()}
		}
	}
	return nil
}

// issueHit projects one card onto the frozen wire hit. URL is derived from
// kind server-side; labels are never nil.
func issueHit(owner, repo string, c issues.Card, matched, excerpt string) IssueHit {
	route := "issues"
	if c.Kind == "pr" {
		route = "pulls"
	}
	labels := c.Labels
	if labels == nil {
		labels = []string{}
	}
	return IssueHit{
		Repo:      owner + "/" + repo,
		Num:       c.Num,
		Kind:      c.Kind,
		Title:     c.Title,
		State:     c.State,
		Author:    c.Author,
		Labels:    labels,
		UpdatedAt: c.UpdatedAt,
		Matched:   matched,
		Excerpt:   excerpt,
		URL:       "/" + owner + "/" + repo + "/" + route + "/" + strconv.Itoa(c.Num),
	}
}

// ---- matching ---------------------------------------------------------------

// issueMatcher is the issue-side twin of the code section's grammar: a fixed
// string or POSIX ERE, ASCII case-insensitive by default, optional whole-word
// boundaries, no ranking. It reports the first qualifying match so an excerpt
// can be clipped around it deterministically.
type issueMatcher struct {
	literal   string
	regex     *regexp.Regexp
	word      bool
	caseExact bool
}

func newIssueMatcher(q IssueQuery) (*issueMatcher, error) {
	m := &issueMatcher{word: q.Word, caseExact: q.CaseExact}
	if q.Regex {
		pat := q.Text
		if !q.CaseExact {
			// CompilePOSIX (above) validated the dialect; the (?i) form is
			// RE2's linear-time case fold, used only to mirror the code
			// section's default case-insensitivity.
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, &InvalidError{Field: "q", Why: "invalid regular expression: " + err.Error()}
		}
		m.regex = re
		return m, nil
	}
	m.literal = q.Text
	if !q.CaseExact {
		m.literal = foldASCII(q.Text)
	}
	return m, nil
}

// first returns the first qualifier's byte span in text.
func (m *issueMatcher) first(text string) (start, end int, ok bool) {
	if m.regex != nil {
		for _, loc := range m.regex.FindAllStringIndex(text, -1) {
			if loc[0] == loc[1] {
				continue // a zero-width match has no excerpt and no word
			}
			if !m.word || wordAt(text, loc[0], loc[1]) {
				return loc[0], loc[1], true
			}
		}
		return 0, 0, false
	}
	hay := text
	if !m.caseExact {
		hay = foldASCII(text)
	}
	for off := 0; off <= len(hay)-len(m.literal); {
		i := strings.Index(hay[off:], m.literal)
		if i < 0 {
			return 0, 0, false
		}
		start = off + i
		end = start + len(m.literal)
		if !m.word || wordAt(text, start, end) {
			return start, end, true
		}
		off = start + 1
	}
	return 0, 0, false
}

// card classifies one thread: metadata first (so a title hit never carries a
// body excerpt), then the opened body, then comments.
func (m *issueMatcher) card(c issues.Card, texts []issues.SearchText) (matched, excerpt string, ok bool) {
	if _, _, hit := m.first(c.Title); hit {
		return MatchTitle, "", true
	}
	for _, label := range c.Labels {
		if _, _, hit := m.first(label); hit {
			return MatchLabel, "", true
		}
	}
	if _, _, hit := m.first(c.Author); hit {
		return MatchAuthor, "", true
	}
	if _, _, hit := m.first(c.State); hit {
		return MatchState, "", true
	}
	if _, _, hit := m.first(c.Kind); hit {
		return MatchKind, "", true
	}
	for _, t := range texts {
		if t.Kind != "body" {
			continue
		}
		if start, end, hit := m.first(t.Text); hit {
			return MatchBody, clipIssueExcerpt(t.Text, start, end), true
		}
	}
	for _, t := range texts {
		if t.Kind != "comment" {
			continue
		}
		if start, end, hit := m.first(t.Text); hit {
			return MatchComment, clipIssueExcerpt(t.Text, start, end), true
		}
	}
	return "", "", false
}

// foldASCII lowercases A-Z only. The code section runs grep under LC_ALL=C,
// so its -i is ASCII-only; issue matching keeps the same promise rather than
// silently accepting Unicode case variants a second way (§5.3, decision 2).
func foldASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// isWordByte mirrors git grep -w's ASCII word character under LC_ALL=C:
// letters, digits and underscore. A non-ASCII byte is not a word character.
func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func wordAt(text string, start, end int) bool {
	if start > 0 && isWordByte(text[start-1]) {
		return false
	}
	if end < len(text) && isWordByte(text[end]) {
		return false
	}
	return true
}

// clipIssueExcerpt keeps a deterministic window around the first match and
// never splits a UTF-8 sequence. The full matched span is always inside the
// window (a match longer than the window widens it); the markers say the text
// was clipped, and the transport escapes the result (§8).
func clipIssueExcerpt(text string, start, end int) string {
	runes := []rune(text)
	if len(runes) <= IssueExcerptRunes {
		return text
	}
	rstart := utf8.RuneCountInString(text[:start])
	rend := utf8.RuneCountInString(text[:end])
	const half = IssueExcerptRunes / 2
	lo := rstart - half
	if lo < 0 {
		lo = 0
	}
	hi := lo + IssueExcerptRunes
	if hi < rend {
		hi = rend
	}
	if hi > len(runes) {
		hi = len(runes)
		lo = hi - IssueExcerptRunes
		if lo < 0 {
			lo = 0
		}
	}
	out := string(runes[lo:hi])
	if lo > 0 {
		out = "…" + out
	}
	if hi < len(runes) {
		out += "…"
	}
	return out
}
