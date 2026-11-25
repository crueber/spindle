// forms.go — the create/import/manage form family (docs/go/12_web_ui.md §2,
// D-WEB-14; migration group `forms`): /new, /import, /orgs/new,
// /{owner}/{name}/fork, /{owner}/{name}/releases/new,
// /{owner}/{name}/issues/new, /{owner}/{name}/pulls/new,
// /{owner}/teams/{slug}, /{owner}/{name}/labels, /{owner}/{name}/milestones.
//
// WIRING (composition, cmd/spindle — this file never edits env.go):
//
//	webui.BindForms(webui.FormsDeps{
//	    Identity: ident,   // *identity.Service
//	    Issues:   iss,     // *issues.Service (needs ListLabels/ListMilestones exported — see GAPS)
//	    Pulls:    pl,      // *pulls.Service
//	    Releases: rel,     // *releases.Service
//	    Import:   imp,     // *repoimport.Service
//	    Mirror:   mir,     // *mirror.Service
//	})
//
// Every dep is nil-tolerant: a missing service renders a 503 page instead of
// panicking (the API's "not configured" shape). The core reads (repo summary,
// refs, commits, registry, object store) come from webui.Env.API, exactly like
// the JSON API, so a page and its API twin cannot drift on data.
//
// GAPS (reported to the foundation owner; nothing here invents a service
// method):
//   - internal/issues now exports ListLabels/ListMilestones for exactly this
//     surface (the handlers used to call the package-private loaders).
//   - internal/webui has no flash transport, HX helpers, same-origin check,
//     or `icon` template func yet (the migration PLAN calls them foundation
//     work). This file carries the minimum it needs, self-contained:
//     formsOriginOK, formsRedirect (HX-Redirect aware), a `?flash=` notice
//     channel, and two inline decorative SVGs in its own templates. Swap
//     them for the foundation helpers when those land.
//
// SPEC DECISIONS taken where the spec left a question open (§8):
//   - Fork POST redirects to the child repo immediately instead of holding
//     the request up to 60 s polling the child manifest (the task is
//     narrated; the repo page is the progress surface). §8.6.
//   - PullNew has no SSE ref picker and no htmx preview: native <select>s fed
//     server-side, and a `preview` submit action that re-renders the page
//     with the comparison. No island, no debounce. §8.8.
//   - IssueNew preview is a `preview=1` submit that re-renders the page with
//     the server-rendered body (no fragment endpoint, no island). §8.3.
//   - Labels use native controls (preset <select> + hex input), no popover.
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/git"
	"github.com/crueber/spindle/internal/httpsec"
	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/mirror"
	"github.com/crueber/spindle/internal/moderation"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/releases"
	"github.com/crueber/spindle/internal/repoimport"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// --- feature-service seam ----------------------------------------------------

// FormsIdentity is the identity surface these pages read and write.
type FormsIdentity interface {
	MemberOrgsFor(ctx context.Context, p auth.Principal) ([]string, error)
	CheckCreateOwner(ctx context.Context, owner string, p auth.Principal) *auth.AuthError
	CheckOrgOwner(ctx context.Context, org string, p auth.Principal) *auth.AuthError
	CreateOrg(ctx context.Context, org, displayName, description, creator string) (*identity.Org, error)
	GetTeam(ctx context.Context, org, slug string) (*identity.Team, store.Version, error)
	SetTeamMember(ctx context.Context, org, slug, principal string) (*identity.Team, error)
	RemoveTeamMember(ctx context.Context, org, slug, principal string) (*identity.Team, error)
	Resolve(ctx context.Context, owner, repo string, p auth.Principal) (identity.Role, *identity.AccessDoc)
}

// FormsIssues is the issues surface these pages read and write.
//
// ListLabels/ListMilestones are the GAP named in the file header: the real
// *issues.Service answers them only through unexported reads today.
type FormsIssues interface {
	CreateIssue(ctx context.Context, owner, repo string, actor auth.Principal, title, body string, meta *issues.IssueMeta) (*issues.Thread, *issues.Event, error)
	ListIssues(ctx context.Context, owner, repo string, p auth.Principal, f issues.ListFilter) (*issues.ListResult, error)
	ListLabels(ctx context.Context, owner, repo string) ([]issues.Label, error)
	CreateLabel(ctx context.Context, owner, repo string, actor auth.Principal, name, color, desc string) (*issues.Label, error)
	DeleteLabel(ctx context.Context, owner, repo string, actor auth.Principal, name string) (int, error)
	ListMilestones(ctx context.Context, owner, repo string) ([]*issues.Milestone, error)
	CreateMilestone(ctx context.Context, owner, repo string, actor auth.Principal, title, desc string, dueOn *string) (*issues.Milestone, error)
	UpdateMilestone(ctx context.Context, owner, repo string, actor auth.Principal, id string, title, desc, dueOn, state *string) (*issues.Milestone, error)
	DeleteMilestone(ctx context.Context, owner, repo string, actor auth.Principal, id string) error
}

// FormsPulls is the pulls surface: fork creation, the fork list (the PR
// form's head picker) and PR opening.
type FormsPulls interface {
	StartFork(ctx context.Context, owner, repo string, actor auth.Principal, in pulls.ForkInput) (*pulls.TaskRecord, string, error)
	ListForks(ctx context.Context, owner, repo string, actor auth.Principal, after string, n int) (*pulls.ForkListPage, error)
	OpenPR(ctx context.Context, owner, repo string, actor auth.Principal, in pulls.OpenInput, correlationID string) (*pulls.Thread, *pulls.PRDoc, error)
}

// FormsReleases is the releases surface: the composer write + autodraft read.
type FormsReleases interface {
	PutRelease(ctx context.Context, owner, repo string, p auth.Principal, tag string, in releases.ReleaseInput, expected store.Version) (*releases.Release, bool, error)
	Autodraft(ctx context.Context, owner, repo string, p auth.Principal, tag, since string) (*releases.Autodraft, error)
}

// FormsImport is the one-shot import starter + its status observer. State
// answers the SAME id Begin handed out (the service id the API endpoint
// accepts), which is what lets the page skip a progress card for an import
// that already finished before it rendered.
type FormsImport interface {
	Begin(ctx context.Context, p auth.Principal, params repoimport.Params, token string) (*repoimport.BeginResult, *wal.TaskRecord, error)
	State(id string) (repoimport.TaskState, bool)
}

// FormsMirror is the mirror sync starter (the creation side uses the
// exported mirror.Create + repoimport validation helpers directly).
type FormsMirror interface {
	SyncAsync(ctx context.Context, owner, name, token string, force bool) string
}

// FormsDeps is the feature-service bag injected by composition.
type FormsDeps struct {
	Identity FormsIdentity
	Issues   FormsIssues
	Pulls    FormsPulls
	Releases FormsReleases
	Import   FormsImport
	Mirror   FormsMirror
}

var formsDeps FormsDeps

// BindForms installs the feature services (called once at boot).
func BindForms(d FormsDeps) { formsDeps = d }

// --- routes ------------------------------------------------------------------

func init() { registerGroup(mountForms) }

// mountForms registers this group's paths. Literal segments beat wildcards in
// the Go 1.22 mux, so /new, /import and /orgs/new never fall into /{owner} or
// /{owner}/{name}; each repo-scoped path is a literal child of the repo shell.
func mountForms(s *Server) {
	s.mux.HandleFunc("GET /new", s.formsNew)
	s.mux.HandleFunc("POST /new", s.formsNewPost)
	s.mux.HandleFunc("GET /import", s.formsImport)
	s.mux.HandleFunc("POST /import", s.formsImportPost)
	s.mux.HandleFunc("GET /orgs/new", s.formsOrgNew)
	s.mux.HandleFunc("POST /orgs/new", s.formsOrgNewPost)

	s.mux.HandleFunc("GET /{owner}/{name}/fork", s.formsFork)
	s.mux.HandleFunc("POST /{owner}/{name}/fork", s.formsForkPost)

	s.mux.HandleFunc("GET /{owner}/{name}/releases/new", s.formsReleaseNew)
	s.mux.HandleFunc("POST /{owner}/{name}/releases/new", s.formsReleaseNewPost)

	s.mux.HandleFunc("GET /{owner}/{name}/issues/new", s.formsIssueNew)
	s.mux.HandleFunc("POST /{owner}/{name}/issues/new", s.formsIssueNewPost)

	s.mux.HandleFunc("GET /{owner}/{name}/pulls/new", s.formsPullNew)
	s.mux.HandleFunc("POST /{owner}/{name}/pulls/new", s.formsPullNewPost)

	s.mux.HandleFunc("GET /{owner}/{name}/labels", s.formsLabels)
	s.mux.HandleFunc("POST /{owner}/{name}/labels", s.formsLabelsPost)
	s.mux.HandleFunc("POST /{owner}/{name}/labels/delete", s.formsLabelDelete)
	s.mux.HandleFunc("DELETE /{owner}/{name}/labels/{label}", s.formsLabelDelete)

	s.mux.HandleFunc("GET /{owner}/{name}/milestones", s.formsMilestones)
	s.mux.HandleFunc("POST /{owner}/{name}/milestones", s.formsMilestonesPost)
	s.mux.HandleFunc("POST /{owner}/{name}/milestones/{id}/state", s.formsMilestoneState)
	s.mux.HandleFunc("POST /{owner}/{name}/milestones/{id}/delete", s.formsMilestoneDelete)
	s.mux.HandleFunc("DELETE /{owner}/{name}/milestones/{id}", s.formsMilestoneDelete)

	// Team pages: /{owner}/teams/{slug} has the SAME shape as every migrated
	// /{owner}/{name}/{tab} page, and Go's ServeMux refuses two overlapping
	// wildcard patterns (neither is more specific). The team routes are
	// therefore answered by one dispatcher per (method, segment count); the
	// dispatcher writes nothing for a path it does not own, which Serve
	// treats as "not migrated" and hands back to the server. Literal repo
	// routes stay registered above and always win over these.
	s.mux.HandleFunc("GET /{a}/{b}/{c}", s.formsDispatchGet)
	s.mux.HandleFunc("POST /{a}/{b}/{c}/{d}", s.formsDispatchPost4)
	s.mux.HandleFunc("POST /{a}/{b}/{c}/{d}/{e}", s.formsDispatchPost5)
	s.mux.HandleFunc("PUT /{a}/{b}/{c}/{d}/{e}", s.formsDispatchPut5)
	s.mux.HandleFunc("DELETE /{a}/{b}/{c}/{d}/{e}", s.formsDispatchDelete5)
}

// formsDispatchGet answers GET /{owner}/teams/{slug}; everything else is not
// ours (no write → the caller falls back).
func (s *Server) formsDispatchGet(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("b") != "teams" {
		return
	}
	r.SetPathValue("owner", r.PathValue("a"))
	r.SetPathValue("slug", r.PathValue("c"))
	s.formsTeam(w, r)
}

// formsDispatchPost4 answers POST /{owner}/teams/{slug}/members.
func (s *Server) formsDispatchPost4(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("b") != "teams" || r.PathValue("d") != "members" {
		return
	}
	r.SetPathValue("owner", r.PathValue("a"))
	r.SetPathValue("slug", r.PathValue("c"))
	s.formsTeamAdd(w, r)
}

// formsDispatchPost5 answers POST /{owner}/teams/{slug}/members/remove.
func (s *Server) formsDispatchPost5(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("b") != "teams" || r.PathValue("d") != "members" || r.PathValue("e") != "remove" {
		return
	}
	r.SetPathValue("owner", r.PathValue("a"))
	r.SetPathValue("slug", r.PathValue("c"))
	s.formsTeamRemove(w, r)
}

// formsDispatchPut5 answers PUT /{owner}/teams/{slug}/members/{principal}.
func (s *Server) formsDispatchPut5(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("b") != "teams" || r.PathValue("d") != "members" {
		return
	}
	r.SetPathValue("owner", r.PathValue("a"))
	r.SetPathValue("slug", r.PathValue("c"))
	r.SetPathValue("principal", r.PathValue("e"))
	s.formsTeamAdd(w, r)
}

// formsDispatchDelete5 answers DELETE /{owner}/teams/{slug}/members/{principal}.
func (s *Server) formsDispatchDelete5(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("b") != "teams" || r.PathValue("d") != "members" {
		return
	}
	r.SetPathValue("owner", r.PathValue("a"))
	r.SetPathValue("slug", r.PathValue("c"))
	r.SetPathValue("principal", r.PathValue("e"))
	s.formsTeamRemove(w, r)
}

