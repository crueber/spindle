// Package search answers "where is this text?" over the repositories spindle
// already holds (docs/features/20_search.md). It owns the scope enumeration,
// the query grammar, the permission-aware fan-out and the completeness
// accounting; the scan itself is the internal/git grep primitive
// (docs/go/04_git.md §17), the serving copies come from a RepoSource, and
// visibility is the identity read gate behind an Access seam.
//
// It imports neither internal/server nor internal/webui (AGENTS.md law 8):
// composition builds it over git.Layer, the WAL registry and identity, and the
// HTTP/API lanes (P3) project the types below onto the frozen wire shape
// (20_search.md §6). There is no index, no bucket key and no dependency: a
// query is one grep per repository against the serving copy that browsing,
// blame and compare already use (§2, decision 1).
package search

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/server/auth"
)

// Caps (docs/features/20_search.md §5.2). Only max_concurrent is config; every
// other number is a package constant with the comment that justifies it, and
// an operator who hits one gets a new key in a later change rather than a
// configuration surface nobody validated (decision 5).
const (
	// MaxPatternBytes bounds the query text. A longer pattern is not a search
	// request, it is a payload.
	MaxPatternBytes = 256
	// MaxPathPatterns / MaxPathBytes bound the pathspec list a caller may send.
	MaxPathPatterns = 8
	MaxPathBytes    = 256
	// MatchesPerFile is kept per file; the primitive is asked for one extra
	// record so per-file truncation is observable (§6.2).
	MatchesPerFile = 5
	// MaxMatchesPerRepo is the hard ceiling on kept matches per repository; a
	// caller's limit may lower it, never raise it.
	MaxMatchesPerRepo = 200
	// MaxOutputBytes bounds the grep stdout read per repository (2 MiB). The
	// byte cap cancels the child, so a runaway match count dies promptly (§11).
	MaxOutputBytes = 2 << 20
	// MaxTextBytes is kept per match, middle-ellipsized around the match
	// column by the primitive (§5.2).
	MaxTextBytes = 4 << 10
	// GrepTimeout is the hard per-subprocess deadline. A fired deadline is an
	// answer with timed_out set, never an error (§10).
	GrepTimeout = 10 * time.Second
	// OwnerDeadline bounds one owner-scope request: 4 workers over at most 50
	// candidates. Repositories not started by then are not_searched (§11).
	OwnerDeadline = 5 * time.Second
	// MaxOwnerCandidates bounds owner scope. The candidate source is the
	// owner's repository registry listing — never a disk walk and never an
	// instance-wide LIST — and only the first 50 names (ascending) are
	// considered, so the fan-out is 50 greps at most (§3).
	MaxOwnerCandidates = 50
	// OwnerWorkers is the fan-out width. Each worker takes a scan-pool slot
	// only around its own grep (the git layer's 2-slot ScanPool).
	OwnerWorkers = 4
	// DefaultLimit / MaxLimit are the "kept matches per repository" default and
	// ceiling; §5.1 clamps limit to 1..200.
	DefaultLimit = 100
	MaxLimit     = 200
	// DefaultMaxConcurrent is [search] max_concurrent's default; a zero value
	// means this one (§5.2).
	DefaultMaxConcurrent = 2
	// MaxConcurrentCeiling mirrors config.SearchMaxConcurrent. It is repeated
	// here because this package must not import internal/config (law 8); the
	// validator and this clamp name each other so a drift is deliberate.
	MaxConcurrentCeiling = 16
)

// Type is the requested result family (§5.1).
type Type string

const (
	TypeCode   Type = "code"
	TypeIssues Type = "issues"
	TypeAll    Type = "all"
)

// Kind is a search scope. Instance-wide code search is deliberately absent in
// v1: without an owner the cost is every repository on the instance and cannot
// be bounded honestly (§1, decision 6). ScopeInstance exists so the refusal is
// a typed answer rather than a silently different query.
type Kind string

const (
	ScopeRepo     Kind = "repo"
	ScopeOwner    Kind = "owner"
	ScopeInstance Kind = "instance"
)

// Scope is the frozen wire scope (§6.2): {"kind":"repo","owner":…,"repo":…} or
// {"kind":"owner","owner":…}.
type Scope struct {
	Kind  Kind   `json:"kind"`
	Owner string `json:"owner"`
	Repo  string `json:"repo,omitempty"`
}

// ID renders the "owner/name" repository id a repo scope names (the same id
// every seam and result uses).
func (s Scope) ID() string {
	if s.Repo == "" {
		return s.Owner
	}
	return s.Owner + "/" + s.Repo
}

// Status is one repository result's state (§6.4). The vocabulary is frozen:
// ok — searched; not_warm — owner scope only, no current local pack set;
// not_searched — the request deadline stopped the fan-out; failed — this
// repository's own work failed (a per-repo answer, never a whole-request 500).
type Status string

