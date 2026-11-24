// admin_runners.go — /admin/runners, the instance runner fleet
// (docs/PLAN-INSTANCE-RUNNERS.md §5).
//
//	GET  /admin/runners          Fleet (?tab= default) · Mint token (?tab=mint) ·
//	                             Security policy (?tab=security)
//	POST /admin/runners          mint a registration token (name + repository policy)
//	POST /admin/runners/policy   change one runner's repository policy (id in the body)
//	POST /admin/runners/revoke   revoke one runner (id in the body)
//
// A runner is INSTANCE infrastructure and only a host administrator may
// authorize one; the page therefore lives in the Administration area and gates
// through the area's one access rule. The one-time token is rendered in place
// (never a redirect, never a URL) and is shown exactly once.
//
// # Tenancy
//
// The mint sets the runner's repository policy — "all repositories" or a bounded
// list of owner/name and owner/* patterns — and that policy is the admin's: the
// runner protocol has no field for it, so a runner can never widen its own
// tenancy. A registered runner's policy is changed HERE, in place
// (SetRunnerPolicy, audited as runner_policy), so a fleet runner is not revoked
// and re-minted — and not taken off an in-flight job — to fix a policy. A record
// with no policy (written before tenancy existed) is shown as "none — no
// repository allowed" and is fixed by the same inline edit.
//
// The policy is TENANCY, not isolation: it decides which repositories' code a
// host is offered, not whether a job that reaches a daemon can escape it
// (docs/threads.md §8.5, §8.7).

package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

// AdminRunnerTenancyDeps is the TENANCY half of the /admin/runners seam: the
// mint that carries the admin-set repository policy, and the in-place policy
// edit for a runner already registered. AdminDeps.MintRunner (a name-only mint
// in admin.go) cannot express either, and AdminDeps belongs to a concurrent
// change, so the policy writes live in this file and the page reads them
// instead. A nil field renders the honest not-wired note and never a form that
// would 503 (composition binds both via BindAdminRunnerTenancy).
type AdminRunnerTenancyDeps struct {
	Mint func(ctx context.Context, in threads.MintInput, p auth.Principal) (id, token, expires string, err error)
	// SetPolicy rewrites ONE runner's recorded policy (the service validates,
	// CASes only allow_all/repos and audits it). actor is the verified
	// administrator's name: the page gate has already run.
	SetPolicy func(ctx context.Context, id string, in threads.RunnerPolicy, actor string) (threads.RunnerPolicy, error)
}

var runnerTenancy AdminRunnerTenancyDeps

// BindAdminRunnerTenancy installs the tenancy seam (composition, once at mount
// time).
func BindAdminRunnerTenancy(d AdminRunnerTenancyDeps) { runnerTenancy = d }

// AdminRunnerSecurity is /admin/runners' security card: the CURRENT image
// policy as read-only facts, each linked to the exact Configuration row that
// owns it, plus the warning copy. It is deliberately not an editor — there is
// one save path, on /admin/config.
type AdminRunnerSecurity struct {
	Known bool
	// AllowText is "unrestricted — any public image" when the allowlist is
	// empty, else the configured prefixes joined for display.
	AllowText string
	// Digest reports threads.image_require_digest; DigestText is its human
	// reading (required/not required).
	Digest     bool
	DigestText string
	AllowHref  string
	DigestHref string
}

// adminRunnerSecurity builds the card from the effective configuration. A nil
// config (unwired build) leaves Known=false: the page says the facts cannot be
// reported instead of inventing "unrestricted".
func adminRunnerSecurity(cfg *config.Config) AdminRunnerSecurity {
	card := AdminRunnerSecurity{
		AllowHref:  "/admin/config#row-threads.image_allow_prefixes",
		DigestHref: "/admin/config#row-threads.image_require_digest",
	}
	if cfg == nil {
		return card
	}
	card.Known = true
	if len(cfg.Threads.ImageAllowPrefixes) == 0 {
		card.AllowText = "unrestricted — any public image"
	} else {
		card.AllowText = strings.Join(cfg.Threads.ImageAllowPrefixes, ", ")
	}
	card.Digest = cfg.Threads.ImageRequireDigest
	if card.Digest {
		card.DigestText = "required — every job image must be pinned to a @sha256: digest"
	} else {
		card.DigestText = "not required — a mutable tag is accepted"
	}
	return card
}

