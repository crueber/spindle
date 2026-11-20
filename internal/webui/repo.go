package webui

// Repo group (docs/go/12_web_ui.md §2; D-WEB-14): the six Code/History/WAL
// pages — Tree (repo root + /tree/*), Blob, Commits, Commit, Wal — plus the
// shared repo chrome (header, tab strip, degraded banner, empty guide) that
// every other group's repo-scoped page reuses.
//
// WIRING (composition, cmd/walhub): api.Env already carries everything the
// pages read (RepoView, RepoRegistry, Tasks, Cfg). The projections the JSON
// summary handler composes from feature hooks are NOT on api.Env, so this
// group declares them here instead of editing foundation files:
//
//	webui.BindRepo(webui.RepoDeps{
//	    Visibility:    apiEnv.RepoVisibility,   // summary visibility badge
//	    CollabCounts:  apiEnv.CollabCounts,     // Issues/Pulls tab badges
//	    ForkInfo:      apiEnv.ForkInfo,         // fork count + "forked from"
//	    ChecksSummary: apiEnv.ChecksSummary,    // Checks tab visibility
//	    Mirror:        apiEnv.MirrorSummary,    // pull-mirror badge + guide
//	    PushMirror:    apiEnv.PushMirrorSummary,// push-mirror badge
//	    Fsck:          <store-backed fsck probe>, // degraded banner + missing_total
//	    Placeholder:   <store-backed placeholder probe>, // reserved empty repo
//
//	    // Star/watch toggles in the header (07 §§7–8) — exact method values
//	    // from the composition's collabWiring (c.socialSvc / c.notifySvc):
//	    SocialCounts: c.socialSvc.Counts,        // social.Service
//	    SocialViewer: c.socialSvc.ViewerState,   // social.Service
//	    Star:         c.socialSvc.Star,          // social.Service
//	    Unstar:       c.socialSvc.Unstar,        // social.Service
//	    WatchSet:     c.notifySvc.SetWatch,      // notify.Service (principal name)
//
//	    // Create tag on the commit page (Forgejo #253/#263):
//	    Tags:   c.tagsSvc.CreateTag,     // tags.Service
//	    RoleOf: func(ctx context.Context, owner, repo string, p auth.Principal) identity.Role {
//	        role, _ := c.ident.Resolve(ctx, owner, repo, p) // identity.Service
//	        return role
//	    },
//
//	    // Per-row check pills on the commits list (bounded batch):
//	    ChecksIndex: c.checksSvc.ListChecks, // checks.Service (n ≤ 200)
//	})
//
// Every binding is optional: unbound → that projection is absent (exactly the
// api.Env hook contract: nil → no probe, +0 store round trips). BindRepo is a
// boot-time call (composition runs before Serve), never a per-request setter.
//
// Templates: templates/pages/repo/{tree,blob,commits,commit,wal}.html and
// templates/partials/repo_*.html. Other groups' repo pages include
// {{template "repo_header" .Repo}} / {{template "repo_tabs" .Repo}} with the
// value from s.RepoChromeFor.
//
// DECISIONS this group makes (for the doc amendments in 12_web_ui.md; the
// docs are foundation-owned, so they are listed here instead):
//   - A missing/invisible repo is a real 404 HTML page (the SPA returned 200
//     with a client-rendered "repository not found"); the visibility gate runs
//     before the anonymous_read flag (Forgejo #345), so a public repo stays
//     readable under anonymous_read=false.
//   - Client-only SPA view state becomes URL state: ?graph=1 (commit graph),
//     ?doc=<name> (directory docs), ?view=code (markdown Code view), ?all=1
//     (all WAL segments). No localStorage, no hidden panels.
//   - The tab strip fails OPEN: a tab hides only on an explicit false from the
//     summary projections (features.*, has_checks); a feature-disabled page
//     re-highlights onto Code (#522) so the strip is never blank.
//   - The empty guide shows BOTH transports as labelled rows (the SPA's
//     protocol toggle needed JS) and never rewrites an advertised http URL.
//   - Two-segment reserved paths that are not repositories (/orgs/new,
//     /{owner}/repositories, /{owner}/organizations) are declined (nothing
//     written) so their groups' literal patterns answer; those patterns
//     outrank this one anyway.
//   - Not rendered here (no seam in api.Env / this group's route set):
//     placeholder delete / op-run forms, the Split diff view and
//     diff-specific #hash (islands group), and the commit page's
//     expandable per-context check detail (collab owns checks fragments).
//     Star/watch toggles and Create tag ARE rendered here now — see
//     parity_repo.go for the write handlers and RepoDeps for the seams.

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/checks"
	"github.com/crueber/walhub/internal/config"
	"github.com/crueber/walhub/internal/git"
	"github.com/crueber/walhub/internal/identity"
	"github.com/crueber/walhub/internal/notify"
	"github.com/crueber/walhub/internal/server/auth"
	"github.com/crueber/walhub/internal/social"
	"github.com/crueber/walhub/internal/tags"
)

// --- bindings ---------------------------------------------------------------

// RepoDeps carries the per-repo projections the repo chrome renders and
// api.Env does not expose (the summary handler builds them from hooks it
// holds privately). Shapes are api's own projection types, so the page and
// the JSON summary twin cannot drift on data.
type RepoDeps struct {
	// Visibility returns "public"|"authenticated"|"private" (ok=false when
	// the identity surface is unwired — the badge then hides).
	Visibility func(owner, repo string) (string, bool)
	// CollabCounts carries the open issue/PR badge numerators.
	CollabCounts func(owner, repo string) (api.CollabCounts, bool)
	// ForkInfo carries the fork parent + direct-children count.
	ForkInfo func(owner, repo string) (api.ForkSummary, bool)
	// ChecksSummary carries the "any check was ever reported" flag.
	ChecksSummary func(owner, repo string) (api.ChecksSummary, bool)
	// Mirror is the pull-only mirror projection.
	Mirror func(owner, repo string) (api.MirrorView, bool)
	// PushMirror is the push-mirror projection.
	PushMirror func(owner, repo string) (api.PushMirrorView, bool)
	// Fsck is the cached audit report (drives the degraded banner and the
	// authoritative missing-object count).
	Fsck func(owner, repo string) (api.FsckInfo, bool)
	// Placeholder is the empty-repo reservation marker (#210).
	Placeholder func(owner, repo string) (RepoPlaceholder, bool)

	// --- star / watch (07 §§7–8; the header toggles) ------------------------
	//
	// All six are the social/notify services' own method values, so the page
	// and the JSON twin cannot drift on counters or gates. A nil field omits
	// that half of the affordance (never a button that cannot write).

	// SocialCounts is social.Service.Counts (the read gate + aliveness probe
	// live in the service).
	SocialCounts func(ctx context.Context, p auth.Principal, owner, repo string) (social.SocialDoc, error)
	// SocialViewer is social.Service.ViewerState (anonymous ⇒ false, false).
	SocialViewer func(ctx context.Context, p auth.Principal, owner, repo string) (starred, watching bool)
	// Star is social.Service.Star (write: authenticated + repo-visible,
	// feature-gated; returns the fresh count).
	Star func(ctx context.Context, p auth.Principal, owner, repo string) (int, error)
	// Unstar is social.Service.Unstar (authenticated only; always allowed).
	Unstar func(ctx context.Context, p auth.Principal, owner, repo string) (int, error)
	// WatchSet is notify.Service.SetWatch — its wire shape takes the principal
	// NAME (not a Principal) and returns the fresh watch state.
	WatchSet func(ctx context.Context, principal, owner, repo string, on bool) (notify.WatchState, error)

	// --- tags (Forgejo #253/#263; the commit page's Create tag form) --------

	// Tags is tags.Service.CreateTag (write role + policy re-checked there).
	Tags func(ctx context.Context, owner, repo string, p auth.Principal, in tags.CreateInput) (*tags.Tag, error)
	// RoleOf resolves the viewer's repo role (identity.Service.Resolve's role
	// half) — the write gate that decides whether Create tag renders at all.
	RoleOf func(ctx context.Context, owner, repo string, p auth.Principal) identity.Role

	// --- checks (the commits list's per-row pills) --------------------------

	// ChecksIndex is checks.Service.ListChecks: ONE bounded index page per
	// commits window (after="", n ≤ 200), joined by sha. Never one Combined
	// call per row (N+1 / unbounded fan-out).
	ChecksIndex func(ctx context.Context, owner, repo string, p auth.Principal, after string, n int) (*checks.ChecksPage, error)
}

// RepoPlaceholder mirrors the summary's #210 sidecar projection.
type RepoPlaceholder struct {
	CreatedBy string
	CreatedAt string
	ExpiresAt string
}

