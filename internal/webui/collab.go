// WIRING — collaboration read pages (htmx migration, group "collab").
//
// This file owns the server-rendered collaboration pages:
//
//	GET /{owner}/{repo}/issues                  collab/issues.html
//	GET /{owner}/{repo}/issues/{num}            collab/issue.html
//	GET /{owner}/{repo}/pulls                   collab/pulls.html
//	GET /{owner}/{repo}/pull/{num}              collab/pull.html
//	GET /{owner}/{repo}/pull/{num}/commits      collab/pull_commits.html
//	GET /{owner}/{repo}/pull/{num}/files        collab/pull_files.html
//	GET /{owner}/{repo}/checks                  collab/checks.html
//	GET /{owner}/{repo}/checks/{sha}            collab/check_detail.html
//	GET /{owner}/{repo}/releases                collab/releases.html
//	GET /{owner}/{repo}/releases/{tag}          collab/release.html
//	GET /{owner}/{repo}/forks                   collab/forks.html
//
// plus the write endpoints that live ON those pages (comment, close/reopen,
// labels, milestone, reactions, review submit/dismiss, reviewers, threads,
// merge, update-branch, release save/publish/delete/asset upload+delete) and
// two htmx fragments (GET .../checks/{sha}/contexts, GET .../pull/{num}/threads/{tid},
// GET .../pull/{num}/merge/task). Page URLs stay GET/HEAD only; writes post
// to sibling paths (spec §1.5).
//
// # WIRING (composition must call BindCollab)
//
// Feature services are not on api.Env (spec §6 F1-F3), so this group declares
// the exact service-method seams it needs in CollabDeps and exposes
// BindCollab. Every function field is bound verbatim to the feature service
// method named in the comment; the services live in
// internal/{identity,issues,pulls,review,checks,releases}:
//
//	RoleOf         identity.(*Service).Resolve      (discard the *AccessDoc)
//	Issues*        (*issues.Service)  ListIssues/GetThread/AddComment/PatchIssue/
//	                                  AddReaction/RemoveReaction
//	Labels         issues.ListLabels (exported for pages)
//	                  (internal/issues/store.go:516). Needs an exported
//	                  ListLabels(ctx, owner, repo) ([]Label, error).
//	Milestones     — GAP: listMilestones is unexported
//	                  (internal/issues/store.go:639). Needs an exported
//	                  ListMilestones(ctx, owner, repo) ([]*Milestone, error).
//	Pulls*         (*pulls.Service)   ListPRs/GetPR/AddComment/UpdatePR/Commits/
//	                                  Diff/ListForks/StartMerge/MergeTask/
//	                                  UpdateBranch
//	Reviews*       (*review.Service)  ListReviews/SubmitReview/DismissReview
//	Threads*/Thread*/Requests*        ListThreads/GetThread/OpenThread/
//	                                  AddThreadComment/Resolve/Unresolve/
//	                                  GetRequests/AddRequests/RemoveRequests
//	Checks*        (*checks.Service)  ListChecks/Combined/GetStatuses
//	Releases*      (*releases.Service) GetRelease/LatestRelease/PutRelease/
//	                                  DeleteRelease/UploadAsset/DeleteAsset
//	ReleasesList   — GAP: ListReleases returns the unexported releaseWithVer
//	                  (internal/releases/service.go:183,191), so it cannot be
//	                  named outside the package. Needs to return
//	                  []*releases.Release (an exported wrapper is enough).
//	PolicyOf       — GAP: api.loadPolicy is unexported (internal/api/policy.go:26,
//	                  spec §6 F3). Needs an exported
//	                  api.Env.PolicyDoc(ctx, git.RepoId) (*policy.Document, error).
//
// A nil field is tolerated: the affected page renders 503 "… not wired" (or
// omits the advisory block for PolicyOf) rather than panicking. Until the
// four GAP seams exist, /releases renders its "list not wired" line, the
// issue pages render with an empty option set (the label/milestone pickers
// then say "no labels/milestones yet"), and the PR merge box treats the
// required-checks/reviews set as unknown (fail-closed, spec §3.4).
//
// Binding shape (composition, per field: `d.F = svc.Method` where the
// signatures match exactly; the two adaptations are closures):
//
//	d.RoleOf = func(ctx context.Context, o, r string, p auth.Principal) identity.Role {
//		role, _ := identitySvc.Resolve(ctx, o, r, p)
//		return role
//	}
//	d.ReleasesList = func(...) ([]*releases.Release, bool, error) { /* GAP */ }
//	d.PolicyOf = func(ctx context.Context, id git.RepoId) (*policy.Document, error) { /* GAP */ }
//
// Everything else is a direct method value (e.g. d.PullGet = pullsSvc.GetPR).
// BindCollab is called once, before the first request, exactly like
// BindForms/BindCollab's siblings in the other group files.
//
// Authority: html forms re-check the same rules the services enforce (author
// or triage for state/lifecycle, triage for labels/milestone, maintain for
// merge and release delete, write for release edits/assets), plus the
// anonymous-write refusal and a same-origin check (spec §1.2 #6, §6 F5).
package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/checks"
	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/identity"
	"github.com/crueber/walhub/internal/issues"
	"github.com/crueber/walhub/internal/policy"
	"github.com/crueber/walhub/internal/pulls"
	"github.com/crueber/walhub/internal/releases"
	"github.com/crueber/walhub/internal/review"
	"github.com/crueber/walhub/internal/server/auth"
	"github.com/crueber/walhub/internal/store"
)

// CollabDeps is the collaboration service seam. Function fields keep the
// binding exact: composition assigns the service method (or a one-line
// closure), so a page and its API twin read through the same code path.
type CollabDeps struct {
	RoleOf func(ctx context.Context, owner, repo string, p auth.Principal) identity.Role

	// internal/issues
	IssuesList   func(ctx context.Context, owner, repo string, p auth.Principal, f issues.ListFilter) (*issues.ListResult, error)
	IssuesGet    func(ctx context.Context, owner, repo string, num int, p auth.Principal, afterSeq, n int) (*issues.ThreadView, error)
	IssueComment func(ctx context.Context, owner, repo string, num int, actor auth.Principal, body string) (*issues.Event, error)
	IssuePatch   func(ctx context.Context, owner, repo string, num int, actor auth.Principal, p issues.IssuePatch) (*issues.Thread, error)
	IssueReact   func(ctx context.Context, owner, repo string, num, target int, actor auth.Principal, content string) (*issues.Thread, *issues.Event, bool, error)
	IssueUnreact func(ctx context.Context, owner, repo string, num, target int, actor auth.Principal, content string) (*issues.Thread, error)
	Labels       func(ctx context.Context, owner, repo string) ([]issues.Label, error)      // GAP, see header
	Milestones   func(ctx context.Context, owner, repo string) ([]*issues.Milestone, error) // GAP, see header

	// internal/pulls
	PullsList    func(ctx context.Context, owner, repo string, p auth.Principal, f pulls.ListFilter) (*pulls.ListResult, error)
	PullGet      func(ctx context.Context, owner, repo string, num int, p auth.Principal, afterSeq, n int) (*pulls.PullView, error)
	PullComment  func(ctx context.Context, owner, repo string, num int, actor auth.Principal, body string) (*pulls.Event, error)
	PullUpdate   func(ctx context.Context, owner, repo string, num int, actor auth.Principal, p pulls.PRPatch) (*pulls.Thread, *pulls.PRDoc, error)
	PullCommits  func(ctx context.Context, owner, repo string, num int, p auth.Principal, skip, n int) ([]pulls.CommitEntry, bool, error)
	PullDiff     func(ctx context.Context, owner, repo string, num int, p auth.Principal) (string, error)
	ForksList    func(ctx context.Context, owner, repo string, actor auth.Principal, after string, n int) (*pulls.ForkListPage, error)
	MergeStart   func(ctx context.Context, owner, repo string, num int, actor auth.Principal, in pulls.MergeInput, correlationID string) (*pulls.TaskRecord, error)
	MergeTask    func(owner, repo string) *pulls.TaskRecord
	UpdateBranch func(ctx context.Context, owner, repo string, num int, actor auth.Principal, expectedHeadSHA string) (*pulls.TaskRecord, error)

	// internal/review
	ReviewsList     func(ctx context.Context, owner, repo string, num int, p auth.Principal, after, n int) (*review.ListResult, error)
	ReviewSubmit    func(ctx context.Context, owner, repo string, num int, actor auth.Principal, in review.SubmitInput) (*review.ReviewEvent, []*review.ThreadHeader, *review.ReviewSummary, error)
	ReviewDismiss   func(ctx context.Context, owner, repo string, num, seq int, actor auth.Principal, reason string) (*review.ReviewEvent, *review.ReviewSummary, error)
	ThreadsList     func(ctx context.Context, owner, repo string, num int, p auth.Principal, resolved *bool, after string, n int) (*review.ThreadListResult, error)
	ThreadGet       func(ctx context.Context, owner, repo string, num int, tid string, p auth.Principal, after, n int) (*review.ThreadView, error)
	ThreadOpen      func(ctx context.Context, owner, repo string, num int, actor auth.Principal, anchor review.Anchor, body string) (*review.ThreadHeader, error)
	ThreadComment   func(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal, body string) (*review.ThreadComment, error)
	ThreadResolve   func(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal) (*review.ThreadHeader, error)
	ThreadUnresolve func(ctx context.Context, owner, repo string, num int, tid string, actor auth.Principal) (*review.ThreadHeader, error)
	RequestsGet     func(ctx context.Context, owner, repo string, num int, p auth.Principal) (*review.ReviewRequests, error)
	RequestsAdd     func(ctx context.Context, owner, repo string, num int, actor auth.Principal, principals []string) (*review.ReviewRequests, error)
	RequestsRemove  func(ctx context.Context, owner, repo string, num int, actor auth.Principal, principals []string) (*review.ReviewRequests, error)

	// internal/checks
	ChecksList     func(ctx context.Context, owner, repo string, p auth.Principal, after string, n int) (*checks.ChecksPage, error)
	ChecksCombined func(ctx context.Context, owner, repo, sha string, p auth.Principal) (*checks.CombinedView, error)
	ChecksStatuses func(ctx context.Context, owner, repo, sha string, p auth.Principal) (*checks.StatusesView, error)

	// internal/releases
	ReleasesList  func(ctx context.Context, owner, repo string, p auth.Principal, n int, after string) ([]*releases.Release, bool, error) // GAP, see header
	ReleaseGet    func(ctx context.Context, owner, repo string, p auth.Principal, tag string) (*releases.Release, store.Version, error)
	ReleaseLatest func(ctx context.Context, owner, repo string, p auth.Principal, includePrereleases bool) (*releases.Release, store.Version, error)
	ReleasePut    func(ctx context.Context, owner, repo string, p auth.Principal, tag string, in releases.ReleaseInput, expectedVer store.Version) (*releases.Release, bool, error)
	ReleaseDelete func(ctx context.Context, owner, repo string, p auth.Principal, tag string) error
	AssetUpload   func(ctx context.Context, owner, repo string, p auth.Principal, tag, name string, body io.Reader, contentLength int64, shaHex, contentType string) (*releases.AssetEntry, error)
	AssetDelete   func(ctx context.Context, owner, repo string, p auth.Principal, tag, name string) (*releases.AssetEntry, error)

	// PolicyOf supplies the merge-box policy document (spec §6 F3).
	PolicyOf func(ctx context.Context, id git.RepoId) (*policy.Document, error)
}

var collabDeps CollabDeps

// BindCollab installs the collaboration service seam (composition, at mount
// time before the first request).
func BindCollab(d CollabDeps) { collabDeps = d }

func init() { registerGroup((*Server).mountCollab) }

// mountCollab registers the collaboration route family. Precedence is
// load-bearing (spec §1.3): static siblings (issues/new, releases/new) are
// registered by the forms group and win over the {num}/{tag} wildcards by
// ServeMux specificity; every deeper path under a claimed family ends in a
// family catch-all that answers 404 instead of falling through to the server.
func (s *Server) mountCollab() {
	m := &s.mux

	// Issues
	m.HandleFunc("GET /{owner}/{repo}/issues", s.issueList)
	m.HandleFunc("GET /{owner}/{repo}/issues/{num}", s.issuePage)
	m.HandleFunc("GET /{owner}/{repo}/issues/{rest...}", s.collabNotFound)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/comments", s.issueComment)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/close", s.issueClose)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/reopen", s.issueReopen)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/labels", s.issueLabels)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/milestone", s.issueMilestone)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/reactions/{seq}", s.issueReact)
	m.HandleFunc("POST /{owner}/{repo}/issues/{num}/reactions/{seq}/{content}/delete", s.issueUnreact)

	// Pulls (list is plural; the conversation is singular /pull/{num})
	m.HandleFunc("GET /{owner}/{repo}/pulls", s.pullList)
	m.HandleFunc("GET /{owner}/{repo}/pulls/{rest...}", s.collabNotFound)
	m.HandleFunc("GET /{owner}/{repo}/pull/{num}", s.pullPage)
	m.HandleFunc("GET /{owner}/{repo}/pull/{num}/commits", s.pullCommits)
	m.HandleFunc("GET /{owner}/{repo}/pull/{num}/files", s.pullFiles)
	m.HandleFunc("GET /{owner}/{repo}/pull/{num}/{rest...}", s.collabNotFound)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/comments", s.pullComment)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/close", s.pullClose)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/reopen", s.pullReopen)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/ready", s.pullReady)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/draft", s.pullDraft)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/reviews", s.pullReview)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/reviews/{seq}/dismiss", s.pullDismiss)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/reviewers", s.pullReviewers)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/reviewers/remove", s.pullReviewersRemove)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/threads", s.pullThreadOpen)
	m.HandleFunc("GET /{owner}/{repo}/pull/{num}/threads/{tid}", s.pullThreadComments)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/threads/{tid}/comments", s.pullThreadComment)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/threads/{tid}/resolve", s.pullThreadResolve)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/threads/{tid}/unresolve", s.pullThreadUnresolve)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/merge", s.pullMerge)
	m.HandleFunc("GET /{owner}/{repo}/pull/{num}/merge/task", s.pullMergeTask)
	m.HandleFunc("POST /{owner}/{repo}/pull/{num}/update-branch", s.pullUpdateBranch)

	// Checks
	m.HandleFunc("GET /{owner}/{repo}/checks", s.checksList)
	m.HandleFunc("GET /{owner}/{repo}/checks/{sha}", s.checkDetail)
	m.HandleFunc("GET /{owner}/{repo}/checks/{sha}/contexts", s.checkContexts)
	m.HandleFunc("GET /{owner}/{repo}/checks/{rest...}", s.collabNotFound)

	// Releases
	//
	// NOTE: no /releases/{rest...} family catch-all — the release ASSET BYTE
	// route /{o}/{r}/releases/{tag}/assets/{name} lives outside the API lanes
	// (internal/releases/http.go HandleRepo) and must win over the page
	// mount. A deeper unknown /releases/... path therefore falls through to
	// the server's repo dispatch, which answers its plain 404.
	m.HandleFunc("GET /{owner}/{repo}/releases", s.releaseList)
	m.HandleFunc("GET /{owner}/{repo}/releases/{tag}", s.releasePage)
	m.HandleFunc("POST /{owner}/{repo}/releases/{tag}/save", s.releaseSave)
	m.HandleFunc("POST /{owner}/{repo}/releases/{tag}/publish", s.releasePublish)
	m.HandleFunc("POST /{owner}/{repo}/releases/{tag}/delete", s.releaseDelete)
	m.HandleFunc("POST /{owner}/{repo}/releases/{tag}/assets", s.releaseAssetUpload)
	m.HandleFunc("POST /{owner}/{repo}/releases/{tag}/assets/{name}/delete", s.releaseAssetDelete)

	// Forks (dead on hard load before this migration — spec §1.4)
	m.HandleFunc("GET /{owner}/{repo}/forks", s.forkList)
}

