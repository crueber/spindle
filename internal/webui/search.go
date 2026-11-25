// search.go — the two code-search pages (docs/features/20_search.md §7;
// docs/go/12_web_ui.md §2.3.1 "Search pages"). Feature 20's HTTP lane is the
// JSON API in internal/api; these pages read the SAME service
// (api.Env.Search, bound by composition) so a page and its API twin can never
// disagree about what was searched or how complete the answer is.
//
//	GET /{owner}/{repo}/search   repository chrome + one h1 (the masthead)
//	GET /search?owner=&q=        the non-repository owner surface (gated())
//
// Design rules this file keeps:
//   - The gate is the Code page's: repoGate/repoVisible, so an invisible
//     repository is a 404 page and the URL has no privilege of its own
//     (AGENTS.md §2 "A page gate must mirror the API's gate exactly").
//   - No JavaScript: both forms are native GET forms; htmx is not used.
//   - The server renderer IS the XSS boundary: the matched span is split into
//     three ordinary strings (before/mark/after) and html/template escapes
//     every one of them. No template.HTML touches a user byte here.
//   - Every cap, deadline and per-repository status the service reports is
//     rendered; an answer is never a silently short list (AGENTS.md law 7).
//   - Search results are principal-, ref- and query-dependent: Cache-Control
//     is no-store and the response carries no ETag (20_search.md §8, §12.11).
package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/cachepolicy"
	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/server/auth"
)

func init() {
	registerGroup(func(s *Server) {
		// The repo route is a literal child of /{owner}/{repo}; /search is a
		// literal top-level route (it outranks GET /{owner} in ServeMux, and
		// "search" is reserved in reservedOwnerNames so an owner by that name
		// is documented as shadowed rather than surprising).
		s.mux.HandleFunc("GET /{owner}/{repo}/search", s.searchRepoPage)
		s.mux.HandleFunc("GET /search", s.searchOwnerPage)
	})
}

// --- view model --------------------------------------------------------------

// searchPage is the shared view model for both pages. The repo page carries
// Repo (the chrome the template renders inside content, the repo-group shape);
// the owner page leaves it nil and the layout adds no masthead.
type searchPage struct {
	Repo *RepoChrome

	// OwnerScope distinguishes the two forms: the owner page gains an owner
	// input and drops the ref control (ref is repository-scope only, §5.1).
	OwnerScope bool
	Owner      string
	Action     string
	// ClearHref resets the query but keeps the surface's context (the owner on
	// the owner page); the repo page's clear link is its own bare path.
	ClearHref string

	// Form state (always echoed back, including after a rejected query).
	Query         string
	Type          string
	Regex         bool
	CaseSensitive bool
	Word          bool
	Ref           string
	DefaultRef    string
	PathsText     string
	OptionsOpen   bool
	Placeholder   string

	// Queried means a non-empty q was submitted; it gates the results block
	// (an empty form is not an empty answer).
	Queried bool

	// Answer projections.
	Code       *searchCodeVM
	Issues     *searchIssueVM
	StatusLine string

	// Error is the plain-text reason (escaped by the template) for a rejected
	// query or an unavailable search: role="alert".
	Error string
	// Notice is an informational state (no query yet, empty repository, why a
	// cap applied): never an error.
	Notice string
	// IncompleteNotice is the section-level "this answer is incomplete"
	// sentence; the per-repository notices below carry the details.
	IncompleteNotice string
}

// searchCodeVM is one "code" section projected for display.
type searchCodeVM struct {
	Complete   bool
	Considered int
	Searched   int
	Repos      []searchRepoVM
	// Note explains an empty candidate set without naming anything: a denied
	// repository contributes no name, count or reason (§8 leak rules).
	Note string
}

// searchRepoVM is one repository's answer (§6.2 code.results[]).
type searchRepoVM struct {
	Repo  string
	Ref   string
	Short string
	Meta  string

	Status string
	Chip   string
	// ChipClass is one of the shared chip classes; state is never colour-only
	// because Chip always carries its word.
	ChipClass string
	Notice    string

	Truncated bool
	TimedOut  bool
	// Degraded is true when this repository's answer is capped or timed out.
	Degraded bool

	MatchCount int
	FileCount  int
	Files      []searchFileVM
}

// searchFileVM groups one path's matches.
type searchFileVM struct {
	Path    string
	Href    string
	Matches []searchMatchVM
}