// repoDeps is set once at boot by BindRepo (composition order: after the api
// env exists, before the server accepts requests). A nil field means the
// projection is not wired and its affordance is omitted — never fabricated.
var repoDeps RepoDeps

// BindRepo installs the repo group's projections. Call once at boot.
func BindRepo(d RepoDeps) { repoDeps = d }

// --- routes -----------------------------------------------------------------

func init() { registerGroup(func(s *Server) { s.registerRepoRoutes() }) }

// registerRepoRoutes owns exactly the six repo page families. Distinct
// literal suffixes keep static-before-dynamic precedence: http.ServeMux gives
// the more specific pattern the request, so /tree/... never falls into the
// two-segment root. The tree family registers all three shapes because a
// subtree pattern alone does not answer /{owner}/{repo}/tree.
func (s *Server) registerRepoRoutes() {
	s.mux.HandleFunc("GET /{owner}/{repo}", s.repoTree)
	s.mux.HandleFunc("GET /{owner}/{repo}/tree", s.repoTree)
	s.mux.HandleFunc("GET /{owner}/{repo}/tree/{rest...}", s.repoTree)
	s.mux.HandleFunc("GET /{owner}/{repo}/blob/{rest...}", s.repoBlob)
	s.mux.HandleFunc("GET /{owner}/{repo}/commits", s.repoCommits)
	s.mux.HandleFunc("GET /{owner}/{repo}/commit/{sha}", s.repoCommit)
	s.mux.HandleFunc("GET /{owner}/{repo}/wal", s.repoWAL)

	// Star/watch toggles (repo header) and Create tag (commit page). These are
	// the SPA's own affordances, re-homed as plain POSTs: the page renders the
	// state, the POST flips it, and the 303 back re-reads it (no optimistic
	// JS). parity_repo.go holds the handlers.
	s.mux.HandleFunc("POST /{owner}/{repo}/star", s.repoStar)
	s.mux.HandleFunc("POST /{owner}/{repo}/unstar", s.repoUnstar)
	s.mux.HandleFunc("POST /{owner}/{repo}/watch", s.repoWatch)
	s.mux.HandleFunc("POST /{owner}/{repo}/unwatch", s.repoUnwatch)
	s.mux.HandleFunc("POST /{owner}/{repo}/tag", s.repoCreateTag)
}

// --- gate + data pass -------------------------------------------------------

// repoRequest is the resolved request identity: the principal and the repo id
// with `.git` stripped (the server serves /{o}/{r}.git everywhere).
type repoRequest struct {
	p    auth.Principal
	id   git.RepoId
	full string
}

// repoGate is the shared read gate (spec §0.3 order):
//
//  1. Authenticate — an invalid credential is a real 401 (server/principal).
//  2. Visibility FIRST (Forgejo #345): a public repo stays readable while
//     anonymous_read=false when the identity surface is wired
//     (CheckRead admits it). A denial is a 404 and never leaks existence.
//  3. With no identity surface the anonymous_read flag decides, and the
//     honest answer for an anonymous viewer is 401 (the gated() rule).
func (s *Server) repoGate(w http.ResponseWriter, r *http.Request) (repoRequest, bool) {
	if s.reservedNonRepoPath(r) {
		// A two-segment path that is NOT a repository (the owner profile
		// collections and /orgs/new) is answered by its own group's literal
		// patterns, which outrank /{owner}/{repo} in the mux. Write NOTHING
		// here so Serve reports "not a page" for this pattern.
		return repoRequest{}, false
	}
	p, ok := s.principal(w, r)
	if !ok {
		return repoRequest{}, false
	}
	owner := r.PathValue("owner")
	name := strings.TrimSuffix(r.PathValue("repo"), ".git")
	id, err := git.ParseRepoId(owner + "/" + name)
	if err != nil {
		// Malformed or empty segment: a plain 404 (the page provider owns
		// this pattern).
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return repoRequest{}, false
	}
	if !s.repoVisible(r.Context(), id.Owner, id.Name, p) {
		if s.auth.CheckRead == nil && p.Anonymous && !s.anonymousRead() {
			s.errorPage(w, r, http.StatusUnauthorized, "authentication required")
			return repoRequest{}, false
		}
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return repoRequest{}, false
	}
	return repoRequest{p: p, id: id, full: id.Owner + "/" + id.Name}, true
}

// reservedNonRepoPath reports the two-segment UI shapes that are not
// repositories: /orgs/new (org creation), /{owner}/repositories and
// /{owner}/organizations (the owner profile's collections). Their literal
// patterns outrank the repo page (spec §0.2), so declining here keeps the
// routes disjoint.
func (s *Server) reservedNonRepoPath(r *http.Request) bool {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) != 2 {
		return false
	}
	if segs[0] == "orgs" || segs[0] == "services" {
		return true
	}
	if strings.HasSuffix(segs[1], ".git") {
		// An explicit .git suffix always addresses a repository: a repo
		// literally named "repositories" stays reachable this way.
		return false
	}
	return segs[1] == "repositories" || segs[1] == "organizations"
}

// repoOpen performs the shared per-repo read pass: existence (never Sync an
// unknown repo into existence), the refs sync every API read performs, and
// the summary the chrome renders. It writes the response on failure.
func (s *Server) repoOpen(w http.ResponseWriter, r *http.Request, rq repoRequest, level api.SyncLevel) (*RepoChrome, bool) {
	if s.env.API == nil || s.env.API.Repo == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "repository data is unavailable")
		return nil, false
	}
	ctx := r.Context()
	if s.env.API.Repos != nil {
		exists, err := s.env.API.Repos.Exists(ctx, rq.id)
		if err != nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "repository lookup failed")
			return nil, false
		}
		if !exists {
			s.errorPage(w, r, http.StatusNotFound, "repository not found")
			return nil, false
		}
	}
	if err := s.env.API.Repo.Sync(ctx, rq.id, level); err != nil {
		s.repoViewError(w, r, err)
		return nil, false
	}
	sum, err := s.env.API.Repo.Summary(ctx, rq.id)
	if err != nil {
		s.repoViewError(w, r, err)
		return nil, false
	}
	return s.buildRepoChrome(r, rq, sum), true
}

// RepoChromeFor is the cross-group seam: collab/forms/settings pages call it
// to render the same header + tab strip (include partials "repo_header" and
// "repo_tabs" with the returned value). ok=false means the response has
// already been written (a gate or data failure).
func (s *Server) RepoChromeFor(w http.ResponseWriter, r *http.Request, owner, name string) (*RepoChrome, bool) {
	id, err := git.ParseRepoId(owner + "/" + name)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return nil, false
	}
	p, ok := s.principal(w, r)
	if !ok {
		return nil, false
	}
	if !s.repoVisible(r.Context(), id.Owner, id.Name, p) {
		if s.auth.CheckRead == nil && p.Anonymous && !s.anonymousRead() {
			s.errorPage(w, r, http.StatusUnauthorized, "authentication required")
			return nil, false
		}
		s.errorPage(w, r, http.StatusNotFound, "repository not found")
		return nil, false
	}
	return s.repoOpen(w, r, repoRequest{p: p, id: id, full: id.Owner + "/" + id.Name}, api.SyncRefs)
}

// repoViewError maps a RepoView error: not-found → 404, everything else →
// 503 (the API's mapViewErr rule, with a message safe for a person).
func (s *Server) repoViewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, api.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "not found in this repository")
	case errors.Is(err, api.ErrPending):
		s.errorPage(w, r, http.StatusServiceUnavailable, "this repository's data surface is not ready")
	default:
		s.errorPage(w, r, http.StatusServiceUnavailable, "repository data is temporarily unavailable")
	}
}

// emptyRepoMarker is the api's machine-readable unborn-repo 404 prefix
// (07_api.md §9.9, issue #209). It is the "guide, not error" signal.
const emptyRepoMarker = "empty repository: "

func isEmptyRepoErr(err error) bool {
	return err != nil && errors.Is(err, api.ErrNotFound) && strings.Contains(err.Error(), emptyRepoMarker)
}

// --- repo chrome ------------------------------------------------------------