// --- shared mutation plumbing (FOUNDATION GAP: see the file header) ---------

// formsOriginOK rejects a cross-origin write. Origin wins when present;
// otherwise Referer. A request carrying neither fails closed: every instance
// has a cookie-backed OIDC session that could authenticate it (PLAN-OIDC-ONLY
// removed the auth-less boot). This is the same rule the collab and public
// groups apply.
func (s *Server) formsOriginOK(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return formsSameHost(r, o)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return formsSameHost(r, ref)
	}
	return false
}

func formsSameHost(r *http.Request, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// formsRedirect answers a successful write: 303 for a browser form, or 200 +
// HX-Redirect for an htmx request (never a partial swap of a whole page).
func (s *Server) formsRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		h := w.Header()
		h.Set("HX-Redirect", target)
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// formsRedirectFlash redirects carrying a one-line success notice in the
// query (the flash transport this package has until the foundation lands a
// session-scoped one). The receiving page reads it through formsBase.
func (s *Server) formsRedirectFlash(w http.ResponseWriter, r *http.Request, target, msg string) {
	if msg != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + "flash=" + url.QueryEscape(msg)
	}
	s.formsRedirect(w, r, target)
}

const formsFlashMax = 240

func formsFlash(r *http.Request) string {
	msg := strings.TrimSpace(r.URL.Query().Get("flash"))
	if len(msg) > formsFlashMax {
		msg = msg[:formsFlashMax]
	}
	return msg
}

// formsSanitizeNext mirrors the login interstitial's server-side rule through
// the shared helper: a rooted same-host path only (no scheme, "//host",
// "/\host" or control characters).
func formsSanitizeNext(next string) string {
	return httpsec.SafeNext(next, "/")
}

// formsLoginRequired builds the anonymous-write interstitial target for the
// current request (path + query preserved, sanitized).
func formsLoginRequired(r *http.Request, action string) string {
	next := r.URL.Path
	if q := r.URL.RawQuery; q != "" {
		next += "?" + q
	}
	href := "/login-required?next=" + url.QueryEscape(formsSanitizeNext(next))
	if action != "" {
		href += "&action=" + url.QueryEscape(action)
	}
	return href
}

// --- shared page model -------------------------------------------------------

// errFormsNoIssues is the missing-service marker both list pages surface (the
// API's "issues service not configured" 503 shape).
var errFormsNoIssues = errors.New("issues service not configured")

// formsBase is embedded in every page model, so one partial renders the
// notices and the shared error card.
type formsBase struct {
	Flash      string
	Err        string
	Winner     string // path for a 409 "take me there" link
	Anonymous  bool   // signed out under token/oidc: writes go to the interstitial
	NoWrite    bool   // signed in without the write flag: the server will refuse
	GateHref   string // interstitial link for the anonymous notice
	GateNotice string // e.g. "sign in to create a repository"
}

// setErr keeps the inline-error channel one shape across page models.
func (b *formsBase) setErr(msg string) { b.Err = msg }

// formsAnonymousViewer is lib/writeGate.js isAnonymousViewer on the server:
// a signed-out viewer (OIDC is the only authentication).
func (s *Server) formsAnonymousViewer(p auth.Principal) bool {
	return p.Anonymous
}

func (s *Server) formsBase(r *http.Request, p auth.Principal, action string) formsBase {
	anon := s.formsAnonymousViewer(p)
	b := formsBase{
		Flash:     formsFlash(r),
		Anonymous: anon,
		NoWrite:   !p.Anonymous && !p.Write,
	}
	if anon {
		b.GateHref = formsLoginRequired(r, action)
		b.GateNotice = "sign in to " + formsLowerFirst(action)
	}
	return b
}

// formsLowerFirst lowercases the first rune of a short label (the
// interstitial copy: "Create a new repository" → "create a new repository").
func formsLowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] += 'a' - 'A'
	}
	return string(r)
}

// formsCheckWrite is the shared POST preflight: same-origin, then the
// anonymous-write interstitial (a 303, never an in-band 200 — law 9).
func (s *Server) formsCheckWrite(w http.ResponseWriter, r *http.Request, p auth.Principal, action string) bool {
	if !s.formsOriginOK(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin request refused")
		return false
	}
	if s.formsAnonymousViewer(p) {
		s.formsRedirect(w, r, formsLoginRequired(r, action))
		return false
	}
	return true
}

// formsNeedWrite refuses a create-namespace write without the write flag
// (the API's AuthWrite gate for POST /api/v1/repos, /imports, /orgs).
func (s *Server) formsNeedWrite(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	if p.Write {
		return true
	}
	s.errorPage(w, r, http.StatusForbidden, "write access required")
	return false
}

// formsRepo parses the route's owner/name and renders 404 on a bad id.
func (s *Server) formsRepo(w http.ResponseWriter, r *http.Request) (owner, name string, ok bool) {
	owner, name = r.PathValue("owner"), r.PathValue("name")
	if _, err := git.ParseRepoId(owner + "/" + name); err != nil {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return "", "", false
	}
	return owner, name, true
}

// formsRepoPage is the shared GET gate for a repo-scoped page: authentication
// (anonymous_read) and then repo visibility. An invisible repo is a 404 so
// the page cannot leak existence.
func (s *Server) formsRepoPage(w http.ResponseWriter, r *http.Request) (auth.Principal, string, string, bool) {
	p, ok := s.gated(w, r)
	if !ok {
		return p, "", "", false
	}
	owner, name, ok := s.formsRepo(w, r)
	if !ok {
		return p, "", "", false
	}
	if !s.repoVisible(r.Context(), owner, name, p) {
		s.errorPage(w, r, http.StatusNotFound, "not found")
		return p, "", "", false
	}
	return p, owner, name, true
}

// formsRepoPageChrome is formsRepoPage plus the repository chrome the layout
// renders above the form (12_web_ui.md §2.2.1). The chrome is built only
// AFTER the gate passed (never before: a denied request must not build — or
// leak — repository metadata), and is nil when the repo read fails.
func (s *Server) formsRepoPageChrome(w http.ResponseWriter, r *http.Request) (auth.Principal, string, string, *RepoChrome, bool) {
	p, owner, name, ok := s.formsRepoPage(w, r)
	if !ok {
		return p, owner, name, nil, false
	}
	return p, owner, name, s.repoChromeFor(r, p, owner, name), true
}

// formsSummary reads the repo summary (features, head, counts); ok=false
// means "unknown" and callers fail open on features.
func (s *Server) formsSummary(r *http.Request, owner, name string) (api.SummaryData, bool) {
	if s.env.API == nil || s.env.API.Repo == nil {
		return api.SummaryData{}, false
	}
	id, err := git.ParseRepoId(owner + "/" + name)
	if err != nil {
		return api.SummaryData{}, false
	}
	sum, err := s.env.API.Repo.Summary(r.Context(), id)
	if err != nil {
		return api.SummaryData{}, false
	}
	return sum, true
}

// formsFeatureEnabled projects one feature flag, failing OPEN exactly like
// the API summary (only an explicit false disables).
func formsFeatureEnabled(sum api.SummaryData, feature string) bool {
	f := sum.Features
	if f == nil {
		return true
	}
	switch feature {
	case "issues":
		return f.Issues
	case "pulls":
		return f.Pulls
	case "releases":
		return f.Releases
	case "forks":
		return f.Forks
	}
	return true
}

// formsBranches lists branch names (up to 100) for a picker.
func (s *Server) formsBranches(r *http.Request, owner, name string) []string {
	if s.env.API == nil || s.env.API.Repo == nil {
		return nil
	}
	id, err := git.ParseRepoId(owner + "/" + name)
	if err != nil {
		return nil
	}
	refs, _, err := s.env.API.Repo.RefList(r.Context(), id, "heads", api.RefQuery{N: 100})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Name)
	}
	return out
}

// formsTags lists release tag names (newest ref order, up to 100).
func (s *Server) formsTags(r *http.Request, owner, name string) []string {
	if s.env.API == nil || s.env.API.Repo == nil {
		return nil
	}
	id, err := git.ParseRepoId(owner + "/" + name)
	if err != nil {
		return nil
	}
	refs, _, err := s.env.API.Repo.RefList(r.Context(), id, "tags", api.RefQuery{N: 100})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, formsShortTag(ref.Name))
	}
	return out
}

func formsShortTag(ref string) string { return strings.TrimPrefix(ref, "refs/tags/") }

func formsShortRef(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }

// formsRole returns the viewer's resolved role at a repo ("" = none) and
// whether the identity service is wired at all.
func (s *Server) formsRole(r *http.Request, owner, name string, p auth.Principal) (identity.Role, bool) {
	if formsDeps.Identity == nil {
		return "", false
	}
	role, _ := formsDeps.Identity.Resolve(r.Context(), owner, name, p)
	return role, true
}

var formsRoleRank = map[identity.Role]int{
	identity.RoleRead:     1,
	identity.RoleTriage:   2,
	identity.RoleWrite:    3,
	identity.RoleMaintain: 4,
	identity.RoleAdmin:    5,
}

// formsRoleAtLeast is the P6 ladder comparison (nil identity → true: the
// service is the authority, the page is only an affordance).
func formsRoleAtLeast(role identity.Role, want identity.Role) bool {
	return formsRoleRank[role] >= formsRoleRank[want]
}

// --- shared error mapping ----------------------------------------------------

// formsErrStatus maps a service error onto the page status (the API's
// statusFor tables, unified here because the page renders every feature).
func formsErrStatus(err error) int {
	var aerr *auth.AuthError
	if errors.As(err, &aerr) {
		switch aerr.Kind {
		case auth.ErrForbidden:
			return http.StatusForbidden
		case auth.ErrUnavailable:
			return http.StatusServiceUnavailable
		default:
			return http.StatusUnauthorized
		}
	}
	var serr *repoimport.StatusError
	if errors.As(err, &serr) && serr.Status != 0 {
		return serr.Status
	}
	// Any error carrying its own HTTP status (e.g. api.ProfileError from the
	// owner-profile seam) maps directly; the page never parses message text.
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) && sc.StatusCode() != 0 {
		return sc.StatusCode()
	}
	var bad formsBadRequest
	if errors.As(err, &bad) {
		return http.StatusBadRequest
	}
	switch {
	case errors.Is(err, issues.ErrInvalid), errors.Is(err, pulls.ErrInvalid),
		errors.Is(err, releases.ErrInvalid), errors.Is(err, identity.ErrInvalid),
		errors.Is(err, moderation.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, issues.ErrConflict), errors.Is(err, pulls.ErrConflict),
		errors.Is(err, releases.ErrConflict), errors.Is(err, identity.ErrConflict),
		errors.Is(err, moderation.ErrConflict), store.IsPreconditionFailed(err):
		return http.StatusConflict
	case errors.Is(err, issues.ErrNotFound), errors.Is(err, pulls.ErrNotFound),
		errors.Is(err, releases.ErrNotFound), errors.Is(err, identity.ErrNotFound),
		errors.Is(err, moderation.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, issues.ErrForbidden), errors.Is(err, pulls.ErrForbidden),
		errors.Is(err, releases.ErrForbidden), errors.Is(err, moderation.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, issues.ErrUnauthorized), errors.Is(err, pulls.ErrUnauthorized),
		errors.Is(err, releases.ErrUnauthorized), errors.Is(err, moderation.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, identity.ErrTooLarge):
		// The avatar-upload verbs' cap (identity.PutUserAvatarBytes /
		// PutOrgAvatar) answers 413 like the API twin.
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, identity.ErrUnsupportedMedia):
		// Magic-sniff allowlist miss (415); SVG is rejected by design.
		return http.StatusUnsupportedMediaType
	case errors.Is(err, pulls.ErrUnavailable), errors.Is(err, releases.ErrUnavailable):
		return http.StatusServiceUnavailable
	}
	return http.StatusServiceUnavailable
}

// formsBadRequest is a page-local 400 (bad form input the services never see).
type formsBadRequest string

func (e formsBadRequest) Error() string { return string(e) }