// searchMatchVM is one line, split around the matched span. Before/Mark/After
// are plain strings: the template escapes them, so a matched line containing
// markup can never become markup.
type searchMatchVM struct {
	Line    int
	Column  int
	HasMark bool
	Before  string
	Mark    string
	After   string
	Text    string // the whole line, rendered when the span could not be located
}

// searchIssueVM is one "issues" section projected for display (§6.3).
type searchIssueVM struct {
	Complete   bool
	StatusLine string
	Notice     string
	Hits       []searchIssueHitVM
}

// searchIssueHitVM is one issue or pull-request hit.
type searchIssueHitVM struct {
	Repo       string
	Num        int
	Kind       string
	Title      string
	State      string
	StateClass string
	Author     string
	Matched    string
	URL        string
	Labels     []string
}

// --- handlers ----------------------------------------------------------------

// searchRepoPage answers GET /{owner}/{repo}/search (§7.1). The gate is the
// Code page's, so an invisible repository is a 404 page; the chrome is the
// shared masthead + tab strip (the search box lives in that masthead).
func (s *Server) searchRepoPage(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	data := &searchPage{
		Repo:        c,
		Action:      "/" + c.Full + "/search",
		ClearHref:   "/" + c.Full + "/search",
		Type:        string(search.TypeCode),
		DefaultRef:  c.HeadLabel,
		Placeholder: "text or pattern",
	}
	if c.Empty {
		// An unborn repository has no tree to grep. The form stays visible
		// (the box is chrome) and the page says why instead of paying a
		// resolve that can only answer "empty repository".
		data.Notice = "This repository is empty, so there is nothing to search yet. Push a commit and search again."
		s.renderSearch(w, r, rq.p, data, http.StatusOK)
		return
	}
	s.runSearchPage(w, r, rq.p, data, search.Scope{
		Kind:  search.ScopeRepo,
		Owner: c.Owner,
		Repo:  c.Name,
	})
}

// searchOwnerPage answers GET /search?owner=&q= (§7.2). It is not repo-scoped,
// so gated() (anonymous_read) applies — the same rule every public page reads.
// Without an owner the page does not guess and never searches the instance.
func (s *Server) searchOwnerPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	owner := normalizeSearchOwner(r.URL.Query().Get("owner"))
	data := &searchPage{
		OwnerScope:  true,
		Owner:       owner,
		Action:      "/search",
		ClearHref:   "/search",
		Type:        string(search.TypeCode),
		Placeholder: "text or pattern",
	}
	if owner != "" {
		data.ClearHref = "/search?owner=" + url.QueryEscape(owner)
	}
	if owner == "" {
		// The template renders the "v1 needs an owner" explanation with the
		// repository-picker link; the form is still there so a visitor can
		// name one.
		s.renderSearch(w, r, p, data, http.StatusOK)
		return
	}
	s.runSearchPage(w, r, p, data, search.Scope{
		Kind:  search.ScopeOwner,
		Owner: owner,
	})
}

// runSearchPage is the one body both handlers share: decode, gate-ordered
// service call, and the documented failure/notice mapping (§10/§9 — the page
// renders the sentences as notices so the search box never disappears).
func (s *Server) runSearchPage(w http.ResponseWriter, r *http.Request, p auth.Principal, data *searchPage, scope search.Scope) {
	form, err := decodeSearchForm(r, data.OwnerScope)
	data.applyForm(form)
	if err != nil {
		data.Error = err.Error()
		s.renderSearch(w, r, p, data, http.StatusBadRequest)
		return
	}
	if form.query == "" {
		if data.OwnerScope {
			data.Notice = "Enter a term to search repositories under " + data.Owner + "."
		} else {
			data.Notice = "Enter a term to search this repository."
		}
		s.renderSearch(w, r, p, data, http.StatusOK)
		return
	}
	data.Queried = true

	svc := s.searchService()
	if svc == nil {
		data.Error = search.ErrUnavailable.Error()
		s.renderSearch(w, r, p, data, http.StatusServiceUnavailable)
		return
	}
	ans, err := svc.Search(r.Context(), scope, form.params(), p)
	if err != nil {
		s.searchFailed(w, r, p, data, err)
		return
	}
	data.applyAnswer(ans)
	s.renderSearch(w, r, p, data, http.StatusOK)
}