const (
	StatusOK          Status = "ok"
	StatusNotWarm     Status = "not_warm"
	StatusNotSearched Status = "not_searched"
	StatusFailed      Status = "failed"
)

// Reason strings for non-ok results. They are short and neutral: never a path,
// never another principal's repository, never a count of denied repositories
// (§6.4, §8 leak rules).
const (
	reasonNotCached   = "not cached on this instance"
	reasonEmptyRepo   = "empty repository"
	reasonDeadline    = "request deadline reached"
	reasonUnavailable = "repository unavailable"
	reasonSearch      = "search failed"
)

// QueryOptions echoes the normalised query so a client can render it (§6.2).
// Paths is never nil: the wire says [] not null.
type QueryOptions struct {
	Regex         bool     `json:"regex"`
	CaseSensitive bool     `json:"case_sensitive"`
	Word          bool     `json:"word"`
	Paths         []string `json:"paths"`
	Limit         int      `json:"limit"`
}

// Match is one matching line (§6.2). Text is the raw line (capped and
// middle-ellipsized by the primitive), rendered by the server-side HTML
// escaping layer — never trusted as markup.
type Match struct {
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

// File groups one path's matches. Matches is never nil.
type File struct {
	Path    string  `json:"path"`
	Matches []Match `json:"matches"`
}

// Result is one repository's answer (§6.2 code.results[]). Files is never nil;
// Complete folds every cap, the deadline and any degradation into one flag
// (§6.4).
type Result struct {
	Repo       string `json:"repo"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	Status     Status `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Complete   bool   `json:"complete"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out"`
	MatchCount int    `json:"match_count"`
	FileCount  int    `json:"file_count"`
	Files      []File `json:"files"`
}

// CodeSection is the "code" section (§6.2). Complete means every considered
// repository answered fully.
type CodeSection struct {
	Complete        bool     `json:"complete"`
	ReposConsidered int      `json:"repos_considered"`
	ReposSearched   int      `json:"repos_searched"`
	Results         []Result `json:"results"`
}

// Answer is one search answer as the service produced it. P3 marshals it
// nearly verbatim: unrequested sections are nil (omitted, never null) and
// every array is non-nil.
type Answer struct {
	Query   string        `json:"query"`
	Type    Type          `json:"type"`
	Options QueryOptions  `json:"options"`
	Scope   Scope         `json:"scope"`
	Code    *CodeSection  `json:"code,omitempty"`
	Issues  *IssueSection `json:"issues,omitempty"`
}

// ---- the frozen P4 seam (docs/features/20_search.md §12 decision 12) --------

// IssueSearcher is the P4 seam (docs/features/20_search.md §12 decision 12).
// The concrete implementation is issue.go over the additive
// issues.Service.SearchProjection read. The caller has already validated the
// query and resolved the scope; the implementation enforces moderation
// redaction BEFORE matching.
type IssueSearcher interface {
	SearchIssues(ctx context.Context, scope IssueScope, q IssueQuery) (IssueSection, error)
}

// IssueScope names the threads to search: an owner is required. P4's service
// enumerates the owner's visible repositories through the same candidate/read
// gate as code search and calls SearchIssues once per repository, so Repo is
// always set on the composed path. Repo=="" remains the frozen seam's
// owner-wide spelling; the concrete searcher refuses it rather than searching
// an owner with no visibility check (§3, §8).
type IssueScope struct {
	Owner string
	Repo  string
}

// IssueQuery is the validated issue-search request.
type IssueQuery struct {
	Text      string // validated: non-empty, ≤ 256 bytes, no NUL/CR/LF
	CaseExact bool   // false = case-insensitive (ASCII, like the code section)
	// Word and Regex are the additive P4 spelling of the §5.1 grammar knobs:
	// a type=all request must search the same text the same way in both
	// sections. They default to the code section's defaults (fixed string,
	// substring).
	Word  bool
	Regex bool
	Body  bool // true = include body/comment text (bounded scan)
	Limit int  // ≤ 200
}

// IssueSection is the "issues" section (§6.3). Hits is never nil.
//
// Reason and Repos are additive P4 fields (20_search.md §12 decision 14): a
// capped scan says which cap hid work, and owner scope reports each visible
// repository's status exactly like the code section's results. Both are
// omitted when not applicable, so a complete single-repository answer is the
// frozen §6.3 shape.
type IssueSection struct {
	Complete bool        `json:"complete"`
	Reason   IssueReason `json:"reason,omitempty"`
	Scanned  IssueScan   `json:"scanned"`
	Hits     []IssueHit  `json:"hits"`
	Repos    []IssueRepo `json:"repos,omitempty"`
}

// IssueReason is the typed cause of IssueSection.Complete == false.
type IssueReason string

const (
	IssueReasonThreads    IssueReason = "thread scan cap reached"
	IssueReasonEvents     IssueReason = "event scan cap reached"
	IssueReasonLimit      IssueReason = "result limit reached"
	IssueReasonIndex      IssueReason = "issue index incomplete"
	IssueReasonDeadline   IssueReason = "request deadline reached"
	IssueReasonFailed     IssueReason = "repository search failed"
	IssueReasonCandidates IssueReason = "repository set capped"
	IssueReasonIncomplete IssueReason = "issue search incomplete"
)

// IssueRepo is one owner-scope repository's issue-search answer, the issue
// twin of the code section's Result.
type IssueRepo struct {
	Repo     string      `json:"repo"`
	Status   Status      `json:"status"`
	Reason   IssueReason `json:"reason,omitempty"`
	Complete bool        `json:"complete"`
	Hits     int         `json:"hits"`
}

// IssueScan reports how much was scanned (§6.3).
type IssueScan struct {
	Threads       int  `json:"threads"`
	Events        int  `json:"events"`
	IndexComplete bool `json:"index_complete"`
}

// IssueHit is one issue or pull request hit. Labels is never nil; URL is
// derived server-side from Kind.
type IssueHit struct {
	Repo      string   `json:"repo"`
	Num       int      `json:"num"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	UpdatedAt string   `json:"updated_at"`
	Matched   string   `json:"matched"`
	// Excerpt is the clipped, escaped-at-the-transport body/comment window
	// around the match. It is set only when Matched is body or comment and is
	// never derived from a moderation-hidden event (20_search.md §8).
	Excerpt string `json:"excerpt,omitempty"`
	URL     string `json:"url"`
}

// ---- classified failures ----------------------------------------------------

// Sentinels. Callers test with errors.Is; each typed Error below carries the
// exact plain-text body a transport may send (§10). The distinction between
// ErrUnauthorized and ErrForbidden is normative: the git-facing convention is a
// real 401 (so a client erases its credential) and the API never answers 404
// where its convention is 401/403.
var (
	ErrNotFound          = errors.New("not found")
	ErrForbidden         = errors.New("read access required")
	ErrUnauthorized      = errors.New("authentication required")
	ErrUnavailable       = errors.New("search is temporarily unavailable")
	ErrInvalidQuery      = errors.New("invalid search query")
	ErrUnsupportedScope  = errors.New("instance-wide code search is not available; choose an owner")
	ErrWarming           = errors.New("search is warming this repository; retry in a few seconds")
	ErrNotLocal          = errors.New("code search needs a local serving copy; this repository's pack set exceeds the cache budget")
	ErrDisabled          = errors.New("search is disabled on this instance")
	ErrIssuesUnavailable = errors.New("issue search is not available on this instance")
)

// Error is one classified failure: Kind is one of the sentinels above (match
// it with errors.Is) and Why is the plain-text body. Composition uses it to
// map backend errors onto the service's vocabulary without duplicating copy.
type Error struct {
	Kind error
	Why  string
}

func (e *Error) Error() string { return e.Why }

// Unwrap exposes the sentinel for errors.Is.
func (e *Error) Unwrap() error { return e.Kind }

// Wrap classifies err's kind with an explicit wire body.
func Wrap(kind error, why string) error { return &Error{Kind: kind, Why: why} }

// InvalidError is a 400-class query failure. Field names the request
// parameter; Why is the plain-text rule the caller violated (§10).
type InvalidError struct {
	Field string
	Why   string
}

func (e *InvalidError) Error() string { return e.Why }

// Unwrap exposes ErrInvalidQuery for errors.Is.
func (e *InvalidError) Unwrap() error { return ErrInvalidQuery }

// ---- seams ------------------------------------------------------------------

// Git is the grep primitive seam; *git.Layer satisfies it (04_git.md §17). The
// service never touches the control-plane git pool: a grep takes the layer's
// derived ScanPool, and only while the subprocess runs (§11, decision 10).
type Git interface {
	Grep(ctx context.Context, repo *git.LocalRepo, rev string, q git.GrepQuery) (*git.GrepResult, error)
}

// Target is one repository ready to grep: the serving copy, the ref that was
// searched (the default branch when the request named none) and the resolved
// commit the answer is pinned to.
type Target struct {
	Repo *git.LocalRepo
	Ref  string
	SHA  string
}

// RepoSource is the repository seam. Composition implements it over the WAL
// registry, the repository registry and stock git; tests implement it directly.
//
// Error contract: an unknown repository or ref (and an unborn repository) is
// ErrNotFound with the wire body ("unknown revision …" / "empty repository:
// …"); a cold repository is ErrWarming; a pack set over the cache budget is
// ErrNotLocal (§9); anything else is ErrUnavailable. Implementations never
// return raw store or subprocess errors.
type RepoSource interface {
	// ListOwner returns the registry's short repository names under owner.
	// This is the only candidate source for owner scope (§3) and it must be a
	// bounded registry listing, never a LIST per candidate.
	ListOwner(ctx context.Context, owner string) ([]string, error)

	// ResolveRepo is repository scope: resolve ref ("" = the default branch)
	// to a commit and materialize the serving copy to serve level (§9). It is
	// the one path that MAY download packs — the user asked for this
	// repository.
	ResolveRepo(ctx context.Context, repo, ref string) (Target, error)

	// WarmRefs is owner scope: sync refs only (cheap), resolve the default
	// branch, and report ready=false when the local pack set is not current
	// (PacksReady, §3.1). It must never materialize a pack set: a 50-repo
	// query must not start 50 downloads.
	WarmRefs(ctx context.Context, repo string) (target Target, ready bool, err error)
}

// Access is the visibility seam: the identity read gate. *identity.Service
// satisfies it directly (CheckRead returns *auth.AuthError, nil = allowed), so
// a typed nil can never read as a successful check.
type Access interface {
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// Options wires the service. Zero values are safe: Disabled false is the
// compiled default (search is part of the ordinary read surface), a zero
// MaxConcurrent selects DefaultMaxConcurrent, and a nil seam answers
// ErrUnavailable rather than searching with no gate (fail closed, law 9).
type Options struct {
	Git      Git
	Repos    RepoSource
	Access   Access
	Issues   IssueSearcher // P4; nil = type issues/all answers ErrIssuesUnavailable
	Disabled bool
	// MaxConcurrent bounds concurrent searches per instance (§5.2, §11).
	MaxConcurrent int
}

// Service is the search surface. It is safe for concurrent use.
type Service struct {
	git      Git
	repos    RepoSource
	access   Access
	issues   IssueSearcher
	disabled bool

	// sem is the request pool (max_concurrent slots). It bounds CPU a caller
	// can consume, not a repository's concurrency (that is gates + the git
	// scan pool).
	sem chan struct{}

	// gates serializes greps of one repository (1 slot keyed by repo id, §11):
	// two searches of one repository queue instead of competing for page
	// cache. A gate is held only around the subprocess.
	gates repoGates

	// ownerDeadline is OwnerDeadline; a field only so tests can drive the
	// deadline without waiting five seconds.
	ownerDeadline time.Duration
}

// New builds the service. It never fails: a missing seam is a per-request
// ErrUnavailable, not a boot error (the routes simply answer 503 until
// composition wires them).
func New(opts Options) *Service {
	n := opts.MaxConcurrent
	if n <= 0 {
		n = DefaultMaxConcurrent
	}
	if n > MaxConcurrentCeiling {
		n = MaxConcurrentCeiling
	}
	return &Service{
		git:           opts.Git,
		repos:         opts.Repos,
		access:        opts.Access,
		issues:        opts.Issues,
		disabled:      opts.Disabled,
		sem:           make(chan struct{}, n),
		ownerDeadline: OwnerDeadline,
	}
}

// Search parses params for scope, applies the gates in the normative order and
// runs the requested sections. The caller has authenticated already (Seam 2);
// p is the resolved principal.
//
// Repository scope follows §8 exactly: the read gate runs BEFORE query
// validation and before any existence, ref or query work, mirroring every
// AuthRead route. Owner scope validates first (a bad query never pays a LIST),
// then lists and gates candidates. A denied repository contributes no name, no
// count, no total and no error text (§8 leak rules).
func (s *Service) Search(ctx context.Context, scope Scope, params Params, p auth.Principal) (*Answer, error) {
	if s == nil {
		return nil, Wrap(ErrUnavailable, ErrUnavailable.Error())
	}
	if s.disabled {
		return nil, Wrap(ErrDisabled, ErrDisabled.Error())
	}
	switch scope.Kind {
	case ScopeRepo:
		return s.searchRepo(ctx, scope, params, p)
	case ScopeOwner:
		return s.searchOwner(ctx, scope, params, p)
	case ScopeInstance:
		return nil, Wrap(ErrUnsupportedScope, ErrUnsupportedScope.Error())
	default:
		return nil, Wrap(ErrUnsupportedScope, "unknown search scope; search a repository or an owner")
	}
}

// searchRepo is single-repository scope (§8 steps 3–8).
func (s *Service) searchRepo(ctx context.Context, scope Scope, params Params, p auth.Principal) (*Answer, error) {
	if scope.Owner == "" || scope.Repo == "" {
		return nil, &InvalidError{Field: "scope", Why: "a repository search needs an owner and a repository"}
	}
	if err := s.checkRead(ctx, scope.Owner, scope.Repo, p); err != nil {
		return nil, err
	}
	q, err := Parse(scope, params)
	if err != nil {
		return nil, err
	}
	ans := &Answer{Query: q.Text, Type: q.Type, Options: q.options(), Scope: q.Scope}
	if q.wantsIssues() {
		sec, err := s.issueSection(ctx, q, p)
		if err != nil {
			return nil, err
		}
		ans.Issues = sec
	}
	if q.wantsCode() {
		if err := s.requireCodeSeams(); err != nil {
			return nil, err
		}
		release, err := s.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		sec, err := s.codeRepo(ctx, q)
		if err != nil {
			return nil, err
		}
		ans.Code = sec
	}
	return ans, nil
}

// searchOwner is bounded owner scope (§8 owner steps 2–4).
func (s *Service) searchOwner(ctx context.Context, scope Scope, params Params, p auth.Principal) (*Answer, error) {
	q, err := Parse(scope, params)
	if err != nil {
		return nil, err
	}
	ans := &Answer{Query: q.Text, Type: q.Type, Options: q.options(), Scope: q.Scope}
	if q.wantsIssues() {
		sec, err := s.issueSection(ctx, q, p)
		if err != nil {
			return nil, err
		}
		ans.Issues = sec
	}
	if q.wantsCode() {
		if err := s.requireCodeSeams(); err != nil {
			return nil, err
		}
		release, err := s.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		sec, err := s.codeOwner(ctx, q, p)
		if err != nil {
			return nil, err
		}
		ans.Code = sec
	}
	return ans, nil
}

// codeRepo greps one repository at its resolved commit.
func (s *Service) codeRepo(ctx context.Context, q Query) (*CodeSection, error) {
	id := q.Scope.ID()
	t, err := s.repos.ResolveRepo(ctx, id, q.Ref)
	if err != nil {
		return nil, err
	}
	release, err := s.gates.acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	res, gerr := s.git.Grep(ctx, t.Repo, t.SHA, q.grepQuery())
	release()
	if gerr != nil {
		return nil, s.repoGrepError(ctx, gerr)
	}
	r := project(t, res)
	r.Repo = id
	return &CodeSection{
		Complete:        r.Complete,
		ReposConsidered: 1,
		ReposSearched:   1,
		Results:         []Result{r},
	}, nil
}

// codeOwner enumerates, gates and fans out over an owner's warm repositories.
func (s *Service) codeOwner(ctx context.Context, q Query, p auth.Principal) (*CodeSection, error) {
	dctx, cancel := context.WithTimeout(ctx, s.deadline())
	defer cancel()

	candidates, capped, err := s.visibleCandidates(dctx, q.Scope.Owner, p)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if dctx.Err() != nil {
			// The request deadline expired inside the listing: a fired
			// deadline is an answer (200 + complete:false, §10), not a 503 —
			// nothing was searched, and the caller is told the set is
			// incomplete instead of being asked to retry an outage.
			return &CodeSection{Complete: false, Results: []Result{}}, nil
		}
		if errors.Is(err, ErrUnavailable) {
			return nil, err // the gate/listing already named the outage
		}
		return nil, Wrap(ErrUnavailable, ErrUnavailable.Error())
	}

	results, patternErr := s.fanOut(dctx, candidates, q)
	if ctx.Err() != nil {
		return nil, ctx.Err() // caller cancellation is an error (§11)
	}
	if patternErr != nil {
		return nil, patternErr
	}

	sec := &CodeSection{Complete: !capped, ReposConsidered: len(results), Results: results}
	for _, r := range results {
		if r.Status == StatusOK {
			sec.ReposSearched++
		}
		if r.Status != StatusOK || !r.Complete {
			sec.Complete = false
		}
	}
	return sec, nil
}

// visibleCandidates is the one owner-scope candidate path (§3, §8): the
// registry listing, the 50-name bound, then a CheckRead per candidate. A denied
// repository contributes no name, no count and no error text. Code search and
// issue search both call it, so neither can grow a second visibility rule.
//
// The caller supplies a deadline context; when it fires during the gating loop
// the returned set is capped and the caller reports complete:false rather than
// treating the deadline as an outage.
func (s *Service) visibleCandidates(ctx context.Context, owner string, p auth.Principal) ([]string, bool, error) {
	if s.repos == nil || s.access == nil {
		return nil, false, Wrap(ErrUnavailable, ErrUnavailable.Error())
	}
	names, err := s.repos.ListOwner(ctx, owner)
	if err != nil {
		return nil, false, err
	}
	names = append([]string(nil), names...)
	sort.Strings(names)
	capped := false
	if len(names) > MaxOwnerCandidates {
		names = names[:MaxOwnerCandidates]
		capped = true
	}
	candidates := make([]string, 0, len(names))
	for _, name := range names {
		if ctx.Err() != nil {
			capped = true
			break
		}
		err := s.checkRead(ctx, owner, name, p)
		if errors.Is(err, ErrUnavailable) {
			// An identity failure cannot vary inside one principal: it is a
			// whole-request answer, never a per-repository status (§8).
			return nil, capped, err
		}
		if err != nil {
			continue // denied: no name, no count, no reason
		}
		candidates = append(candidates, owner+"/"+name)
	}
	return candidates, capped, nil
}

// fanOut runs the 4-worker owner fan-out. Every candidate starts as
// not_searched with the deadline reason; a worker overwrites the entry it
// actually answered. Results keep candidate order (§3.2) and the wire is
// never a silently short list: unstarted candidates keep their status.
func (s *Service) fanOut(ctx context.Context, candidates []string, q Query) ([]Result, error) {
	results := make([]Result, len(candidates))
	for i, id := range candidates {
		results[i] = notSearched(Result{Repo: id}, reasonDeadline)
	}
	if len(candidates) == 0 {
		return results, nil
	}

	var (
		mu         sync.Mutex
		patternErr error
	)
	workers := OwnerWorkers
	if len(candidates) < workers {
		workers = len(candidates)
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if ctx.Err() != nil {
					continue // leave not_searched
				}
				r, err := s.searchCandidate(ctx, candidates[i], q)
				if err != nil {
					mu.Lock()
					if patternErr == nil {
						patternErr = err
					}
					mu.Unlock()
					continue
				}
				results[i] = r
			}
		}()
	}
feed:
	for i := range candidates {
		select {
		case work <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return results, patternErr
}

// searchCandidate answers one owner-scope candidate. A non-nil error is only a
// whole-request failure (a pattern git rejected at run time); every per-repo
// failure becomes a status with a neutral reason instead.
func (s *Service) searchCandidate(ctx context.Context, id string, q Query) (Result, error) {
	r := Result{Repo: id}
	t, ready, err := s.repos.WarmRefs(ctx, id)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			// The request deadline stopped this candidate before any scan:
			// it was not searched (§6.4).
			return notSearched(r, reasonDeadline), nil
		}
		return failed(r, reasonUnavailable), nil
	case !ready:
		r.Ref = t.Ref
		return notWarm(r, reasonNotCached), nil
	case t.SHA == "":
		r.Ref = t.Ref
		return notWarm(r, reasonEmptyRepo), nil
	}

	release, err := s.gates.acquire(ctx, id)
	if err != nil {
		return notSearched(r, reasonDeadline), nil
	}
	res, gerr := s.git.Grep(ctx, t.Repo, t.SHA, q.grepQuery())
	release()
	switch {
	case gerr == nil:
		out := project(t, res)
		out.Repo = id
		return out, nil
	case ctx.Err() != nil:
		// Our request deadline (the caller's cancellation is returned as an
		// error by codeOwner): the partial scan is still an answer, and the
		// wire says so instead of showing a short list (§10 "request deadline
		// fired").
		out := project(t, res)
		out.Repo = id
		out.TimedOut = true
		out.Complete = false
		return out, nil
	case errors.Is(gerr, git.ErrGrepPattern):
		return Result{}, patternInvalidError(gerr)
	default:
		return failed(r, reasonSearch), nil
	}
}

// ---- helpers ----------------------------------------------------------------

func (s *Service) deadline() time.Duration {
	if s.ownerDeadline > 0 {
		return s.ownerDeadline
	}
	return OwnerDeadline
}

// acquire takes a request-pool slot, honoring ctx (a caller disconnecting
// while queued never proceeds to a subprocess).
func (s *Service) acquire(ctx context.Context) (func(), error) {
	// A caller that already disconnected never starts work: the explicit
	// check makes that deterministic (a select with a ready slot AND a ready
	// Done would pick at random).
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) requireCodeSeams() error {
	if s.git == nil || s.repos == nil {
		return Wrap(ErrUnavailable, ErrUnavailable.Error())
	}
	return nil
}

// checkRead maps the identity gate onto the service vocabulary. A nil gate is
// fail-closed: an unwired search route must never answer "allowed" (law 9).
func (s *Service) checkRead(ctx context.Context, owner, repo string, p auth.Principal) error {
	if s.access == nil {
		return Wrap(ErrUnavailable, "access check unavailable")
	}
	ae := s.access.CheckRead(ctx, owner, repo, p)
	if ae == nil {
		return nil
	}
	switch ae.Kind {
	case auth.ErrUnauthorized, auth.ErrInvalid:
		return Wrap(ErrUnauthorized, ErrUnauthorized.Error())
	case auth.ErrForbidden:
		return Wrap(ErrForbidden, ErrForbidden.Error())
	default:
		return Wrap(ErrUnavailable, "access check unavailable")
	}
}

// repoGrepError classifies a single-repository grep failure (§10): a bad
// pattern is a 400, a caller cancellation is the context error, anything else
// is a 503-class answer. Never a 500, never an empty success.
func (s *Service) repoGrepError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, git.ErrGrepPattern):
		return patternInvalidError(err)
	}
	return Wrap(ErrUnavailable, ErrUnavailable.Error())
}

// patternInvalidError renders git's run-time pattern rejection as the 400 the
// wire names (§10). The pre-flight CompilePOSIX check catches nearly every
// case; this is the dialect corner it missed.
func patternInvalidError(err error) error {
	why := "invalid regular expression"
	var pe *git.GrepPatternError
	if errors.As(err, &pe) && pe.Detail != "" {
		why = "invalid regular expression: " + pe.Detail
	}
	return &InvalidError{Field: "q", Why: why}
}

// issueSection runs the requested issue/PR section. Single-repository scope
// uses the read gate the service already applied before parsing; owner scope
// enumerates the same visible candidate set as code search (visibleCandidates)
// and calls the P4 searcher once per repository, so issue search has no second
// visibility rule and a denied repository is absent from hits and counts.
func (s *Service) issueSection(ctx context.Context, q Query, p auth.Principal) (*IssueSection, error) {
	if s.issues == nil {
		return nil, Wrap(ErrIssuesUnavailable, ErrIssuesUnavailable.Error())
	}
	iq := IssueQuery{
		Text:      q.Text,
		CaseExact: q.CaseSensitive,
		Word:      q.Word,
		Regex:     q.Regex,
		// v1 ships body/comment text (decision 4): the bounded scan is the
		// searcher's, and it redacts through moderationView before matching.
		Body:  true,
		Limit: q.Limit,
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	if q.Scope.Kind == ScopeRepo {
		sec, err := s.issues.SearchIssues(ctx, IssueScope{Owner: q.Scope.Owner, Repo: q.Scope.Repo}, iq)
		if err != nil {
			return nil, err
		}
		normalizeIssueSection(&sec)
		return &sec, nil
	}
	return s.issueOwner(ctx, q, iq, p)
}

// issueOwner is the bounded owner-scope issue fan-out. It shares the candidate
// source and read gate with code search; each visible repository is one
// searcher call with its own status, so one broken repository never hides the
// rest (the code section's decision 7).
func (s *Service) issueOwner(ctx context.Context, q Query, iq IssueQuery, p auth.Principal) (*IssueSection, error) {
	dctx, cancel := context.WithTimeout(ctx, s.deadline())
	defer cancel()

	candidates, capped, err := s.visibleCandidates(dctx, q.Scope.Owner, p)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if dctx.Err() != nil {
			// The deadline fired inside the listing: nothing was searched, and
			// the honest answer is an incomplete section, not an outage.
			return &IssueSection{
				Complete: false,
				Reason:   IssueReasonDeadline,
				Hits:     []IssueHit{},
				Repos:    []IssueRepo{},
			}, nil
		}
		if errors.Is(err, ErrUnavailable) {
			return nil, err // the gate/listing already named the outage
		}
		return nil, Wrap(ErrUnavailable, ErrUnavailable.Error())
	}

	sections := make([]*IssueSection, len(candidates))
	repos := make([]IssueRepo, len(candidates))
	for i, id := range candidates {
		repos[i] = IssueRepo{Repo: id, Status: StatusNotSearched, Reason: IssueReasonDeadline}
	}

	workers := OwnerWorkers
	if len(candidates) < workers {
		workers = len(candidates)
	}
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if dctx.Err() != nil {
					continue // leave not_searched
				}
				id := candidates[i]
				sec, serr := s.issues.SearchIssues(dctx, IssueScope{
					Owner: q.Scope.Owner,
					Repo:  strings.TrimPrefix(id, q.Scope.Owner+"/"),
				}, iq)
				if serr != nil {
					if dctx.Err() != nil {
						repos[i] = IssueRepo{Repo: id, Status: StatusNotSearched, Reason: IssueReasonDeadline}
					} else {
						repos[i] = IssueRepo{Repo: id, Status: StatusFailed, Reason: IssueReasonFailed}
					}
					continue
				}
				normalizeIssueSection(&sec)
				sections[i] = &sec
				repos[i] = IssueRepo{Repo: id, Status: StatusOK, Complete: sec.Complete, Reason: sec.Reason, Hits: len(sec.Hits)}
			}
		}()
	}
feed:
	for i := range candidates {
		select {
		case work <- i:
		case <-dctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	sec := &IssueSection{Complete: !capped, Hits: []IssueHit{}, Repos: repos}
	sec.Scanned.IndexComplete = true
	var reasons []IssueReason
	for i, r := range repos {
		if r.Status != StatusOK || sections[i] == nil {
			sec.Complete = false
			sec.Scanned.IndexComplete = false
			if r.Reason != "" {
				reasons = append(reasons, r.Reason)
			}
			continue
		}
		part := sections[i]
		sec.Scanned.Threads += part.Scanned.Threads
		sec.Scanned.Events += part.Scanned.Events
		if !part.Scanned.IndexComplete {
			sec.Scanned.IndexComplete = false
		}
		sec.Hits = append(sec.Hits, part.Hits...)
		if !part.Complete {
			sec.Complete = false
			if part.Reason != "" {
				reasons = append(reasons, part.Reason)
			}
		}
	}
	if !sec.Complete {
		sec.Reason = issueOwnerReason(capped, reasons)
	}
	return sec, nil
}

// issueOwnerReason picks the section-level typed reason deterministically:
// an unstarted/unread repository outranks a per-repository cap, then the caps
// in the order the scan meets them.
func issueOwnerReason(capped bool, reasons []IssueReason) IssueReason {
	rank := func(r IssueReason) int {
		switch r {
		case IssueReasonDeadline:
			return 0
		case IssueReasonFailed:
			return 1
		case IssueReasonCandidates:
			return 2
		case IssueReasonEvents:
			return 3
		case IssueReasonThreads:
			return 4
		case IssueReasonLimit:
			return 5
		case IssueReasonIndex:
			return 6
		}
		return 7
	}
	if capped {
		reasons = append(reasons, IssueReasonCandidates)
	}
	if len(reasons) == 0 {
		return IssueReasonIncomplete
	}
	best := reasons[0]
	for _, r := range reasons[1:] {
		if rank(r) < rank(best) {
			best = r
		}
	}
	return best
}

// normalizeIssueSection enforces the never-nil contract on a searcher answer
// ([] not null, §12.12) regardless of the implementation behind the seam.
func normalizeIssueSection(sec *IssueSection) {
	if sec.Hits == nil {
		sec.Hits = []IssueHit{}
	}
	for i := range sec.Hits {
		if sec.Hits[i].Labels == nil {
			sec.Hits[i].Labels = []string{}
		}
	}
	if sec.Repos == nil {
		sec.Repos = []IssueRepo{}
	}
}

// project converts one primitive answer into the wire result and folds every
// cap into the repository-level flags (§6.4, decision 8).
func project(t Target, res *git.GrepResult) Result {
	r := Result{
		Ref:      t.Ref,
		SHA:      t.SHA,
		Status:   StatusOK,
		Complete: true,
		Files:    []File{},
	}
	if res == nil {
		r.Complete = false
		return r
	}
	r.Truncated = res.Truncated
	r.TimedOut = res.TimedOut
	r.Files = make([]File, 0, len(res.Files))
	for _, f := range res.Files {
		out := File{Path: f.Path, Matches: make([]Match, 0, len(f.Matches))}
		if f.Truncated {
			// Per-file truncation folds into the repository flag (decision 8):
			// the wire needs one "some files have more matches" signal.
			r.Truncated = true
		}
		for _, m := range f.Matches {
			out.Matches = append(out.Matches, Match{Line: m.Line, Column: m.Column, Text: m.Text})
		}
		r.Files = append(r.Files, out)
	}
	r.MatchCount = res.Matches
	r.FileCount = res.FileCount
	r.Complete = !r.Truncated && !r.TimedOut
	return r
}

func notWarm(r Result, reason string) Result {
	r.Status = StatusNotWarm
	r.Reason = reason
	r.Complete = false
	r.Files = []File{}
	return r
}

func notSearched(r Result, reason string) Result {
	r.Status = StatusNotSearched
	r.Reason = reason
	r.Complete = false
	r.Files = []File{}
	return r
}

func failed(r Result, reason string) Result {
	r.Status = StatusFailed
	r.Reason = reason
	r.Complete = false
	r.Files = []File{}
	return r
}

// ---- the per-repository gate -------------------------------------------------

// repoGates is the 1-slot gate per repository id (§11). It is a queue, not a
// correctness lock: nothing under it is shared state, only the page cache the
// grep walks. Entries are reference counted and dropped when idle, so a long
// tail of repositories cannot grow the map.
type repoGates struct {
	mu    sync.Mutex
	gates map[string]*repoGate
}

type repoGate struct {
	ch   chan struct{}
	refs int
}

// acquire blocks until the repository's slot is free or ctx is done. The
// release func is idempotent-by-construction (call it exactly once).
func (g *repoGates) acquire(ctx context.Context, key string) (func(), error) {
	g.mu.Lock()
	if g.gates == nil {
		g.gates = make(map[string]*repoGate)
	}
	e := g.gates[key]
	if e == nil {
		e = &repoGate{ch: make(chan struct{}, 1)}
		g.gates[key] = e
	}
	e.refs++
	g.mu.Unlock()

	// A disconnected caller never takes the slot (see Service.acquire: a
	// ready slot and a ready Done would otherwise be a coin flip).
	if err := ctx.Err(); err != nil {
		g.drop(key, e)
		return nil, err
	}
	select {
	case e.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-e.ch
				g.drop(key, e)
			})
		}, nil
	case <-ctx.Done():
		g.drop(key, e)
		return nil, ctx.Err()
	}
}

func (g *repoGates) drop(key string, e *repoGate) {
	g.mu.Lock()
	e.refs--
	if e.refs == 0 {
		delete(g.gates, key)
	}
	g.mu.Unlock()
}