// formsErrMessage is the server text, verbatim (the spec's §5 rule). An
// anonymous 401 is never rendered: the caller redirects to the interstitial.
func formsErrMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// formsFail re-renders a page with the service error inline, or falls back to
// the shared error page for an unexpected 5xx. chrome is the page's repository
// chrome (nil for the owner/org-scoped forms).
func (s *Server) formsFail(w http.ResponseWriter, r *http.Request, p auth.Principal, chrome *RepoChrome, name, title string, data any, err error) {
	st := formsErrStatus(err)
	if st >= 500 {
		s.errorPage(w, r, st, formsErrMessage(err))
		return
	}
	setFormsErr(data, formsErrMessage(err))
	s.pageRepo(w, r, p, chrome, st, name, title, data)
}

// setFormsErr writes the inline error into a page model. Every model embeds
// formsBase, whose setErr method the pointer inherits. A model that carries a
// submitted secret (the import source token) also gets scrubSecrets, so a
// rejected form can never echo it back into HTML.
func setFormsErr(data any, msg string) {
	if d, ok := data.(interface{ setErr(string) }); ok {
		d.setErr(msg)
	}
	if d, ok := data.(interface{ scrubSecrets() }); ok {
		d.scrubSecrets()
	}
}

// --- owner options -----------------------------------------------------------

// formsOwnerPart mirrors git.validPart for the owner half (lib/orgs.js
// isValidOwnerPart): an @-carrying principal can never be an owner (#370).
func formsOwnerPart(s string) bool {
	if len(s) == 0 || len(s) > 100 || s == ".." || s[0] == '.' {
		return false
	}
	for i := range s {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// formsOwnerOptions is [self, ...member orgs] (sorted, deduped, valid owner
// segments only). The SERVER stays authoritative; this is the dropdown.
func (s *Server) formsOwnerOptions(r *http.Request, p auth.Principal) []string {
	if p.Anonymous || formsDeps.Identity == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(o string) {
		if o == "" || seen[o] || !formsOwnerPart(o) {
			return
		}
		seen[o] = true
		out = append(out, o)
	}
	self := strings.TrimSpace(p.Name)
	orgs, err := formsDeps.Identity.MemberOrgsFor(r.Context(), p)
	if err != nil {
		orgs = nil
	}
	sort.Strings(orgs)
	add(self)
	for _, o := range orgs {
		add(o)
	}
	return out
}

// formsClampOwner keeps the requested owner inside the admitted set (a stale
// ?owner= preset or a foreign org falls back to self).
func formsClampOwner(owners []string, want string) string {
	want = strings.TrimSpace(want)
	if want != "" {
		for _, o := range owners {
			if o == want {
				return want
			}
		}
	}
	if len(owners) > 0 {
		return owners[0]
	}
	return ""
}

// --- validation mirrors (the lib/*.js rules, verbatim copy) ------------------

// formsRepoNameErr mirrors sdk/create.js validateRepoName (server re-validates
// through git.ParseRepoId).
func formsRepoNameErr(owner, name string) (string, string, string) {
	o := strings.TrimSpace(owner)
	n := strings.TrimSpace(strings.TrimSuffix(name, ".git"))
	part := func(v string) bool {
		if len(v) < 1 || len(v) > 100 || v == ".." || strings.HasPrefix(v, ".") {
			return false
		}
		for i := range v {
			c := v[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '.' || c == '_' || c == '-' {
				continue
			}
			return false
		}
		return true
	}
	switch {
	case o == "" || n == "":
		return o, n, "owner and name are required"
	case !part(o):
		return o, n, fmt.Sprintf("invalid owner %q", o)
	case !part(n):
		return o, n, fmt.Sprintf("invalid name %q", n)
	}
	return o, n, ""
}

// formsNameCharsErr mirrors lib/repo-name.js validateRepoChars: empty stays ""
// (required rides the disabled submit), invalid returns the rule text.
func formsNameCharsErr(name string) string {
	raw := strings.TrimSpace(name)
	if raw == "" {
		return ""
	}
	n := strings.TrimSuffix(raw, ".git")
	if n == "" || n == ".." || strings.HasPrefix(n, ".") || len(n) > 100 {
		return formsRepoNameRule
	}
	for i := range n {
		c := n[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		return formsRepoNameRule
	}
	return ""
}

const formsRepoNameRule = "repository names are letters, digits, and . _ -, 1–100 characters, no leading dot"

// formsUIReservedOwner mirrors sdk/create.js isUiRouteCollision.
func formsUIReservedOwner(owner string) bool {
	switch strings.ToLower(strings.TrimSpace(owner)) {
	case "import", "api", "keys", "setup", "explore", "how-it-works", "new", "notifications":
		return true
	}
	return false
}

// formsOrgNameErr mirrors lib/orgs.js validateOrgName (identity.ValidOrg
// re-validates server-side).
func formsOrgNameErr(name string) (string, string) {
	org := strings.ToLower(strings.TrimSpace(name))
	if org == "" {
		return "", "organization name is required"
	}
	if !identity.ValidOrg(org) {
		return org, "organization names are lowercase letters, digits, and hyphens, 1–39 characters"
	}
	return org, ""
}

// formsVisibility validates the create/fork visibility spelling.
func formsVisibility(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "":
		return "public", true
	case "public", "authenticated", "private":
		return v, true
	}
	return "", false
}

// --- /new --------------------------------------------------------------------

type formsNewData struct {
	formsBase
	Owners     []string
	Owner      string
	Name       string
	Format     string
	Visibility string
	NameErr    string
	Collision  bool
}

func (s *Server) formsNewDataFor(r *http.Request, p auth.Principal, owner, name, format, visibility string) *formsNewData {
	owners := s.formsOwnerOptions(r, p)
	if owner == "" {
		owner = formsClampOwner(owners, r.URL.Query().Get("owner"))
	}
	if format == "" {
		format = "sha1"
	}
	if visibility == "" {
		visibility = "public"
	}
	return &formsNewData{
		formsBase:  s.formsBase(r, p, "Create a new repository"),
		Owners:     owners,
		Owner:      owner,
		Name:       name,
		Format:     format,
		Visibility: visibility,
		NameErr:    formsNameCharsErr(name),
		Collision:  owner != "" && formsUIReservedOwner(owner),
	}
}

func (s *Server) formsNew(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "forms/new", "New repository", s.formsNewDataFor(r, p, "", "", "", ""))
}

func (s *Server) formsNewPost(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Create a new repository") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data := s.formsNewDataFor(r, p,
		r.PostFormValue("owner"), r.PostFormValue("name"),
		r.PostFormValue("format"), r.PostFormValue("visibility"))
	if !s.formsNeedWrite(w, r, p) {
		return
	}
	owner, name, msg := formsRepoNameErr(data.Owner, data.Name)
	data.Owner, data.Name = owner, name
	if msg != "" {
		data.Err = msg
		s.page(w, r, p, http.StatusBadRequest, "forms/new", "New repository", data)
		return
	}
	format, ferr := git.ObjectFormatFrom(data.Format)
	if ferr != nil {
		data.Err = ferr.Error()
		s.page(w, r, p, http.StatusBadRequest, "forms/new", "New repository", data)
		return
	}
	visibility, vok := formsVisibility(data.Visibility)
	if !vok {
		data.Err = "visibility must be public|authenticated|private"
		s.page(w, r, p, http.StatusBadRequest, "forms/new", "New repository", data)
		return
	}
	id, _ := git.ParseRepoId(owner + "/" + name)
	if formsDeps.Identity != nil {
		if aerr := formsDeps.Identity.CheckCreateOwner(r.Context(), id.Owner, p); aerr != nil {
			data.Err = aerr.Why
			s.page(w, r, p, formsAuthStatus(aerr), "forms/new", "New repository", data)
			return
		}
	}
	if s.env.API == nil || s.env.API.Repos == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	if cerr := s.env.API.Repos.Create(r.Context(), id, format); cerr != nil {
		if errors.Is(cerr, api.ErrExists) || store.IsPreconditionFailed(cerr) {
			data.Err = "repository already exists: " + id.String()
			data.Winner = "/" + id.String()
			s.page(w, r, p, http.StatusConflict, "forms/new", "New repository", data)
			return
		}
		s.errorPage(w, r, http.StatusServiceUnavailable, formsErrMessage(cerr))
		return
	}
	if perr := s.formsCreatePlaceholder(r.Context(), id, format, p.Name, visibility); perr != nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, formsErrMessage(perr))
		return
	}
	s.formsRedirect(w, r, "/"+id.String())
}

// formsAuthStatus maps an AuthError to a page status.
func formsAuthStatus(aerr *auth.AuthError) int {
	switch aerr.Kind {
	case auth.ErrForbidden:
		return http.StatusForbidden
	case auth.ErrUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnauthorized
	}
}

// formsCreatePlaceholder completes the create exactly like the API's
// createPlaceholder (placeholder.go): marker + eager access.json, both
// Create-wins/adopt-on-412, then the adoption hint. The API keeps that
// function unexported, so the pieces it publishes (PlaceholderDoc,
// PlaceholderKeySuffix, AccessBoot, PlaceholderHints) are composed here —
// the marker key is the same expression, no new format.
func (s *Server) formsCreatePlaceholder(ctx context.Context, id git.RepoId, format git.ObjectFormat, principal, visibility string) error {
	e := s.env.API
	if e == nil || e.Store == nil {
		return nil
	}
	now := s.env.now()
	doc := api.PlaceholderDoc{
		Version:      1,
		CreatedBy:    principal,
		CreatedAt:    now.UTC().Format("2006-01-02T15:04:05Z07:00"),
		ObjectFormat: format.String(),
	}
	raw, _ := json.Marshal(doc)
	key := id.StorePrefix() + api.PlaceholderKeySuffix
	if _, err := e.Store.Put(ctx, key, store.PutBody{Bytes: raw},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}); err != nil && !store.IsPreconditionFailed(err) {
		return err
	}
	if e.AccessBoot != nil {
		if err := e.AccessBoot.EnsureRepoAccess(ctx, id.Owner, id.Name, principal, visibility); err != nil {
			return err
		}
	}
	e.PlaceholderHints.Add(id)
	return nil
}

// --- /import -----------------------------------------------------------------

// formsImportPreset is one schedule option (MIRROR_PRESETS labels).
type formsImportPreset struct {
	ID    string
	Label string
}

// formsImportDone is the finished/no-op card.
type formsImportDone struct {
	Repo      string
	SourceURL string
	Noop      bool
	Heads     []formsHead
}

type formsHead struct {
	Ref string
	SHA string
}

type formsImportData struct {
	formsBase
	Owners     []string
	Owner      string
	Name       string
	SourceURL  string
	Token      string
	Mode       string
	Schedule   string
	Format     string
	BranchOnly bool
	PullHeads  bool
	Notes      bool
	Dangerous  bool
	Presets    []formsImportPreset
	SourceHint string
	SourceErr  string
	// Progress phase: form | running | done | failed.
	Phase     string
	TaskID    string
	TaskError string
	TaskLog   []string
	Repo      string
	Done      *formsImportDone
}

func formsImportPresets() []formsImportPreset {
	labels := map[string]string{
		"hourly":  "Every hour",
		"8h":      "Every 8 hours",
		"daily":   "Every day",
		"weekly":  "Every week",
		"monthly": "Every month",
	}
	out := make([]formsImportPreset, 0, len(mirror.Presets))
	for _, p := range mirror.Presets {
		out = append(out, formsImportPreset{ID: p, Label: labels[p]})
	}
	return out
}

// formsSourceHint is the client-side normalizeSource display (sdk/import.js):
// the canonical URL for the typed source, or the transport error.
func formsSourceHint(raw string) (canonical, errText string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}
	n, err := repoimport.NormalizeSource(raw)
	if err != nil {
		var serr *repoimport.StatusError
		if errors.As(err, &serr) {
			return "", serr.Message
		}
		return "", ""
	}
	if err := repoimport.ValidateTransport(n, false); err != nil {
		var serr *repoimport.StatusError
		if errors.As(err, &serr) {
			return n.URL, serr.Message
		}
		return n.URL, ""
	}
	return n.URL, ""
}

// scrubSecrets drops the submitted source token before a re-render: the token
// is request-scoped (sent once, never stored, never echoed), so every failure
// card and retry form starts from an empty field. The other form fields stay.
func (d *formsImportData) scrubSecrets() { d.Token = "" }