// searchFailed maps the service's classified failures onto the page (§10).
// A query failure keeps the form (400 + alert); cold/remote/disabled states
// render their sentence with the box visible (§9). Existence and auth
// decisions are the ordinary error page, exactly like every other repo page.
func (s *Server) searchFailed(w http.ResponseWriter, r *http.Request, p auth.Principal, data *searchPage, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller went away: nothing to render.
		return
	case errors.Is(err, search.ErrInvalidQuery), errors.Is(err, search.ErrUnsupportedScope):
		data.Error = err.Error()
		s.renderSearch(w, r, p, data, http.StatusBadRequest)
	case errors.Is(err, search.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, err.Error())
	case errors.Is(err, search.ErrUnauthorized):
		s.errorPage(w, r, http.StatusUnauthorized, err.Error())
	case errors.Is(err, search.ErrForbidden):
		s.errorPage(w, r, http.StatusForbidden, err.Error())
	case errors.Is(err, search.ErrWarming):
		// §9: the serve bound fired; the caller retries in a few seconds.
		w.Header().Set("Retry-After", "5")
		data.Error = err.Error()
		s.renderSearch(w, r, p, data, http.StatusServiceUnavailable)
	case errors.Is(err, search.ErrNotLocal),
		errors.Is(err, search.ErrDisabled),
		errors.Is(err, search.ErrIssuesUnavailable),
		errors.Is(err, search.ErrUnavailable):
		data.Error = err.Error()
		s.renderSearch(w, r, p, data, http.StatusServiceUnavailable)
	default:
		// Never a 500 for a search: the documented 503 (the service already
		// classifies everything it owns).
		data.Error = search.ErrUnavailable.Error()
		s.renderSearch(w, r, p, data, http.StatusServiceUnavailable)
	}
}

// searchService returns the code-search seam, or nil when the composition did
// not bind it. A nil seam answers the documented 503: never an ungated search.
func (s *Server) searchService() api.SearchService {
	if s.env.API == nil {
		return nil
	}
	return s.env.API.Search
}

// renderSearch writes one search page. no-store is set before the render (the
// renderer keeps a page's own class and skips the ETag for a no-store body):
// the result is principal-, ref- and query-dependent (§8, §12.11).
func (s *Server) renderSearch(w http.ResponseWriter, r *http.Request, p auth.Principal, data *searchPage, status int) {
	w.Header().Set("Cache-Control", cachepolicy.NoStore)
	name, title := "public/search", "Search"
	if data.Repo != nil {
		name = "repo/search"
		title = "Search · " + data.Repo.Full
	} else if data.Owner != "" {
		title = "Search · " + data.Owner
	}
	// The repo page renders the chrome inside its content (the repo-group
	// shape), so the layout must not add a second masthead.
	s.page(w, r, p, status, name, title, data)
}

// --- request decoding --------------------------------------------------------

// searchForm is one decoded page request. It is populated as far as the input
// allows even when a knob is rejected, so the re-rendered form echoes what the
// visitor typed.
type searchForm struct {
	query         string
	typ           string
	regex         bool
	caseSensitive bool
	word          bool
	ref           string
	paths         []string
}

// params projects the form onto the service's frozen request shape (§5.1).
// The page does not expose `limit`: the service's default applies and the
// status line states the cap the service echoed back.
func (f searchForm) params() search.Params {
	return search.Params{
		Text:  f.query,
		Type:  f.typ,
		Regex: f.regex,
		Case:  f.caseSensitive,
		Word:  f.word,
		Paths: f.paths,
		Ref:   f.ref,
	}
}

// decodeSearchForm reads the §5.1 knobs from the query string. It validates
// only the shape the page owns (the 0|1 spellings and the type word); text,
// paths and ref rules are the service's Parse, so every lane reads the same
// sentences. The page's default type is code on both surfaces: coding is what
// ships in v1, and the form sends the choice explicitly.
func decodeSearchForm(r *http.Request, ownerScope bool) (searchForm, error) {
	q := r.URL.Query()
	f := searchForm{
		query: q.Get("q"),
		typ:   strings.TrimSpace(q.Get("type")),
		ref:   q.Get("ref"),
	}
	if ownerScope {
		// ref is repository-scope only (§5.1); the owner form does not carry
		// it, so a stray parameter is not forwarded into a 400.
		f.ref = ""
	}
	f.paths = splitSearchPaths(q["path"])
	if f.typ == "" {
		f.typ = string(search.TypeCode)
	}
	switch search.Type(f.typ) {
	case search.TypeCode, search.TypeIssues, search.TypeAll:
	default:
		return f, fmt.Errorf("type must be code, issues or all (got %q)", f.typ)
	}
	var err error
	if f.regex, err = decodeSearchSwitch(q.Get("regex"), "regex"); err != nil {
		return f, err
	}
	if f.caseSensitive, err = decodeSearchSwitch(q.Get("case"), "case"); err != nil {
		return f, err
	}
	if f.word, err = decodeSearchSwitch(q.Get("word"), "word"); err != nil {
		return f, err
	}
	return f, nil
}