// collabNotFound is the family catch-all: unknown paths under a claimed
// family 404 on their own page (spec §1.3 #7).
func (s *Server) collabNotFound(w http.ResponseWriter, r *http.Request) {
	s.collabError(w, r, http.StatusNotFound, "not found")
}

// --- repo context, gates -----------------------------------------------------

// collabRepo is the per-page repo model: identity, resolved role, feature
// flags, and the hrefs whose gating depends on them.
type collabRepo struct {
	Owner string
	Name  string
	Full  string

	Features config.ResolvedFeatures
	Role     identity.Role

	SignedIn  bool
	Anonymous bool
	CanWrite  bool
	CanTriage bool
	CanMaint  bool
	CanAdmin  bool

	// NewIssueHref is the gated/shaped creation link ("" = feature off or no
	// target). Anonymous viewers get the login-required interstitial.
	NewIssueHref   string
	NewPullHref    string
	NewReleaseHref string
	LoginHref      string // /login-required target for generic write affordances
}

func roleRank(r identity.Role) int {
	switch r {
	case identity.RoleRead:
		return 1
	case identity.RoleTriage:
		return 2
	case identity.RoleWrite:
		return 3
	case identity.RoleMaintain:
		return 4
	case identity.RoleAdmin:
		return 5
	}
	return 0
}

func (c collabRepo) atLeast(min identity.Role) bool { return roleRank(c.Role) >= roleRank(min) }

// collabGate resolves the principal and the repo read authority: a public
// repo admits anonymous viewers even when anonymous_read is off (the repo
// visibility rule, Forgejo #345), a private one keeps the legacy 401/403
// outcome.
func (s *Server) collabGate(w http.ResponseWriter, r *http.Request, owner, repo string) (auth.Principal, bool) {
	p, ok := s.principal(w, r)
	if !ok {
		return p, false
	}
	if !s.repoVisible(r.Context(), owner, repo, p) {
		if p.Anonymous {
			s.collabError(w, r, http.StatusUnauthorized, "authentication required")
		} else {
			s.collabError(w, r, http.StatusForbidden, "read access required")
		}
		return p, false
	}
	return p, true
}

// collabEnv loads the gated repo model for a read page. Unknown repo → 404,
// read-gate failure → 401/403, view failure → 503.
func (s *Server) collabEnv(w http.ResponseWriter, r *http.Request) (collabRepo, auth.Principal, bool) {
	rc := collabRepo{Owner: r.PathValue("owner"), Name: r.PathValue("repo"), Features: config.AllFeatures()}
	rc.Full = rc.Owner + "/" + rc.Name
	p, ok := s.collabGate(w, r, rc.Owner, rc.Name)
	if !ok {
		return rc, p, false
	}
	if !s.collabRepoExists(r.Context(), rc.Owner, rc.Name, &rc.Features) {
		s.collabError(w, r, http.StatusNotFound, "unknown repository")
		return rc, p, false
	}
	rc.Role = s.collabRole(r.Context(), rc.Owner, rc.Name, p)
	rc.Anonymous = p.Anonymous
	rc.SignedIn = !p.Anonymous && s.authMode() != "" && s.authMode() != "none"
	rc.CanWrite = rc.atLeast(identity.RoleWrite)
	rc.CanTriage = rc.atLeast(identity.RoleTriage)
	rc.CanMaint = rc.atLeast(identity.RoleMaintain)
	rc.CanAdmin = rc.atLeast(identity.RoleAdmin)
	rc.LoginHref = "/login-required?next=" + url.QueryEscape(r.URL.Path)
	next := r.URL.Path
	if rc.Features.Issues {
		rc.NewIssueHref = s.collabWriteHref(p, next+"/new", "Create a new issue")
	}
	if rc.Features.Pulls {
		rc.NewPullHref = s.collabWriteHref(p, next+"/new", "Open a pull request")
	}
	if rc.Features.Releases {
		rc.NewReleaseHref = s.collabWriteHref(p, next+"/new", "Create a release")
	}
	return rc, p, true
}

// collabRepoExists probes the registry (and reads the summary's feature
// flags). The summary is the per-repo read the API's own summary handler
// uses, so the page and the JSON twin agree on feature flags.
func (s *Server) collabRepoExists(ctx context.Context, owner, name string, feats *config.ResolvedFeatures) bool {
	if s.env.API == nil {
		return false
	}
	id := git.RepoId{Owner: owner, Name: name}
	if s.env.API.Repo != nil {
		sum, err := s.env.API.Repo.Summary(ctx, id)
		if err != nil {
			if errors.Is(err, api.ErrNotFound) {
				return false
			}
			// A view outage is not "missing": answer the page body empty so
			// the inline error can say so.
			return true
		}
		if sum.Features != nil {
			*feats = *sum.Features
		}
		return true
	}
	if s.env.API.Repos != nil {
		ex, err := s.env.API.Repos.Exists(ctx, id)
		return err == nil && ex
	}
	return false
}

// collabRole resolves the P6 role. Without the identity seam it mirrors the
// services' own fallback (admin/write flags, read for signed-in strangers).
func (s *Server) collabRole(ctx context.Context, owner, repo string, p auth.Principal) identity.Role {
	if collabDeps.RoleOf != nil {
		return collabDeps.RoleOf(ctx, owner, repo, p)
	}
	if p.Admin {
		return identity.RoleAdmin
	}
	if p.Write {
		return identity.RoleWrite
	}
	if p.Anonymous {
		return ""
	}
	return identity.RoleRead
}

// collabWriteHref shapes a write affordance for the viewer: anonymous
// viewers (token/oidc mode) get the login-required interstitial, everyone
// else the plain path.
func (s *Server) collabWriteHref(p auth.Principal, next, action string) string {
	if !s.anonWrite(p) {
		return next
	}
	return "/login-required?next=" + url.QueryEscape(next) + "&action=" + url.QueryEscape(action)
}

// anonWrite mirrors web/src/lib/writeGate.isAnonymousViewer: only a usable
// auth mode (token/oidc) gates, and only for an anonymous/absent identity.
func (s *Server) anonWrite(p auth.Principal) bool {
	mode := s.authMode()
	if mode != "token" && mode != "oidc" {
		return false
	}
	return p.Anonymous
}

// collabError is the error page for this group. It mirrors Server.errorPage
// (HTML page + Accept: text/plain twin) but renders its own template: the
// shared templates/pages/error.html currently fails to execute
// (error.html:6 compares the int .Data.Status with the string-only eq func,
// helpers.go:16), so every errorPage HTML render answers 500. Reported as a
// foundation gap; switch this helper's page name back to "error" once
// error.html compiles/executes.
func (s *Server) collabError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if w.Header().Get("Content-Type") == "" && headerWantsText(r) {
		http.Error(w, msg, status)
		return
	}
	anon := auth.Principal{Name: "anonymous", Anonymous: true}
	s.page(w, r, anon, status, "collab/error", statusTitle(status), struct {
		Title   string
		Message string
	}{statusTitle(status), msg})
}

// collabWrite is the POST gate: same-origin, principal, anonymous refusal,
// repo visibility, and the per-action role floor. On failure the response is
// already written.
func (s *Server) collabWrite(w http.ResponseWriter, r *http.Request, min identity.Role, action string) (collabRepo, auth.Principal, bool) {
	if !sameOrigin(r) {
		s.collabError(w, r, http.StatusForbidden, "cross-origin request refused")
		return collabRepo{}, auth.Principal{}, false
	}
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return rc, p, false
	}
	if s.anonWrite(p) {
		next := r.URL.Path
		target := "/login-required?next=" + url.QueryEscape(next)
		if action != "" {
			target += "&action=" + url.QueryEscape(action)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return rc, p, false
	}
	if min != "" && !rc.atLeast(min) {
		if rc.Role == "" {
			s.collabError(w, r, http.StatusUnauthorized, "authentication required")
			return rc, p, false
		}
		s.collabError(w, r, http.StatusForbidden, string(min)+" access required")
		return rc, p, false
	}
	return rc, p, true
}

// sameOrigin enforces the browser mutation rule (PLAN §Writes): an Origin or
// Referer, when present, must name this host; a form post that carries
// neither is refused (fail closed).
func sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return false
		}
		return u.Host == r.Host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		return u.Host == r.Host
	}
	return false
}