func (s *Server) formsImportDataFor(r *http.Request, p auth.Principal) *formsImportData {
	owners := s.formsOwnerOptions(r, p)
	data := &formsImportData{
		formsBase: s.formsBase(r, p, "Start an import"),
		Owners:    owners,
		Owner:     formsClampOwner(owners, r.URL.Query().Get("owner")),
		Mode:      "import",
		Schedule:  mirror.DefaultPreset,
		Presets:   formsImportPresets(),
		Phase:     "form",
	}
	return data
}

// formsImportRender renders the import page after scrubbing the submitted
// source token: the request's credential lives only long enough to start the
// task/mirror, never in the HTML that follows (F4).
func (s *Server) formsImportRender(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, data *formsImportData) {
	data.scrubSecrets()
	s.page(w, r, p, status, "forms/import", "Import repository", data)
}

func (s *Server) formsImport(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "forms/import", "Import repository", s.formsImportDataFor(r, p))
}

func (s *Server) formsImportPost(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	data := s.formsImportDataFor(r, p)
	if !s.formsCheckWrite(w, r, p, "Start an import") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data.SourceURL = strings.TrimSpace(r.PostFormValue("source_url"))
	data.Owner = strings.TrimSpace(r.PostFormValue("owner"))
	data.Name = strings.TrimSpace(r.PostFormValue("name"))
	data.Token = r.PostFormValue("token")
	data.Schedule = strings.TrimSpace(r.PostFormValue("schedule"))
	data.Format = strings.TrimSpace(r.PostFormValue("format"))
	data.BranchOnly = r.PostFormValue("default_branch_only") != ""
	data.PullHeads = r.PostFormValue("include_pull_heads") != ""
	data.Notes = r.PostFormValue("include_notes") != ""
	data.Dangerous = r.PostFormValue("dangerous") != ""
	if r.PostFormValue("kind") == "mirror" {
		data.Mode = "mirror"
	}
	if data.Schedule == "" {
		data.Schedule = mirror.DefaultPreset
	}
	data.SourceHint, data.SourceErr = formsSourceHint(data.SourceURL)
	if data.Mode == "mirror" {
		s.formsImportMirror(w, r, p, data)
		return
	}
	if !s.formsNeedWrite(w, r, p) {
		return
	}
	s.formsImportOnce(w, r, p, data)
}

// formsImportWireBody builds the exact POST /api/v1/repos/imports body so the
// server's own ParseRequest (unknown keys, target, source, SSRF, refs, format)
// validates the page — no second rule set.
func formsImportWireBody(data *formsImportData) []byte {
	body := map[string]any{
		"source_url":          data.SourceURL,
		"owner":               data.Owner,
		"name":                data.Name,
		"token":               data.Token,
		"refs":                []string{},
		"default_branch_only": data.BranchOnly,
		"include_pull_heads":  data.PullHeads,
		"include_notes":       data.Notes,
		"format":              data.Format,
		"dangerous":           data.Dangerous,
	}
	raw, _ := json.Marshal(body)
	return raw
}

func (s *Server) formsImportOnce(w http.ResponseWriter, r *http.Request, p auth.Principal, data *formsImportData) {
	params, token, perr := repoimport.ParseRequest(formsImportWireBody(data), s.env.Cfg, s.env.LiveImport)
	if perr != nil {
		s.formsFail(w, r, p, nil, "forms/import", "Import repository", data, perr)
		return
	}
	if formsDeps.Import == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "import service not configured")
		return
	}
	res, rec, berr := formsDeps.Import.Begin(r.Context(), p, params, token)
	if berr != nil {
		s.formsFail(w, r, p, nil, "forms/import", "Import repository", data, berr)
		return
	}
	if res == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "import service not configured")
		return
	}
	if res.NoOp != nil {
		data.Phase = "done"
		data.Done = &formsImportDone{
			Repo:      params.Owner + "/" + params.Name,
			SourceURL: res.NoOp.SourceURL,
			Noop:      true,
			Heads:     formsHeads(res.NoOp.HeadSHAs),
		}
		s.formsImportRender(w, r, p, http.StatusOK, data)
		return
	}
	data.Phase = "running"
	data.TaskID = res.TaskID
	if data.TaskID == "" && rec != nil {
		data.TaskID = rec.ID
	}
	// Fast-import race: a small source can finish between Begin and render,
	// and a spinner for a task that is over is exactly the silent wait law 7
	// forbids. One bounded settle window (no polling loop left behind) turns
	// that into the terminal card — done with its heads, or failed with the
	// summary + log. A slower import renders the live card and the island
	// streams it (replay covers anything that already happened).
	if st, ok := s.formsImportAwait(r.Context(), data.TaskID, formsImportSettle); ok {
		if st.Finished {
			s.formsImportTerminal(w, r, p, data, params, st)
			return
		}
		// Still running: seed the card with whatever the task already
		// narrated so the first paint is not an empty box.
		if st.Record != nil {
			data.TaskLog = append([]string{}, st.Record.LogTail...)
		}
	}
	s.formsImportRender(w, r, p, http.StatusOK, data)
}

// formsImportSettle is the bounded window in which a just-started import may
// reach its terminal state before the POST response is rendered. Small enough
// to stay invisible next to a clone, large enough to catch a tiny source, a
// resume that converges instantly, or a join of an already-finished task.
const formsImportSettle = 400 * time.Millisecond

// formsImportAwait waits up to wait for the import to finish. It returns the
// service's observer view and whether the id is known at all (a nil or
// non-conforming service answers unknown, so the page degrades to the live
// card instead of claiming anything).
func (s *Server) formsImportAwait(ctx context.Context, id string, wait time.Duration) (repoimport.TaskState, bool) {
	deadline := time.Now().Add(wait)
	for {
		st, ok := formsDeps.Import.State(id)
		if !ok || st.Finished || !time.Now().Before(deadline) || ctx.Err() != nil {
			return st, ok
		}
		select {
		case <-ctx.Done():
			return st, ok
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// formsImportTerminal renders the terminal state of an import the page did
// not have to stream: the done card (repo + heads + a link into it) on
// success, the failed card (summary, log tail, retry link) otherwise.
func (s *Server) formsImportTerminal(w http.ResponseWriter, r *http.Request, p auth.Principal, data *formsImportData, params repoimport.Params, st repoimport.TaskState) {
	target := params.Owner + "/" + params.Name
	failed := ""
	if st.Outcome != nil {
		if st.Outcome.Err != nil {
			failed = st.Outcome.Err.Message
		}
		if st.Outcome.Repo != "" {
			target = st.Outcome.Repo
		}
	}
	if failed == "" && st.Record != nil && st.Record.OK != nil && !*st.Record.OK {
		failed = st.Record.Summary
	}
	if failed != "" {
		data.Phase = "failed"
		data.Repo = target
		data.TaskError = failed
		data.Token = "" // never re-echo a source credential into a page
		if st.Record != nil {
			data.TaskLog = append([]string{}, st.Record.LogTail...)
		}
		s.formsImportRender(w, r, p, http.StatusOK, data)
		return
	}
	done := &formsImportDone{Repo: target, SourceURL: data.SourceHint}
	if st.Outcome != nil {
		if st.Outcome.SourceURL != "" {
			done.SourceURL = st.Outcome.SourceURL
		}
		done.Heads = formsHeads(st.Outcome.HeadSHAs)
	}
	if done.SourceURL == "" && data.SourceURL != "" {
		done.SourceURL = data.SourceURL
	}
	data.Phase = "done"
	data.Done = done
	data.TaskID = ""
	s.formsImportRender(w, r, p, http.StatusOK, data)
}

func formsHeads(m map[string]string) []formsHead {
	refs := make([]string, 0, len(m))
	for ref := range m {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]formsHead, 0, len(refs))
	for _, ref := range refs {
		out = append(out, formsHead{Ref: ref, SHA: m[ref]})
	}
	return out
}

// formsImportMirror creates a pull-only mirror: the same order the mirror
// handler uses (authority, target, preset, source, transport, SSRF, repo
// create, sidecar, async first sync).
func (s *Server) formsImportMirror(w http.ResponseWriter, r *http.Request, p auth.Principal, data *formsImportData) {
	if !p.Write && !p.Admin {
		data.Err = "write access required"
		s.formsImportRender(w, r, p, http.StatusForbidden, data)
		return
	}
	if formsDeps.Mirror == nil || s.env.API == nil || s.env.API.Store == nil || s.env.API.Repos == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "mirror service not configured")
		return
	}
	owner, name, msg := formsRepoNameErr(data.Owner, data.Name)
	if msg != "" {
		data.Err = msg
		s.formsImportRender(w, r, p, http.StatusBadRequest, data)
		return
	}
	id, _ := git.ParseRepoId(owner + "/" + name)
	data.Owner, data.Name = owner, name
	if !mirror.ValidPreset(data.Schedule) {
		data.Err = fmt.Sprintf("unknown schedule preset %q: want one of %s", data.Schedule, strings.Join(mirror.Presets, "|"))
		s.formsImportRender(w, r, p, http.StatusBadRequest, data)
		return
	}
	n, nerr := repoimport.NormalizeSource(data.SourceURL)
	if nerr != nil {
		s.formsFail(w, r, p, nil, "forms/import", "Import repository", data, nerr)
		return
	}
	if verr := repoimport.ValidateTransport(n, data.Token != ""); verr != nil {
		s.formsFail(w, r, p, nil, "forms/import", "Import repository", data, verr)
		return
	}
	if data.Dangerous && !formsDangerousAllowed(p) {
		data.Err = "dangerous:true requires a signed-in host admin (OIDC admin group)"
		s.formsImportRender(w, r, p, http.StatusForbidden, data)
		return
	}
	if formsDeps.Identity != nil {
		if aerr := formsDeps.Identity.CheckCreateOwner(r.Context(), id.Owner, p); aerr != nil {
			data.Err = aerr.Why
			s.formsImportRender(w, r, p, formsAuthStatus(aerr), data)
			return
		}
	}
	if serr := repoimport.CheckSSRF(n, repoimport.SSRFPolicy(
		config.ImportPolicy(s.env.LiveImport, s.env.Cfg), data.Dangerous), nil); serr != nil {
		s.formsFail(w, r, p, nil, "forms/import", "Import repository", data, serr)
		return
	}
	if cerr := s.env.API.Repos.Create(r.Context(), id, git.Sha1); cerr != nil {
		if errors.Is(cerr, api.ErrExists) || store.IsPreconditionFailed(cerr) {
			data.Err = "repository already exists"
			data.Winner = "/" + id.String()
			s.formsImportRender(w, r, p, http.StatusConflict, data)
			return
		}
		s.errorPage(w, r, http.StatusServiceUnavailable, formsErrMessage(cerr))
		return
	}
	if _, merr := mirror.Create(r.Context(), s.env.API.Store, id.Owner, id.Name, n.URL, data.Schedule); merr != nil {
		if store.IsPreconditionFailed(merr) {
			data.Err = "already a mirror"
			data.Winner = "/" + id.String()
			s.formsImportRender(w, r, p, http.StatusConflict, data)
			return
		}
		s.errorPage(w, r, http.StatusServiceUnavailable, formsErrMessage(merr))
		return
	}
	formsDeps.Mirror.SyncAsync(r.Context(), id.Owner, id.Name, data.Token, false)
	s.formsRedirect(w, r, "/"+id.String())
}

// formsDangerousAllowed mirrors the import/mirror dangerous-confirm authority:
// a signed-in host admin (the OIDC group predicate; anonymous is never admin).
func formsDangerousAllowed(p auth.Principal) bool {
	return !p.Anonymous && p.Admin
}

// --- /orgs/new ---------------------------------------------------------------

type formsOrgNewData struct {
	formsBase
	Org     string
	Display string
	Desc    string
	NameErr string
}

func (s *Server) formsOrgNewDataFor(r *http.Request, p auth.Principal, org, display, desc string) *formsOrgNewData {
	return &formsOrgNewData{
		formsBase: s.formsBase(r, p, "Create a new organization"),
		Org:       org,
		Display:   display,
		Desc:      desc,
		NameErr:   func() string { _, e := formsOrgNameErr(org); return e }(),
	}
}

func (s *Server) formsOrgNew(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	s.page(w, r, p, http.StatusOK, "forms/org_new", "New organization", s.formsOrgNewDataFor(r, p, "", "", ""))
}