// AdminRunnerRow is one fleet row (never a secret).
type AdminRunnerRow struct {
	ID         string
	Name       string
	Labels     string
	Capacity   string
	State      string
	LastSeen   string
	CurrentJob string
	Revoked    bool
	// AllowAll / Repos are the ADMIN-set repository policy; ForkRuns is the
	// recorded trust class (`allow_fork_runs`). Both are facts about the
	// recorded registry document, never inputs the runner supplied.
	AllowAll bool
	Repos    []string
	ForkRuns bool

	// PolicyOpen / FormAllowAll / FormRepos are the inline policy editor's state
	// for ONE row: set by a rejected POST so the details renders open with the
	// typed values echoed (the message itself is the view's PolicyError). Unset
	// on a normal render (the form then shows the recorded policy).
	PolicyOpen   bool
	FormAllowAll bool
	FormRepos    string
	// PolicyWired is true when the in-place edit seam is bound; false renders
	// the record read-only (the honest not-wired note, never a dead form).
	PolicyWired bool
}

// PolicyText is the row's tenancy reading, in FULL. The fleet is a grid of
// cards, so a long allowlist wraps inside its card instead of being truncated
// to a table cell's width (a card that hides part of the policy would be a
// silent lie about who may be offered this host). An empty policy is "none — no
// repository allowed" — the honest reading of a record written before tenancy
// existed (it is never shown as "all").
func (r AdminRunnerRow) PolicyText() string {
	if r.AllowAll {
		return "all repositories"
	}
	if len(r.Repos) == 0 {
		return "none — no repository allowed"
	}
	return strings.Join(r.Repos, ", ")
}

// LabelChips splits the recorded label list for the card's wrapping chip row.
// A runner with no declared labels says so (the record's em-dash placeholder is
// not a label).
func (r AdminRunnerRow) LabelChips() []string {
	parts := strings.Split(r.Labels, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "—" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// IsRevoked is the row's terminal state, read from either field: the registry
// projection may carry the state string alone (the composition seam sets
// State), so a revoked runner never renders a control the service would refuse.
func (r AdminRunnerRow) IsRevoked() bool { return r.Revoked || r.State == "revoked" }

// PolicyAllowAll is the inline editor's checkbox state: the typed value while a
// rejected edit is being corrected, the recorded policy otherwise.
func (r AdminRunnerRow) PolicyAllowAll() bool {
	if r.PolicyOpen {
		return r.FormAllowAll
	}
	return r.AllowAll
}

// PolicyRepos is the inline editor's pattern field: one pattern per line, typed
// value while correcting, recorded policy otherwise.
func (r AdminRunnerRow) PolicyRepos() string {
	if r.PolicyOpen {
		return r.FormRepos
	}
	return strings.Join(r.Repos, "\n")
}

// PolicyTitle is the trust class in words, for the cell's title attribute (and
// the place a summarised policy prints in full).
func (r AdminRunnerRow) PolicyTitle() string {
	if r.AllowAll {
		return "This runner may be offered a job from any repository on this instance."
	}
	if len(r.Repos) == 0 {
		return "No repository policy is recorded, so this runner is offered nothing. Set one here (all repositories, or owner/name and owner/* patterns)."
	}
	return "This runner is offered only these repositories: " + strings.Join(r.Repos, ", ")
}

// ForkRunsText is the trust class as text (never colour-only).
func (r AdminRunnerRow) ForkRunsText() string {
	if r.ForkRuns {
		return "on — untrusted fork code"
	}
	return "off — trusted code only"
}

// adminRunnersTab normalizes ?tab=. The fleet is the page's default view, so an
// absent or unknown value lands there (a hand-written URL never renders a blank
// panel).
func adminRunnersTab(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "mint":
		return "mint"
	case "security":
		return "security"
	default:
		return "fleet"
	}
}

// AdminRunnersView is /admin/runners.
type AdminRunnersView struct {
	// Tab is the active view: "fleet" (default), "mint" or "security". They are
	// server-rendered ?tab= links, so every view is a deep link and works with
	// no JS.
	Tab string
	// Wired is false when the threads service is not bound on this build.
	Wired bool
	// MintWired is true when the policy-aware mint seam is bound; false renders
	// the honest not-wired note instead of a form that would 503.
	MintWired bool
	// PolicyWired is true when the in-place policy edit is bound; false renders
	// each row's recorded policy read-only instead of a dead form.
	PolicyWired bool
	Rows        []AdminRunnerRow
	// NewRunnerToken is the one-time registration token from the mint that just
	// rendered this page; Expires is its TTL. They are never in a URL.
	NewRunnerToken string
	Expires        string
	Error          string
	// PolicyError is a rejected inline policy edit's message (the service's own
	// text, so the page and the API say the same thing), announced above the
	// fleet; the offending row renders open with the typed values echoed.
	PolicyError string
	// AllowAll / ReposText echo a rejected mint's policy fields back into the
	// form so an admin does not retype them.
	AllowAll  bool
	ReposText string
}

// FleetTabURL / MintTabURL / SecurityTabURL are the tab links. Fleet is the
// default view, so its URL omits ?tab= (one canonical spelling per view).
func (v *AdminRunnersView) FleetTabURL() string    { return "/admin/runners" }
func (v *AdminRunnersView) MintTabURL() string     { return "/admin/runners?tab=mint" }
func (v *AdminRunnersView) SecurityTabURL() string { return "/admin/runners?tab=security" }

// markPolicyWired stamps every row with whether the inline editor is bound, so
// the template never renders a form the service would refuse with a 503.
func markPolicyWired(view *AdminRunnersView) {
	if view == nil {
		return
	}
	for i := range view.Rows {
		view.Rows[i].PolicyWired = view.PolicyWired
	}
}

// adminRunnersPage answers GET /admin/runners.
func (s *Server) adminRunnersPage(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "runners")
	if !ok {
		return
	}
	s.adminShell(p, page, "runners")
	view := &AdminRunnersView{
		Tab:         adminRunnersTab(r.URL.Query().Get("tab")),
		MintWired:   runnerTenancy.Mint != nil,
		PolicyWired: runnerTenancy.SetPolicy != nil,
	}
	d := adminDeps()
	if d.Runners == nil {
		view.Error = "the Threads runner service is not wired on this build"
	} else if rows, err := d.Runners(r.Context(), p); err != nil {
		view.Error = err.Error()
	} else {
		view.Wired = true
		view.Rows = rows
	}
	markPolicyWired(view)
	if r.URL.Query().Get("revoked") == "1" {
		page.Notice = "runner revoked"
	}
	// The edited runner's id travels in the query (bounded, escaped on render)
	// so the notice names what changed; the fleet below is the store truth.
	if id := truncateQuery(r.URL.Query().Get("policy"), 40); id != "" {
		page.Notice = "repository policy updated for runner " + id
	}
	page.RunnerSecurity = adminRunnerSecurity(s.env.Cfg)
	page.Runners = view
	s.adminRender(w, r, p, http.StatusOK, page)
}