// RepoChrome is the header + tab strip model (spec §1.2). Every value is
// precomputed here: templates only print.
type RepoChrome struct {
	Owner, Name, Full string
	Description       string
	Head              *api.Ref
	HeadLabel         string

	Branches, Tags int
	Empty          bool
	Degraded       bool
	MissingTotal   uint64

	VisLabel, VisTitle string
	ForkParent         string
	Forks              int

	MirrorBadge     string
	MirrorTitle     string
	MirrorNext      string
	PushMirrorBadge string
	PushMirrorTitle string
	PushMirrorNext  string

	CloneURL    string
	SSHURL      string
	HTTPLabel   string
	ForkHref    string
	ForksHref   string
	SettingsRef string

	// Star/watch (07 §§7–8). ShowStar/ShowWatch mean "the counters were
	// read"; CanStar/CanWatch mean "the write seam is wired"; Off means the
	// feature is disabled AND the viewer has not engaged (Forgejo #522: the
	// retained state always stays actionable so it can be undone).
	Stars    int
	Starred  bool
	ShowStar bool
	CanStar  bool
	StarOff  bool

	Watchers  int
	Watching  bool
	ShowWatch bool
	CanWatch  bool
	WatchOff  bool

	// Here is the current request URI (path + query) so the toggle forms can
	// return the viewer to exactly this view after the write.
	Here string

	Placeholder *RepoPlaceholder

	Tabs    []repoTab
	Section string
}

// repoTab is one entry of the repo tab strip.
type repoTab struct {
	ID     string
	Label  string
	Href   string
	Active bool
	Badge  int
}

// buildRepoChrome composes the header model in ONE pass (no N+1 browser
// fetches; the projections below are boot-bound in-process reads).
func (s *Server) buildRepoChrome(r *http.Request, rq repoRequest, sum api.SummaryData) *RepoChrome {
	c := &RepoChrome{
		Owner:        rq.id.Owner,
		Name:         rq.id.Name,
		Full:         rq.full,
		Description:  sum.Description,
		Head:         sum.Head,
		Branches:     sum.Branches,
		Tags:         sum.Tags,
		MissingTotal: sum.MissingTotal,
		ForksHref:    "/" + rq.full + "/forks",
		ForkHref:     "/" + rq.full + "/fork",
		SettingsRef:  "/" + rq.full + "/settings",
	}
	// Empty is the api's vocabulary plus the refs==0 fallback for views that
	// predate Health (same predicate summarizeSnapshot uses).
	c.Empty = sum.Health == api.RepoHealthEmpty ||
		(sum.Head == nil && sum.Branches == 0 && sum.Tags == 0)
	c.Degraded = sum.Health == api.RepoHealthDegraded
	c.Here = r.URL.RequestURI()

	// Features fail OPEN (Forgejo #522): a nil projection or an older view
	// keeps every feature; only an explicit false hides one. Hoisted: the tab
	// strip AND the star/watch toggles read the same resolved set.
	feat := config.AllFeatures()
	if sum.Features != nil {
		feat = *sum.Features
	}
	if sum.Head != nil {
		c.HeadLabel = shortRefName(sum.Head.Name)
		if c.HeadLabel == "" {
			c.HeadLabel = shortSHA(sum.Head.SHA)
		}
	}

	// Degraded override from the cached audit report (the summary handler's
	// probeFsck branch): an explicit Health wins, the report refines stacks.
	if repoDeps.Fsck != nil {
		if rep, ok := repoDeps.Fsck(c.Owner, c.Name); ok && rep.MissingTotal > 0 && !c.Empty {
			c.Degraded = true
			c.MissingTotal = rep.MissingTotal
		}
	}

	// Visibility badge (#345): unknown renders nothing — never assume private.
	if repoDeps.Visibility != nil {
		if vis, ok := repoDeps.Visibility(c.Owner, c.Name); ok {
			switch strings.ToLower(vis) {
			case "public":
				c.VisLabel, c.VisTitle = "public", "public — anyone may read"
			case "authenticated":
				c.VisLabel, c.VisTitle = "authenticated", "private — signed-in users may read"
			case "private":
				c.VisLabel, c.VisTitle = "private", "private — owner or organization members only"
			}
		}
	}

	counts := api.CollabCounts{}
	if repoDeps.CollabCounts != nil {
		if v, ok := repoDeps.CollabCounts(c.Owner, c.Name); ok {
			counts = v
		}
	}
	if repoDeps.ForkInfo != nil {
		if v, ok := repoDeps.ForkInfo(c.Owner, c.Name); ok {
			c.ForkParent, c.Forks = v.Parent, v.Count
		}
	}
	hasChecks := false
	if repoDeps.ChecksSummary != nil {
		if v, ok := repoDeps.ChecksSummary(c.Owner, c.Name); ok {
			hasChecks = v.HasChecks
		}
	}
	if repoDeps.Mirror != nil {
		if v, ok := repoDeps.Mirror(c.Owner, c.Name); ok {
			c.MirrorBadge = "mirror · pull-only"
			c.MirrorNext = nextSyncText(v, s.env.now())
			c.MirrorTitle = strings.TrimSpace(v.UpstreamURL + " · " + c.MirrorNext)
		}
	}
	if repoDeps.PushMirror != nil {
		if v, ok := repoDeps.PushMirror(c.Owner, c.Name); ok {
			c.PushMirrorBadge = "mirror · push"
			c.PushMirrorNext = nextSyncText2(v, s.env.now())
			c.PushMirrorTitle = strings.TrimSpace(v.UpstreamURL + " · " + c.PushMirrorNext)
		}
	}
	if repoDeps.Placeholder != nil && c.Empty {
		if ph, ok := repoDeps.Placeholder(c.Owner, c.Name); ok {
			c.Placeholder = &ph
		}
	}

	// Star/watch counters + viewer flags (ONE social read for the whole page;
	// the SPA spent one GET social + one GET watch). A read error degrades
	// the pill to absent — display metadata never fails the page.
	if repoDeps.SocialCounts != nil {
		if doc, err := repoDeps.SocialCounts(r.Context(), rq.p, c.Owner, c.Name); err == nil {
			c.Stars, c.Watchers = doc.Stars, doc.Watchers
			c.ShowStar, c.ShowWatch = true, true
			if repoDeps.SocialViewer != nil {
				c.Starred, c.Watching = repoDeps.SocialViewer(r.Context(), rq.p, c.Owner, c.Name)
			}
			c.CanStar = repoDeps.Star != nil && repoDeps.Unstar != nil
			c.CanWatch = repoDeps.WatchSet != nil
			c.StarOff = !feat.Star && !c.Starred
			c.WatchOff = !feat.Watch && !c.Watching
		}
	}

	// Clone URLs are the server's advertisement, shown VERBATIM (issue #124:
	// never upgrade http→https for display; the label derives from the URL).
	base := s.repoBaseURL(r)
	c.CloneURL = base + "/" + c.Full + ".git"
	c.SSHURL = s.repoSSHURL(r, c.Full)
	c.HTTPLabel = "HTTP"
	if strings.HasPrefix(strings.ToLower(c.CloneURL), "https://") {
		c.HTTPLabel = "HTTPS"
	}

	// Features fail OPEN (Forgejo #522): a nil projection or an older view
	// keeps every tab; only an explicit false hides one. Checks hides on an
	// explicit has_checks=false (#505) — we always hold the summary here, so
	// "unknown" never reaches the strip.
	c.Section = repoTabSection(r.URL.Path)
	c.Tabs = []repoTab{
		{ID: "code", Label: "Code", Href: "/" + c.Full},
		{ID: "commits", Label: "Commits", Href: "/" + c.Full + "/commits"},
	}
	if feat.Issues {
		c.Tabs = append(c.Tabs, repoTab{ID: "issues", Label: "Issues", Href: "/" + c.Full + "/issues", Badge: counts.OpenIssues})
	}
	if feat.Pulls {
		c.Tabs = append(c.Tabs, repoTab{ID: "pulls", Label: "Pulls", Href: "/" + c.Full + "/pulls", Badge: counts.OpenPulls})
	}
	if hasChecks {
		c.Tabs = append(c.Tabs, repoTab{ID: "checks", Label: "Checks", Href: "/" + c.Full + "/checks"})
	}
	if feat.Releases {
		c.Tabs = append(c.Tabs, repoTab{ID: "releases", Label: "Releases", Href: "/" + c.Full + "/releases"})
	}
	c.Tabs = append(c.Tabs, repoTab{ID: "settings", Label: "Settings", Href: "/" + c.Full + "/settings"})
	active := false
	for i := range c.Tabs {
		c.Tabs[i].Active = c.Tabs[i].ID == c.Section
		active = active || c.Tabs[i].Active
	}
	if !active && (c.Section == "issues" || c.Section == "pulls" || c.Section == "releases") {
		// Forgejo #522: a hidden feature tab must never leave the strip blank
		// (and must never hold aria-current) — the page re-highlights onto
		// Code. Checks keeps its mapping (#505/#513): a check-less repo's
		// /checks page simply has no current tab.
		c.Section = "code"
		for i := range c.Tabs {
			c.Tabs[i].Active = c.Tabs[i].ID == "code"
		}
	}
	return c
}