func (s *Server) formsOrgNewPost(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Create a new organization") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data := s.formsOrgNewDataFor(r, p,
		r.PostFormValue("org"), r.PostFormValue("display_name"), r.PostFormValue("description"))
	if !s.formsNeedWrite(w, r, p) {
		return
	}
	org, msg := formsOrgNameErr(data.Org)
	data.Org = org
	if msg != "" {
		data.Err = msg
		s.page(w, r, p, http.StatusBadRequest, "forms/org_new", "New organization", data)
		return
	}
	if formsDeps.Identity == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	created, cerr := formsDeps.Identity.CreateOrg(r.Context(), org, strings.TrimSpace(data.Display), strings.TrimSpace(data.Desc), p.Name)
	if cerr != nil {
		setFormsErr(data, formsErrMessage(cerr))
		status := formsErrStatus(cerr)
		if errors.Is(cerr, identity.ErrConflict) {
			status = http.StatusConflict
		}
		s.page(w, r, p, status, "forms/org_new", "New organization", data)
		return
	}
	slug := org
	if created != nil && created.Org != "" {
		slug = created.Org
	}
	s.formsRedirect(w, r, "/"+slug+"/settings")
}

// --- /{owner}/{name}/fork ----------------------------------------------------

type formsForkData struct {
	formsBase
	Full        string
	DefaultName string
	Owners      []string
	Owner       string
	Name        string
	Visibility  string
	Branch      string
	Head        string
	Branches    []string
	Description string
	ForksOff    bool
	NameErr     string
}

func (s *Server) formsForkDataFor(r *http.Request, p auth.Principal, owner, name string) *formsForkData {
	// A missing summary fails open: features stay enabled and the branch
	// picker is empty (the server refuses a bad branch anyway).
	sum, _ := s.formsSummary(r, owner, name)
	owners := s.formsOwnerOptions(r, p)
	head := ""
	if sum.Head != nil {
		head = sum.Head.Name
	}
	branches := s.formsBranches(r, owner, name)
	if head != "" && !formsContains(branches, head) {
		branches = append([]string{head}, branches...)
	}
	return &formsForkData{
		formsBase:   s.formsBase(r, p, "Fork this repository"),
		Full:        owner + "/" + name,
		DefaultName: name + "-fork",
		Owners:      owners,
		Owner:       formsClampOwner(owners, ""),
		Name:        name + "-fork",
		Visibility:  "public",
		Branch:      head,
		Head:        head,
		Branches:    branches,
		ForksOff:    !formsFeatureEnabled(sum, "forks"),
	}
}

func formsContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func (s *Server) formsFork(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/fork", "Fork "+owner+"/"+name, s.formsForkDataFor(r, p, owner, name))
}

func (s *Server) formsForkPost(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Fork this repository") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data := s.formsForkDataFor(r, p, owner, name)
	data.Owner = strings.TrimSpace(r.PostFormValue("owner"))
	data.Name = strings.TrimSpace(r.PostFormValue("name"))
	data.Visibility = strings.TrimSpace(r.PostFormValue("visibility"))
	data.Branch = strings.TrimSpace(r.PostFormValue("branch"))
	data.Description = strings.TrimSpace(r.PostFormValue("description"))
	data.NameErr = ""
	if data.ForksOff {
		data.Err = "Forking is disabled for this repository."
		s.pageRepo(w, r, p, chrome, http.StatusForbidden, "forms/fork", "Fork "+owner+"/"+name, data)
		return
	}
	o, n, msg := formsRepoNameErr(data.Owner, data.Name)
	data.Owner, data.Name = o, n
	if msg != "" {
		data.Err = msg
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/fork", "Fork "+owner+"/"+name, data)
		return
	}
	if len(data.Description) > pulls.MaxForkDescription {
		data.Err = fmt.Sprintf("description exceeds %d bytes", pulls.MaxForkDescription)
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/fork", "Fork "+owner+"/"+name, data)
		return
	}
	if formsDeps.Pulls == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "pulls service not configured")
		return
	}
	_, child, ferr := formsDeps.Pulls.StartFork(r.Context(), owner, name, p, pulls.ForkInput{
		TargetOwner: data.Owner,
		Name:        data.Name,
		Visibility:  data.Visibility,
		Branch:      data.Branch,
		Description: data.Description,
	})
	if ferr != nil {
		if formsErrStatus(ferr) == http.StatusConflict {
			data.Winner = "/" + data.Owner + "/" + data.Name
		}
		s.formsFail(w, r, p, chrome, "forms/fork", "Fork "+owner+"/"+name, data, ferr)
		return
	}
	if child == "" {
		child = data.Owner + "/" + data.Name
	}
	// The 202 queued the fork: land on the child page immediately (the task
	// narrates; polling the child manifest here would hold the request).
	s.formsRedirect(w, r, "/"+child)
}

// --- /{owner}/{name}/releases/new -------------------------------------------

type formsReleaseNewData struct {
	formsBase
	Full       string
	Tags       []string
	Tag        string
	Title      string
	Body       string
	Since      string
	Draft      bool
	Prerelease bool
	Disabled   bool
	// WP3.3: "create tag on publish" — a draft declares a not-yet-existing
	// tag name + target commit; the ref is published at publish, never at
	// create.
	NewTag    string
	Target    string
	CreateTag bool
}

func (s *Server) formsReleaseNewDataFor(r *http.Request, p auth.Principal, owner, name string) *formsReleaseNewData {
	sum, _ := s.formsSummary(r, owner, name)
	return &formsReleaseNewData{
		formsBase: s.formsBase(r, p, "Create a new release"),
		Full:      owner + "/" + name,
		Tags:      s.formsTags(r, owner, name),
		Disabled:  !formsFeatureEnabled(sum, "releases"),
	}
}

func (s *Server) formsReleaseNew(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/release_new", "New release", s.formsReleaseNewDataFor(r, p, owner, name))
}

func (s *Server) formsReleaseNewPost(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Create a new release") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data := s.formsReleaseNewDataFor(r, p, owner, name)
	data.Tag = strings.TrimSpace(r.PostFormValue("tag"))
	data.Title = strings.TrimSpace(r.PostFormValue("name"))
	data.Body = r.PostFormValue("body")
	data.Draft = r.PostFormValue("draft") != ""
	data.Prerelease = r.PostFormValue("prerelease") != ""
	data.NewTag = strings.TrimSpace(r.PostFormValue("new_tag"))
	data.Target = strings.TrimSpace(r.PostFormValue("target"))
	data.CreateTag = r.PostFormValue("create_tag") != ""
	tagForAPI := data.Tag
	if data.CreateTag {
		tagForAPI = data.NewTag
		data.Draft = true
	}
	if data.Disabled {
		data.Err = "Releases are disabled for this repository."
		s.pageRepo(w, r, p, chrome, http.StatusForbidden, "forms/release_new", "New release", data)
		return
	}
	if tagForAPI == "" {
		data.Err = "Choose a tag for this release."
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/release_new", "New release", data)
		return
	}
	if data.CreateTag && data.Target == "" {
		data.Err = "A new tag needs a target commit (sha or ref)."
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/release_new", "New release", data)
		return
	}
	if formsDeps.Releases == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "releases service not configured")
		return
	}
	if r.PostFormValue("action") == "autodraft" {
		if data.CreateTag {
			data.Err = "Autodraft needs an existing tag — create the tag on publish, then edit the notes after publishing."
			s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/release_new", "New release", data)
			return
		}
		ad, aerr := formsDeps.Releases.Autodraft(r.Context(), owner, name, p, data.Tag, "")
		if aerr != nil {
			s.formsFail(w, r, p, chrome, "forms/release_new", "New release", data, aerr)
			return
		}
		if ad != nil {
			data.Body = ad.Body
			data.Since = ad.Since
			if data.Title == "" {
				data.Title = data.Tag
			}
		}
		s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/release_new", "New release", data)
		return
	}
	in := releases.ReleaseInput{Body: &data.Body, Draft: &data.Draft, Prerelease: &data.Prerelease}
	if data.Title != "" {
		in.Name = &data.Title
	}
	if data.CreateTag {
		target := data.Target
		in.Target = &target
	}
	rel, _, perr := formsDeps.Releases.PutRelease(r.Context(), owner, name, p, tagForAPI, in, "")
	if perr != nil {
		s.formsFail(w, r, p, chrome, "forms/release_new", "New release", data, perr)
		return
	}
	tag := data.Tag
	if rel != nil && rel.Tag != "" {
		tag = rel.Tag
	}
	s.formsRedirect(w, r, "/"+owner+"/"+name+"/releases/"+url.PathEscape(tag))
}

// --- /{owner}/{name}/issues/new ---------------------------------------------

// formsIssueTemplates is the optional §15 read half: the concrete
// *issues.Service answers it; a test fake that does not is simply treated as
// "no templates in this repo" (the blank composer).
type formsIssueTemplates interface {
	Templates(ctx context.Context, owner, repo string) (*issues.TemplateDoc, error)
}

// issueTemplateChoice is one chooser entry (no body: the body arrives through
// the selected template, never in the list markup).
type issueTemplateChoice struct {
	Path  string
	Name  string
	About string
	Title string
}

type formsIssueNewData struct {
	formsBase
	Full        string
	Title       string
	Body        string
	Preview     bool
	PreviewHTML template.HTML
	Disabled    bool

	// §15 templates: the chooser plus the selected template's metadata. A
	// selection arrives as ?template=<path> (GET) and rides the form as a
	// hidden field (POST), so no client JavaScript is needed to prefill.
	Templates []issueTemplateChoice
	Selected  string
	Template  *issues.IssueTemplate
	Labels    string
	Assignees string
}

// formsIssueTemplatesFor reads the cached template projection. One
// exact-key bucket GET — never a git call on a composer request (law 6).
func (s *Server) formsIssueTemplatesFor(r *http.Request, owner, name string) []issues.IssueTemplate {
	if formsDeps.Issues == nil {
		return nil
	}
	src, ok := formsDeps.Issues.(formsIssueTemplates)
	if !ok {
		return nil
	}
	doc, err := src.Templates(r.Context(), owner, name)
	if err != nil || doc == nil {
		return nil
	}
	return doc.Templates
}

// formsTemplateByPath resolves one selection ("no traversal": the path must
// be exactly one of the discovered entries).
func formsTemplateByPath(list []issues.IssueTemplate, p string) *issues.IssueTemplate {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	for i := range list {
		if list[i].Path == p {
			return &list[i]
		}
	}
	return nil
}

func (s *Server) formsIssueNewDataFor(r *http.Request, p auth.Principal, owner, name string) *formsIssueNewData {
	sum, _ := s.formsSummary(r, owner, name)
	data := &formsIssueNewData{
		formsBase: s.formsBase(r, p, "Create a new issue"),
		Full:      owner + "/" + name,
		Disabled:  !formsFeatureEnabled(sum, "issues"),
	}
	// One projection read serves both the chooser and the selection (one
	// exact-key GET per composer render; never a second store round trip).
	list := s.formsIssueTemplatesFor(r, owner, name)
	for _, t := range list {
		data.Templates = append(data.Templates, issueTemplateChoice{Path: t.Path, Name: t.Name, About: t.About, Title: t.Title})
	}
	sel := strings.TrimSpace(r.URL.Query().Get("template"))
	if sel == "" {
		return data
	}
	tpl := formsTemplateByPath(list, sel)
	if tpl == nil {
		data.setErr("unknown issue template")
		return data
	}
	data.Selected = tpl.Path
	data.Template = tpl
	data.Title = tpl.Title
	data.Body = tpl.Body
	data.Labels = strings.Join(tpl.Labels, ", ")
	data.Assignees = strings.Join(tpl.Assignees, ", ")
	return data
}