// collabFragment renders one named template block from a page's set (htmx
// fragments reuse the page's partials; no second template tree).
func (s *Server) collabFragment(w http.ResponseWriter, r *http.Request, page, name string, data any) {
	t, ok := s.tpl[page]
	if !ok {
		s.collabError(w, r, http.StatusInternalServerError, "page template missing: "+page)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.collabError(w, r, http.StatusInternalServerError, "render: "+err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

// collabErrStatus maps a feature-service sentinel onto the page status.
func collabErrStatus(err error) int {
	switch {
	case errors.Is(err, issues.ErrNotFound), errors.Is(err, pulls.ErrNotFound),
		errors.Is(err, review.ErrNotFound), errors.Is(err, checks.ErrNotFound),
		errors.Is(err, releases.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, issues.ErrInvalid), errors.Is(err, pulls.ErrInvalid),
		errors.Is(err, review.ErrInvalid), errors.Is(err, checks.ErrInvalid),
		errors.Is(err, releases.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, issues.ErrLocked), errors.Is(err, pulls.ErrLocked),
		errors.Is(err, review.ErrLocked):
		return http.StatusConflict
	case errors.Is(err, issues.ErrConflict), errors.Is(err, pulls.ErrConflict),
		errors.Is(err, review.ErrConflict), errors.Is(err, checks.ErrConflict),
		errors.Is(err, releases.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, pulls.ErrUnprocessable), errors.Is(err, review.ErrUnprocessable),
		errors.Is(err, checks.ErrUnprocessable):
		return http.StatusUnprocessableEntity
	case errors.Is(err, issues.ErrUnauthorized), errors.Is(err, pulls.ErrUnauthorized),
		errors.Is(err, review.ErrUnauthorized), errors.Is(err, checks.ErrUnauthorized),
		errors.Is(err, releases.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, issues.ErrForbidden), errors.Is(err, pulls.ErrForbidden),
		errors.Is(err, review.ErrForbidden), errors.Is(err, checks.ErrForbidden),
		errors.Is(err, releases.ErrForbidden):
		return http.StatusForbidden
	default:
		return http.StatusServiceUnavailable
	}
}

// redirectPage answers a successful write with 303 to the page (fragment
// preserved) or an HX-Redirect for htmx form posts.
func redirectPage(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// --- path/query parsing ------------------------------------------------------

// issueNum parses the {num} wildcard: ^[1-9][0-9]{0,8}$ and ≤ 0xFFFFFF, the
// shared allocator rule (spec §1.3 #1).
func issueNum(v string) (int, bool) {
	if v == "" || len(v) > 9 || v[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 0xFFFFFF {
		return 0, false
	}
	return n, true
}

// fullSHA reports a 40/64 lowercase-hex sha (the checks route rule).
func fullSHA(v string) bool {
	if len(v) != 40 && len(v) != 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// wireState maps the resolved value for the list service: all → "" (omitted).
func wireState(v string) string {
	if v == "all" || v == "" {
		return ""
	}
	return v
}

func afterParam(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// --- timestamps, sizes -------------------------------------------------------

// fmtAssetSize renders a byte count in binary units ("1.4 MiB").
func fmtAssetSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	names := []string{"KiB", "MiB", "GiB", "TiB"}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + names[exp]
}

// --- chips, labels, milestones ----------------------------------------------

// labelChip is one applied label: Color==""/ok=false renders the bare name
// (deleted label), the label-chip self-heal rule.
type labelChip struct {
	Name  string
	Color string
	Known bool
}

func labelChips(names []string, all []issues.Label) []labelChip {
	if len(names) == 0 {
		return nil
	}
	by := make(map[string]issues.Label, len(all))
	for _, l := range all {
		by[strings.ToLower(l.Name)] = l
	}
	out := make([]labelChip, 0, len(names))
	for _, n := range names {
		l, ok := by[strings.ToLower(n)]
		out = append(out, labelChip{Name: n, Color: l.Color, Known: ok})
	}
	return out
}

// milestoneChip resolves an id to its display title; a missing id renders
// the bare id (deleted-milestone self-heal).
type milestoneChip struct {
	ID     string
	Title  string
	Closed bool
	Known  bool
	Href   string
}

func milestoneFor(id *string, all []*issues.Milestone, owner, repo string) *milestoneChip {
	if id == nil || *id == "" {
		return nil
	}
	c := &milestoneChip{ID: *id, Title: *id, Href: "/" + owner + "/" + repo + "/issues?milestone=" + url.QueryEscape(*id) + "&state=all"}
	for _, m := range all {
		if m.ID == *id {
			c.Title, c.Closed, c.Known = m.Title, m.State == "closed", true
			break
		}
	}
	return c
}

func milestoneTitleOf(id *string, all []*issues.Milestone) string {
	if id == nil {
		return ""
	}
	for _, m := range all {
		if m.ID == *id {
			return m.Title
		}
	}
	return *id
}

// --- reactions (spec §2.6 / §4.6) -------------------------------------------

var reactionContents = []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}

var reactionGlyphs = map[string]string{
	"+1": "👍", "-1": "👎", "laugh": "😄", "hooray": "🎉",
	"confused": "😕", "heart": "❤️", "rocket": "🚀", "eyes": "👀",
}

type reactionChip struct {
	Content string
	Glyph   string
	Count   int
}

// reactionRows folds thread.reaction_summary[%06x seq] into the chip row and
// the addable "+" set, in the fixed REACTIONS order (unknown contents after).
func reactionRows(summary map[string]map[string]int, seq int) (shown, addable []reactionChip) {
	key := fmt.Sprintf("%06x", seq)
	per := summary[key]
	seen := map[string]bool{}
	for _, c := range reactionContents {
		if n := per[c]; n > 0 {
			shown = append(shown, reactionChip{Content: c, Glyph: reactionGlyphs[c], Count: n})
			seen[c] = true
		}
	}
	var extra []string
	for c := range per {
		if !containsStr(reactionContents, c) && per[c] > 0 {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	for _, c := range extra {
		shown = append(shown, reactionChip{Content: c, Glyph: c, Count: per[c]})
		seen[c] = true
	}
	for _, c := range reactionContents {
		if !seen[c] {
			addable = append(addable, reactionChip{Content: c, Glyph: reactionGlyphs[c]})
		}
	}
	return shown, addable
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- issue system-event text (issue-events.js port) -------------------------

// issueEventText returns "" for comment kinds (rendered as bodies) and a
// plain sentence fragment for every other event kind.
func issueEventText(ev *issues.Event, milestones []*issues.Milestone) string {
	switch ev.Type {
	case issues.EventOpened, issues.EventCommented:
		return ""
	case issues.EventTitleChanged:
		return "retitled “" + ptrStr(ev.From) + "” → “" + ptrStr(ev.To) + "”"
	case issues.EventLabelsChanged:
		return labelFragment(ev.Added, ev.Removed)
	case issues.EventAssigneesChanged:
		return assigneeFragment(ev.Added, ev.Removed)
	case issues.EventStateChanged:
		if ptrStr(ev.To) == "closed" {
			switch ptrStr(ev.Reason) {
			case issues.ReasonNotPlanned:
				return "closed as not planned"
			case issues.ReasonCompleted:
				return "closed as completed"
			}
			return "closed"
		}
		return "reopened"
	case issues.EventMilestoneChanged:
		from, to := "", ""
		if ev.From != nil {
			from = milestoneTitleOf(ev.From, milestones)
		}
		if ev.To != nil {
			to = milestoneTitleOf(ev.To, milestones)
		}
		switch {
		case from == "" && to != "":
			return "added this to the “" + to + "” milestone"
		case to == "" && from != "":
			return "removed this from the “" + from + "” milestone"
		case from != "" && to != "":
			return "moved this from “" + from + "” to “" + to + "”"
		}
		return "changed the milestone"
	case issues.EventReferenced:
		return "referenced this from #" + sourceNum(ev.Source)
	case issues.EventCrossReferenced:
		return "referenced this from " + sourceRepo(ev.Source) + "#" + sourceNum(ev.Source)
	case issues.EventClosedByPR:
		return "closed this via #" + itoaPtr(ev.PRNum) + " (" + ptrStr(ev.Keyword) + ")"
	default:
		if ev.Type == "" {
			return "event"
		}
		return ev.Type
	}
}

func labelFragment(added, removed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added the "+quoteList(added)+" label"+pluralSuffix(len(added)))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed the "+quoteList(removed)+" label"+pluralSuffix(len(removed)))
	}
	if len(parts) == 0 {
		return "changed the labels"
	}
	return strings.Join(parts, " and ")
}

func assigneeFragment(added, removed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "assigned "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "unassigned "+strings.Join(removed, ", "))
	}
	if len(parts) == 0 {
		return "changed the assignees"
	}
	return strings.Join(parts, " and ")
}

func quoteList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "“" + n + "”"
	}
	return strings.Join(out, ", ")
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func sourceNum(src map[string]any) string {
	if src == nil {
		return "?"
	}
	switch v := src["num"].(type) {
	case float64:
		return strconv.Itoa(int(v))
	case int:
		return strconv.Itoa(v)
	case string:
		return v
	}
	return "?"
}

func sourceRepo(src map[string]any) string {
	if src == nil {
		return "?"
	}
	if s, ok := src["repo"].(string); ok && s != "" {
		return s
	}
	return "?"
}

func itoaPtr(v *int) string {
	if v == nil {
		return "?"
	}
	return strconv.Itoa(*v)
}

func ptrStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// closedStateLabel is the header state line for a closed thread.
func closedStateLabel(reason *string) string {
	switch ptrStr(reason) {
	case issues.ReasonNotPlanned:
		return "Closed as not planned"
	case issues.ReasonCompleted:
		return "Closed as completed"
	}
	return "Closed"
}

// --- issue comment lock (comment-lock-594 port) ------------------------------

type lockState struct {
	Locked bool
	Reason string
}

func issueCommentLock(th *issues.Thread) lockState {
	if th != nil && th.State != "open" {
		return lockState{Locked: true, Reason: "This conversation is closed"}
	}
	return lockState{}
}

func pullCommentLock(th *pulls.Thread, pr *pulls.PRDoc) lockState {
	if pr != nil && pr.Merged {
		return lockState{Locked: true, Reason: "Merged — commenting is locked"}
	}
	if th != nil && th.State != "open" {
		return lockState{Locked: true, Reason: "This conversation is closed"}
	}
	return lockState{}
}

// --- PR state helpers (pull-state.js port) ---------------------------------

func normPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }

func isPullAuthor(author string, p auth.Principal) bool {
	a := normPrincipal(author)
	return a != "" && a == normPrincipal(p.Name)
}

func canModifyPull(author string, p auth.Principal, role identity.Role) bool {
	return isPullAuthor(author, p) || roleRank(role) >= roleRank(identity.RoleTriage)
}

type stateChip struct {
	Text string
	Cls  string
}

// pullBadgeView is the header badge: merged wins, then closed, then draft.
func pullBadgeView(th *pulls.Thread, pr *pulls.PRDoc) stateChip {
	if pr != nil && pr.Merged {
		return stateChip{"Merged", "chip chip-merged"}
	}
	if th != nil && th.State != "open" {
		return stateChip{"Closed", "chip chip-closed"}
	}
	if pr != nil && pr.Draft {
		return stateChip{"Draft", "chip chip-draft"}
	}
	return stateChip{"Open", "chip chip-open"}
}

// pullListChip is the list-row chip (lowercase text convention).
func pullListChip(row pulls.PROut) stateChip {
	if row.Merged {
		return stateChip{"merged", "chip chip-merged"}
	}
	if row.State != "open" {
		return stateChip{"closed", "chip chip-closed"}
	}
	if row.Draft {
		return stateChip{"draft", "chip chip-draft"}
	}
	return stateChip{"open", "chip chip-open"}
}

// pullEventText is the conversation textFor map: "" = body row.
func pullEventText(ev *pulls.Event) string {
	switch ev.Type {
	case pulls.EventOpened:
		return "opened"
	case pulls.EventCommented:
		return ""
	case pulls.EventTitleChanged:
		return "retitled “" + ptrStr(ev.From) + "” → “" + ptrStr(ev.To) + "”"
	case pulls.EventStateChanged:
		if ptrStr(ev.To) == "closed" {
			return "closed"
		}
		return "reopened"
	case pulls.EventDraftChanged:
		if ptrStr(ev.To) == "ready" {
			return "marked as ready for review"
		}
		return "converted to draft"
	case pulls.EventMerged:
		return "merged as " + short12(ptrStr(ev.MergeCommitSHA)) + " (" + defaultStr(ptrStr(ev.Strategy), "merge") + ")"
	case pulls.EventHeadForcePushed:
		return "head force-pushed " + short12(ptrStr(ev.From)) + " → " + short12(ptrStr(ev.To))
	default:
		return ev.Type
	}
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// reviewVerdictLabel maps review wire states to display text.
func reviewVerdictLabel(state string) string {
	switch state {
	case review.StateApproved:
		return "Approved"
	case review.StateChangesRequested:
		return "Changes requested"
	case review.StateCommented:
		return "Commented"
	case review.DecisionReviewRequired:
		return "Review required"
	case review.StateDismissed:
		return "Dismissed"
	}
	if state == "" {
		return "Unknown"
	}
	return state
}

// reviewVerdictChip maps a verdict onto the state-chip family.
func reviewVerdictChip(state string) stateChip {
	switch state {
	case review.StateApproved:
		return stateChip{reviewVerdictLabel(state), "chip chip-open"}
	case review.StateChangesRequested:
		return stateChip{reviewVerdictLabel(state), "chip chip-closed"}
	case review.StateCommented, review.StateDismissed:
		return stateChip{reviewVerdictLabel(state), "chip chip-neutral"}
	}
	return stateChip{reviewVerdictLabel(state), "chip chip-draft"}
}

// terminalMergeDetail is the merged Status secondary line.
type mergeDetail struct {
	SHA      string
	Strategy string
	By       string
}

func terminalMergeDetail(pr *pulls.PRDoc, events []*pulls.Event) mergeDetail {
	d := mergeDetail{}
	if pr != nil {
		d.SHA = ptrStr(pr.MergeCommitSHA)
		d.Strategy = ptrStr(pr.MergeStrategy)
		d.By = ptrStr(pr.MergedBy)
	}
	for _, ev := range events {
		if ev.Type != pulls.EventMerged {
			continue
		}
		if d.SHA == "" {
			d.SHA = ptrStr(ev.MergeCommitSHA)
		}
		if d.Strategy == "" {
			d.Strategy = ptrStr(ev.Strategy)
		}
		if d.By == "" {
			d.By = ev.Actor
		}
		break
	}
	return d
}

// --- merge machine (MergeBox.jsx port) -------------------------------------

type mergeStateView struct {
	State        string // merged|failed|merging|draft|blocked|mergeable|ready
	CanMerge     bool
	CanUpdate    bool
	Blockers     []string
	BlockersText string
	Tooltip      string
	Task         *pulls.TaskRecord
	Detail       mergeDetail
	Strategy     string
}

func pullMergeState(pr *pulls.PRDoc, mergeable *pulls.MergeableDoc, checksBlockers []string, reviewDecision string, requiresReviews *bool, role identity.Role, task *pulls.TaskRecord) mergeStateView {
	st := "ready"
	switch {
	case pr != nil && pr.Merged:
		st = "merged"
	case task != nil && (string(task.State) == "failed" || task.Error != ""):
		st = "failed"
	case task != nil && string(task.State) == "running":
		st = "merging"
	case pr != nil && pr.Draft:
		st = "draft"
	case len(checksBlockers) > 0:
		st = "blocked"
	case reviewDecision == review.DecisionChangesRequested && (requiresReviews == nil || *requiresReviews):
		st = "blocked"
	case mergeable != nil && mergeable.State == pulls.MergeableDirty:
		st = "blocked"
	case mergeable != nil && (mergeable.State == pulls.MergeableClean || mergeable.State == pulls.MergeableBehind):
		st = "mergeable"
	}
	// Forgejo #612: "changes requested" is listed only when it blocks.
	var blockers []string
	blockers = append(blockers, checksBlockers...)
	if reviewDecision == review.DecisionChangesRequested && (requiresReviews == nil || *requiresReviews) {
		blockers = append(blockers, "changes requested")
	}
	if mergeable != nil && mergeable.State == pulls.MergeableDirty {
		blockers = append(blockers, "conflicts: "+strings.Join(mergeable.Conflicts, ", "))
	}
	v := mergeStateView{
		State:        st,
		CanMerge:     st == "mergeable" && roleRank(role) >= roleRank(identity.RoleMaintain),
		CanUpdate:    roleRank(role) >= roleRank(identity.RoleWrite),
		Blockers:     blockers,
		BlockersText: strings.Join(blockers, "; "),
		Task:         task,
		Detail:       terminalMergeDetail(pr, nil),
		Strategy:     "merge",
	}
	switch {
	case roleRank(role) < roleRank(identity.RoleMaintain):
		v.Tooltip = "merging requires the maintain role"
	case len(blockers) > 0:
		v.Tooltip = "blocked: " + strings.Join(blockers, "; ")
	case st == "draft":
		v.Tooltip = "draft PRs cannot merge"
	case st == "merging":
		v.Tooltip = "merge already running"
	case st == "merged":
		v.Tooltip = "already merged"
	default:
		v.Tooltip = "merge pull request"
	}
	return v
}

// --- mergeability display (mergeability-display-588 port) -------------------

const (
	mergeabilityOKCls    = "text-emerald-600 dark:text-emerald-400"
	mergeabilityBadCls   = "text-red-600 dark:text-red-400"
	mergeabilityMutedCls = "text-zinc-500 dark:text-zinc-400"
)

type mergeabilityText struct {
	Text string
	Cls  string
	Sub  string
}

func mergeabilityDisplay(state string, mergeable *pulls.MergeableDoc, blockers []string, reviewDecision string, draft, zeroChecks bool) mergeabilityText {
	wire := ""
	if mergeable != nil {
		wire = mergeable.State
	}
	behind := state == "behind" || wire == pulls.MergeableBehind
	switch {
	case state == "merged" || state == "up_to_date":
		return mergeabilityText{"Already merged", mergeabilityMutedCls, ""}
	case state == "failed":
		return mergeabilityText{"Merge failed", mergeabilityBadCls, ""}
	case state == "merging":
		return mergeabilityText{"Merging…", mergeabilityMutedCls, ""}
	case state == "draft" || draft:
		return mergeabilityText{"Draft pull request", mergeabilityBadCls, ""}
	case len(blockers) > 0:
		return mergeabilityText{"Checks failing", mergeabilityBadCls, ""}
	case reviewDecision == review.DecisionChangesRequested:
		return mergeabilityText{"Changes requested", mergeabilityBadCls, ""}
	case state == "dirty" || wire == pulls.MergeableDirty:
		return mergeabilityText{"Merge conflicts", mergeabilityBadCls, ""}
	case state == "clean" || state == "mergeable" || behind:
		sub := ""
		if behind {
			sub = "branch is behind — update"
		}
		return mergeabilityText{"Able to be Merged", mergeabilityOKCls, sub}
	case state == "ready" || state == "checking" || state == "unknown":
		if zeroChecks {
			return mergeabilityText{"No checks required", mergeabilityMutedCls, ""}
		}
		return mergeabilityText{"Checking mergeability", mergeabilityMutedCls, ""}
	case state == "":
		return mergeabilityText{"Unknown", mergeabilityMutedCls, ""}
	}
	return mergeabilityText{state, mergeabilityMutedCls, ""}
}

// --- checks helpers ----------------------------------------------------------

func checkStateDot(state string) string {
	switch state {
	case checks.StateSuccess:
		return "inline-block h-2.5 w-2.5 rounded-full bg-emerald-500"
	case checks.StatePending:
		return "inline-block h-2.5 w-2.5 rounded-full bg-amber-400"
	case checks.StateFailure:
		return "inline-block h-2.5 w-2.5 rounded-full bg-red-500"
	case checks.StateError:
		return "inline-block h-2.5 w-2.5 rounded-full bg-red-700"
	}
	return "inline-block h-2.5 w-2.5 rounded-full bg-zinc-400"
}

func zeroChecksTitle(required []string, known bool) string {
	if known && len(required) == 0 {
		return "No checks configured"
	}
	return "No checks reported yet"
}

// requiredCheckBlockers: every required context whose latest state is not
// success blocks ("<context> (state)" / "<context> (missing)").
func requiredCheckBlockers(required []string, statuses []*checks.StatusDoc) []string {
	if len(required) == 0 {
		return nil
	}
	by := map[string]string{}
	for _, st := range statuses {
		by[st.Context] = st.State
	}
	var out []string
	for _, c := range required {
		if by[c] != checks.StateSuccess {
			if st, ok := by[c]; ok {
				out = append(out, c+" ("+st+")")
			} else {
				out = append(out, c+" (missing)")
			}
		}
	}
	return out
}

// requiredChecks is the union of require_checks over rules whose match.refs
// is empty or exact-includes the PR's base ref.
func requiredChecks(doc *policy.Document, baseRef string) ([]string, bool) {
	if doc == nil {
		return nil, false
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range doc.Rules {
		p := r.Protect()
		if p == nil || len(p.RequireChecks) == 0 {
			continue
		}
		if len(r.Match.Refs) > 0 && !containsStr(r.Match.Refs, baseRef) {
			continue
		}
		for _, c := range p.RequireChecks {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out, true
}

// requiredReviewsApplies: ANY required-reviews rule selecting the base ref.
func requiredReviewsApplies(doc *policy.Document, baseRef string) bool {
	if doc == nil {
		return false
	}
	for _, r := range doc.Rules {
		if r.RequiredReviews() == nil {
			continue
		}
		if len(r.Match.Refs) > 0 && !containsStr(r.Match.Refs, baseRef) {
			continue
		}
		return true
	}
	return false
}

// --- release helpers (releases.js port) -------------------------------------

const latestAssetLimit = 3

func keyAssets(assets []releases.AssetEntry, limit int) (shown []releases.AssetEntry, extra int) {
	if limit <= 0 {
		return nil, len(assets)
	}
	if limit > len(assets) {
		limit = len(assets)
	}
	return assets[:limit], len(assets) - limit
}

// excerptBody: first non-blank line, markers stripped, whitespace collapsed,
// truncated to max with an ellipsis.
func excerptBody(body string, max int) string {
	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	text := strings.TrimSpace(line)
	for _, p := range []string{"# ", "## ", "### ", "#### ", "##### ", "###### ", "> ", "- ", "* ", "+ "} {
		if strings.HasPrefix(text, p) {
			text = strings.TrimSpace(strings.TrimPrefix(text, p))
			break
		}
	}
	// ordered-list marker "1. " / "1) "
	if i := strings.IndexByte(text, '.'); i > 0 && i <= 3 && allDigits(text[:i]) {
		if rest := strings.TrimSpace(text[i+1:]); rest != "" {
			text = rest
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if max <= 0 {
		max = 140
	}
	if len([]rune(text)) <= max {
		return text
	}
	r := []rune(text)
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// --- forks ------------------------------------------------------------------

type forkRow struct {
	Repo     string
	ForkedAt string
	Date     string
	Href     string
}

func forkDate(stamp string) string {
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return stamp
}

// --- shared timeline model ---------------------------------------------------

// collabEvent is one rendered timeline row (system or comment).
type collabEvent struct {
	Seq       int
	Actor     string
	System    string // non-empty → system row; empty → comment body
	Body      string
	At        string
	Reactions []reactionChip
	Addable   []reactionChip
	CanAdd    bool
	AddReason string
	Locked    bool
}

// timelineData carries the root context the timeline partial needs (the md
// helper autolinks #N/PRN against the repo).
type timelineData struct {
	Owner, Repo string
	Base        string // "/{owner}/{repo}/issues/{num}" or "/pull/{num}"
	Events      []collabEvent
	CanReact    bool
	Locked      bool
	LockReason  string
}

// orderedEvents sorts the service's newest-first window into the one
// chronological render order (stable, seq ascending — thread-order.js).
func orderedEvents(events []collabEvent) []collabEvent {
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	return events
}

// olderCursor is the after_seq cursor for the next older window: the oldest
// loaded seq (0 when empty).
func olderCursor(events []collabEvent) int {
	oldest := 0
	for _, ev := range events {
		if ev.Seq > 0 && (oldest == 0 || ev.Seq < oldest) {
			oldest = ev.Seq
		}
	}
	return oldest
}

// --- issues list -------------------------------------------------------------

type issueListRow struct {
	Num          int
	Title        string
	Href         string
	Closed       bool
	Labels       []labelChip
	Milestone    *milestoneChip
	CommentCount int
	UpdatedAt    string
}

// labelOption is one filter/picker row.
type labelOption struct {
	Name    string
	Color   string
	Known   bool
	Checked bool
}

// msOption is one milestone <option>/picker row.
type msOption struct {
	ID       string
	Title    string
	Selected bool
	Closed   bool
}

func labelOptions(all []issues.Label, applied []string) []labelOption {
	out := make([]labelOption, 0, len(all))
	for _, l := range all {
		checked := false
		for _, a := range applied {
			if strings.EqualFold(a, l.Name) {
				checked = true
			}
		}
		out = append(out, labelOption{Name: l.Name, Color: l.Color, Known: true, Checked: checked})
	}
	return out
}

func msOptions(all []*issues.Milestone, selected string) []msOption {
	sel := strings.ToLower(selected)
	out := make([]msOption, 0, len(all))
	for _, m := range all {
		out = append(out, msOption{ID: m.ID, Title: m.Title, Selected: sel == strings.ToLower(m.ID), Closed: m.State == "closed"})
	}
	return out
}

// pagerHref clones the current query and overrides one key (filters survive
// paging).
func pagerHref(r *http.Request, key, val string) string {
	q := r.URL.Query()
	q.Set(key, val)
	return r.URL.Path + "?" + q.Encode()
}

type issuesListData struct {
	Repo collabRepo

	State          string // resolved select value: open|closed|all
	StateRaw       string // the raw ?state= value (echoed for deep links)
	Assignee       string
	LabelsParam    string
	MilestoneParam string
	LabelFilter    []string

	Rows       []issueListRow
	More       bool
	After      int
	Labels     []issues.Label
	Milestones []*issues.Milestone
	Total      int

	LabelOptions     []labelOption
	MilestoneOptions []msOption
	MilestoneUnknown bool
	OlderHref        string

	Error  string
	Notice string
}

func (s *Server) issueList(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	s.renderIssues(w, r, rc, p, http.StatusOK, "", "")
}

// renderIssues answers GET /issues (and re-renders it after a validation
// failure elsewhere on the page).
func (s *Server) renderIssues(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, status int, errMsg, notice string) {
	q := r.URL.Query()
	data := issuesListData{
		Repo:           rc,
		Assignee:       q.Get("assignee"),
		MilestoneParam: q.Get("milestone"),
		Error:          errMsg,
		Notice:         notice,
	}
	// State: absent → open; all/"" → all; anything else passes through to the
	// service which answers 400 (the SPA's passthrough).
	rawState, stateSet := q["state"]
	if !stateSet {
		data.State = "open"
	} else {
		data.StateRaw = strings.Join(rawState, ",")
		switch data.StateRaw {
		case "", "all":
			data.State = "all"
		default:
			data.State = data.StateRaw
		}
	}
	if data.State != "open" && data.State != "closed" && data.State != "all" {
		data.Error = "invalid state: must be open, closed or all"
		s.page(w, r, p, http.StatusBadRequest, "collab/issues", "Issues", data)
		return
	}
	data.LabelFilter = splitLabels(q)
	data.LabelsParam = strings.Join(data.LabelFilter, ",")

	if s.resources(r.Context(), rc, &data) != nil {
		// option sets are advisory; the list still renders
		data.Notice = "labels and milestones unavailable"
	}

	if collabDeps.IssuesList == nil {
		data.Error = "issues service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/issues", "Issues", data)
		return
	}
	res, err := collabDeps.IssuesList(r.Context(), rc.Owner, rc.Name, p, issues.ListFilter{
		State:     wireState(data.State),
		Labels:    data.LabelFilter,
		Assignee:  data.Assignee,
		Milestone: strings.ToLower(data.MilestoneParam),
		After:     afterParam(r),
		N:         50,
	})
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.page(w, r, p, collabErrStatus(err), "collab/issues", "Issues", data)
		return
	}
	data.After = afterParam(r)
	data.More = res.More
	data.LabelOptions = labelOptions(data.Labels, data.LabelFilter)
	data.MilestoneOptions = msOptions(data.Milestones, data.MilestoneParam)
	if len(data.MilestoneOptions) == 0 {
		data.MilestoneUnknown = data.MilestoneParam != "" && data.MilestoneParam != "none"
	} else {
		known := false
		for _, o := range data.MilestoneOptions {
			if o.Selected {
				known = true
			}
		}
		data.MilestoneUnknown = data.MilestoneParam != "" && data.MilestoneParam != "none" && !known
	}
	if data.More && len(res.Issues) > 0 {
		data.OlderHref = pagerHref(r, "after", strconv.Itoa(res.Issues[len(res.Issues)-1].Num))
	}
	for _, c := range res.Issues {
		row := issueListRow{
			Num:          c.Num,
			Title:        c.Title,
			Href:         "/" + rc.Full + "/issues/" + strconv.Itoa(c.Num),
			Closed:       c.State != "open",
			Labels:       labelChips(c.Labels, data.Labels),
			Milestone:    milestoneFor(c.Milestone, data.Milestones, rc.Owner, rc.Name),
			CommentCount: c.CommentCount,
			UpdatedAt:    c.UpdatedAt,
		}
		data.Rows = append(data.Rows, row)
	}
	data.Total = len(data.Rows)
	s.page(w, r, p, status, "collab/issues", "Issues", data)
}

// splitLabels accepts both the repeated form-field shape (labels=a&labels=b)
// and the comma shape (?labels=a,b) the SDK uses.
func splitLabels(q url.Values) []string {
	var out []string
	for _, v := range q["labels"] {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// resources loads the repo label + milestone option sets (F2 gaps render
// empty, never fail the page).
func (s *Server) resources(ctx context.Context, rc collabRepo, data *issuesListData) error {
	var firstErr error
	if collabDeps.Labels != nil {
		ls, err := collabDeps.Labels(ctx, rc.Owner, rc.Name)
		if err != nil {
			firstErr = err
		} else {
			data.Labels = ls
		}
	}
	if collabDeps.Milestones != nil {
		ms, err := collabDeps.Milestones(ctx, rc.Owner, rc.Name)
		if err != nil && firstErr == nil {
			firstErr = err
		} else if err == nil {
			data.Milestones = ms
		}
	}
	return firstErr
}

func (s *Server) loadLabels(ctx context.Context, rc collabRepo) []issues.Label {
	if collabDeps.Labels == nil {
		return nil
	}
	ls, err := collabDeps.Labels(ctx, rc.Owner, rc.Name)
	if err != nil {
		return nil
	}
	return ls
}

func (s *Server) loadMilestones(ctx context.Context, rc collabRepo) []*issues.Milestone {
	if collabDeps.Milestones == nil {
		return nil
	}
	ms, err := collabDeps.Milestones(ctx, rc.Owner, rc.Name)
	if err != nil {
		return nil
	}
	return ms
}

// composerModel is the CommentComposer view model (spec §4.2).
type composerModel struct {
	Action              string
	CloseAction         string
	ReopenAction        string
	Label               string
	Placeholder         string
	SubmitLabel         string
	Locked              bool
	LockReason          string
	ShowClose           bool
	ShowReopen          bool
	ShowCommentAndClose bool
	MentionNames        []string
	MentionID           string
	Body                string
	Error               string
}

// mentionNames dedupes, sorts and caps the @-mention datalist options.
func mentionNames(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

// --- issue thread ------------------------------------------------------------

type issueData struct {
	Repo collabRepo

	Num          int
	Title        string
	StateLabel   string
	StateCls     string
	Closed       bool
	Author       string
	CreatedAt    string
	CommentCount int

	Labels     []labelChip
	Milestones []*issues.Milestone
	Milestone  *milestoneChip
	Assignees  []string

	Timeline   timelineData
	EventsMore bool
	OlderSeq   int

	Lock         lockState
	CanTriage    bool
	AllLabels    []issues.Label
	LabelOptions []labelOption
	CanModify    bool
	Composer     composerModel

	Error  string
	Notice string
}

func (s *Server) issuePage(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	rc, p, gok := s.collabEnv(w, r)
	if !gok {
		return
	}
	s.renderIssue(w, r, rc, p, num, http.StatusOK, "", "")
}

func (s *Server) renderIssue(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, num, status int, errMsg, notice string) {
	afterSeq, _ := strconv.Atoi(r.URL.Query().Get("after_seq"))
	if collabDeps.IssuesGet == nil {
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/issue", "Issue", issueData{
			Repo: rc, Num: num, Error: "issues service not wired",
		})
		return
	}
	view, err := collabDeps.IssuesGet(r.Context(), rc.Owner, rc.Name, num, p, afterSeq, 50)
	if err != nil {
		st := collabErrStatus(err)
		if st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		s.page(w, r, p, st, "collab/issue", "Issue", issueData{Repo: rc, Num: num, Error: err.Error()})
		return
	}
	th := view.Thread
	milestones := s.loadMilestones(r.Context(), rc)
	labels := s.loadLabels(r.Context(), rc)
	lock := issueCommentLock(th)
	data := issueData{
		Repo:         rc,
		Num:          th.Num,
		Title:        th.Title,
		Closed:       th.State != "open",
		Author:       th.Author,
		CreatedAt:    th.CreatedAt,
		CommentCount: th.CommentCount,
		Labels:       labelChips(th.Labels, labels),
		Milestones:   milestones,
		Milestone:    milestoneFor(th.Milestone, milestones, rc.Owner, rc.Name),
		Assignees:    th.Assignees,
		EventsMore:   view.EventsMore,
		Lock:         lock,
		CanTriage:    rc.CanTriage,
		AllLabels:    labels,
		Error:        errMsg,
		Notice:       notice,
	}
	if data.Closed {
		data.StateLabel = closedStateLabel(th.StateReason)
		data.StateCls = "chip chip-closed"
	} else {
		data.StateLabel = "Open"
		data.StateCls = "chip chip-open"
	}
	tl := timelineData{
		Owner:      rc.Owner,
		Repo:       rc.Name,
		Base:       "/" + rc.Full + "/issues/" + strconv.Itoa(th.Num),
		CanReact:   !rc.Anonymous,
		Locked:     lock.Locked,
		LockReason: lock.Reason,
	}
	for _, ev := range view.Events {
		if ev.Type == issues.EventReactionChanged {
			continue // folded into the per-comment summary row
		}
		row := collabEvent{
			Seq:    ev.Seq,
			Actor:  ev.Actor,
			System: issueEventText(ev, milestones),
			At:     ev.At,
		}
		if row.System == "" {
			row.Body = ptrStr(ev.Body)
			row.Reactions, row.Addable = reactionRows(th.ReactionSummary, ev.Seq)
			row.CanAdd = tl.CanReact && !lock.Locked
			row.AddReason = lock.Reason
			row.Locked = lock.Locked
		}
		tl.Events = append(tl.Events, row)
	}
	tl.Events = orderedEvents(tl.Events)
	data.OlderSeq = olderCursor(tl.Events)
	data.Timeline = tl
	data.CanModify = isPullAuthor(th.Author, p) || rc.CanTriage
	data.LabelOptions = labelOptions(labels, th.Labels)
	base := "/" + rc.Full + "/issues/" + strconv.Itoa(th.Num)
	data.Composer = composerModel{
		Action:              base + "/comments",
		CloseAction:         base + "/close",
		ReopenAction:        base + "/reopen",
		Label:               "New comment",
		Placeholder:         "Write a comment… (#N links issues, @user mentions)",
		SubmitLabel:         "Comment",
		Locked:              lock.Locked,
		LockReason:          lock.Reason,
		ShowClose:           !lock.Locked && data.CanModify,
		ShowReopen:          lock.Locked && data.CanModify,
		ShowCommentAndClose: !lock.Locked && data.CanModify,
		MentionNames:        mentionNames(th.Participants),
		MentionID:           "mention-issue-" + strconv.Itoa(th.Num),
	}
	s.page(w, r, p, status, "collab/issue", data.Title, data)
}

// --- issue writes ------------------------------------------------------------

// issueWriteBack re-renders the thread with an inline error (the SPA's
// per-surface error line, never a toast).
func (s *Server) issueWriteBack(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, num int, err error) {
	status := collabErrStatus(err)
	if status < 400 {
		status = http.StatusBadRequest
	}
	if status == http.StatusNotFound || status == http.StatusUnauthorized || status == http.StatusForbidden {
		s.collabError(w, r, status, err.Error())
		return
	}
	// Re-render at the original status so the page body stays usable; the
	// error line carries the service's message.
	s.renderIssue(w, r, rc, p, num, status, err.Error(), "")
}

func (s *Server) issueComment(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Comment on an issue")
	if !ok {
		return
	}
	if collabDeps.IssueComment == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "issues service not wired")
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	ev, err := collabDeps.IssueComment(r.Context(), rc.Owner, rc.Name, num, p, body)
	if err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num)+"#event-"+strconv.Itoa(ev.Seq))
}

func (s *Server) issueClose(w http.ResponseWriter, r *http.Request) {
	s.issueSetState(w, r, "closed")
}

func (s *Server) issueReopen(w http.ResponseWriter, r *http.Request) {
	s.issueSetState(w, r, "open")
}

// issueSetState is close/reopen AND the "comment and close" composite: a
// non-empty body posts the comment first, then the state change (GitHub
// semantics, one form).
func (s *Server) issueSetState(w http.ResponseWriter, r *http.Request, want string) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	action := "Close an issue"
	if want == "open" {
		action = "Reopen an issue"
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, action)
	if !ok {
		return
	}
	if collabDeps.IssuePatch == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "issues service not wired")
		return
	}
	if body := strings.TrimSpace(r.PostFormValue("body")); body != "" && collabDeps.IssueComment != nil {
		if _, err := collabDeps.IssueComment(r.Context(), rc.Owner, rc.Name, num, p, body); err != nil {
			s.issueWriteBack(w, r, rc, p, num, err)
			return
		}
	}
	patch := issues.IssuePatch{State: &want}
	if want == "closed" {
		reason := r.PostFormValue("reason")
		switch reason {
		case issues.ReasonNotPlanned:
			patch.StateReason = &reason
		case issues.ReasonCompleted, "":
			// The UI always shows the reason it sends; the split button's
			// primary segment is "close as completed", so an empty reason
			// from THIS form means completed.
			reason = issues.ReasonCompleted
			patch.StateReason = &reason
		default:
			s.issueWriteBack(w, r, rc, p, num,
				fmt.Errorf("%w: close reason must be completed or not_planned", issues.ErrInvalid))
			return
		}
	}
	if _, err := collabDeps.IssuePatch(r.Context(), rc.Owner, rc.Name, num, p, patch); err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num))
}