// repoTabSection maps the FIRST path segment after /{owner}/{repo} to a tab
// (web/src/lib/tabs.js SECTION_TABS). Only the first segment decides, so a
// filename like checks.go deep in a blob path never selects Checks (#25).
// Feature-disabled pages re-highlight onto Code (#522) — a hidden tab must
// never hold aria-current — while Checks keeps its mapping (#505/#513).
func repoTabSection(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 3 {
		return "code"
	}
	switch strings.ToLower(segs[2]) {
	case "tree", "blob":
		return "code"
	case "commits", "commit":
		return "commits"
	case "issues", "labels", "milestones":
		return "issues"
	case "pulls", "pull":
		return "pulls"
	case "checks", "check":
		return "checks"
	case "releases", "release":
		return "releases"
	case "wal", "settings":
		return "settings"
	}
	return "code"
}

// repoBaseURL is the public base (server.public_url, else the request Host;
// X-Forwarded-Proto/TLS select https). Same rule as the API's baseURL —
// duplicated here because that helper is unexported and pages must advertise
// the identical URL the JSON summary does.
func (s *Server) repoBaseURL(r *http.Request) string {
	if s.env.Cfg != nil && s.env.Cfg.Server.PublicURL != "" {
		return strings.TrimSuffix(s.env.Cfg.Server.PublicURL, "/")
	}
	scheme := "http"
	if proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); strings.EqualFold(strings.TrimSpace(proto), "https") {
		scheme = "https"
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// repoSSHURL advertises the SSH transport exactly like api's sshCloneURL
// (17_ssh.md §3): ssh://git@host[:port]/owner/repo.git at the advertised port,
// port 22 omitted; "" when no port is advertised.
func (s *Server) repoSSHURL(r *http.Request, full string) string {
	port := 0
	if s.env.Cfg != nil {
		port = s.env.Cfg.Server.SSH.AdvertisedSSHPort()
	}
	if port == 0 {
		return ""
	}
	host := ""
	if u, err := url.Parse(s.repoBaseURL(r)); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		return ""
	}
	hostPort := host
	if port != 22 {
		hostPort = net.JoinHostPort(host, strconv.Itoa(port))
	} else if strings.Contains(host, ":") {
		hostPort = "[" + host + "]"
	}
	return "ssh://git@" + hostPort + "/" + full + ".git"
}

// nextSyncText phrases a pull-mirror next fire (web/src/lib/mirror.js
// formatNextSync) from the SERVER clock — the page carries the absolute stamp
// in its title, so staleness is visible, never silent.
func nextSyncText(m api.MirrorView, now time.Time) string {
	if m.Due || m.NextSyncAt == "" {
		if m.LastSyncedAt != "" {
			return "sync due"
		}
		return "never synced — first sync pending"
	}
	at, err := time.Parse(time.RFC3339, m.NextSyncAt)
	if err != nil {
		return "next sync unknown"
	}
	d := at.Sub(now)
	if d <= 0 {
		return "sync due"
	}
	mins := int(d.Minutes())
	switch {
	case mins < 1:
		return "next sync < 1m"
	case mins < 60:
		return fmt.Sprintf("next sync in %dm", mins)
	case mins < 48*60:
		return fmt.Sprintf("next sync in %dh", mins/60)
	default:
		return fmt.Sprintf("next sync in %dd", mins/60/24)
	}
}

// nextSyncText2 phrases a push-mirror next fire (formatPushNextSync: one
// spelling for both mirror kinds — the badge wording is shared).
func nextSyncText2(m api.PushMirrorView, now time.Time) string {
	return nextSyncText(api.MirrorView{
		UpstreamURL:  m.UpstreamURL,
		NextSyncAt:   m.NextSyncAt,
		LastSyncedAt: m.LastSyncedAt,
		Due:          m.Due,
	}, now)
}

// --- shared display helpers -------------------------------------------------

// shortRefName strips the refs/heads|tags prefix for display (never for
// identity — the blob/tree routes take short refs, the API takes shas).
func shortRefName(ref string) string {
	return strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
}

// escapePath percent-encodes each segment of a repo path, keeping "/" as the
// separator (the server decodes one segment at a time).
func escapePath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// sizeUnits are the two size spellings, exactly as format.js carries them:
// fmtSize keeps the compact lowercase form ("92b"/"47.2k"), the split columns
// use the uppercase 1024-based units ("B"/"KB").
var (
	sizeUnits      = []string{"b", "k", "MB", "GB", "TB", "PB"}
	sizeUnitsParts = []string{"B", "KB", "MB", "GB", "TB", "PB"}
)

// sizeScale is the shared 1024-step ladder behind fmtSize/fmtSizeParts.
func sizeScale(v int64) (string, int) {
	if v < 1024 {
		return strconv.FormatInt(v, 10), 0
	}
	scaled := float64(v)
	step := 0
	for scaled >= 1024 && step < len(sizeUnits)-1 {
		scaled /= 1024
		step++
	}
	text := strconv.FormatFloat(math.Round(scaled*10)/10, 'f', -1, 64)
	if scaled >= 100 {
		text = strconv.FormatFloat(math.Round(scaled), 'f', 0, 64)
	}
	return text, step
}

// fmtSize renders "92b"/"47.2k"; negative or unknown → "?".
func fmtSize(n int64) string {
	if n < 0 {
		return "?"
	}
	text, step := sizeScale(n)
	return text + sizeUnits[step]
}

// fmtSizeParts splits the size into the two tree columns.
func fmtSizeParts(n int64) (num, unit string) {
	if n < 0 {
		return "?", ""
	}
	text, step := sizeScale(n)
	return text, sizeUnitsParts[step]
}