// formsTemplateMeta filters the selected template's metadata to what this
// author may actually apply: labels must exist in the repo and the actor must
// hold the create-metadata floor (triage, 02 §7); assignees must resolve at
// triage. Dropped values are listed in the page's notice — never silently
// half-applied and never a failed create (a reader may still file an issue
// from a template).
func (s *Server) formsTemplateMeta(r *http.Request, p auth.Principal, owner, name string, tpl *issues.IssueTemplate) (labels, assignees []string, dropped []string) {
	if tpl == nil {
		return nil, nil, nil
	}
	triage := false
	if formsDeps.Identity != nil {
		role, _ := formsDeps.Identity.Resolve(r.Context(), owner, name, p)
		triage = roleRank(role) >= roleRank(identity.RoleTriage)
	} else if p.Admin || p.Write {
		triage = true
	}
	if !triage {
		if len(tpl.Labels) > 0 || len(tpl.Assignees) > 0 {
			dropped = append(dropped, "labels/assignees (needs triage)")
		}
		return nil, nil, dropped
	}
	if len(tpl.Labels) > 0 {
		var known []issues.Label
		if formsDeps.Issues != nil {
			known, _ = formsDeps.Issues.ListLabels(r.Context(), owner, name)
		}
		for _, want := range tpl.Labels {
			found := false
			for _, l := range known {
				if strings.EqualFold(l.Name, want) {
					labels = append(labels, l.Name)
					found = true
					break
				}
			}
			if !found {
				dropped = append(dropped, "label "+want)
			}
		}
	}
	for _, want := range tpl.Assignees {
		role := identity.Role("")
		if formsDeps.Identity != nil {
			role, _ = formsDeps.Identity.Resolve(r.Context(), owner, name, auth.Principal{Name: normPrincipal(want)})
		}
		if roleRank(role) >= roleRank(identity.RoleTriage) {
			assignees = append(assignees, normPrincipal(want))
		} else {
			dropped = append(dropped, "assignee "+want)
		}
	}
	return labels, assignees, dropped
}

func (s *Server) formsIssueNew(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/issue_new", "New issue", s.formsIssueNewDataFor(r, p, owner, name))
}

func (s *Server) formsIssueNewPost(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Create a new issue") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data := s.formsIssueNewDataFor(r, p, owner, name)
	data.Title = r.PostFormValue("title")
	data.Body = r.PostFormValue("body")
	if sel := strings.TrimSpace(r.PostFormValue("template")); sel != "" {
		data.Selected = sel
		data.Template = formsTemplateByPath(s.formsIssueTemplatesFor(r, owner, name), sel)
		if data.Template == nil {
			data.setErr("the selected issue template no longer exists")
			s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/issue_new", "New issue", data)
			return
		}
	}
	if data.Disabled {
		data.Err = "Issues are disabled for this repository."
		s.pageRepo(w, r, p, chrome, http.StatusForbidden, "forms/issue_new", "New issue", data)
		return
	}
	if r.PostFormValue("action") == "preview" {
		data.Preview = true
		data.PreviewHTML = RenderMarkdownCtx(data.Body, MarkdownCtx{Owner: owner, Repo: name, Ref: chrome.HeadLabel})
		s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/issue_new", "New issue", data)
		return
	}
	if formsDeps.Issues == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "issues service not configured")
		return
	}
	labels, assignees, dropped := s.formsTemplateMeta(r, p, owner, name, data.Template)
	var meta *issues.IssueMeta
	if len(labels) > 0 || len(assignees) > 0 {
		meta = &issues.IssueMeta{Labels: labels, Assignees: assignees}
	}
	th, _, cerr := formsDeps.Issues.CreateIssue(r.Context(), owner, name, p, data.Title, data.Body, meta)
	if cerr != nil {
		s.formsFail(w, r, p, chrome, "forms/issue_new", "New issue", data, cerr)
		return
	}
	num := 0
	if th != nil {
		num = th.Num
	}
	notice := ""
	if len(dropped) > 0 {
		notice = "?template_skipped=" + url.QueryEscape(strings.Join(dropped, ", "))
	}
	s.formsRedirect(w, r, fmt.Sprintf("/%s/%s/issues/%d%s", owner, name, num, notice))
}

// --- /{owner}/{name}/pulls/new ---------------------------------------------

// formsPRPreview is the comparison panel (lib/compare.js states).
type formsPRPreview struct {
	Status        string // idle | same-names | error | same | ready
	Error         string
	From, To      string
	FromFull      string
	BaseSHA       string
	HeadSHA       string
	Ahead         int
	Behind        int
	BoundedAhead  string
	BoundedBehind string
	Truncated     bool
	Merged        bool
	Unique        []api.Commit
	More          int
}

type formsPullNewData struct {
	formsBase
	Full      string
	FromRepos []string
	// FromRepoGroups renders the head repository picker grouped by owner:
	// this repository, the base owner's other repos, and the fork network
	// (WP2.1). The fork list is one index read; each fork's branches load
	// only after "list branches" selects it (no N-repo scan per render).
	FromRepoGroups []formsRepoGroup
	ToRepos        []string
	FromRepo       string
	ToRepo         string
	FromRefs       []string
	ToRefs         []string
	FromRef        string
	ToRef          string
	Title          string
	Body           string
	Draft          bool
	Role           string
	RoleOK         bool
	Disabled       bool
	Preview        formsPRPreview
	Window         int
}

// formsRepoGroup is one <optgroup> of the From repository picker.
type formsRepoGroup struct {
	Label string
	Repos []string
}

const formsPreviewWindow = 100

func (s *Server) formsPullNewDataFor(r *http.Request, p auth.Principal, owner, name string) *formsPullNewData {
	sum, _ := s.formsSummary(r, owner, name)
	current := owner + "/" + name
	q := r.URL.Query()
	// The head endpoint arrives in one of two shapes:
	//   ?from_repo=<owner>/<repo>&head=<branch>      (this form's own keys)
	//   ?head=<owner>/<repo>:<branch>                (the sync-fork PR link)
	// The second is GitHub's compare shape with the fork's full name. Split it
	// so the fork picker preselects the fork AND its branch instead of reading
	// "bob/widgets:main" as one impossible ref (audit FAIL 3). An explicit
	// from_repo wins over the repo carried in head.
	headRepo := strings.TrimSpace(q.Get("from_repo"))
	headRef := strings.TrimSpace(q.Get("from_ref"))
	if rawHead := strings.TrimSpace(q.Get("head")); rawHead != "" {
		if repoPart, branch, ok := strings.Cut(rawHead, ":"); ok && branch != "" {
			if headRepo == "" && strings.Contains(repoPart, "/") {
				headRepo = repoPart
			}
			headRef = branch
		} else {
			headRef = rawHead
		}
	}
	if headRepo == "" {
		headRepo = current
	}
	data := &formsPullNewData{
		formsBase: s.formsBase(r, p, "Open a new pull request"),
		Full:      current,
		FromRepo:  headRepo,
		ToRepo:    current,
		FromRef:   headRef,
		ToRef:     strings.TrimSpace(q.Get("base")),
		Disabled:  !formsFeatureEnabled(sum, "pulls"),
		FromRepos: []string{current},
		ToRepos:   []string{current},
		Preview:   formsPRPreview{Status: "idle"},
		Window:    formsPreviewWindow,
	}
	// The owner's repos, split by the viewer's resolved role: To needs write
	// (OpenPR requires base write), From needs any resolved role. Without a
	// wired identity every repo stays selectable (the server is the rule).
	if formsDeps.Identity != nil && s.env.API != nil && s.env.API.Repos != nil {
		names, err := s.env.API.Repos.Repos(r.Context(), owner)
		if err == nil {
			all := make([]string, 0, len(names)+1)
			all = append(all, current)
			for _, n := range names {
				full := owner + "/" + n
				if full != current {
					all = append(all, full)
				}
			}
			roles := map[string]identity.Role{}
			for _, full := range all {
				fo, fn, _ := strings.Cut(full, "/")
				role, _ := formsDeps.Identity.Resolve(r.Context(), fo, fn, p)
				roles[full] = role
			}
			data.FromRepos = formsWithCurrent(formsReposByRole(all, roles, identity.RoleRead), current)
			data.ToRepos = formsWithCurrent(formsReposByRole(all, roles, identity.RoleWrite), current)
		}
	}
	// WP2.1: the fork network joins the head picker (one index read on the
	// parent; each fork is shown only when the viewer can read it). The
	// existing API path already accepts a cross-owner head (OpenInput.Fork).
	forks := s.formsForks(r, p, owner, name)
	data.FromRepos = append(data.FromRepos, forks...)
	if headRepo != current && !formsListContains(data.FromRepos, headRepo) {
		// A cross-owner head named in ?from_repo= (a fork opened from its
		// own page, or an old link) stays selectable; the server is the
		// authority on reachability and role.
		data.FromRepos = append(data.FromRepos, headRepo)
	}
	data.FromRepoGroups = formsReposGrouped(data.FromRepos, current)
	if formsDeps.Identity != nil {
		role, _ := formsDeps.Identity.Resolve(r.Context(), owner, name, p)
		data.Role = string(role)
		data.RoleOK = formsRoleAtLeast(role, identity.RoleWrite)
	} else {
		data.RoleOK = true
	}
	data.FromRefs = s.formsBranchesFor(r, data.FromRepo)
	if data.FromRepo == current || len(data.FromRefs) == 0 {
		data.FromRefs = s.formsBranches(r, owner, name)
	}
	data.ToRefs = s.formsBranches(r, owner, name)
	// Query-supplied branch names are short ("main"); the selects carry full
	// refs ("refs/heads/main"). Map the name onto the option spelling so a
	// link preselects its branch instead of landing on "Pick a branch…".
	data.FromRef = formsMatchRef(data.FromRef, data.FromRefs)
	data.ToRef = formsMatchRef(data.ToRef, data.ToRefs)
	return data
}

// formsMatchRef resolves a display branch name against the option list: an
// exact ref passes through, a short name matches its refs/heads/<name> (or
// refs/tags/<name>) sibling, and anything unknown stays as supplied (the
// server remains the authority on reachability).
func formsMatchRef(ref string, refs []string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	for _, r := range refs {
		if r == ref {
			return ref
		}
	}
	for _, r := range refs {
		if shortRefName(r) == ref {
			return r
		}
	}
	return ref
}

// formsForks lists the parent's fork index for the head picker, filtered to
// forks the viewer can read (a private fork must not leak its existence).
// Unwired services or unreadable indexes yield no forks — the picker still
// works for same-repo branches.
func (s *Server) formsForks(r *http.Request, p auth.Principal, owner, name string) []string {
	if formsDeps.Pulls == nil {
		return nil
	}
	page, err := formsDeps.Pulls.ListForks(r.Context(), owner, name, p, "", pulls.MaxForkList)
	if err != nil || page == nil {
		return nil
	}
	out := make([]string, 0, len(page.Forks))
	for _, f := range page.Forks {
		if f.Repo == "" {
			continue
		}
		if formsDeps.Identity != nil {
			fo, fn, ok := strings.Cut(f.Repo, "/")
			if !ok {
				continue
			}
			role, _ := formsDeps.Identity.Resolve(r.Context(), fo, fn, p)
			if !formsRoleAtLeast(role, identity.RoleRead) {
				continue
			}
		}
		out = append(out, f.Repo)
	}
	return out
}

func formsListContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// formsReposGrouped groups the head repos for <optgroup> rendering: this
// repository, the base owner's other repos, then the fork network. The
// selected head's group is not reordered — the picker keeps a stable shape,
// so a fork selection survives the "list branches" round trip.
func formsReposGrouped(all []string, current string) []formsRepoGroup {
	baseOwner, _, _ := strings.Cut(current, "/")
	groups := []formsRepoGroup{}
	index := map[string]int{}
	add := func(label, repo string) {
		i, ok := index[label]
		if !ok {
			i = len(groups)
			index[label] = i
			groups = append(groups, formsRepoGroup{Label: label})
		}
		groups[i].Repos = append(groups[i].Repos, repo)
	}
	for _, full := range all {
		if full == current {
			add("This repository", full)
			continue
		}
		o, _, ok := strings.Cut(full, "/")
		if !ok {
			continue
		}
		label := o
		if o == baseOwner {
			label = "Same owner"
		} else {
			label = "Forks"
		}
		add(label, full)
	}
	return groups
}

func formsReposByRole(all []string, roles map[string]identity.Role, want identity.Role) []string {
	out := make([]string, 0, len(all))
	for _, full := range all {
		if formsRoleAtLeast(roles[full], want) {
			out = append(out, full)
		}
	}
	return out
}