// issueLabels applies one label toggle: the form names the label and the op,
// and the handler sends the full next set (the wire contract).
func (s *Server) issueLabels(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleTriage, "Edit issue labels")
	if !ok {
		return
	}
	if collabDeps.IssuePatch == nil || collabDeps.IssuesGet == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "issues service not wired")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("label"))
	if name == "" {
		s.issueWriteBack(w, r, rc, p, num, fmt.Errorf("%w: label is required", issues.ErrInvalid))
		return
	}
	view, err := collabDeps.IssuesGet(r.Context(), rc.Owner, rc.Name, num, p, 0, 1)
	if err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	cur := view.Thread.Labels
	next := make([]string, 0, len(cur)+1)
	found := false
	for _, l := range cur {
		if strings.EqualFold(l, name) {
			found = true
			continue
		}
		next = append(next, l)
	}
	if !found {
		next = append(next, name)
	}
	if _, err := collabDeps.IssuePatch(r.Context(), rc.Owner, rc.Name, num, p, issues.IssuePatch{Labels: &next}); err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num))
}

func (s *Server) issueMilestone(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleTriage, "Set the issue milestone")
	if !ok {
		return
	}
	if collabDeps.IssuePatch == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "issues service not wired")
		return
	}
	var id *string
	if v := strings.TrimSpace(r.PostFormValue("milestone")); v != "" {
		id = &v
	}
	patch := issues.IssuePatch{Milestone: &id}
	if _, err := collabDeps.IssuePatch(r.Context(), rc.Owner, rc.Name, num, p, patch); err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num))
}