// decodeSearchSwitch reads a 0|1 knob. Absent and "0" are false (the §5.1
// defaults); anything else names the knob, exactly like the API's 400.
func decodeSearchSwitch(raw, name string) (bool, error) {
	switch raw {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be 0 or 1", name)
	}
}

// splitSearchPaths decodes the path filter. The form is one textarea (one
// pathspec per line, the honest no-JS way to accept up to eight), while a
// hand-written URL may repeat ?path=. Both spellings land in the same list;
// empty lines are dropped (an untouched textarea posts one empty value).
func splitSearchPaths(values []string) []string {
	var out []string
	for _, v := range values {
		for _, line := range strings.Split(v, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// normalizeSearchOwner accepts the handle spelling people paste ("@acme") and
// trims surrounding space. Character validation stays the service's (§5.1).
func normalizeSearchOwner(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "@")
}

// --- form/answer projection --------------------------------------------------

// applyForm copies the decoded request onto the view model.
func (d *searchPage) applyForm(f searchForm) {
	d.Query = f.query
	d.Type = f.typ
	d.Regex = f.regex
	d.CaseSensitive = f.caseSensitive
	d.Word = f.word
	d.Ref = f.ref
	d.PathsText = strings.Join(f.paths, "\n")
	d.OptionsOpen = f.regex || f.caseSensitive || f.word || f.ref != "" || len(f.paths) > 0
}

// applyAnswer projects the service's answer onto the page. The form state is
// re-read from the answer's options echo so the rendered controls show the
// NORMALISED query (the same values the API publishes).
func (d *searchPage) applyAnswer(ans *search.Answer) {
	if ans == nil {
		return
	}
	if ans.Query != "" {
		d.Query = ans.Query
	}
	if ans.Type != "" {
		d.Type = string(ans.Type)
	}
	d.Regex = ans.Options.Regex
	d.CaseSensitive = ans.Options.CaseSensitive
	d.Word = ans.Options.Word
	if len(ans.Options.Paths) > 0 {
		d.PathsText = strings.Join(ans.Options.Paths, "\n")
	}
	d.OptionsOpen = d.OptionsOpen || d.Regex || d.CaseSensitive || d.Word ||
		d.Ref != "" || d.PathsText != ""

	var lines []string
	if ans.Code != nil {
		d.Code = projectCodeSection(ans.Code, ans.Query, ans.Options, d.OwnerScope)
		if line := codeStatusLine(d.Code, d.OwnerScope); line != "" {
			lines = append(lines, line)
		}
	}
	if ans.Issues != nil {
		d.Issues = projectIssueSection(ans.Issues)
		if d.Issues.StatusLine != "" {
			lines = append(lines, d.Issues.StatusLine)
		}
	}
	d.StatusLine = strings.Join(lines, " · ")
	if (d.Code != nil && !d.Code.Complete) || (d.Issues != nil && !d.Issues.Complete) {
		d.IncompleteNotice = "This answer is incomplete — every gap is listed with its reason below."
	}
}

// projectCodeSection converts the wire section into display rows (§6.2/§6.4).
func projectCodeSection(sec *search.CodeSection, pattern string, opts search.QueryOptions, ownerScope bool) *searchCodeVM {
	vm := &searchCodeVM{Complete: sec.Complete, Considered: sec.ReposConsidered, Searched: sec.ReposSearched}
	if ownerScope && sec.ReposConsidered == 0 {
		vm.Note = "No repositories were considered: the owner has none, or none are visible to you."
	}
	for _, r := range sec.Results {
		rv := searchRepoVM{
			Repo:       r.Repo,
			Ref:        shortRefName(r.Ref),
			Status:     string(r.Status),
			MatchCount: r.MatchCount,
			FileCount:  r.FileCount,
			Truncated:  r.Truncated,
			TimedOut:   r.TimedOut,
			Degraded:   r.Truncated || r.TimedOut,
		}
		if r.SHA != "" {
			rv.Short = shortSHA(r.SHA)
		}
		// The state is always a word, never colour alone (§6.4); status:"ok"
		// needs no chip, only a cap does.
		switch search.Status(r.Status) {
		case search.StatusOK:
			switch {
			case r.TimedOut:
				rv.Chip, rv.ChipClass = "timed out", "chip chip-draft"
			case r.Truncated:
				rv.Chip, rv.ChipClass = "truncated", "chip chip-draft"
			}
		case search.StatusNotWarm:
			rv.Chip, rv.ChipClass = "not cached", "chip chip-neutral"
		case search.StatusNotSearched:
			rv.Chip, rv.ChipClass = "not searched", "chip chip-neutral"
		case search.StatusFailed:
			rv.Chip, rv.ChipClass = "failed", "chip chip-closed"
		default:
			rv.Chip, rv.ChipClass = string(r.Status), "chip chip-neutral"
		}
		if r.Status != search.StatusOK {
			rv.Notice = "Not in this answer — " + firstNonEmpty(r.Reason, "no reason reported") + "."
		}
		if ownerScope {
			rv.Meta = strings.TrimSpace(rv.Ref)
			if rv.Short != "" {
				rv.Meta = strings.TrimSpace(rv.Meta + " · " + rv.Short)
			}
		}
		for _, f := range r.Files {
			fv := searchFileVM{
				Path: f.Path,
				Href: searchFileHref(r.Repo, r.SHA, f.Path),
			}
			for _, m := range f.Matches {
				fv.Matches = append(fv.Matches, projectMatch(m, pattern, opts))
			}
			rv.Files = append(rv.Files, fv)
		}
		vm.Repos = append(vm.Repos, rv)
	}
	return vm
}

// projectIssueSection converts the issues section into display rows (§6.3).
func projectIssueSection(sec *search.IssueSection) *searchIssueVM {
	vm := &searchIssueVM{Complete: sec.Complete}
	for _, h := range sec.Hits {
		vm.Hits = append(vm.Hits, searchIssueHitVM{
			Repo:       h.Repo,
			Num:        h.Num,
			Kind:       h.Kind,
			Title:      h.Title,
			State:      h.State,
			StateClass: issueStateChip(h.State),
			Author:     h.Author,
			Matched:    h.Matched,
			URL:        h.URL,
			Labels:     h.Labels,
		})
	}
	count := len(sec.Hits)
	vm.StatusLine = fmt.Sprintf("%d %s", count, pluralWord(count, "issue or pull-request match", "issue and pull-request matches"))
	scanned := fmt.Sprintf("scanned %d %s", sec.Scanned.Threads, pluralWord(sec.Scanned.Threads, "thread", "threads"))
	if sec.Scanned.Events > 0 {
		scanned += fmt.Sprintf(" and %d %s", sec.Scanned.Events, pluralWord(sec.Scanned.Events, "event", "events"))
	}
	vm.StatusLine += " · " + scanned
	if !sec.Complete {
		vm.Notice = "The issue scan hit its bound, so this list is partial; refine the query."
	}
	return vm
}

// issueStateChip maps a thread state onto the shared chip classes. The word is
// always rendered next to the colour (AGENTS.md: state is never colour-only).
func issueStateChip(state string) string {
	switch strings.ToLower(state) {
	case "open", "reopened":
		return "chip chip-open"
	case "closed":
		return "chip chip-closed"
	case "merged":
		return "chip chip-merged"
	case "draft":
		return "chip chip-draft"
	default:
		return "chip chip-neutral"
	}
}

// codeStatusLine is the role="status" count line: matches, files, the
// searched/considered accounting and every cap the service reported (§7.1).
func codeStatusLine(vm *searchCodeVM, ownerScope bool) string {
	var matches, files, searchedOK int
	var truncated, timedOut bool
	for _, r := range vm.Repos {
		matches += r.MatchCount
		files += r.FileCount
		if r.Status == string(search.StatusOK) {
			searchedOK++
		}
		truncated = truncated || r.Truncated
		timedOut = timedOut || r.TimedOut
	}
	var parts []string
	switch {
	case searchedOK == 0:
		parts = append(parts, "Nothing was searched")
	case matches == 0:
		parts = append(parts, "No matches")
	default:
		parts = append(parts, fmt.Sprintf("%d %s in %d %s",
			matches, pluralWord(matches, "match", "matches"),
			files, pluralWord(files, "file", "files")))
	}
	if ownerScope {
		parts = append(parts, fmt.Sprintf("searched %d of %d %s",
			vm.Searched, vm.Considered, pluralWord(vm.Considered, "repository", "repositories")))
	} else if len(vm.Repos) == 1 && vm.Repos[0].Status == string(search.StatusOK) {
		r := vm.Repos[0]
		at := r.Ref
		if r.Short != "" {
			at = strings.TrimSpace(at + " (" + r.Short + ")")
		}
		if at != "" {
			parts = append(parts, "searched "+at)
		}
	}
	if truncated {
		parts = append(parts, fmt.Sprintf("showing the first %d %s; refine the query",
			matches, pluralWord(matches, "match", "matches")))
	}
	if timedOut {
		parts = append(parts, "deadline fired; these results are partial")
	}
	return strings.Join(parts, " · ")
}

// searchFileHref is the blob link a result row opens: the SHA the answer is
// pinned to (never a moving branch) and the path's escaped segments, with the
// line anchor the blob page's per-line rows carry.
func searchFileHref(repo, sha, path string) string {
	href := "/" + escapePath(repo) + "/blob/" + url.PathEscape(sha)
	if path != "" {
		href += "/" + escapePath(path)
	}
	return href
}

// --- match highlighting ------------------------------------------------------

// projectMatch splits one matched line around the matched span so the template
// can wrap it in <mark>. The pieces stay ordinary strings — the XSS boundary
// is html/template's escaping, never template.HTML.
func projectMatch(m search.Match, pattern string, opts search.QueryOptions) searchMatchVM {
	vm := searchMatchVM{Line: m.Line, Column: m.Column, Text: m.Text}
	if before, mid, after, ok := locateMatchSpan(m.Text, m.Column, pattern, opts); ok {
		vm.Before, vm.Mark, vm.After, vm.HasMark = before, mid, after, true
	}
	return vm
}

// locateMatchSpan finds the span the primitive matched at. The column is a
// byte offset into the ORIGINAL line, but the text may be middle-ellipsized
// around the match (4 KiB cap), so the column is only a hint: try it, then fall
// back to the first occurrence. No span is fabricated — a line whose span
// cannot be located renders whole, unmarked.
func locateMatchSpan(text string, column int, pattern string, opts search.QueryOptions) (before, mark, after string, ok bool) {
	if text == "" || pattern == "" {
		return "", "", "", false
	}
	if opts.Regex {
		re := compileMarkRegexp(pattern, opts.CaseSensitive)
		if re == nil {
			return "", "", "", false
		}
		if column > 0 && column-1 < len(text) {
			if loc := re.FindStringIndex(text[column-1:]); loc != nil && loc[0] == 0 && loc[1] > 0 {
				start := column - 1
				return text[:start], text[start : start+loc[1]], text[start+loc[1]:], true
			}
		}
		if loc := re.FindStringIndex(text); loc != nil && loc[0] != loc[1] {
			return text[:loc[0]], text[loc[0]:loc[1]], text[loc[1]:], true
		}
		return "", "", "", false
	}
	n := len(pattern)
	if column > 0 && column-1+n <= len(text) && equalSearchSpan(text[column-1:column-1+n], pattern, opts.CaseSensitive) {
		start := column - 1
		return text[:start], text[start : start+n], text[start+n:], true
	}
	if opts.CaseSensitive {
		if i := strings.Index(text, pattern); i >= 0 {
			return text[:i], text[i : i+n], text[i+n:], true
		}
		return "", "", "", false
	}
	// ASCII case folding is what the engine itself does under LC_ALL=C (§5.3);
	// strings.EqualFold is used only to confirm the slice, so a Unicode edge
	// case simply yields no mark rather than a wrong one.
	if i := strings.Index(strings.ToLower(text), strings.ToLower(pattern)); i >= 0 && i+n <= len(text) &&
		equalSearchSpan(text[i:i+n], pattern, false) {
		return text[:i], text[i : i+n], text[i+n:], true
	}
	return "", "", "", false
}

// compileMarkRegexp builds the display-only matcher. The query was already
// validated as POSIX ERE by the service; case-insensitivity rides an inline
// flag on the (linear-time) RE2 engine. A failure yields no mark.
func compileMarkRegexp(pattern string, caseSensitive bool) *regexp.Regexp {
	expr := pattern
	if !caseSensitive {
		expr = "(?i)" + pattern
	}
	if re, err := regexp.CompilePOSIX(expr); err == nil {
		return re
	}
	if re, err := regexp.Compile(expr); err == nil {
		return re
	}
	return nil
}

// equalSearchSpan compares one candidate span under the query's case rule.
func equalSearchSpan(a, b string, caseSensitive bool) bool {
	if caseSensitive {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// pluralWord is the Go-side twin of the templates' plural helper.
func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