func formsWithCurrent(list []string, current string) []string {
	out := []string{current}
	for _, v := range list {
		if v != "" && v != current {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) formsPullNew(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/pull_new", "New pull request", s.formsPullNewDataFor(r, p, owner, name))
}

func (s *Server) formsPullNewPost(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Open a new pull request") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	data := s.formsPullNewDataFor(r, p, owner, name)
	data.FromRepo = strings.TrimSpace(r.PostFormValue("from_repo"))
	if data.FromRepo == "" {
		data.FromRepo = data.Full
	}
	data.ToRepo = strings.TrimSpace(r.PostFormValue("to_repo"))
	if data.ToRepo == "" {
		data.ToRepo = data.Full
	}
	data.FromRef = strings.TrimSpace(r.PostFormValue("from_ref"))
	data.ToRef = strings.TrimSpace(r.PostFormValue("to_ref"))
	data.Title = strings.TrimSpace(r.PostFormValue("title"))
	data.Body = r.PostFormValue("body")
	data.Draft = r.PostFormValue("draft") != ""
	data.FromRefs = s.formsBranchesFor(r, data.FromRepo)
	data.ToRefs = s.formsBranchesFor(r, data.ToRepo)
	if data.Disabled {
		data.Err = "Pull requests are disabled for this repository."
		s.pageRepo(w, r, p, chrome, http.StatusForbidden, "forms/pull_new", "New pull request", data)
		return
	}
	if r.PostFormValue("action") == "refs" {
		// No-JS branch discovery: picking a head repository (including a
		// fork) reloads just that repo's branch options. The POST carries
		// no PR write, so the feature gate above is the only check.
		s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/pull_new", "New pull request", data)
		return
	}
	if r.PostFormValue("action") == "preview" {
		data.Preview = s.formsCompare(r, data)
		s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/pull_new", "New pull request", data)
		return
	}
	if data.Title == "" || data.FromRef == "" {
		data.Err = "a title and a From branch are required"
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/pull_new", "New pull request", data)
		return
	}
	if data.FromRepo == data.ToRepo && data.FromRef == data.ToRef {
		data.Err = "From and To are the same endpoint — pick two different branches."
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/pull_new", "New pull request", data)
		return
	}
	if formsDeps.Pulls == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "pulls service not configured")
		return
	}
	if _, err := git.ParseRepoId(data.FromRepo); err != nil {
		data.Err = fmt.Sprintf("invalid From repository %q", data.FromRepo)
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/pull_new", "New pull request", data)
		return
	}
	if _, err := git.ParseRepoId(data.ToRepo); err != nil {
		data.Err = fmt.Sprintf("invalid To repository %q", data.ToRepo)
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/pull_new", "New pull request", data)
		return
	}
	toOwner, toName, _ := strings.Cut(data.ToRepo, "/")
	in := pulls.OpenInput{
		Title:   data.Title,
		BaseRef: data.ToRef,
		HeadRef: data.FromRef,
		Body:    strings.TrimSpace(data.Body),
		Draft:   data.Draft,
	}
	if data.FromRepo != data.ToRepo {
		in.Fork = &pulls.ForkInfo{Repo: data.FromRepo}
	}
	th, _, oerr := formsDeps.Pulls.OpenPR(r.Context(), toOwner, toName, p, in, "")
	if oerr != nil {
		data.Err = formsOpenErrorMessage(oerr)
		s.pageRepo(w, r, p, chrome, formsErrStatus(oerr), "forms/pull_new", "New pull request", data)
		return
	}
	num := 0
	if th != nil {
		num = th.Num
	}
	s.formsRedirect(w, r, fmt.Sprintf("/%s/pull/%d", data.ToRepo, num))
}

// formsBranchesFor lists a repo's branches by "owner/name".
func (s *Server) formsBranchesFor(r *http.Request, full string) []string {
	o, n, ok := strings.Cut(full, "/")
	if !ok || o == "" || n == "" {
		return nil
	}
	return s.formsBranches(r, o, n)
}

// formsCompare is the two-read intersect (lib/compare.js compareHistories):
// both histories newest-first, n = PREVIEW_WINDOW, in parallel.
func (s *Server) formsCompare(r *http.Request, data *formsPullNewData) formsPRPreview {
	pv := formsPRPreview{FromFull: data.FromRepo}
	if data.FromRef == "" || data.ToRef == "" {
		pv.Status = "idle"
		return pv
	}
	if data.FromRepo == data.ToRepo && data.FromRef == data.ToRef {
		pv.Status = "same-names"
		return pv
	}
	if s.env.API == nil || s.env.API.Repo == nil {
		pv.Status = "error"
		pv.Error = "compare unavailable"
		return pv
	}
	fromID, err1 := git.ParseRepoId(data.FromRepo)
	toID, err2 := git.ParseRepoId(data.ToRepo)
	if err1 != nil || err2 != nil {
		pv.Status = "error"
		pv.Error = "compare unavailable"
		return pv
	}
	type result struct {
		page api.CommitPage
		err  error
	}
	fromCh := make(chan result, 1)
	toCh := make(chan result, 1)
	go func() {
		page, err := s.env.API.Repo.Commits(r.Context(), fromID, formsShortRef(data.FromRef), "", 0, formsPreviewWindow)
		fromCh <- result{page, err}
	}()
	go func() {
		page, err := s.env.API.Repo.Commits(r.Context(), toID, formsShortRef(data.ToRef), "", 0, formsPreviewWindow)
		toCh <- result{page, err}
	}()
	headRes, baseRes := <-fromCh, <-toCh
	if headRes.err != nil {
		pv.Status = "error"
		pv.Error = formsPreviewErrorMessage("From", headRes.err)
		return pv
	}
	if baseRes.err != nil {
		pv.Status = "error"
		pv.Error = formsPreviewErrorMessage("To", baseRes.err)
		return pv
	}
	pv.HeadSHA = headRes.page.SHA
	pv.BaseSHA = baseRes.page.SHA
	if pv.HeadSHA != "" && pv.HeadSHA == pv.BaseSHA {
		pv.Status = "same"
		return pv
	}
	unique, ahead, behind, truncHead, truncBase := formsCompareHistories(headRes.page, baseRes.page)
	pv.Status = "ready"
	pv.Unique = unique
	pv.Ahead = ahead
	pv.Behind = behind
	pv.Merged = len(unique) == 0
	pv.BoundedAhead = formsFmtBounded(ahead, truncHead)
	pv.BoundedBehind = formsFmtBounded(behind, truncBase)
	pv.Truncated = truncHead || truncBase
	if len(pv.Unique) > 10 {
		pv.More = len(pv.Unique) - 10
	}
	return pv
}

// formsCompareHistories is compareHistories: the From walk stops at the first
// commit present in To (unique = what lands on the PR), the To walk counts
// base-only commits up to the first commit present in From.
func formsCompareHistories(headPage, basePage api.CommitPage) (unique []api.Commit, ahead, behind int, truncHead, truncBase bool) {
	baseSet := make(map[string]bool, len(basePage.Commits))
	for _, c := range basePage.Commits {
		baseSet[c.SHA] = true
	}
	headSet := make(map[string]bool, len(headPage.Commits))
	for _, c := range headPage.Commits {
		headSet[c.SHA] = true
	}
	metBase := false
	for _, c := range headPage.Commits {
		if c.SHA == "" {
			continue
		}
		if baseSet[c.SHA] {
			metBase = true
			break
		}
		unique = append(unique, c)
	}
	metHead := false
	for _, c := range basePage.Commits {
		if c.SHA == "" {
			continue
		}
		if headSet[c.SHA] {
			metHead = true
			break
		}
		behind++
	}
	return unique, len(unique), behind, !metBase && headPage.More, !metHead && basePage.More
}

func formsFmtBounded(n int, truncated bool) string {
	if truncated {
		return fmt.Sprintf("%d+", n)
	}
	return fmt.Sprintf("%d", n)
}

// formsPreviewErrorMessage is previewErrorMessage (lib/pr-composer.js).
func formsPreviewErrorMessage(side string, err error) string {
	if formsErrStatus(err) == http.StatusNotFound {
		return "No such " + side + " branch — check the name."
	}
	return "Couldn't compare: " + formsErrMessage(err)
}

// formsOpenErrorMessage is openErrorMessage (lib/pr-composer.js), verbatim.
func formsOpenErrorMessage(err error) string {
	msg := strings.TrimSpace(formsErrMessage(err))
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not reachable") || strings.Contains(lower, "push first"):
		return "The From branch's commits aren't in the To repo — push the From branch first."
	case formsErrStatus(err) == http.StatusConflict || strings.Contains(lower, "already pairs"):
		return "An open pull request already compares this From and To — find it in the pulls list."
	case formsErrStatus(err) == http.StatusNotFound || strings.Contains(lower, "unknown revision") || strings.Contains(lower, "no such ref"):
		return "Unknown revision — check the From and To branch names. (" + msg + ")"
	}
	return "Couldn't open: " + msg
}

// --- /{owner}/teams/{slug} ---------------------------------------------------

type formsTeamData struct {
	formsBase
	Org       string
	Slug      string
	Name      string
	Members   []string
	CanManage bool
}

func (s *Server) formsTeam(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	org, slug := r.PathValue("owner"), r.PathValue("slug")
	if formsDeps.Identity == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "identity service not configured")
		return
	}
	team, _, err := formsDeps.Identity.GetTeam(r.Context(), org, slug)
	if err != nil {
		s.errorPage(w, r, formsErrStatus(err), formsErrMessage(err))
		return
	}
	if team == nil {
		s.errorPage(w, r, http.StatusNotFound, "unknown team")
		return
	}
	data := &formsTeamData{
		formsBase: s.formsBase(r, p, "Manage team members"),
		Org:       org,
		Slug:      slug,
		Name:      team.Name,
		Members:   team.Members,
		CanManage: formsDeps.Identity.CheckOrgOwner(r.Context(), org, p) == nil,
	}
	s.page(w, r, p, http.StatusOK, "forms/team", org+"/"+slug, data)
}

// formsTeamGate is the shared write gate for team membership: same-origin,
// anonymous interstitial, org-owner authority (the service methods trust
// their caller, exactly like the identity HTTP handlers).
func (s *Server) formsTeamGate(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	if !s.formsCheckWrite(w, r, p, "Manage team members") {
		return false
	}
	if formsDeps.Identity == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "identity service not configured")
		return false
	}
	if aerr := formsDeps.Identity.CheckOrgOwner(r.Context(), r.PathValue("owner"), p); aerr != nil {
		s.errorPage(w, r, formsAuthStatus(aerr), aerr.Why)
		return false
	}
	return true
}

func (s *Server) formsTeamAdd(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	if !s.formsTeamGate(w, r, p) {
		return
	}
	principal := r.PathValue("principal")
	if principal == "" {
		if err := r.ParseForm(); err != nil {
			s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
			return
		}
		principal = strings.TrimSpace(r.PostFormValue("principal"))
	}
	org, slug := r.PathValue("owner"), r.PathValue("slug")
	if principal == "" {
		s.formsTeamFail(w, r, p, org, slug, errFormsPrincipal)
		return
	}
	if _, err := formsDeps.Identity.SetTeamMember(r.Context(), org, slug, principal); err != nil {
		s.formsTeamFail(w, r, p, org, slug, err)
		return
	}
	s.formsRedirectFlash(w, r, formsTeamPath(org, slug), "member added")
}

func (s *Server) formsTeamRemove(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	if !s.formsTeamGate(w, r, p) {
		return
	}
	principal := r.PathValue("principal")
	if principal == "" {
		if err := r.ParseForm(); err != nil {
			s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
			return
		}
		principal = strings.TrimSpace(r.PostFormValue("principal"))
	}
	org, slug := r.PathValue("owner"), r.PathValue("slug")
	if principal == "" {
		s.formsTeamFail(w, r, p, org, slug, errFormsPrincipal)
		return
	}
	if _, err := formsDeps.Identity.RemoveTeamMember(r.Context(), org, slug, principal); err != nil {
		s.formsTeamFail(w, r, p, org, slug, err)
		return
	}
	s.formsRedirectFlash(w, r, formsTeamPath(org, slug), "member removed")
}

func formsTeamPath(org, slug string) string { return "/" + org + "/teams/" + slug }

// errFormsPrincipal is the empty-principal field error (400, inline).
var errFormsPrincipal = errors.New("invalid principal")

// formsTeamFail re-renders the team page with an inline error (expected
// outcomes stay on the page; 5xx uses the error page).
func (s *Server) formsTeamFail(w http.ResponseWriter, r *http.Request, p auth.Principal, org, slug string, err error) {
	st := formsErrStatus(err)
	if errors.Is(err, errFormsPrincipal) {
		st = http.StatusBadRequest
	}
	if st >= 500 {
		s.errorPage(w, r, st, formsErrMessage(err))
		return
	}
	team, _, _ := formsDeps.Identity.GetTeam(r.Context(), org, slug)
	data := &formsTeamData{
		formsBase: s.formsBase(r, p, "Manage team members"),
		Org:       org,
		Slug:      slug,
		CanManage: true,
	}
	if team != nil {
		data.Name = team.Name
		data.Members = team.Members
	}
	data.Err = formsErrMessage(err)
	s.page(w, r, p, st, "forms/team", org+"/"+slug, data)
}