// fmtBytes is the WAL page's binary-unit spelling (Repo.jsx fmtBytes).
func fmtBytes(n int64) string {
	if n < 0 {
		return "—"
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	u := -1
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if v >= 100 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + " " + units[u]
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[u]
}

// fmtMode renders the ls-style mode cell (format.js fmtMode): a leading type
// char plus the nine permission chars.
func fmtMode(mode, typ string) string {
	m := strings.TrimSpace(mode)
	if m == "" {
		return ""
	}
	body := fmtModeBody(m)
	if body == "" {
		return ""
	}
	return fmtModeLead(m, typ) + body
}

var modeGlyphs = map[string]string{
	"100644": "rw-r--r--",
	"100755": "rwxr-xr-x",
	"120000": "rwxrwxrwx",
	"160000": "---------",
	"40000":  "rwxr-xr-x",
	"040000": "rwxr-xr-x",
}

func fmtModeLead(m, typ string) string {
	switch {
	case m == "040000" || m == "40000" || typ == "tree":
		return "d"
	case m == "160000" || typ == "commit":
		return "m"
	case m == "120000":
		return "l"
	default:
		return "."
	}
}

func fmtModeBody(m string) string {
	if g, ok := modeGlyphs[m]; ok {
		return g
	}
	digits := ""
	for i := len(m) - 1; i >= 0 && len(digits) < 3; i-- {
		if m[i] >= '0' && m[i] <= '7' {
			digits = string(m[i]) + digits
		}
	}
	if digits == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(digits); i++ {
		d := int(digits[i] - '0')
		if d&4 != 0 {
			b.WriteByte('r')
		} else {
			b.WriteByte('-')
		}
		if d&2 != 0 {
			b.WriteByte('w')
		} else {
			b.WriteByte('-')
		}
		if d&1 != 0 {
			b.WriteByte('x')
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// --- tree page --------------------------------------------------------------

type repoCrumb struct {
	Name string
	Href string
	Last bool
}

type treeRow struct {
	Name     string
	Mode     string
	ModeText string
	SizeNum  string
	SizeUnit string
	Size     int64
	IsBlob   bool
	Icon     string
	Href     string
	Stamp    string
}

type repoDoc struct {
	Name     string
	Query    string
	Selected bool
}

type treePage struct {
	Repo      *RepoChrome
	Guide     *repoGuide
	Crumbs    []repoCrumb
	Entries   []treeRow
	Ref       string
	Path      string
	Docs      []repoDoc
	DocBase   string
	DocName   string
	DocBody   template.HTML
	DocTooBig bool
	DocBinary bool
	DocSize   string
}

func (s *Server) repoTree(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	ctx := r.Context()
	rest := strings.TrimSuffix(r.PathValue("rest"), "/")
	data := &treePage{Repo: c}
	if c.Empty {
		// Issue #209: the guided empty state, with ZERO resolve/tree reads.
		data.Guide = s.repoGuide(c)
		s.page(w, r, rq.p, http.StatusOK, "repo/tree", "Code · "+c.Full, data)
		return
	}
	res, err := s.env.API.Repo.Resolve(ctx, rq.id, rest)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			s.page(w, r, rq.p, http.StatusOK, "repo/tree", "Code · "+c.Full, data)
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	tree, err := s.env.API.Repo.Tree(ctx, rq.id, res.SHA, res.Path)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			s.page(w, r, rq.p, http.StatusOK, "repo/tree", "Code · "+c.Full, data)
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	data.Ref = tree.Ref
	data.Path = tree.Path
	treeRest := shortRefName(tree.Ref)
	if treeRest == "" {
		treeRest = tree.SHA
	}
	// Entry links address the file under the DISPLAYED directory: short ref
	// plus the current path (#482 keeps ref names short so slashes in
	// refs/heads/... never split the route).
	entryBase := treeRest
	if tree.Path != "" {
		entryBase += "/" + tree.Path
	}
	// Breadcrumb: root then each segment; intermediate segments are tree
	// links that keep the SHORT display ref (never the full ref name — a
	// slash in `refs/heads/main` would split the route; #482).
	if tree.Path != "" {
		var crumbs []repoCrumb
		parts := strings.Split(tree.Path, "/")
		for i, part := range parts {
			sub := strings.Join(parts[:i+1], "/")
			crumbs = append(crumbs, repoCrumb{
				Name: part,
				Href: "/" + c.Full + "/tree/" + escapePath(treeRest+"/"+sub),
				Last: i == len(parts)-1,
			})
		}
		data.Crumbs = crumbs
	}
	for _, e := range tree.Entries {
		row := treeRow{
			Name:   e.Name,
			Mode:   e.Mode,
			IsBlob: e.Type == "blob",
			Stamp:  e.CommitTime,
		}
		row.ModeText = fmtMode(e.Mode, e.Type)
		switch e.Type {
		case "tree":
			row.Icon = "📁"
			row.Href = "/" + c.Full + "/tree/" + escapePath(entryBase+"/"+e.Name)
		case "commit":
			row.Icon = "↗"
			row.Href = "/" + c.Full + "/tree/" + escapePath(entryBase+"/"+e.Name)
		default:
			row.Icon = "📄"
			row.Href = "/" + c.Full + "/blob/" + escapePath(entryBase+"/"+e.Name)
		}
		if row.IsBlob && e.Size >= 0 {
			row.Size = e.Size
			row.SizeNum, row.SizeUnit = fmtSizeParts(e.Size)
		} else if row.IsBlob {
			row.SizeNum = "-"
		}
		data.Entries = append(data.Entries, row)
	}

	// Directory docs (issue #170): README first, rest alphabetical; the
	// selected tab is a LINK (?doc=) — the honest no-JS form of the SPA's
	// client tabs (Q4), one round trip per switch, no hidden panels.
	docNames := docCandidates(tree.Entries, tree.Readme)
	data.DocBase = r.URL.Path
	if len(docNames) > 0 {
		selected := r.URL.Query().Get("doc")
		if !containsString(docNames, selected) {
			selected = docNames[0]
		}
		for _, name := range docNames {
			data.Docs = append(data.Docs, repoDoc{Name: name, Query: url.QueryEscape(name), Selected: name == selected})
		}
		data.DocName = selected
		if tree.Readme != nil && tree.Readme.Name == selected {
			data.DocBody = RenderMarkdownIn(tree.Readme.Contents, c.Owner, c.Name)
		} else {
			blob, berr := s.env.API.Repo.Blob(ctx, rq.id, tree.SHA, docPath(tree.Path, selected), false)
			if berr != nil {
				if isEmptyRepoErr(berr) {
					data.Guide = s.repoGuide(c)
					data.Docs, data.DocName, data.DocBody = nil, "", ""
					s.page(w, r, rq.p, http.StatusOK, "repo/tree", "Code · "+c.Full, data)
					return
				}
				s.repoViewError(w, r, berr)
				return
			}
			switch {
			case blob.TooLarge:
				data.DocTooBig = true
				data.DocSize = fmtSize(blob.Size)
			case blob.Binary:
				data.DocBinary = true
				data.DocSize = fmtSize(blob.Size)
			default:
				data.DocBody = RenderMarkdownIn(string(blob.Contents), c.Owner, c.Name)
			}
		}
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/tree", "Code · "+c.Full, data)
}

func docPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func containsString(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// docCandidates ports web/src/lib/doctabs.js: blobs ending .md/.markdown,
// README (case-insensitive) first, rest alphabetical case-insensitively with
// the raw spelling as the deterministic tie-break; a probed readme with a
// non-md spelling is appended so the old single-readme render never regresses.
func docCandidates(entries []api.TreeEntry, readme *api.Readme) []string {
	var names []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Type != "blob" || !isMarkdownName(e.Name) {
			continue
		}
		names = append(names, e.Name)
		seen[e.Name] = true
	}
	sort.SliceStable(names, func(i, j int) bool {
		ri, rj := isReadmeName(names[i]), isReadmeName(names[j])
		if ri != rj {
			return ri
		}
		return lessFold(names[i], names[j])
	})
	if readme != nil && readme.Name != "" && readme.Contents != "" && !seen[readme.Name] {
		names = append(names, readme.Name)
	}
	return names
}

func isMarkdownName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown")
}

func isReadmeName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "readme.md" || lower == "readme.markdown"
}

func lessFold(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// --- empty guide ------------------------------------------------------------

// repoGuide is the #209/#210 empty state (and the mirror waiting state,
// Forgejo #281 — a mirror never shows push guidance).
type repoGuide struct {
	Mirror       bool
	CloneHTTP    string
	CloneSSH     string
	HTTPLabel    string
	CloneCmd     string
	CloneCmdSSH  string
	RemoteCmd    string
	PushCmd      string
	Upstream     string
	MirrorLast   string
	MirrorResult string
	MirrorBad    bool
	Placeholder  *RepoPlaceholder
}

func (s *Server) repoGuide(c *RepoChrome) *repoGuide {
	g := &repoGuide{
		CloneHTTP:   c.CloneURL,
		CloneSSH:    c.SSHURL,
		HTTPLabel:   c.HTTPLabel,
		CloneCmd:    "git clone " + c.CloneURL,
		RemoteCmd:   "git remote add origin " + c.CloneURL,
		PushCmd:     "git push -u origin main",
		Placeholder: c.Placeholder,
	}
	if g.CloneSSH != "" {
		g.CloneCmdSSH = "git clone " + g.CloneSSH
	}
	if repoDeps.Mirror != nil {
		if v, ok := repoDeps.Mirror(c.Owner, c.Name); ok {
			g.Mirror = true
			g.Upstream = v.UpstreamURL
			next := nextSyncText(v, s.env.now())
			if v.LastSyncedAt != "" {
				g.MirrorLast = "Last synced " + v.LastSyncedAt + " — " + next + "."
			} else {
				g.MirrorLast = "First sync pending — the scheduled sync will populate this repository."
			}
			if v.LastResult != "" && v.LastResult != "ok" {
				g.MirrorBad = true
				g.MirrorResult = v.LastResult
			}
		}
	}
	if g.Mirror {
		// A pull-only mirror rejects pushes: the guide renders the waiting
		// state INSTEAD of the push instructions (Forgejo #281).
		g.RemoteCmd, g.PushCmd = "", ""
	}
	return g
}

// --- blob page --------------------------------------------------------------

type blobLine struct {
	No    int
	Code  template.HTML
	Blank bool
}

type blobPage struct {
	Repo       *RepoChrome
	Guide      *repoGuide
	Crumbs     []repoCrumb
	Path       string
	PathLink   string
	Name       string
	Lang       string
	Size       string
	SHA        string
	RawHref    string
	TooLarge   bool
	Binary     bool
	IsMarkdown bool
	CodeView   bool
	Markdown   template.HTML
	Code       template.HTML
	Lines      []blobLine
}

func (s *Server) repoBlob(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	ctx := r.Context()
	rest := strings.TrimSuffix(r.PathValue("rest"), "/")
	if rest == "" {
		// A blob needs a path (`blob requires a path`, api blob handler).
		s.errorPage(w, r, http.StatusNotFound, "file not found")
		return
	}
	data := &blobPage{Repo: c}
	if c.Empty {
		data.Guide = s.repoGuide(c)
		s.page(w, r, rq.p, http.StatusOK, "repo/blob", "Code · "+c.Full, data)
		return
	}
	res, err := s.env.API.Repo.Resolve(ctx, rq.id, rest)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			s.page(w, r, rq.p, http.StatusOK, "repo/blob", "Code · "+c.Full, data)
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	if res.Path == "" {
		s.errorPage(w, r, http.StatusNotFound, "file not found")
		return
	}
	blob, err := s.env.API.Repo.Blob(ctx, rq.id, res.SHA, res.Path, false)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			s.page(w, r, rq.p, http.StatusOK, "repo/blob", "Code · "+c.Full, data)
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	name := blob.Name
	if name == "" {
		name = res.Path
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
	}
	data.Path, data.Name = blob.Path, name
	if data.Path == "" {
		data.Path = res.Path
	}
	data.PathLink = r.URL.Path
	data.Lang = LanguageFor(name)
	data.Size = fmtSize(blob.Size)
	data.SHA = shortSHA(blob.SHA)
	if blob.Ref != "" {
		data.RawHref = "/" + c.Full + "/api/blob/" + escapePath(shortRefName(blob.Ref)) + "/" + escapePath(data.Path) + "?raw"
	}
	data.TooLarge, data.Binary = blob.TooLarge, blob.Binary
	displayRef := shortRefName(blob.Ref)
	if displayRef == "" {
		displayRef = blob.SHA
	}
	if data.Path != "" {
		parts := strings.Split(data.Path, "/")
		for i, part := range parts {
			sub := strings.Join(parts[:i+1], "/")
			data.Crumbs = append(data.Crumbs, repoCrumb{
				Name: part,
				Href: "/" + c.Full + "/tree/" + escapePath(displayRef+"/"+sub),
				Last: i == len(parts)-1,
			})
		}
	}
	data.IsMarkdown = isMarkdownName(name)
	switch {
	case blob.TooLarge, blob.Binary:
		// placeholders only
	case data.IsMarkdown:
		data.CodeView = r.URL.Query().Get("view") == "code"
		data.Markdown = RenderMarkdownIn(string(blob.Contents), c.Owner, c.Name)
		data.Code = Highlight(name, string(blob.Contents))
	default:
		for i, line := range splitFileLines(string(blob.Contents)) {
			data.Lines = append(data.Lines, blobLine{
				No:    i + 1,
				Code:  Highlight(name, line),
				Blank: line == "",
			})
		}
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/blob", name+" · "+c.Full, data)
}

// splitFileLines splits file text into display lines, dropping the artifact
// trailing newline ("a\nb\n" is 2 lines — blob-lines.js splitLines).
func splitFileLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// --- commits page -----------------------------------------------------------

type commitRow struct {
	SHA          string
	Short        string
	Subject      string
	Author       string
	Email        string
	Stamp        string
	ParentsLabel string
	Parents      []string
	Trailers     int
	Href         string
	Check        *checkPillView
}

type commitsPage struct {
	Repo         *RepoChrome
	Guide        *repoGuide
	RefShort     string
	Path         string
	Graph        bool
	GraphOnHref  string
	GraphOffHref string
	GraphRows    []template.HTML
	Rows         []commitRow
	More         bool
	OlderHref    string
	Total        int
	Window       int
}

func (s *Server) repoCommits(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	ref := strings.TrimSpace(q.Get("ref"))
	path := strings.TrimSpace(q.Get("path"))
	skip := 0
	if v, err := strconv.Atoi(q.Get("skip")); err == nil && v > 0 {
		skip = v
	}
	data := &commitsPage{Repo: c, Path: path, Graph: q.Get("graph") == "1"}
	if c.Empty {
		data.Guide = s.repoGuide(c)
		s.page(w, r, rq.p, http.StatusOK, "repo/commits", "Commits · "+c.Full, data)
		return
	}
	res, err := s.env.API.Repo.Resolve(ctx, rq.id, ref)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			s.page(w, r, rq.p, http.StatusOK, "repo/commits", "Commits · "+c.Full, data)
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	const window = 35
	page, err := s.env.API.Repo.Commits(ctx, rq.id, res.SHA, path, skip, window)
	if err != nil {
		s.repoViewError(w, r, err)
		return
	}
	data.RefShort = shortRefName(page.Ref)
	if data.RefShort == "" {
		data.RefShort = shortSHA(page.SHA)
	}
	data.More = page.More
	data.Total = skip + len(page.Commits)
	for _, cm := range page.Commits {
		row := commitRow{
			SHA:      cm.SHA,
			Short:    shortSHA(cm.SHA),
			Subject:  cm.Subject,
			Author:   cm.Author,
			Email:    cm.AuthorEmail,
			Stamp:    rfc3339(cm.AuthorDate),
			Href:     "/" + c.Full + "/commit/" + url.PathEscape(cm.SHA),
			Parents:  cm.Parents,
			Trailers: len(cm.Trailers),
		}
		if len(cm.Parents) > 1 {
			row.ParentsLabel = fmt.Sprintf("merge parents (%d) ", len(cm.Parents))
		} else if len(cm.Parents) == 1 {
			row.ParentsLabel = "parent "
		}
		data.Rows = append(data.Rows, row)
	}
	if data.Graph {
		gc := make([]GraphCommit, 0, len(page.Commits))
		for _, cm := range page.Commits {
			gc = append(gc, GraphCommit{SHA: cm.SHA, Short: shortSHA(cm.SHA), Subject: cm.Subject, Parents: cm.Parents})
		}
		data.GraphRows = RenderCommitGraphRows(gc)
	}
	// Per-row check pills: ONE bounded index read for the whole window (the
	// SPA fetched each row's combined view — an N+1 fan-out). Rows outside
	// the hot window render nothing rather than a guessed state; a repo that
	// never reported a check renders the honest neutral pill (#518).
	if repoDeps.ChecksIndex != nil {
		if idx, ierr := repoDeps.ChecksIndex(ctx, c.Owner, c.Name, rq.p, "", commitsChecksBatch); ierr == nil {
			states := make(map[string]string, len(idx.Checks))
			for _, row := range idx.Checks {
				states[strings.ToLower(row.SHA)] = row.State
			}
			anyChecked := len(idx.Checks) > 0
			for i := range data.Rows {
				sha := data.Rows[i].SHA
				if state, ok := states[strings.ToLower(sha)]; ok {
					pill := checkPillFor(c.Owner, c.Name, sha, state, false)
					data.Rows[i].Check = &pill
				} else if !anyChecked {
					pill := checkPillFor(c.Owner, c.Name, sha, "", true)
					data.Rows[i].Check = &pill
				}
			}
		}
	}
	if data.More {
		v := url.Values{}
		if ref != "" {
			v.Set("ref", ref)
		}
		if path != "" {
			v.Set("path", path)
		}
		v.Set("skip", strconv.Itoa(data.Total))
		if data.Graph {
			v.Set("graph", "1")
		}
		data.OlderHref = "/" + c.Full + "/commits?" + v.Encode()
	}
	// Graph toggle links keep every bit of page state (ref, path, skip) so
	// flipping the graph never drops the window you were reading.
	state := url.Values{}
	if ref != "" {
		state.Set("ref", ref)
	}
	if path != "" {
		state.Set("path", path)
	}
	if skip > 0 {
		state.Set("skip", strconv.Itoa(skip))
	}
	on := url.Values{}
	for k, v := range state {
		on[k] = v
	}
	on.Set("graph", "1")
	data.GraphOnHref = "/" + c.Full + "/commits?" + on.Encode()
	data.GraphOffHref = "/" + c.Full + "/commits"
	if qs := state.Encode(); qs != "" {
		data.GraphOffHref += "?" + qs
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/commits", "Commits · "+c.Full, data)
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- commit page ------------------------------------------------------------

type trailerRow struct {
	Key   string
	Name  string
	Email string
	SHA   string
	Text  string
}

type trailerGroup struct {
	Name string
	Rows []trailerRow
}

type repoDiffRow struct {
	Class    string
	Sign     string
	LineNo   int
	Side     string
	GutterNo int
	Text     string
}

type repoDiffHunk struct {
	Header string
	Rows   []repoDiffRow
}

type commitFile struct {
	Path    string
	Anchor  string
	Flags   string
	Added   int
	Deleted int
	Binary  bool
	Hunks   []repoDiffHunk
	Lines   int
}

type commitPage struct {
	Repo         *RepoChrome
	Guide        *repoGuide
	SHA          string
	Commit       api.Commit
	Subject      string
	AuthorStamp  string
	CommitStamp  string
	Body         template.HTML
	Parents      []string
	Trailers     []trailerGroup
	TrailerCount int
	Files        []commitFile
	Adds         int64
	Dels         int64
	StatN        int

	// Create tag (Forgejo #253/#263): CanCreateTag is the write authority the
	// SPA computed client-side (roleAtLeast(role, "write")); the service
	// re-checks on POST. TagName/TagMessage re-seed the form after a rejected
	// submit so the user never retypes.
	CanCreateTag bool
	TagName      string
	TagMessage   string
	TagErr       string
}

var (
	urlRe  = regexp.MustCompile(`https?://[^\s<>")\]]+`)
	shaRe  = regexp.MustCompile(`\b(?:[0-9a-f]{40}|[0-9a-f]{64})\b`)
	mailRe = regexp.MustCompile(`^(.*?)\s*<([^>]+@[^>]+)>\s*$`)
	hexRe  = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
)

func (s *Server) repoCommit(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	data, ok := s.commitPageData(w, r, rq, r.PathValue("sha"))
	if !ok {
		return
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/commit", shortSHA(data.SHA)+" · "+data.Repo.Full, data)
}

// commitPageData builds the commit view for one sha (the GET handler and the
// Create tag POST's error re-render share it, so a rejected tag lands on the
// exact page the form was posted from). It writes the response on failure.
func (s *Server) commitPageData(w http.ResponseWriter, r *http.Request, rq repoRequest, sha string) (*commitPage, bool) {
	ctx := r.Context()
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return nil, false
	}
	data := &commitPage{Repo: c, SHA: sha, CanCreateTag: repoCanWrite(ctx, c.Owner, c.Name, rq.p) && repoDeps.Tags != nil}
	if c.Empty {
		data.Guide = s.repoGuide(c)
		return data, true
	}
	res, err := s.env.API.Repo.Resolve(ctx, rq.id, sha)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			return data, true
		}
		s.repoViewError(w, r, err)
		return nil, false
	}
	detail, err := s.env.API.Repo.Commit(ctx, rq.id, res.SHA)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			return data, true
		}
		s.repoViewError(w, r, err)
		return nil, false
	}
	cm := detail.Commit
	data.SHA = cm.SHA
	if data.SHA == "" {
		data.SHA = res.SHA
	}
	data.Commit = cm
	data.Subject = cm.Subject
	if data.Subject == "" {
		data.Subject = "(no message)"
	}
	data.AuthorStamp = rfc3339(cm.AuthorDate)
	data.CommitStamp = rfc3339(cm.CommitDate)
	data.Parents = cm.Parents
	// The body is plain text in the wire (git body minus trailers): escape
	// first, then link bare http(s) URLs and 40/64-hex shas (lib/diff.js
	// linkifyBody). Markdown rendering would invent emphasis the message
	// never had.
	data.Body = repoLinkify(cm.Body, "/"+c.Full)
	data.Trailers = groupTrailers(cm.Trailers)
	for _, g := range data.Trailers {
		data.TrailerCount += len(g.Rows)
	}
	for _, st := range detail.Stats {
		data.Adds += st.Additions
		data.Dels += st.Deletions
	}
	data.StatN = len(detail.Stats)
	for _, f := range ParseUnifiedDiff(detail.Patch) {
		cf := commitFile{
			Path:    f.Path,
			Anchor:  fileAnchor(f.Path),
			Added:   f.Added,
			Deleted: f.Deleted,
			Binary:  f.Binary,
		}
		switch f.Status {
		case "added":
			cf.Flags = "added"
		case "deleted":
			cf.Flags = "deleted"
		case "renamed":
			cf.Flags = "renamed from " + f.OldPath
		case "binary":
			cf.Flags = "binary"
		}
		for _, h := range f.Hunks {
			dh := repoDiffHunk{Header: h.Header}
			for _, l := range h.Lines {
				row := repoDiffRow{Text: l.Text}
				switch l.Kind {
				case "add":
					row.Class, row.Sign, row.LineNo, row.Side = "diff-add", "+", l.NewNo, "new"
				case "del":
					row.Class, row.Sign, row.LineNo, row.Side = "diff-del", "-", l.OldNo, "old"
				default:
					row.Sign, row.LineNo, row.Side = " ", l.NewNo, "new"
				}
				row.GutterNo = row.LineNo
				dh.Rows = append(dh.Rows, row)
			}
			cf.Hunks = append(cf.Hunks, dh)
		}
		data.Files = append(data.Files, cf)
	}
	return data, true
}

// fileAnchor is the per-file fragment id (Commit.jsx fileAnchor): every byte
// outside [A-Za-z0-9_-] becomes "-" so the anchor is a safe fragment.
func fileAnchor(path string) string {
	var b strings.Builder
	b.WriteString("f-")
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// repoLinkify escapes the text, then wraps bare http(s) URLs and 40/64-hex
// shas (as commit links). Escape-first is the safety boundary: the only tags
// in the output are the ones written here.
func repoLinkify(text, repoBase string) template.HTML {
	if text == "" {
		return ""
	}
	var b strings.Builder
	last := 0
	emitText := func(s string) { b.WriteString(escapeText(s)) }
	writeURL := func(m string) {
		tail := ""
		if n := len(m); n > 0 && strings.ContainsRune(".,;:!?", rune(m[n-1])) {
			tail = m[n-1:]
			m = m[:n-1]
		}
		b.WriteString(`<a href="` + escapeAttr(m) + `" rel="noopener">` + escapeText(m) + `</a>`)
		emitText(tail)
	}
	// URLs first: a sha-looking path segment inside a URL must not become a
	// nested anchor.
	for _, loc := range urlRe.FindAllStringIndex(text, -1) {
		between := text[last:loc[0]]
		b.WriteString(linkifySHAs(between, repoBase))
		writeURL(text[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(linkifySHAs(text[last:], repoBase))
	return template.HTML(b.String())
}

func linkifySHAs(s, repoBase string) string {
	var b strings.Builder
	last := 0
	for _, loc := range shaRe.FindAllStringIndex(s, -1) {
		b.WriteString(escapeText(s[last:loc[0]]))
		sha := s[loc[0]:loc[1]]
		if repoBase == "" {
			b.WriteString(escapeText(sha))
		} else {
			b.WriteString(`<a class="sha" href="` + escapeAttr(repoBase+"/commit/"+sha) + `" title="Commit">` + escapeText(shortSHA(sha)) + `</a>`)
		}
		last = loc[1]
	}
	b.WriteString(escapeText(s[last:]))
	return b.String()
}

var peopleTrailerKeys = map[string]bool{
	"co-authored-by": true, "assisted-by": true, "signed-off-by": true,
	"reviewed-by": true, "acked-by": true, "tested-by": true,
}

// groupTrailers ports lib/diff.js groupTrailers: People / merge-queue / Other,
// in that fixed order, empty groups dropped.
func groupTrailers(trailers []api.Trailer) []trailerGroup {
	buckets := map[string][]trailerRow{}
	for _, t := range trailers {
		key := strings.ToLower(t.Key)
		row := trailerRow{Key: t.Key}
		v := strings.TrimSpace(t.Value)
		switch {
		case hexRe.MatchString(v):
			row.SHA = v
		default:
			if m := mailRe.FindStringSubmatch(v); m != nil {
				row.Name, row.Email = strings.TrimSpace(m[1]), m[2]
			} else {
				row.Text = v
			}
		}
		var group string
		switch {
		case peopleTrailerKeys[key]:
			group = "People"
		case strings.HasPrefix(key, "merge-queue-") || strings.Contains(key, "ci-sha") || strings.Contains(key, "ci-boundary"):
			group = "Merge queue"
		default:
			group = "Other"
		}
		buckets[group] = append(buckets[group], row)
	}
	var out []trailerGroup
	for _, name := range []string{"Merge queue", "People", "Other"} {
		if rows := buckets[name]; len(rows) > 0 {
			out = append(out, trailerGroup{Name: name, Rows: rows})
		}
	}
	return out
}

// --- wal page ---------------------------------------------------------------

type walKV struct {
	Label string
	Value string
	Stamp string
}

type walParam struct {
	Name     string
	Required bool
	Kind     string
	Values   []string
}

type walOp struct {
	Op     string
	Params []walParam
}

// compactionRow is one COMPACT row with its stamp pre-rendered (templates
// cannot format a time.Time without a func).
type compactionRow struct {
	Seq   uint64
	Stamp string
	At    string
	Packs string
	Host  string
}

type bundleNode struct {
	Short        string
	StrategyKind string
	Size         string
	Token        string
	Orphan       bool
	Children     []bundleNode
}

type walTask struct {
	Label string
	Kind  string
	State string
	Stamp string
	OK    *bool
}

// planRow is one bundle-slot table row with its precomputed chip tone (the
// shared CHIP_TONES mapping needs slots without a template func).
type planRow struct {
	Strategy string
	Slot     int
	Status   string
	Chip     string
	Detail   string
	Bundle   string
}

type walPage struct {
	Repo         *RepoChrome
	Overview     api.OverviewData
	HealthTone   string
	HealthStatus string
	Fsck         *api.FsckInfo
	FsckAt       string
	MissingShown []string
	MissingMore  uint64
	Manifest     []walKV
	Local        []walKV
	Packs        string
	Bundles      []bundleNode
	BundleNone   bool
	PlanCounts   string
	PlanRows     []planRow
	Compactions  []compactionRow
	Segments     []string
	SegmentAll   bool
	SegmentMore  bool
	Ops          []walOp
	Recent       []walTask
	OpsAvailable int
	OpsRecent    int
}

func (s *Server) repoWAL(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	ctx := r.Context()
	ov, err := s.env.API.Repo.Overview(ctx, rq.id)
	if err != nil {
		s.repoViewError(w, r, err)
		return
	}
	data := &walPage{Repo: c, Overview: ov}
	data.HealthStatus = ov.Health.Status
	if data.HealthStatus == "" {
		data.HealthStatus = "ok"
	}
	data.HealthTone = chipTone(data.HealthStatus)
	if ov.Fsck != nil {
		data.Fsck = ov.Fsck
		if ov.Fsck.At != nil {
			data.FsckAt = rfc3339(*ov.Fsck.At)
		}
		if n := len(ov.Fsck.Missing); n > 20 {
			data.MissingShown = ov.Fsck.Missing[:20]
			more := uint64(n - 20)
			if ov.Fsck.MissingTotal > uint64(20) {
				more = ov.Fsck.MissingTotal - 20
			}
			data.MissingMore = more
		} else {
			data.MissingShown = ov.Fsck.Missing
		}
	}
	// Manifest/Local rows: the wire carries zero-value numbers for "absent";
	// a card prints the value it has, never "undefined" (spec §6.2 item 4).
	lastPush := ""
	if ov.Manifest.LastPush != nil {
		lastPush = rfc3339(*ov.Manifest.LastPush)
	}
	data.Manifest = []walKV{
		{Label: "version", Value: strconv.FormatUint(ov.Manifest.Version, 10)},
		{Label: "next_seq", Value: strconv.FormatUint(ov.Manifest.NextSeq, 10)},
		{Label: "min_seq", Value: strconv.FormatUint(ov.Manifest.MinSeq, 10)},
		{Label: "entries", Value: strconv.FormatUint(ov.Manifest.Entries, 10)},
		{Label: "tail entries", Value: strconv.FormatUint(ov.Manifest.TailEntries, 10)},
		{Label: "checkpoint", Value: anyLabel(ov.Manifest.Checkpoint, "@")},
		{Label: "packset", Value: anyLabel(ov.Manifest.Packset, "")},
		{Label: "last push", Stamp: lastPush},
	}
	for i := range data.Manifest {
		if data.Manifest[i].Value == "" && data.Manifest[i].Stamp == "" {
			data.Manifest[i].Value = "—"
		}
	}
	data.Local = []walKV{
		{Label: "version", Value: strconv.FormatUint(ov.Local.Version, 10)},
		{Label: "next_seq", Value: strconv.FormatUint(ov.Local.NextSeq, 10)},
		{Label: "bootstrapped", Value: strconv.FormatBool(ov.Local.Bootstrap)},
		{Label: "reconciled", Value: strconv.FormatBool(ov.Local.Reconciled)},
		{Label: "size", Value: fmtBytes(ov.Local.SizeBytes)},
	}
	data.Packs = fmt.Sprintf("%d live packs · %s · %d pushes recorded",
		ov.Packs.Live, fmtBytes(ov.Packs.LiveBytes), ov.Packs.Pushes)
	data.Bundles = bundleNodes(ov.Bundles)
	data.BundleNone = len(ov.Bundles) == 0
	built, skipped, small, unavailable := 0, 0, 0, 0
	for _, sl := range ov.BundlePlan.Slots {
		switch sl.Status {
		case "built":
			built++
		case "skipped":
			skipped++
		case "too_small", "too-small":
			small++
		default:
			unavailable++
		}
		data.PlanRows = append(data.PlanRows, planRow{
			Strategy: sl.Strategy,
			Slot:     sl.Slot,
			Status:   sl.Status,
			Chip:     chipTone(sl.Status),
			Detail:   sl.Detail,
			Bundle:   shortSHA(sl.BundleID),
		})
	}
	data.PlanCounts = fmt.Sprintf("built %d · skipped %d · too-small %d · unavailable %d", built, skipped, small, unavailable)
	for _, cp := range ov.Compactions {
		data.Compactions = append(data.Compactions, compactionRow{
			Seq:   cp.Seq,
			Stamp: rfc3339(cp.At),
			At:    cp.At.UTC().Format("2006-01-02 15:04"),
			Packs: strconv.Itoa(cp.Before) + " → " + strconv.Itoa(cp.After),
			Host:  cp.Hostname,
		})
	}
	// The segments wire field is []string (api.ManifestInfo) — keys, not the
	// seq/pack/entries objects the pre-migration JSX expected. Render what
	// exists: one row per segment key, newest last, 5 by default + "all".
	segs := ov.Manifest.Segments
	data.SegmentAll = r.URL.Query().Get("all") == "1"
	if !data.SegmentAll && len(segs) > 5 {
		data.Segments = segs[len(segs)-5:]
		data.SegmentMore = true
	} else {
		data.Segments = segs
	}

	// Ops inventory (api.Tasks): the wire is {op,params[{name,values[]}]} —
	// there is no kind/doc, so params render as an inventory (checkbox vs
	// select vs text is derived from values, spec §6.2 item 3). Running an op
	// is a POST stream (the islands group's form); no dead buttons here.
	if s.env.API.Tasks != nil {
		for _, op := range s.env.API.Tasks.Ops() {
			row := walOp{Op: op.Op}
			for _, p := range op.Params {
				wp := walParam{Name: p.Name, Values: p.Values}
				switch {
				case len(p.Values) == 0:
					wp.Kind, wp.Required = "text", true
				case len(p.Values) == 1 && p.Values[0] == "1":
					wp.Kind = "checkbox"
				default:
					wp.Kind = "select"
				}
				row.Params = append(row.Params, wp)
			}
			data.Ops = append(data.Ops, row)
		}
		data.OpsAvailable = len(data.Ops)
		if _, recent, terr := s.env.API.Tasks.List(ctx, rq.id); terr == nil {
			for _, t := range recent {
				data.Recent = append(data.Recent, walTask{
					Label: t.Kind,
					Kind:  t.Kind,
					State: t.Summary,
					Stamp: t.Finished,
					OK:    t.OK,
				})
			}
			data.OpsRecent = len(data.Recent)
		}
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/wal", "WAL · "+c.Full, data)
}

// chipTone maps a status word to the shared chip tone (Wal.jsx CHIP_TONES).
func chipTone(status string) string {
	switch status {
	case "ok", "built":
		return "chip chip-open"
	case "error", "failed", "unavailable":
		return "chip chip-closed"
	case "degraded", "skipped", "too_small", "too-small":
		return "chip chip-draft"
	}
	return "chip chip-closed"
}

// anyLabel renders an `any` wire field (checkpoint/packset): a map's seq /
// count when present, else its printed form; nil → "".
func anyLabel(v any, prefix string) string {
	switch x := v.(type) {
	case nil:
		return ""
	case map[string]any:
		for _, k := range []string{"seq", "count"} {
			if n, ok := x[k]; ok {
				return prefix + fmt.Sprint(n)
			}
		}
		if packs, ok := x["packs"].([]any); ok {
			return strconv.Itoa(len(packs))
		}
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// bundleNodes builds the bundle chain (Wal.jsx BundleChain): roots are full
// bundles plus incrementals whose base vanished (orphans, warned), children
// nest under base_id sorted by creation_token.
func bundleNodes(bundles []api.BundleInfo) []bundleNode {
	known := make(map[string]bool, len(bundles))
	byBase := map[string][]api.BundleInfo{}
	for _, b := range bundles {
		known[b.SHA] = true
		if b.BaseID != "" {
			byBase[b.BaseID] = append(byBase[b.BaseID], b)
		}
	}
	var build func(b api.BundleInfo) bundleNode
	build = func(b api.BundleInfo) bundleNode {
		n := bundleNode{
			Short:        shortSHA(b.SHA),
			StrategyKind: b.Strategy + "/" + b.Kind,
			Size:         fmtBytes(b.Size),
			Token:        b.CreationToken,
			Orphan:       b.Kind != "full" && b.BaseID != "" && !known[b.BaseID],
		}
		if n.Token == "" {
			n.Token = "—"
		}
		kids := byBase[b.SHA]
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].CreationToken < kids[j].CreationToken })
		for _, k := range kids {
			n.Children = append(n.Children, build(k))
		}
		return n
	}
	var out []bundleNode
	for _, b := range bundles {
		if b.BaseID == "" || b.Kind == "full" || !known[b.BaseID] {
			out = append(out, build(b))
		}
	}
	return out
}