func (s *Server) issueReact(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	target, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil || target < 1 {
		s.collabError(w, r, http.StatusNotFound, "unknown event")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "React to a comment")
	if !ok {
		return
	}
	if collabDeps.IssueReact == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "issues service not wired")
		return
	}
	if _, _, _, err := collabDeps.IssueReact(r.Context(), rc.Owner, rc.Name, num, target, p, r.PostFormValue("content")); err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num)+"#event-"+strconv.Itoa(target))
}

func (s *Server) issueUnreact(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown issue")
		return
	}
	target, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil || target < 1 {
		s.collabError(w, r, http.StatusNotFound, "unknown event")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Remove a reaction")
	if !ok {
		return
	}
	if collabDeps.IssueUnreact == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "issues service not wired")
		return
	}
	if _, err := collabDeps.IssueUnreact(r.Context(), rc.Owner, rc.Name, num, target, p, r.PathValue("content")); err != nil {
		s.issueWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/issues/"+strconv.Itoa(num)+"#event-"+strconv.Itoa(target))
}

// --- pulls list --------------------------------------------------------------

type pullListRow struct {
	Num       int
	Title     string
	Href      string
	Chip      stateChip
	BaseRef   string
	HeadRef   string
	Author    string
	UpdatedAt string
}

type pullsListData struct {
	Repo        collabRepo
	State       string
	StateRaw    string
	Base        string
	Head        string
	Rows        []pullListRow
	More        bool
	After       int
	OlderHref   string
	NewPullHref string
	OpenHref    string
	ClosedHref  string
	AllHref     string
	RefreshHref string
	Error       string
	Notice      string
}

func (s *Server) pullList(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	data := pullsListData{Repo: rc, Base: q.Get("base"), Head: q.Get("head")}
	if raw, set := q["state"]; set {
		data.StateRaw = strings.Join(raw, ",")
		switch data.StateRaw {
		case "", "all":
			data.State = "all"
		default:
			data.State = data.StateRaw
		}
	} else {
		data.State = "open"
	}
	if collabDeps.PullsList == nil {
		data.Error = "pulls service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/pulls", "Pull requests", data)
		return
	}
	res, err := collabDeps.PullsList(r.Context(), rc.Owner, rc.Name, p, pulls.ListFilter{
		State: wireState(data.State),
		Base:  data.Base,
		Head:  data.Head,
		After: afterParam(r),
		N:     50,
	})
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.page(w, r, p, collabErrStatus(err), "collab/pulls", "Pull requests", data)
		return
	}
	data.After = afterParam(r)
	data.More = res.More
	base := "/" + rc.Full + "/pulls"
	data.OpenHref = base + "?state=open"
	data.ClosedHref = base + "?state=closed"
	data.AllHref = base + "?state=all"
	data.RefreshHref = r.URL.RequestURI()
	newPull := base + "/new"
	if data.Base != "" || data.Head != "" {
		q := url.Values{}
		if data.Base != "" {
			q.Set("base", data.Base)
		}
		if data.Head != "" {
			q.Set("head", data.Head)
		}
		newPull += "?" + q.Encode()
	}
	data.NewPullHref = s.collabWriteHref(p, newPull, "Open a pull request")
	if data.More && len(res.Pulls) > 0 {
		data.OlderHref = pagerHref(r, "after", strconv.Itoa(res.Pulls[len(res.Pulls)-1].Num))
	}
	for _, row := range res.Pulls {
		data.Rows = append(data.Rows, pullListRow{
			Num:       row.Num,
			Title:     row.Title,
			Href:      "/" + rc.Full + "/pull/" + strconv.Itoa(row.Num),
			Chip:      pullListChip(row),
			BaseRef:   row.BaseRef,
			HeadRef:   row.HeadRef,
			Author:    row.Author,
			UpdatedAt: row.UpdatedAt,
		})
	}
	s.page(w, r, p, http.StatusOK, "collab/pulls", "Pull requests", data)
}

// --- pull conversation -------------------------------------------------------

type threadCommentRow struct {
	By   string
	At   string
	Body string
}

type threadCard struct {
	TID             string
	Path            string
	Side            string
	Start           int
	LineCount       int
	Label           string
	Resolved        bool
	ResolvedBy      string
	CreatedBy       string
	CommentCount    int
	UpdatedAt       string
	Comments        []threadCommentRow
	CommentsHref    string
	CanResolve      bool
	ReplyAction     string
	ResolveAction   string
	UnresolveAction string
}

func threadCards(headers []*review.ThreadHeader, base string, canResolve bool) []threadCard {
	out := make([]threadCard, 0, len(headers))
	for _, h := range headers {
		start, lines := h.Anchor.NewStart, h.Anchor.NewLines
		if h.Anchor.Side == review.SideOld {
			start, lines = h.Anchor.OldStart, h.Anchor.OldLines
		}
		label := h.Anchor.Path + ":" + strconv.Itoa(start)
		if lines > 1 {
			label = h.Anchor.Path + ":" + strconv.Itoa(start) + "-" + strconv.Itoa(start+lines-1)
		}
		card := threadCard{
			TID:             h.TID,
			Path:            h.Anchor.Path,
			Side:            h.Anchor.Side,
			Start:           start,
			LineCount:       lines,
			Label:           label,
			Resolved:        h.Resolved,
			ResolvedBy:      h.ResolvedBy,
			CreatedBy:       h.CreatedBy,
			CommentCount:    h.CommentCount,
			UpdatedAt:       h.UpdatedAt,
			CanResolve:      canResolve,
			CommentsHref:    base + "/threads/" + h.TID,
			ReplyAction:     base + "/threads/" + h.TID + "/comments",
			ResolveAction:   base + "/threads/" + h.TID + "/resolve",
			UnresolveAction: base + "/threads/" + h.TID + "/unresolve",
		}
		out = append(out, card)
	}
	return out
}

// diffRow is one rendered line of the conversation/files diff.
type diffRow struct {
	ID        string
	Kind      string // ctx|add|del
	OldNo     int
	NewNo     int
	Text      string
	Sign      string
	LineCls   string
	Side      string // for the files page stage link (new|old)
	LineNo    int
	StageHref string
	Staged    bool
	Threads   []threadCard
}

type diffHunk struct {
	Header string
	Rows   []diffRow
}

type diffFile struct {
	Path    string
	Status  string
	Binary  bool
	Added   int
	Deleted int
	Hunks   []diffHunk
	Drifted []threadCard // anchors that no longer locate in the file
}

// convDiff is the rendered diff plus honest counts. Full/Num/Sel carry the
// page coordinates the files-tab staging form needs.
type convDiff struct {
	Files   []diffFile
	Added   int
	Deleted int
	Drifted []threadCard
	Full    string
	Num     int
	Sel     *stageSelection
}

func lineClass(kind string) string {
	switch kind {
	case "add":
		return "diff-add"
	case "del":
		return "diff-del"
	}
	return ""
}

func diffSlug(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// buildDiff turns the parsed patch into render rows and places thread cards
// under their anchored line. Anchors that no longer locate stay at the file
// end, marked anchored — never relocated (spec §3.4 Files). stageBase != ""
// makes every row's sign cell a staging link (the files page, no-JS path).
func buildDiff(files []DiffFile, cards []threadCard, stage *stageSelection, stageBase string) convDiff {
	byLine := map[string][]threadCard{}
	for _, c := range cards {
		byLine[threadKey(c.Path, c.Side, c.Start)] = append(byLine[threadKey(c.Path, c.Side, c.Start)], c)
	}
	out := convDiff{}
	used := map[string]bool{}
	var drift []threadCard
	for _, f := range files {
		df := diffFile{Path: f.Path, Status: f.Status, Binary: f.Binary, Added: f.Added, Deleted: f.Deleted}
		out.Added += f.Added
		out.Deleted += f.Deleted
		for hi, h := range f.Hunks {
			dh := diffHunk{Header: h.Header}
			for ri, l := range h.Lines {
				sign := " "
				switch l.Kind {
				case "add":
					sign = "+"
				case "del":
					sign = "-"
				}
				row := diffRow{
					ID:      "d-" + diffSlug(f.Path) + "-" + strconv.Itoa(hi) + "-" + strconv.Itoa(ri),
					Kind:    l.Kind,
					OldNo:   l.OldNo,
					NewNo:   l.NewNo,
					Text:    l.Text,
					Sign:    sign,
					LineCls: lineClass(l.Kind),
				}
				if l.NewNo > 0 {
					row.Side, row.LineNo = "new", l.NewNo
					if stage != nil && stage.Path == f.Path && stage.Side == "new" && stage.Line == l.NewNo {
						row.Staged = true
					}
				} else if l.OldNo > 0 {
					row.Side, row.LineNo = "old", l.OldNo
					if stage != nil && stage.Path == f.Path && stage.Side == "old" && stage.Line == l.OldNo {
						row.Staged = true
					}
				}
				if row.Side != "" {
					if stageBase != "" {
						q := url.Values{}
						q.Set("path", f.Path)
						q.Set("side", row.Side)
						q.Set("line", strconv.Itoa(row.LineNo))
						row.StageHref = stageBase + "?" + q.Encode()
					}
					k := threadKey(f.Path, strings.ToUpper(row.Side), row.LineNo)
					if cs := byLine[k]; len(cs) > 0 {
						row.Threads = cs
						used[k] = true
					}
				}
				dh.Rows = append(dh.Rows, row)
			}
			df.Hunks = append(df.Hunks, dh)
		}
		out.Files = append(out.Files, df)
	}
	for k, cs := range byLine {
		if !used[k] {
			drift = append(drift, cs...)
		}
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Label < drift[j].Label })
	out.Drifted = drift
	return out
}

func threadKey(path, side string, line int) string {
	return path + "\x00" + side + "\x00" + strconv.Itoa(line)
}

// stageSelection is one server-staged diff line (files page, no-JS path).
type stageSelection struct {
	Path string
	Side string
	Line int
	Href string
}

// anchorFor builds the §4 anchor for a line selection from the parsed diff:
// shape convention NEW zeroes old_*, OLD zeroes new_* (review-anchor.js),
// and context_sha is the review.DriftHash over the adjacent context lines.
func anchorFor(files []DiffFile, path, side string, start, end int, head string) (review.Anchor, bool) {
	if head == "" || (side != "new" && side != "old") || start < 1 || end < start {
		return review.Anchor{}, false
	}
	for _, f := range files {
		if f.Path != path {
			continue
		}
		for _, h := range f.Hunks {
			var idxs []int
			for i, l := range h.Lines {
				no := l.NewNo
				if side == "old" {
					no = l.OldNo
				}
				if no >= start && no <= end {
					idxs = append(idxs, i)
				}
			}
			if len(idxs) == 0 {
				continue
			}
			lo, hi := idxs[0], idxs[len(idxs)-1]
			var before, after []string
			for i := lo - 1; i >= 0 && h.Lines[i].Kind == "ctx"; i-- {
				before = append([]string{h.Lines[i].Text}, before...)
			}
			for i := hi + 1; i < len(h.Lines) && h.Lines[i].Kind == "ctx"; i++ {
				after = append(after, h.Lines[i].Text)
			}
			a := review.Anchor{
				Path:       path,
				CommitSHA:  head,
				ContextSHA: review.DriftHash(path, before, after),
			}
			span := end - start + 1
			if side == "new" {
				a.Side, a.NewStart, a.NewLines = review.SideNew, start, span
			} else {
				a.Side, a.OldStart, a.OldLines = review.SideOld, start, span
			}
			return a, true
		}
	}
	return review.Anchor{}, false
}

type reviewRow struct {
	Seq        int
	By         string
	At         string
	Chip       stateChip
	Body       string
	CommitSHA  string
	Stale      bool
	Dismissed  bool
	Reason     string
	DismissSeq int
	CanDismiss bool
	Action     string
}

