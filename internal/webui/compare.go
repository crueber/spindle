package webui

// compare.go — the Compare page (`/{o}/{r}/compare/{base}...{head}`;
// repo-browsing parity M3; argv in docs/go/04_git.md §16.3).
//
// It answers three questions in one page: which commits the head has that the
// base does not, what the change summary is, and the unified diff itself. The
// diff is rendered by the EXISTING commit renderer (commitFilesFrom →
// ParseUnifiedDiff + the repo_diff partial) — there is no second differ.
//
// Cross-repository comparison is not reimplemented here. The PR compare URL
// shape (`?base=<branch>&head=<owner>/<repo>:<branch>`, the sync-fork link)
// is accepted and lands on an honest card pointing at the pull-request
// composer, which is the shipped cross-repo comparison path (the fork object
// bridge lives in internal/pulls). Same-repository comparisons render fully.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/spindle/internal/api"
)

type comparePage struct {
	Repo    *RepoChrome
	Guide   *repoGuide
	Base    string
	Head    string
	BaseSHA string
	HeadSHA string

	BaseHref string
	HeadHref string

	Ahead            int
	Behind           int
	Commits          []commitRow
	More             bool
	StatN            int
	Adds, Dels       int64
	Files            []commitFile
	Identical        bool
	NoCommonAncestor bool
	PatchTruncated   bool

	// CrossRepo is the head repository ("owner/name") when the head names
	// another repository; PRHref is the composer link that owns that compare.
	CrossRepo string
	PRHref    string
	Err       string
}

func (s *Server) repoCompare(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	spec := strings.TrimSuffix(r.PathValue("spec"), "/")
	if spec == "" {
		// The PR compare shape: /{o}/{r}/compare?base=…&head=…
		base := strings.TrimSpace(r.URL.Query().Get("base"))
		head := strings.TrimSpace(r.URL.Query().Get("head"))
		if base == "" || head == "" {
			s.errorPage(w, r, http.StatusNotFound, "compare needs two refs: /compare/<base>...<head>")
			return
		}
		s.compareRedirect(w, r, rq.full, base, head)
		return
	}
	idx := strings.Index(spec, "...")
	if idx < 0 {
		s.errorPage(w, r, http.StatusNotFound, "compare needs two refs: /compare/<base>...<head>")
		return
	}
	baseSpec := strings.Trim(strings.TrimSpace(spec[:idx]), "/")
	headSpec := strings.Trim(strings.TrimSpace(spec[idx+3:]), "/")
	if baseSpec == "" || headSpec == "" {
		s.errorPage(w, r, http.StatusNotFound, "compare needs two refs: /compare/<base>...<head>")
		return
	}
	if c.Empty {
		data := &comparePage{Repo: c, Guide: s.repoGuide(c), Base: baseSpec, Head: headSpec}
		s.page(w, r, rq.p, http.StatusOK, "repo/compare", "Compare · "+c.Full, data)
		return
	}
	headRepo, headRef, ok := splitCompareHead(headSpec, rq.full)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "compare head must be a branch in this repository or <owner>/<repo>:<branch>")
		return
	}
	data := &comparePage{Repo: c, Base: baseSpec, Head: headSpec}
	if headRepo != rq.full {
		// Another repository's head: the composer owns that comparison, and
		// its button is the page's one primary action (projectPrimary demotes
		// the masthead Clone).
		data.CrossRepo = headRepo
		data.PRHref = "/" + rq.full + "/pulls/new?base=" + url.QueryEscape(baseSpec) +
			"&head=" + url.QueryEscape(headRepo+":"+headRef)
		s.page(w, r, rq.p, http.StatusOK, "repo/compare", "Compare · "+c.Full, data)
		return
	}
	ctx := r.Context()
	baseRes, err := s.env.API.Repo.Resolve(ctx, rq.id, baseSpec)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) && !isEmptyRepoErr(err) {
			s.errorPage(w, r, http.StatusNotFound, "base ref "+baseSpec+" not found in this repository")
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	headRes, err := s.env.API.Repo.Resolve(ctx, rq.id, headRef)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) && !isEmptyRepoErr(err) {
			s.errorPage(w, r, http.StatusNotFound, "head ref "+headRef+" not found in this repository")
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	browse, ok := s.env.API.Repo.(api.RepoBrowse)
	if !ok {
		s.errorPage(w, r, http.StatusServiceUnavailable, "compare is unavailable on this build")
		return
	}
	cmp, err := browse.Compare(ctx, rq.id, baseRes.SHA, headRes.SHA, api.MaxCompareCommits)
	if err != nil {
		s.repoViewError(w, r, err)
		return
	}
	data.BaseSHA, data.HeadSHA = cmp.BaseSHA, cmp.HeadSHA
	data.BaseHref = "/" + c.Full + "/commits?ref=" + url.QueryEscape(baseSpec)
	data.HeadHref = "/" + c.Full + "/commits?ref=" + url.QueryEscape(headRef)
	data.Identical = cmp.Identical
	data.NoCommonAncestor = cmp.NoCommonAncestor
	data.PatchTruncated = cmp.PatchTruncated
	data.Ahead, data.Behind = cmp.Ahead, cmp.Behind
	data.More = cmp.More
	data.StatN = len(cmp.Stats)
	for _, st := range cmp.Stats {
		data.Adds += st.Additions
		data.Dels += st.Deletions
	}
	for _, cm := range cmp.Commits {
		data.Commits = append(data.Commits, commitRow{
			SHA:     cm.SHA,
			Short:   shortSHA(cm.SHA),
			Subject: cm.Subject,
			Author:  cm.Author,
			Stamp:   rfc3339(cm.AuthorDate),
			Href:    "/" + c.Full + "/commit/" + url.PathEscape(cm.SHA),
		})
	}
	data.Files = commitFilesFrom(cmp.Patch)
	s.page(w, r, rq.p, http.StatusOK, "repo/compare", "Compare · "+c.Full, data)
}

// compareRedirect turns the query shape into the canonical path shape.
func (s *Server) compareRedirect(w http.ResponseWriter, r *http.Request, full, base, head string) {
	target := "/" + full + "/compare/" + escapePath(base) + "..." + escapePath(head)
	s.formsRedirect(w, r, target)
}

// splitCompareHead parses the head spelling: a branch in the current repo, or
// the PR link's `<owner>/<repo>:<branch>` (a refname cannot contain ':').
// ok=false for a colon form whose repo half is not owner/repo.
func splitCompareHead(spec, current string) (repo, ref string, ok bool) {
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		repoPart, branch := spec[:i], spec[i+1:]
		if branch == "" {
			return "", "", false
		}
		if !strings.Contains(repoPart, "/") {
			return "", "", false
		}
		return repoPart, branch, true
	}
	return current, spec, true
}