// truncateQuery bounds a query string echoed into a notice (the id is the
// service's own, but a hand-written URL is not).
func truncateQuery(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// adminRunnerMint answers POST /admin/runners: mint a one-time token (with the
// ADMIN-set repository policy) and render it in place with the fleet (never a
// redirect, so the secret cannot reach a URL or the history).
func (s *Server) adminRunnerMint(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "runners")
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "a cross-origin form submission is refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	s.adminShell(p, page, "runners")
	pol := runnerPolicyInput(r)
	d := adminDeps()
	// The mint answer (the one-time token) lives on the mint tab, so a POST
	// renders that view regardless of the tab the request named.
	view := &AdminRunnersView{
		Tab: "mint", Wired: d.Runners != nil, MintWired: runnerTenancy.Mint != nil,
		PolicyWired: runnerTenancy.SetPolicy != nil,
		AllowAll:    pol.AllowAll, ReposText: strings.Join(pol.Repos, "\n"),
	}
	status := http.StatusOK
	if runnerTenancy.Mint == nil {
		view.Error = "the Threads runner service is not wired on this build"
	} else if id, token, expires, err := runnerTenancy.Mint(r.Context(), threads.MintInput{
		Name: strings.TrimSpace(r.PostFormValue("name")), RunnerPolicyInput: pol,
	}, p); err != nil {
		view.Error = err.Error()
		// A malformed/empty policy is a form mistake: say so with a status a
		// client can act on, and re-render the typed values.
		if errors.Is(err, threads.ErrInvalid) {
			status = http.StatusUnprocessableEntity
		}
	} else {
		view.NewRunnerToken = token
		view.Expires = expires
		page.Notice = "registration token minted for runner " + id + " — copy it now, it is shown once"
	}
	if d.Runners != nil {
		if rows, err := d.Runners(r.Context(), p); err == nil {
			view.Rows = rows
		}
	}
	markPolicyWired(view)
	page.Runners = view
	page.RunnerSecurity = adminRunnerSecurity(s.env.Cfg)
	w.Header().Set("Cache-Control", "no-store") // the one-time token must not be cached
	s.adminRender(w, r, p, status, page)
}