type pullData struct {
	Repo collabRepo

	Num       int
	Title     string
	Badge     stateChip
	Author    string
	UpdatedAt string
	Merged    bool
	Terminal  bool
	Draft     bool

	Body        string
	OpenedActor string
	OpenedAt    string

	Timeline   timelineData
	EventsMore bool
	OlderSeq   int
	Lock       lockState
	Composer   composerModel

	CanModify  bool
	ShowClose  bool
	ShowReopen bool
	ShowReady  bool
	ShowDraft  bool
	CanWrite   bool
	CanTriage  bool
	CanMaint   bool

	Reviews           []reviewRow
	ReviewsMore       bool
	ReviewDec         string
	Approvals         int
	ThreadsUnresolved int
	ThreadsTotal      int

	Requests        []string
	CanEditRequests bool

	Threads     []threadCard
	ThreadsMore bool
	Index       []threadCard

	Checks            *checks.CombinedView
	CheckPill         checkPillView
	ZeroChecks        bool
	ZeroChecksTitle   string
	RequiredChecks    []string
	RequiredKnown     bool
	CheckBlockers     []string
	CheckBlockersText string
	RequiredText      string
	ConflictsText     string
	RequiresReviews   *bool

	Mergeable    *pulls.MergeableDoc
	MergeState   mergeStateView
	Mergeability mergeabilityText

	Diff       convDiff
	DiffError  string
	FilesCount int

	HeadSHA   string
	BaseSHA   string
	BaseRef   string
	HeadRef   string
	HeadRefOK bool
	ForkRepo  string

	Error  string
	Notice string
}

func (s *Server) pullPage(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, gok := s.collabEnv(w, r)
	if !gok {
		return
	}
	s.renderPull(w, r, rc, p, num, http.StatusOK, "", "")
}

func (s *Server) renderPull(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, num, status int, errMsg, notice string) {
	ctx := r.Context()
	if collabDeps.PullGet == nil {
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/pull", "Pull request", pullData{
			Repo: rc, Num: num, Error: "pulls service not wired",
		})
		return
	}
	afterSeq, _ := strconv.Atoi(r.URL.Query().Get("after_seq"))
	view, err := collabDeps.PullGet(ctx, rc.Owner, rc.Name, num, p, afterSeq, 50)
	if err != nil {
		st := collabErrStatus(err)
		if st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		s.page(w, r, p, st, "collab/pull", "Pull request", pullData{Repo: rc, Num: num, Error: err.Error()})
		return
	}
	th, pr := view.Thread, view.PR
	head := view.HeadLive
	if head == "" && pr != nil {
		head = pr.Head.SHA
	}
	base := "/" + rc.Full + "/pull/" + strconv.Itoa(num)
	lock := pullCommentLock(th, pr)
	data := pullData{
		Repo:       rc,
		Num:        th.Num,
		Title:      th.Title,
		Badge:      pullBadgeView(th, pr),
		Author:     th.Author,
		UpdatedAt:  th.UpdatedAt,
		Merged:     pr != nil && pr.Merged,
		Draft:      pr != nil && pr.Draft,
		Lock:       lock,
		EventsMore: view.EventsMore,
		CanWrite:   rc.CanWrite,
		CanTriage:  rc.CanTriage,
		CanMaint:   rc.CanMaint,
		HeadSHA:    head,
		HeadRefOK:  view.HeadRefOk,
		Error:      errMsg,
		Notice:     notice,
	}
	data.Terminal = data.Merged || th.State == "closed"
	if pr != nil {
		data.Body = pr.Body
		data.BaseRef, data.BaseSHA, data.HeadRef = pr.Base.Ref, pr.Base.SHA, pr.Head.Ref
		if pr.Fork != nil {
			data.ForkRepo = pr.Fork.Repo
		}
		if view.BaseLive != "" {
			data.BaseSHA = view.BaseLive
		}
	}
	data.CanModify = canModifyPull(th.Author, p, rc.Role)
	closeVis := pullCloseVisibility(th, pr, p, rc.Role)
	data.ShowClose, data.ShowReopen = closeVis[0], closeVis[1]
	draftVis := pullDraftVisibility(th, pr, p, rc.Role)
	data.ShowReady, data.ShowDraft = draftVis[0], draftVis[1]

	// Timeline (opened/commented are bodies for issues; a PR's opened event
	// is a system row because the description renders once, above).
	tl := timelineData{Owner: rc.Owner, Repo: rc.Name, Base: base, CanReact: !rc.Anonymous, Locked: lock.Locked, LockReason: lock.Reason}
	for _, ev := range view.Events {
		row := collabEvent{Seq: ev.Seq, Actor: ev.Actor, System: pullEventText(ev), At: ev.At}
		if row.System == "" {
			row.Body = ptrStr(ev.Body)
		}
		if ev.Type == pulls.EventOpened {
			data.OpenedActor, data.OpenedAt = ev.Actor, ev.At
		}
		tl.Events = append(tl.Events, row)
	}
	tl.Events = orderedEvents(tl.Events)
	data.OlderSeq = olderCursor(tl.Events)
	data.Timeline = tl
	data.Composer = composerModel{
		Action:              base + "/comments",
		CloseAction:         base + "/close",
		ReopenAction:        base + "/reopen",
		Label:               "New comment",
		Placeholder:         "Write a comment… (#N links issues, @user mentions)",
		SubmitLabel:         "Comment",
		Locked:              lock.Locked,
		LockReason:          lock.Reason,
		ShowClose:           data.ShowClose,
		ShowReopen:          data.ShowReopen,
		ShowCommentAndClose: data.ShowClose,
		MentionNames:        mentionNames(th.Participants),
		MentionID:           "mention-pr-" + strconv.Itoa(num),
	}

	// Reviews
	if collabDeps.ReviewsList != nil {
		if revs, rerr := collabDeps.ReviewsList(ctx, rc.Owner, rc.Name, num, p, 0, 50); rerr == nil {
			data.ReviewsMore = revs.More
			for _, rv := range revs.Reviews {
				row := reviewRow{Seq: rv.Seq, By: rv.By, At: rv.At, Body: rv.Body, CommitSHA: rv.CommitSHA}
				if rv.Kind == review.KindReviewDismissed {
					row.Dismissed = true
					row.Reason = rv.Reason
					if rv.Dismisses != nil {
						row.DismissSeq = *rv.Dismisses
					}
					row.Chip = stateChip{reviewVerdictLabel(review.StateDismissed), "chip chip-neutral"}
				} else {
					row.Chip = reviewVerdictChip(rv.State)
					row.Stale = rv.CommitSHA != "" && head != "" && rv.CommitSHA != head
					row.CanDismiss = rc.CanMaint
					row.Action = base + "/reviews/" + strconv.Itoa(rv.Seq) + "/dismiss"
				}
				data.Reviews = append(data.Reviews, row)
			}
		}
	}
	// Review rollup (thread.review_summary is the denormalized cache)
	if len(th.ReviewSummary) > 0 {
		var sum review.ReviewSummary
		if json.Unmarshal(th.ReviewSummary, &sum) == nil {
			data.ReviewDec = sum.Decision
			data.Approvals = sum.Approvals
			data.ThreadsUnresolved = sum.ThreadsUnresolved
			data.ThreadsTotal = sum.ThreadsTotal
		}
	}
	// Requested reviewers
	if collabDeps.RequestsGet != nil {
		if reqs, rerr := collabDeps.RequestsGet(ctx, rc.Owner, rc.Name, num, p); rerr == nil {
			for _, rv := range reqs.Reviewers {
				data.Requests = append(data.Requests, rv.Principal)
			}
		}
	}
	data.CanEditRequests = (data.CanModify || rc.CanTriage) && !lock.Locked

	// Threads
	if collabDeps.ThreadsList != nil {
		if threads, terr := collabDeps.ThreadsList(ctx, rc.Owner, rc.Name, num, p, nil, "", 100); terr == nil {
			data.ThreadsMore = threads.More
			data.Threads = threadCards(threads.Threads, base, !rc.Anonymous)
			data.Index = sortedThreadIndex(data.Threads)
		}
	}

	// Head checks + policy advisory
	if collabDeps.ChecksCombined != nil && head != "" {
		if cv, cerr := collabDeps.ChecksCombined(ctx, rc.Owner, rc.Name, head, p); cerr == nil {
			data.Checks = cv
			data.ZeroChecks = len(cv.Statuses) == 0
			data.CheckPill = checkPillFor(rc.Owner, rc.Name, cv.SHA, cv.State, data.ZeroChecks)
		}
	}
	var pol *policy.Document
	if collabDeps.PolicyOf != nil {
		if doc, perr := collabDeps.PolicyOf(ctx, git.RepoId{Owner: rc.Owner, Name: rc.Name}); perr == nil {
			pol = doc
		}
	}
	if pol != nil && pr != nil {
		data.RequiredChecks, data.RequiredKnown = requiredChecks(pol, pr.Base.Ref)
		applies := requiredReviewsApplies(pol, pr.Base.Ref)
		data.RequiresReviews = &applies
	}
	var statuses []*checks.StatusDoc
	if data.Checks != nil {
		statuses = data.Checks.Statuses
	}
	data.ZeroChecksTitle = zeroChecksTitle(data.RequiredChecks, data.RequiredKnown)
	data.CheckBlockers = requiredCheckBlockers(data.RequiredChecks, statuses)
	data.CheckBlockersText = strings.Join(data.CheckBlockers, "; ")
	data.RequiredText = strings.Join(data.RequiredChecks, ", ")
	if view.Mergeable != nil {
		data.ConflictsText = strings.Join(view.Mergeable.Conflicts, ", ")
	}

	// Merge machine
	data.Mergeable = view.Mergeable
	data.MergeState = pullMergeState(pr, view.Mergeable, data.CheckBlockers, data.ReviewDec, data.RequiresReviews, rc.Role, mergeTaskFor(rc.Owner, rc.Name))
	data.MergeState.Detail = terminalMergeDetail(pr, view.Events)
	data.Mergeability = mergeabilityDisplay(data.MergeState.State, view.Mergeable, data.CheckBlockers, data.ReviewDec, data.Draft, data.ZeroChecks && len(data.RequiredChecks) == 0)

	// Diff + file-level thread placement
	if collabDeps.PullDiff != nil {
		raw, derr := collabDeps.PullDiff(ctx, rc.Owner, rc.Name, num, p)
		if derr != nil {
			data.DiffError = derr.Error()
		} else {
			data.Diff = buildDiff(ParseUnifiedDiff(raw), data.Threads, nil, "")
			data.Diff.Full, data.Diff.Num = rc.Full, num
		}
	}
	data.FilesCount = len(data.Diff.Files)

	s.page(w, r, p, status, "collab/pull", data.Title, data)
}

func sortedThreadIndex(cards []threadCard) []threadCard {
	out := append([]threadCard{}, cards...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Resolved != out[j].Resolved {
			return !out[i].Resolved
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// checkPillView is the shared checks pill model (Checks.jsx CheckPill): a
// link to /checks/{sha} with a state dot, neutral when zero contexts
// reported.
type checkPillView struct {
	Href  string
	Dot   string
	Label string
	Title string
}

func checkPillFor(owner, repo, sha, state string, zero bool) checkPillView {
	label := state
	title := "checks: " + state
	if zero {
		label = "no checks"
		title = "checks: no contexts reported"
	}
	return checkPillView{
		Href:  "/" + owner + "/" + repo + "/checks/" + sha,
		Dot:   checkStateDot(state),
		Label: label,
		Title: title,
	}
}

// mergeTaskFor reads the current merge task for the repo (nil when none).
func mergeTaskFor(owner, repo string) *pulls.TaskRecord {
	if collabDeps.MergeTask == nil {
		return nil
	}
	return collabDeps.MergeTask(owner, repo)
}

// pullCloseVisibility is the close/reopen control rule: unmerged only,
// author-or-triage, driven by the live thread state.
func pullCloseVisibility(th *pulls.Thread, pr *pulls.PRDoc, p auth.Principal, role identity.Role) [2]bool {
	if pr != nil && pr.Merged {
		return [2]bool{}
	}
	if th == nil || !canModifyPull(th.Author, p, role) {
		return [2]bool{}
	}
	if th.State == "open" {
		return [2]bool{true, false}
	}
	return [2]bool{false, true}
}

// pullDraftVisibility is the mark-ready / convert-to-draft toggle rule.
func pullDraftVisibility(th *pulls.Thread, pr *pulls.PRDoc, p auth.Principal, role identity.Role) [2]bool {
	if pr != nil && pr.Merged {
		return [2]bool{}
	}
	if th == nil || !canModifyPull(th.Author, p, role) {
		return [2]bool{}
	}
	if pr != nil && pr.Draft {
		return [2]bool{true, false}
	}
	return [2]bool{false, true}
}

// --- pull writes -------------------------------------------------------------

func (s *Server) pullWriteBack(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, num int, err error) {
	status := collabErrStatus(err)
	if status < 400 {
		status = http.StatusBadRequest
	}
	if status == http.StatusNotFound || status == http.StatusUnauthorized || status == http.StatusForbidden {
		s.collabError(w, r, status, err.Error())
		return
	}
	s.renderPull(w, r, rc, p, num, status, err.Error(), "")
}

// safeNext accepts only a same-site absolute path (the login-required
// sanitizeNext rule) — open redirects are refused.
func safeNext(v, fallback string) string {
	if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") && !strings.Contains(v, "\\") {
		return v
	}
	return fallback
}

func (s *Server) pullComment(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Comment on a pull request")
	if !ok {
		return
	}
	if collabDeps.PullComment == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "pulls service not wired")
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" {
		s.pullWriteBack(w, r, rc, p, num, fmt.Errorf("%w: comment body must not be empty", pulls.ErrInvalid))
		return
	}
	ev, err := collabDeps.PullComment(r.Context(), rc.Owner, rc.Name, num, p, body)
	if err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num)+"#event-"+strconv.Itoa(ev.Seq))
}

// pullSetState closes/reopens (author or triage; the service enforces it).
func (s *Server) pullSetState(w http.ResponseWriter, r *http.Request, want string) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Change pull request state")
	if !ok {
		return
	}
	if collabDeps.PullUpdate == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "pulls service not wired")
		return
	}
	if body := strings.TrimSpace(r.PostFormValue("body")); body != "" && collabDeps.PullComment != nil {
		if _, err := collabDeps.PullComment(r.Context(), rc.Owner, rc.Name, num, p, body); err != nil {
			s.pullWriteBack(w, r, rc, p, num, err)
			return
		}
	}
	if _, _, err := collabDeps.PullUpdate(r.Context(), rc.Owner, rc.Name, num, p, pulls.PRPatch{State: &want}); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num))
}

func (s *Server) pullClose(w http.ResponseWriter, r *http.Request)  { s.pullSetState(w, r, "closed") }
func (s *Server) pullReopen(w http.ResponseWriter, r *http.Request) { s.pullSetState(w, r, "open") }

// pullSetDraft flips the draft flag (mark ready / convert to draft).
func (s *Server) pullSetDraft(w http.ResponseWriter, r *http.Request, draft bool) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Change pull request draft state")
	if !ok {
		return
	}
	if collabDeps.PullUpdate == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "pulls service not wired")
		return
	}
	if _, _, err := collabDeps.PullUpdate(r.Context(), rc.Owner, rc.Name, num, p, pulls.PRPatch{Draft: &draft}); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num))
}

func (s *Server) pullReady(w http.ResponseWriter, r *http.Request) { s.pullSetDraft(w, r, false) }
func (s *Server) pullDraft(w http.ResponseWriter, r *http.Request) { s.pullSetDraft(w, r, true) }