// --- /{owner}/{name}/labels --------------------------------------------------

type formsLabelPackEntry struct {
	Name        string
	Color       string
	Description string
}

type formsLabelPack struct {
	ID     string
	Title  string
	Labels []formsLabelPackEntry
}

// formsLabelPacks is lib/label-packs.js as a Go table (GitHub + GitLab
// defaults, colors 6-hex without "#", descriptions within the service bound).
var formsLabelPacks = []formsLabelPack{
	{
		ID:    "github",
		Title: "GitHub defaults",
		Labels: []formsLabelPackEntry{
			{Name: "bug", Color: "d73a4a", Description: "Something isn't working"},
			{Name: "duplicate", Color: "cfd3d7", Description: "This issue or pull request already exists"},
			{Name: "enhancement", Color: "a2eeef", Description: "New feature or request"},
			{Name: "good first issue", Color: "7057ff", Description: "Good for newcomers"},
			{Name: "help wanted", Color: "008672", Description: "Extra attention is needed"},
			{Name: "invalid", Color: "e4e669", Description: "This doesn't seem right"},
			{Name: "question", Color: "d876e3", Description: "Further information is requested"},
			{Name: "wontfix", Color: "ffffff", Description: "This will not be worked on"},
		},
	},
	{
		ID:    "gitlab",
		Title: "GitLab defaults",
		Labels: []formsLabelPackEntry{
			{Name: "bug", Color: "d9534f"},
			{Name: "critical", Color: "d9534f"},
			{Name: "confirmed", Color: "d9534f"},
			{Name: "documentation", Color: "f0ad4e"},
			{Name: "support", Color: "f0ad4e"},
			{Name: "discussion", Color: "428bca"},
			{Name: "suggestion", Color: "428bca"},
			{Name: "enhancement", Color: "5cb85c"},
		},
	},
}

var formsLabelPresets = []string{"d73a4a", "a2eeef", "7057ff", "008672", "e4e669", "d876e3", "f0ad4e", "5cb85c", "428bca"}

type formsLabelsData struct {
	formsBase
	Full      string
	Labels    []issues.Label
	ShowPacks bool
	Packs     []formsLabelPack
	Presets   []string
	CanTriage bool
	LoadErr   error
}

func (s *Server) formsLabelsDataFor(r *http.Request, p auth.Principal, owner, name string) *formsLabelsData {
	data := &formsLabelsData{
		formsBase: s.formsBase(r, p, "Manage labels"),
		Full:      owner + "/" + name,
		Packs:     formsLabelPacks,
		Presets:   formsLabelPresets,
	}
	role, wired := s.formsRole(r, owner, name, p)
	data.CanTriage = !wired || formsRoleAtLeast(role, identity.RoleTriage)
	if formsDeps.Issues == nil {
		data.LoadErr = errFormsNoIssues
		return data
	}
	labels, err := formsDeps.Issues.ListLabels(r.Context(), owner, name)
	if err != nil {
		data.LoadErr = err
		return data
	}
	data.Labels = labels
	data.ShowPacks = len(data.Labels) == 0
	return data
}

func (s *Server) formsLabels(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	data := s.formsLabelsDataFor(r, p, owner, name)
	if data.LoadErr != nil {
		s.errorPage(w, r, formsErrStatus(data.LoadErr), formsErrMessage(data.LoadErr))
		return
	}
	s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/labels", "Labels", data)
}

func (s *Server) formsLabelsPost(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Manage labels") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	if formsDeps.Issues == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "issues service not configured")
		return
	}
	data := s.formsLabelsDataFor(r, p, owner, name)
	if packID := r.PostFormValue("pack"); packID != "" {
		added, err := s.formsAddLabelPack(r, p, owner, name, packID, data.Labels)
		if err != nil {
			s.formsFail(w, r, p, chrome, "forms/labels", "Labels", data, err)
			return
		}
		s.formsRedirectFlash(w, r, "/"+owner+"/"+name+"/labels", fmt.Sprintf("%d labels added", added))
		return
	}
	color := strings.TrimSpace(r.PostFormValue("color_custom"))
	if color == "" {
		color = strings.TrimSpace(r.PostFormValue("color"))
	}
	label, err := formsDeps.Issues.CreateLabel(r.Context(), owner, name, p,
		strings.TrimSpace(r.PostFormValue("name")), color,
		strings.TrimSpace(r.PostFormValue("description")))
	if err != nil {
		s.formsFail(w, r, p, chrome, "forms/labels", "Labels", data, err)
		return
	}
	msg := "label created"
	if label != nil {
		msg = "label created — " + label.Name
	}
	s.formsRedirectFlash(w, r, "/"+owner+"/"+name+"/labels", msg)
}

// formsAddLabelPack creates the pack entries missing from the current set
// (case-insensitive), sequentially — the API has one label per request.
func (s *Server) formsAddLabelPack(r *http.Request, p auth.Principal, owner, name, packID string, existing []issues.Label) (int, error) {
	var pack *formsLabelPack
	for i := range formsLabelPacks {
		if formsLabelPacks[i].ID == packID {
			pack = &formsLabelPacks[i]
			break
		}
	}
	if pack == nil {
		return 0, formsBadRequest(fmt.Sprintf("unknown label pack %q", packID))
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[strings.ToLower(l.Name)] = true
	}
	added := 0
	var firstErr error
	for _, l := range pack.Labels {
		if have[strings.ToLower(l.Name)] {
			continue
		}
		if _, err := formsDeps.Issues.CreateLabel(r.Context(), owner, name, p, l.Name, l.Color, l.Description); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		added++
	}
	return added, firstErr
}

func (s *Server) formsLabelDelete(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Manage labels") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	label := r.PathValue("label")
	if label == "" {
		label = r.PostFormValue("name")
	}
	label = strings.TrimSpace(label)
	if formsDeps.Issues == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "issues service not configured")
		return
	}
	data := s.formsLabelsDataFor(r, p, owner, name)
	affected, err := formsDeps.Issues.DeleteLabel(r.Context(), owner, name, p, label)
	if err != nil {
		s.formsFail(w, r, p, chrome, "forms/labels", "Labels", data, err)
		return
	}
	s.formsRedirectFlash(w, r, "/"+owner+"/"+name+"/labels",
		fmt.Sprintf("label deleted — %d threads updated", affected))
}

// --- /{owner}/{name}/milestones ---------------------------------------------

type formsMilestoneRow struct {
	M      *issues.Milestone
	Issues []issues.Card
	View   string
	Href   string
}

type formsMilestonesData struct {
	formsBase
	Full      string
	Open      []formsMilestoneRow
	Closed    []formsMilestoneRow
	HasAny    bool
	CanTriage bool
	LoadErr   error
}

func (s *Server) formsMilestonesDataFor(r *http.Request, p auth.Principal, owner, name string) *formsMilestonesData {
	data := &formsMilestonesData{
		formsBase: s.formsBase(r, p, "Manage milestones"),
		Full:      owner + "/" + name,
	}
	role, wired := s.formsRole(r, owner, name, p)
	data.CanTriage = !wired || formsRoleAtLeast(role, identity.RoleTriage)
	if formsDeps.Issues == nil {
		data.LoadErr = errFormsNoIssues
		return data
	}
	list, err := formsDeps.Issues.ListMilestones(r.Context(), owner, name)
	if err != nil {
		data.LoadErr = err
		return data
	}
	for _, m := range list {
		if m == nil {
			continue
		}
		row := formsMilestoneRow{M: m, Href: formsMilestoneFilterHref(owner+"/"+name, m.ID)}
		total := m.OpenIssues + m.ClosedIssues
		if total == 1 {
			row.View = "View 1 issue"
		} else {
			row.View = fmt.Sprintf("View %d issues", total)
		}
		if m.State == issues.StateClosed {
			data.Closed = append(data.Closed, row)
			continue
		}
		if page, lerr := formsDeps.Issues.ListIssues(r.Context(), owner, name, p, issues.ListFilter{
			State: "all", Milestone: m.ID, N: 100,
		}); lerr == nil && page != nil {
			row.Issues = page.Issues
		}
		data.Open = append(data.Open, row)
	}
	data.HasAny = len(data.Open) > 0 || len(data.Closed) > 0
	return data
}

// formsMilestoneFilterHref is milestoneFilterHref: the issues list filtered to
// one milestone across both states.
func formsMilestoneFilterHref(full, id string) string {
	return "/" + full + "/issues?milestone=" + url.QueryEscape(id) + "&state=all"
}

func (s *Server) formsMilestones(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	data := s.formsMilestonesDataFor(r, p, owner, name)
	if data.LoadErr != nil {
		s.errorPage(w, r, formsErrStatus(data.LoadErr), formsErrMessage(data.LoadErr))
		return
	}
	s.pageRepo(w, r, p, chrome, http.StatusOK, "forms/milestones", "Milestones", data)
}

func (s *Server) formsMilestonesPost(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Manage milestones") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	if formsDeps.Issues == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "issues service not configured")
		return
	}
	data := s.formsMilestonesDataFor(r, p, owner, name)
	if _, err := formsDeps.Issues.CreateMilestone(r.Context(), owner, name, p,
		strings.TrimSpace(r.PostFormValue("title")), strings.TrimSpace(r.PostFormValue("description")), nil); err != nil {
		s.formsFail(w, r, p, chrome, "forms/milestones", "Milestones", data, err)
		return
	}
	s.formsRedirectFlash(w, r, "/"+owner+"/"+name+"/milestones", "milestone created")
}

// formsMilestoneID validates the 6-hex id shape before a write.
func formsMilestoneID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 6 {
		return ""
	}
	for i := range id {
		c := id[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return ""
	}
	return id
}

func (s *Server) formsMilestoneState(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Manage milestones") {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "unreadable form")
		return
	}
	if formsDeps.Issues == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "issues service not configured")
		return
	}
	id := formsMilestoneID(r.PathValue("id"))
	if id == "" {
		s.errorPage(w, r, http.StatusNotFound, "unknown milestone")
		return
	}
	data := s.formsMilestonesDataFor(r, p, owner, name)
	state := strings.ToLower(strings.TrimSpace(r.PostFormValue("state")))
	if state != issues.StateOpen && state != issues.StateClosed {
		data.Err = "state must be open|closed"
		s.pageRepo(w, r, p, chrome, http.StatusBadRequest, "forms/milestones", "Milestones", data)
		return
	}
	if _, err := formsDeps.Issues.UpdateMilestone(r.Context(), owner, name, p, id, nil, nil, nil, &state); err != nil {
		s.formsFail(w, r, p, chrome, "forms/milestones", "Milestones", data, err)
		return
	}
	msg := "milestone closed"
	if state == issues.StateOpen {
		msg = "milestone reopened"
	}
	s.formsRedirectFlash(w, r, "/"+owner+"/"+name+"/milestones", msg)
}

func (s *Server) formsMilestoneDelete(w http.ResponseWriter, r *http.Request) {
	p, owner, name, chrome, ok := s.formsRepoPageChrome(w, r)
	if !ok {
		return
	}
	if !s.formsCheckWrite(w, r, p, "Manage milestones") {
		return
	}
	if formsDeps.Issues == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "issues service not configured")
		return
	}
	id := formsMilestoneID(r.PathValue("id"))
	if id == "" {
		s.errorPage(w, r, http.StatusNotFound, "unknown milestone")
		return
	}
	data := s.formsMilestonesDataFor(r, p, owner, name)
	if err := formsDeps.Issues.DeleteMilestone(r.Context(), owner, name, p, id); err != nil {
		msg := formsErrMessage(err)
		if formsErrStatus(err) == http.StatusConflict {
			msg += " — close it instead; open issues still reference it"
		}
		data.Err = msg
		s.pageRepo(w, r, p, chrome, formsErrStatus(err), "forms/milestones", "Milestones", data)
		return
	}
	s.formsRedirectFlash(w, r, "/"+owner+"/"+name+"/milestones", "milestone deleted")
}