// adminRunnerPolicy answers POST /admin/runners/policy: change one REGISTERED
// runner's repository policy in place (the audited SetRunnerPolicy write, the
// same call the JSON lane makes). The id travels in the body for the revoke
// form's reason (a literal multi-segment /admin path would overlap the repo
// family's wildcard POST patterns). A success redirects to the fleet so the row
// shows the recorded policy; a refusal re-renders the page with the service's
// own message and the offending row's form open, echoing what was typed.
func (s *Server) adminRunnerPolicy(w http.ResponseWriter, r *http.Request) {
	p, page, ok := s.requireAdminPage(w, r, "runners")
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "a cross-origin form submission is refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PostFormValue("id")))
	if id == "" {
		s.errorPage(w, r, http.StatusBadRequest, "a runner id is required")
		return
	}
	if runnerTenancy.SetPolicy == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "the Threads runner service is not wired on this build")
		return
	}
	pol := runnerPolicyInput(r)
	if _, err := runnerTenancy.SetPolicy(r.Context(), id,
		threads.RunnerPolicy{AllowAll: pol.AllowAll, Repos: pol.Repos}, p.Name); err != nil {
		// A malformed/empty policy is a form mistake (422, like the mint); a
		// revoked or unknown runner is a state refusal (409/404) that the
		// message spells out.
		status := http.StatusConflict
		switch {
		case errors.Is(err, threads.ErrInvalid):
			status = http.StatusUnprocessableEntity
		case errors.Is(err, threads.ErrNotFound):
			status = http.StatusNotFound
		}
		s.adminShell(p, page, "runners")
		d := adminDeps()
		// The offending row (and its open form) is on the fleet tab.
		view := &AdminRunnersView{
			Tab: "fleet", Wired: d.Runners != nil, MintWired: runnerTenancy.Mint != nil, PolicyWired: true,
			PolicyError: err.Error(),
		}
		if d.Runners != nil {
			if rows, rerr := d.Runners(r.Context(), p); rerr == nil {
				view.Rows = rows
			}
		}
		for i := range view.Rows {
			if strings.EqualFold(view.Rows[i].ID, id) {
				view.Rows[i].PolicyOpen = true
				view.Rows[i].FormAllowAll = pol.AllowAll
				view.Rows[i].FormRepos = strings.Join(pol.Repos, "\n")
			}
		}
		markPolicyWired(view)
		page.RunnerSecurity = adminRunnerSecurity(s.env.Cfg)
		page.Runners = view
		s.adminRender(w, r, p, status, page)
		return
	}
	redirectPage(w, r, "/admin/runners?policy="+url.QueryEscape(id)+"&tab=fleet")
}

// runnerPolicyInput reads a policy form's tenancy fields: the "all repositories"
// checkbox and the pattern list (one per line, or comma/space separated).
// Validation is the SERVICE's (threads.ValidateRunnerPolicy) so the page and the
// JSON lane share one rule and one spelling.
func runnerPolicyInput(r *http.Request) threads.RunnerPolicyInput {
	raw := strings.TrimSpace(r.PostFormValue("allow_all"))
	in := threads.RunnerPolicyInput{
		AllowAll: raw == "1" || strings.EqualFold(raw, "true") || strings.EqualFold(raw, "on"),
	}
	in.Repos = strings.FieldsFunc(r.PostFormValue("repos"), func(c rune) bool {
		return c == ',' || c == '\n' || c == '\r' || c == '\t' || c == ' '
	})
	return in
}

// adminRunnerRevoke answers POST /admin/runners/revoke. The id travels in the
// body because a literal multi-segment /admin path would overlap the repo
// family's wildcard POST patterns (the /admin/reports/resolve precedent).
func (s *Server) adminRunnerRevoke(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.requireAdminPage(w, r, "runners")
	if !ok {
		return
	}
	if !settingsSameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "a cross-origin form submission is refused")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PostFormValue("id")))
	if id == "" {
		s.errorPage(w, r, http.StatusBadRequest, "a runner id is required")
		return
	}
	d := adminDeps()
	if d.RevokeRunner == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "the Threads runner service is not wired on this build")
		return
	}
	if err := d.RevokeRunner(r.Context(), id, p); err != nil {
		page := &AdminPage{}
		s.adminShell(p, page, "runners")
		page.Error = err.Error()
		page.RunnerSecurity = adminRunnerSecurity(s.env.Cfg)
		page.Runners = &AdminRunnersView{Tab: "fleet", Wired: true, PolicyWired: runnerTenancy.SetPolicy != nil}
		if d.Runners != nil {
			if rows, rerr := d.Runners(r.Context(), p); rerr == nil {
				page.Runners.Rows = rows
			}
		}
		markPolicyWired(page.Runners)
		s.adminRender(w, r, p, http.StatusConflict, page)
		return
	}
	redirectPage(w, r, "/admin/runners?revoked=1&tab=fleet")
}

// ChipClass / ChipLabel map a registry state onto the shared chip classes and
// its TEXT label (state is never colour-only).
func (r AdminRunnerRow) ChipClass() string {
	switch r.State {
	case "active":
		return "chip-open"
	case "revoked":
		return "chip-closed"
	default:
		return "chip chip-neutral"
	}
}

func (r AdminRunnerRow) ChipLabel() string {
	switch r.State {
	case "active", "revoked":
		return r.State
	default:
		return "pending"
	}
}