// pullReview submits one review verdict. Staged line comments are an island
// concern (spec §7 Q4) — the server accepts the verdict and body only.
func (s *Server) pullReview(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Submit a review")
	if !ok {
		return
	}
	if collabDeps.ReviewSubmit == nil || collabDeps.PullGet == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
		return
	}
	state := r.PostFormValue("verdict")
	sha := strings.TrimSpace(r.PostFormValue("commit_sha"))
	if sha == "" {
		view, err := collabDeps.PullGet(r.Context(), rc.Owner, rc.Name, num, p, 0, 1)
		if err != nil {
			s.pullWriteBack(w, r, rc, p, num, err)
			return
		}
		sha = view.HeadLive
		if sha == "" && view.PR != nil {
			sha = view.PR.Head.SHA
		}
	}
	if _, _, _, err := collabDeps.ReviewSubmit(r.Context(), rc.Owner, rc.Name, num, p, review.SubmitInput{
		State:     state,
		Body:      r.PostFormValue("body"),
		CommitSHA: sha,
	}); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num)+"#reviews")
}

func (s *Server) pullDismiss(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	seq, serr := strconv.Atoi(r.PathValue("seq"))
	if serr != nil || seq < 1 {
		s.collabError(w, r, http.StatusNotFound, "unknown review")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleMaintain, "Dismiss a review")
	if !ok {
		return
	}
	if collabDeps.ReviewDismiss == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
		return
	}
	if _, _, err := collabDeps.ReviewDismiss(r.Context(), rc.Owner, rc.Name, num, seq, p, strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num)+"#reviews")
}

// pullReviewers adds or removes review requests (author or triage; the
// service answers 422 unless the PR is open and unmerged).
func (s *Server) pullReviewers(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Request reviewers")
	if !ok {
		return
	}
	if collabDeps.RequestsAdd == nil || collabDeps.RequestsRemove == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
		return
	}
	var names []string
	for _, part := range strings.Split(r.PostFormValue("reviewers"), ",") {
		if n := strings.TrimSpace(part); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		s.pullWriteBack(w, r, rc, p, num, fmt.Errorf("%w: name at least one reviewer", review.ErrInvalid))
		return
	}
	var err error
	if strings.HasSuffix(r.URL.Path, "/reviewers/remove") {
		_, err = collabDeps.RequestsRemove(r.Context(), rc.Owner, rc.Name, num, p, names)
	} else {
		_, err = collabDeps.RequestsAdd(r.Context(), rc.Owner, rc.Name, num, p, names)
	}
	if err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num)+"#reviewers")
}

func (s *Server) pullReviewersRemove(w http.ResponseWriter, r *http.Request) { s.pullReviewers(w, r) }

// pullThreadOpen stages one line-anchored thread from a (path, side, start,
// end) selection: the anchor (including the §4 context hash) is built
// SERVER-side from the live diff — the browser never computes the hash.
func (s *Server) pullThreadOpen(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Start a review thread")
	if !ok {
		return
	}
	fallback := "/" + rc.Full + "/pull/" + strconv.Itoa(num) + "/files"
	next := safeNext(r.PostFormValue("next"), fallback)
	if collabDeps.ThreadOpen == nil || collabDeps.PullDiff == nil || collabDeps.PullGet == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
		return
	}
	path := r.PostFormValue("path")
	side := r.PostFormValue("side")
	start, _ := strconv.Atoi(r.PostFormValue("start"))
	end, _ := strconv.Atoi(r.PostFormValue("end"))
	if end < start {
		end = start
	}
	view, err := collabDeps.PullGet(r.Context(), rc.Owner, rc.Name, num, p, 0, 1)
	if err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	head := view.HeadLive
	if head == "" && view.PR != nil {
		head = view.PR.Head.SHA
	}
	raw, err := collabDeps.PullDiff(r.Context(), rc.Owner, rc.Name, num, p)
	if err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	anchor, okAnchor := anchorFor(ParseUnifiedDiff(raw), path, side, start, end, head)
	if !okAnchor {
		// Stale selection: re-render the files page with the honest error.
		s.renderFiles(w, r, rc, p, num, http.StatusBadRequest, "that selection names no line in this diff anymore — pick again.", "")
		return
	}
	hdr, err := collabDeps.ThreadOpen(r.Context(), rc.Owner, rc.Name, num, p, anchor, strings.TrimSpace(r.PostFormValue("body")))
	if err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, next+"#thread-"+hdr.TID)
}

func (s *Server) pullThreadComment(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	tid := r.PathValue("tid")
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Reply to a review thread")
	if !ok {
		return
	}
	if collabDeps.ThreadComment == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" {
		s.pullWriteBack(w, r, rc, p, num, fmt.Errorf("%w: comment body must not be empty", review.ErrInvalid))
		return
	}
	if _, err := collabDeps.ThreadComment(r.Context(), rc.Owner, rc.Name, num, tid, p, body); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	next := safeNext(r.PostFormValue("next"), "/"+rc.Full+"/pull/"+strconv.Itoa(num))
	redirectPage(w, r, next+"#thread-"+tid)
}

func (s *Server) pullThreadResolve(w http.ResponseWriter, r *http.Request) {
	s.pullThreadSetResolved(w, r, true)
}
func (s *Server) pullThreadUnresolve(w http.ResponseWriter, r *http.Request) {
	s.pullThreadSetResolved(w, r, false)
}

func (s *Server) pullThreadSetResolved(w http.ResponseWriter, r *http.Request, resolved bool) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	tid := r.PathValue("tid")
	rc, p, ok := s.collabWrite(w, r, identity.RoleRead, "Resolve a review thread")
	if !ok {
		return
	}
	var err error
	if resolved {
		if collabDeps.ThreadResolve == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
			return
		}
		_, err = collabDeps.ThreadResolve(r.Context(), rc.Owner, rc.Name, num, tid, p)
	} else {
		if collabDeps.ThreadUnresolve == nil {
			s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
			return
		}
		_, err = collabDeps.ThreadUnresolve(r.Context(), rc.Owner, rc.Name, num, tid, p)
	}
	if err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	next := safeNext(r.PostFormValue("next"), "/"+rc.Full+"/pull/"+strconv.Itoa(num))
	redirectPage(w, r, next+"#thread-"+tid)
}

// pullThreadComments is the lazy per-card comments fragment (htmx target).
func (s *Server) pullThreadComments(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if collabDeps.ThreadGet == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "review service not wired")
		return
	}
	tid := r.PathValue("tid")
	view, err := collabDeps.ThreadGet(r.Context(), rc.Owner, rc.Name, num, tid, p, 0, 100)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	var comments []threadCommentRow
	for _, c := range view.Comments {
		comments = append(comments, threadCommentRow{By: c.By, At: c.At, Body: c.Body})
	}
	s.collabFragment(w, r, "collab/pull", "collab_thread_comments", comments)
}

func (s *Server) pullMerge(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleMaintain, "Merge a pull request")
	if !ok {
		return
	}
	if collabDeps.MergeStart == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "pulls service not wired")
		return
	}
	strategy := r.PostFormValue("strategy")
	switch strategy {
	case pulls.StrategyMerge, pulls.StrategySquash, pulls.StrategyRebase:
	default:
		s.pullWriteBack(w, r, rc, p, num, fmt.Errorf("%w: strategy must be merge|squash|rebase", pulls.ErrInvalid))
		return
	}
	if _, err := collabDeps.MergeStart(r.Context(), rc.Owner, rc.Name, num, p, pulls.MergeInput{Strategy: strategy}, ""); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num))
}

// pullMergeTask is the merge-task progress fragment (htmx polls every 2 s
// while the task runs; no JS fallback needed — the page shows the same
// block on reload).
func (s *Server) pullMergeTask(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, _, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	s.collabFragment(w, r, "collab/pull", "collab_merge_task", mergeTaskView{
		Task: mergeTaskFor(rc.Owner, rc.Name),
		Num:  num,
		Full: rc.Full,
	})
}

type mergeTaskView struct {
	Task *pulls.TaskRecord
	Num  int
	Full string
}

func (s *Server) pullUpdateBranch(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, ok := s.collabWrite(w, r, identity.RoleWrite, "Update the pull request branch")
	if !ok {
		return
	}
	if collabDeps.UpdateBranch == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "pulls service not wired")
		return
	}
	if _, err := collabDeps.UpdateBranch(r.Context(), rc.Owner, rc.Name, num, p, r.PostFormValue("expected_head_sha")); err != nil {
		s.pullWriteBack(w, r, rc, p, num, err)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/pull/"+strconv.Itoa(num))
}

// --- pull commits ------------------------------------------------------------

type pullCommitsData struct {
	Repo      collabRepo
	Num       int
	Title     string
	Rows      []pulls.CommitEntry
	More      bool
	OlderHref string
	Error     string
	Notice    string
}

func (s *Server) pullCommits(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, gok := s.collabEnv(w, r)
	if !gok {
		return
	}
	data := pullCommitsData{Repo: rc, Num: num}
	if collabDeps.PullGet == nil || collabDeps.PullCommits == nil {
		data.Error = "pulls service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/pull_commits", "Commits", data)
		return
	}
	view, err := collabDeps.PullGet(r.Context(), rc.Owner, rc.Name, num, p, 0, 1)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	data.Title = view.Thread.Title
	skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
	if skip < 0 {
		skip = 0
	}
	commits, more, err := collabDeps.PullCommits(r.Context(), rc.Owner, rc.Name, num, p, skip, 100)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	data.Rows = commits
	data.More = more
	if more {
		data.OlderHref = pagerHref(r, "skip", strconv.Itoa(skip+len(commits)))
	}
	s.page(w, r, p, http.StatusOK, "collab/pull_commits", "Commits on #"+strconv.Itoa(num), data)
}

// --- pull files --------------------------------------------------------------

type filesData struct {
	Repo  collabRepo
	Num   int
	Title string
	Lock  lockState

	Diff       convDiff
	DiffError  string
	FilesCount int

	Sel    *stageSelection
	SelErr string
	Notice string
	Error  string
}

func (s *Server) pullFiles(w http.ResponseWriter, r *http.Request) {
	num, ok := issueNum(r.PathValue("num"))
	if !ok {
		s.collabError(w, r, http.StatusNotFound, "unknown pull request")
		return
	}
	rc, p, gok := s.collabEnv(w, r)
	if !gok {
		return
	}
	s.renderFiles(w, r, rc, p, num, http.StatusOK, "", "")
}

func (s *Server) renderFiles(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, num, status int, errMsg, notice string) {
	if collabDeps.PullGet == nil || collabDeps.PullDiff == nil {
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/pull_files", "Files", filesData{
			Repo: rc, Num: num, Error: "pulls service not wired",
		})
		return
	}
	view, err := collabDeps.PullGet(r.Context(), rc.Owner, rc.Name, num, p, 0, 1)
	if err != nil {
		st := collabErrStatus(err)
		if st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		s.page(w, r, p, st, "collab/pull_files", "Files", filesData{Repo: rc, Num: num, Error: err.Error()})
		return
	}
	data := filesData{Repo: rc, Num: num, Title: view.Thread.Title, Lock: pullCommentLock(view.Thread, view.PR), Error: errMsg, Notice: notice}
	raw, derr := collabDeps.PullDiff(r.Context(), rc.Owner, rc.Name, num, p)
	if derr != nil {
		data.DiffError = derr.Error()
		s.page(w, r, p, http.StatusOK, "collab/pull_files", "Files", data)
		return
	}
	parsed := ParseUnifiedDiff(raw)
	// The ?path=&side=&line= selection is the server-rendered staging path
	// (islands enhance it to ranges; the wire shape is identical).
	sel := &stageSelection{Path: r.URL.Query().Get("path"), Side: r.URL.Query().Get("side")}
	sel.Line, _ = strconv.Atoi(r.URL.Query().Get("line"))
	if sel.Path != "" {
		if data.Lock.Locked || rc.Anonymous {
			data.SelErr = "commenting is locked on this pull request"
		} else if sel.Side != "new" && sel.Side != "old" {
			data.SelErr = "that selection names no line in this diff anymore — pick again."
		} else {
			found := false
			for _, f := range parsed {
				if f.Path == sel.Path {
					for _, h := range f.Hunks {
						for _, l := range h.Lines {
							no := l.NewNo
							if sel.Side == "old" {
								no = l.OldNo
							}
							if no == sel.Line {
								found = true
							}
						}
					}
				}
			}
			if found {
				data.Sel = sel
			} else {
				data.SelErr = "that selection names no line in this diff anymore — pick again."
			}
		}
	}
	base := "/" + rc.Full + "/pull/" + strconv.Itoa(num) + "/files"
	data.Diff = buildDiff(parsed, nil, data.Sel, base)
	data.Diff.Full, data.Diff.Num, data.Diff.Sel = rc.Full, num, data.Sel
	data.FilesCount = len(data.Diff.Files)
	s.page(w, r, p, status, "collab/pull_files", "Files on #"+strconv.Itoa(num), data)
}

// --- checks index ------------------------------------------------------------

type checkRow struct {
	SHA          string
	Short        string
	Href         string
	Dot          string
	State        string
	UpdatedAt    string
	Contexts     []checks.IndexContext
	ContextCount int
	Expanded     bool
	ExpandHref   string
	Rows         []*checks.StatusDoc
}

type checksData struct {
	Repo collabRepo

	Rows        []checkRow
	More        bool
	After       string
	OlderHref   string
	RefreshHref string
	Expanded    string
	State       string
	Context     string
	Filtered    bool

	Error  string
	Notice string
}

func (s *Server) checksList(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	data := checksData{
		Repo:        rc,
		RefreshHref: r.URL.RequestURI(),
		After:       r.URL.Query().Get("after"),
		Expanded:    r.URL.Query().Get("expanded"),
		State:       r.URL.Query().Get("state"),
		Context:     strings.ToLower(strings.TrimSpace(r.URL.Query().Get("context"))),
	}
	if collabDeps.ChecksList == nil {
		data.Error = "checks service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/checks", "Checks", data)
		return
	}
	page, err := collabDeps.ChecksList(r.Context(), rc.Owner, rc.Name, p, data.After, 50)
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.page(w, r, p, collabErrStatus(err), "collab/checks", "Checks", data)
		return
	}
	data.More = page.More
	for _, row := range page.Checks {
		cr := checkRow{
			SHA:          row.SHA,
			Short:        shortSHA(row.SHA),
			Href:         "/" + rc.Full + "/commit/" + row.SHA,
			Dot:          checkStateDot(row.State),
			State:        row.State,
			UpdatedAt:    row.UpdatedAt,
			Contexts:     row.Contexts,
			ContextCount: len(row.Contexts),
			ExpandHref:   "/" + rc.Full + "/checks/" + row.SHA,
		}
		if data.State != "" && data.State != "all" && row.State != data.State {
			data.Filtered = true
			continue
		}
		if data.Context != "" {
			match := false
			for _, c := range row.Contexts {
				if strings.Contains(strings.ToLower(c.Name), data.Context) {
					match = true
				}
			}
			if !match {
				data.Filtered = true
				continue
			}
		}
		if data.Expanded == row.SHA && collabDeps.ChecksStatuses != nil {
			if sv, serr := collabDeps.ChecksStatuses(r.Context(), rc.Owner, rc.Name, row.SHA, p); serr == nil {
				cr.Expanded = true
				cr.Rows = sv.Statuses
			}
		}
		data.Rows = append(data.Rows, cr)
	}
	if data.More && len(page.Checks) > 0 {
		data.OlderHref = pagerHref(r, "after", page.Checks[len(page.Checks)-1].SHA)
	}
	s.page(w, r, p, http.StatusOK, "collab/checks", "Checks", data)
}

// checkContexts is the lazy per-row fragment (htmx target).
func (s *Server) checkContexts(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !fullSHA(sha) {
		s.collabError(w, r, http.StatusNotFound, "unknown check")
		return
	}
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	if collabDeps.ChecksStatuses == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "checks service not wired")
		return
	}
	sv, err := collabDeps.ChecksStatuses(r.Context(), rc.Owner, rc.Name, sha, p)
	if err != nil {
		s.collabError(w, r, collabErrStatus(err), err.Error())
		return
	}
	s.collabFragment(w, r, "collab/checks", "collab_check_contexts", sv.Statuses)
}

// --- check detail ------------------------------------------------------------

type checkDetailData struct {
	Repo     collabRepo
	SHA      string
	Short    string
	Commit   string
	Combined *checks.CombinedView
	Pill     checkPillView
	Zero     bool
	Error    string
}

func (s *Server) checkDetail(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !fullSHA(sha) {
		// Anything that is not a full 40/64-hex sha is 404, never a 400 page
		// (the service's normalizer rule, spec §1.3 #5).
		s.collabError(w, r, http.StatusNotFound, "unknown check")
		return
	}
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	data := checkDetailData{Repo: rc, SHA: sha, Short: shortSHA(sha), Commit: "/" + rc.Full + "/commit/" + sha}
	if collabDeps.ChecksCombined == nil {
		data.Error = "checks service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/check_detail", "Checks", data)
		return
	}
	cv, err := collabDeps.ChecksCombined(r.Context(), rc.Owner, rc.Name, sha, p)
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound {
			s.collabError(w, r, st, err.Error())
			return
		}
		st := collabErrStatus(err)
		data.Error = err.Error()
		s.page(w, r, p, st, "collab/check_detail", "Checks", data)
		return
	}
	data.Combined = cv
	data.Zero = len(cv.Statuses) == 0
	data.Pill = checkPillFor(rc.Owner, rc.Name, cv.SHA, cv.State, data.Zero)
	s.page(w, r, p, http.StatusOK, "collab/check_detail", "Checks for "+data.Short, data)
}

// --- releases list -----------------------------------------------------------

type releaseRow struct {
	Tag         string
	Name        string
	Href        string
	Published   string
	Author      string
	Excerpt     string
	TagName     bool // Name was blank → tag is the title
	Draft       bool
	Prerelease  bool
	Latest      bool
	AssetCount  int
	KeyAssets   []releases.AssetEntry
	ExtraAssets int
	Cursor      string
	AssetBase   string
}

type releasesData struct {
	Repo collabRepo

	Rows   []releaseRow
	More   bool
	After  string
	Filter string
	Latest string

	Total       int
	Drafts      int
	Prereleases int

	OlderHref       string
	AllHref         string
	DraftsHref      string
	PrereleasesHref string
	RefreshHref     string

	Error  string
	Notice string
}

func (s *Server) releaseList(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	data := releasesData{Repo: rc, After: r.URL.Query().Get("after"), Filter: r.URL.Query().Get("filter")}
	switch data.Filter {
	case "drafts", "prereleases":
	default:
		data.Filter = "all"
	}
	if collabDeps.ReleasesList == nil {
		data.Error = "releases list not wired (internal/releases ListReleases returns an unexported type — see WIRING)"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/releases", "Releases", data)
		return
	}
	list, more, err := collabDeps.ReleasesList(r.Context(), rc.Owner, rc.Name, p, 50, data.After)
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.page(w, r, p, collabErrStatus(err), "collab/releases", "Releases", data)
		return
	}
	if collabDeps.ReleaseLatest != nil {
		if latest, _, lerr := collabDeps.ReleaseLatest(r.Context(), rc.Owner, rc.Name, p, false); lerr == nil && latest != nil {
			data.Latest = latest.Tag
		}
	}
	data.More = more
	for _, rel := range list {
		data.Total++
		if rel.Draft {
			data.Drafts++
		}
		if rel.Prerelease {
			data.Prereleases++
		}
		switch data.Filter {
		case "drafts":
			if !rel.Draft {
				continue
			}
		case "prereleases":
			if !rel.Prerelease {
				continue
			}
		}
		row := releaseRow{
			Tag:        rel.Tag,
			Name:       rel.Name,
			Href:       "/" + rc.Full + "/releases/" + url.PathEscape(rel.Tag),
			Author:     rel.Author,
			Excerpt:    excerptBody(rel.Body, 140),
			Draft:      rel.Draft,
			Prerelease: rel.Prerelease,
			AssetCount: len(rel.Assets),
			AssetBase:  "/" + rc.Full + "/releases/" + url.PathEscape(rel.Tag) + "/assets/",
			Cursor:     rel.CreatedAt + "|" + rel.Tag,
		}
		if rel.Name == "" {
			row.Name, row.TagName = rel.Tag, true
		}
		if row.Excerpt == row.Name {
			row.Excerpt = ""
		}
		if rel.PublishedAt != nil {
			row.Published = *rel.PublishedAt
		} else {
			row.Published = rel.CreatedAt
		}
		row.Latest = data.Latest == rel.Tag && data.Filter == "all" && data.After == ""
		if row.Latest {
			row.KeyAssets, row.ExtraAssets = keyAssets(rel.Assets, latestAssetLimit)
		}
		data.Rows = append(data.Rows, row)
	}
	base := "/" + rc.Full + "/releases"
	data.AllHref, data.DraftsHref, data.PrereleasesHref = base+"?filter=all", base+"?filter=drafts", base+"?filter=prereleases"
	data.RefreshHref = r.URL.RequestURI()
	if data.More && len(list) > 0 {
		last := list[len(list)-1]
		data.OlderHref = pagerHref(r, "after", last.CreatedAt+"|"+last.Tag)
	}
	s.page(w, r, p, http.StatusOK, "collab/releases", "Releases", data)
}

// --- release detail ----------------------------------------------------------

type assetRow struct {
	Name         string
	Href         string
	Size         string
	SHA12        string
	SHA          string
	Content      string
	Uploaded     string
	Uploader     string
	DeleteAction string
}

type releaseDetailData struct {
	Repo collabRepo

	Tag        string
	Name       string
	Body       string
	Author     string
	TagSHA     string
	CreatedAt  string
	Published  string
	Draft      bool
	Prerelease bool
	Assets     []assetRow
	AssetCount int

	CanWrite bool
	CanMaint bool

	Editing  bool
	FormName string
	FormBody string

	DownloadBase  string
	UploadAction  string
	SaveAction    string
	PublishAction string
	DeleteAction  string

	Error  string
	Notice string
}

func (s *Server) releasePage(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	tag := r.PathValue("tag")
	if tag == "new" {
		// Reserved sibling: /releases/new is the forms group's composer.
		s.collabError(w, r, http.StatusNotFound, "not found")
		return
	}
	editing := r.URL.Query().Get("edit") != ""
	s.renderRelease(w, r, rc, p, tag, http.StatusOK, "", "", editing, "", "")
}

func (s *Server) renderRelease(w http.ResponseWriter, r *http.Request, rc collabRepo, p auth.Principal, tag string, status int, errMsg, notice string, editing bool, formName, formBody string) {
	data := releaseDetailData{
		Repo:     rc,
		Tag:      tag,
		CanWrite: rc.CanWrite,
		CanMaint: rc.CanMaint,
		Editing:  editing,
		FormName: formName,
		FormBody: formBody,
		Error:    errMsg,
		Notice:   notice,
	}
	base := "/" + rc.Full + "/releases/" + url.PathEscape(tag)
	data.DownloadBase = base + "/assets/"
	data.UploadAction = base + "/assets"
	data.SaveAction = base + "/save"
	data.PublishAction = base + "/publish"
	data.DeleteAction = base + "/delete"
	if collabDeps.ReleaseGet == nil {
		data.Error = "releases service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/release", "Release", data)
		return
	}
	rel, _, err := collabDeps.ReleaseGet(r.Context(), rc.Owner, rc.Name, p, tag)
	if err != nil {
		st := collabErrStatus(err)
		if st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.page(w, r, p, st, "collab/release", "Release", data)
		return
	}
	data.Name, data.Body, data.Author = rel.Name, rel.Body, rel.Author
	data.TagSHA, data.CreatedAt = rel.TagSHA, rel.CreatedAt
	data.Draft, data.Prerelease = rel.Draft, rel.Prerelease
	if rel.PublishedAt != nil {
		data.Published = *rel.PublishedAt
	}
	for _, a := range rel.Assets {
		data.Assets = append(data.Assets, assetRow{
			Name:         a.Name,
			Href:         data.DownloadBase + url.PathEscape(a.Name),
			Size:         fmtAssetSize(a.Size),
			SHA12:        shortSHA(a.SHA256),
			SHA:          a.SHA256,
			Content:      a.ContentType,
			Uploaded:     a.UploadedAt,
			Uploader:     a.Uploader,
			DeleteAction: data.DownloadBase + url.PathEscape(a.Name) + "/delete",
		})
	}
	data.AssetCount = len(data.Assets)
	if data.Editing && data.FormName == "" && data.FormBody == "" {
		data.FormName, data.FormBody = data.Name, data.Body
	}
	s.page(w, r, p, status, "collab/release", data.Name, data)
}

func (s *Server) releaseSave(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	rc, p, ok := s.collabWrite(w, r, identity.RoleWrite, "Edit a release")
	if !ok {
		return
	}
	if collabDeps.ReleasePut == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "releases service not wired")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	body := r.PostFormValue("body")
	// Read the current CAS token so a concurrent editor gets a 409 instead of
	// silently overwriting (the API's If-Match discipline).
	var ver store.Version
	if collabDeps.ReleaseGet != nil {
		if cur, v, err := collabDeps.ReleaseGet(r.Context(), rc.Owner, rc.Name, p, tag); err == nil && cur != nil {
			ver = v
		}
	}
	if _, _, err := collabDeps.ReleasePut(r.Context(), rc.Owner, rc.Name, p, tag, releases.ReleaseInput{Name: &name, Body: &body}, ver); err != nil {
		s.renderRelease(w, r, rc, p, tag, collabErrStatus(err), err.Error(), "", true, name, body)
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/releases/"+url.PathEscape(tag))
}

func (s *Server) releasePublish(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	rc, p, ok := s.collabWrite(w, r, identity.RoleWrite, "Publish a release")
	if !ok {
		return
	}
	if collabDeps.ReleasePut == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "releases service not wired")
		return
	}
	publish := false
	if _, _, err := collabDeps.ReleasePut(r.Context(), rc.Owner, rc.Name, p, tag, releases.ReleaseInput{Draft: &publish}, ""); err != nil {
		s.renderRelease(w, r, rc, p, tag, collabErrStatus(err), err.Error(), "", false, "", "")
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/releases/"+url.PathEscape(tag))
}

func (s *Server) releaseDelete(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	rc, p, ok := s.collabWrite(w, r, identity.RoleMaintain, "Delete a release")
	if !ok {
		return
	}
	if collabDeps.ReleaseDelete == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "releases service not wired")
		return
	}
	if err := collabDeps.ReleaseDelete(r.Context(), rc.Owner, rc.Name, p, tag); err != nil {
		s.renderRelease(w, r, rc, p, tag, collabErrStatus(err), err.Error(), "", false, "", "")
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/releases")
}

// releaseAssetUpload accepts the no-JS multipart upload: the handler hashes
// the part (the browser never computes the digest) and streams it into the
// service.
func (s *Server) releaseAssetUpload(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	rc, p, ok := s.collabWrite(w, r, identity.RoleWrite, "Upload a release asset")
	if !ok {
		return
	}
	if collabDeps.AssetUpload == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "releases service not wired")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.renderRelease(w, r, rc, p, tag, http.StatusBadRequest, "upload must be multipart/form-data", "", false, "", "")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		s.renderRelease(w, r, rc, p, tag, http.StatusBadRequest, "choose a file to attach", "", false, "", "")
		return
	}
	defer file.Close()
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = hdr.Filename
	}
	// Hash while spooling to a temp file (never buffer the whole asset in
	// memory): the content-addressed PUT needs the digest up front, and the
	// service enforces its own size bound while streaming.
	tmp, terr := os.CreateTemp("", "walhub-asset-*")
	if terr != nil {
		s.renderRelease(w, r, rc, p, tag, http.StatusServiceUnavailable, "could not stage the upload", "", false, "", "")
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	h := sha256.New()
	size, cerr := io.Copy(io.MultiWriter(tmp, h), file)
	if cerr != nil {
		s.renderRelease(w, r, rc, p, tag, http.StatusBadRequest, "could not read the upload", "", false, "", "")
		return
	}
	if _, serr := tmp.Seek(0, io.SeekStart); serr != nil {
		s.renderRelease(w, r, rc, p, tag, http.StatusServiceUnavailable, "could not stage the upload", "", false, "", "")
		return
	}
	contentType := hdr.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if _, err := collabDeps.AssetUpload(r.Context(), rc.Owner, rc.Name, p, tag, name, tmp, size, hex.EncodeToString(h.Sum(nil)), contentType); err != nil {
		s.renderRelease(w, r, rc, p, tag, collabErrStatus(err), err.Error(), "", false, "", "")
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/releases/"+url.PathEscape(tag)+"#assets")
}

func (s *Server) releaseAssetDelete(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	name := r.PathValue("name")
	rc, p, ok := s.collabWrite(w, r, identity.RoleWrite, "Delete a release asset")
	if !ok {
		return
	}
	if collabDeps.AssetDelete == nil {
		s.collabError(w, r, http.StatusServiceUnavailable, "releases service not wired")
		return
	}
	if _, err := collabDeps.AssetDelete(r.Context(), rc.Owner, rc.Name, p, tag, name); err != nil {
		s.renderRelease(w, r, rc, p, tag, collabErrStatus(err), err.Error(), "", false, "", "")
		return
	}
	redirectPage(w, r, "/"+rc.Full+"/releases/"+url.PathEscape(tag)+"#assets")
}

// --- forks -------------------------------------------------------------------

type forksData struct {
	Repo      collabRepo
	Rows      []forkRow
	More      bool
	After     string
	OlderHref string
	Error     string
}

func (s *Server) forkList(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.collabEnv(w, r)
	if !ok {
		return
	}
	data := forksData{Repo: rc, After: r.URL.Query().Get("after")}
	if collabDeps.ForksList == nil {
		data.Error = "forks service not wired"
		s.page(w, r, p, http.StatusServiceUnavailable, "collab/forks", "Forks", data)
		return
	}
	page, err := collabDeps.ForksList(r.Context(), rc.Owner, rc.Name, p, data.After, 50)
	if err != nil {
		if st := collabErrStatus(err); st == http.StatusNotFound || st == http.StatusUnauthorized || st == http.StatusForbidden {
			s.collabError(w, r, st, err.Error())
			return
		}
		data.Error = err.Error()
		s.page(w, r, p, collabErrStatus(err), "collab/forks", "Forks", data)
		return
	}
	data.More = page.More
	if data.More && len(page.Forks) > 0 {
		data.OlderHref = pagerHref(r, "after", page.Forks[len(page.Forks)-1].Repo)
	}
	for _, f := range page.Forks {
		data.Rows = append(data.Rows, forkRow{
			Repo:     f.Repo,
			ForkedAt: f.ForkedAt,
			Date:     forkDate(f.ForkedAt),
			Href:     "/" + f.Repo,
		})
	}
	s.page(w, r, p, http.StatusOK, "collab/forks", "Forks of "+rc.Full, data)
}
