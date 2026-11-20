package webui

// parity_repo.go — the repo group's last SPA-parity affordances, restored as
// server-rendered forms + plain POSTs (D-WEB-14; no new JS):
//
//	POST /{owner}/{repo}/star       social.Service.Star    → 303 back
//	POST /{owner}/{repo}/unstar     social.Service.Unstar  → 303 back
//	POST /{owner}/{repo}/watch      notify.SetWatch(true)  → 303 back
//	POST /{owner}/{repo}/unwatch    notify.SetWatch(false) → 303 back
//	POST /{owner}/{repo}/tag        tags.Service.CreateTag → 303 to the tag
//
// Semantics are the SPA's (Repo.jsx StarToggle/WatchToggle, Commit.jsx
// CreateTag) and the JSON twin's (07 §§7–8, Forgejo #253/#263):
//
//   - The header renders the counts + viewer state server-side; the write is a
//     POST and the 303 re-reads the header, so there is no optimistic flip to
//     reconcile (the SPA's client state machine has no server twin).
//   - Anonymous viewers get the login-required interstitial (?next=), exactly
//     like lib/writeGate.js's anonWriteTarget — never an in-band "sign in".
//   - Feature-disabled star/watch is refused by the service (403, #522) and
//     the header renders the disabled affordance with the reason instead of a
//     form that could only fail.
//   - Create tag hides without repo write authority (the SPA's
//     roleAtLeast(role, "write")); the service re-checks role + protect
//     policy, and its error text re-renders the commit page inline (400 name
//     / sha, 409 exists, 404 unknown sha, 503 unavailable) — never a 500.
//
// The seams are RepoDeps method values (repo.go); an unbound seam omits the
// affordance rather than shipping a dead button.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/notify"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/social"
	"github.com/crueber/spindle/internal/tags"
)

// commitsChecksBatch is the commits page's ONE check-index read: the service
// caps a page at 200 (checks.Service.ListChecks n-max), and a commits window
// is 35 rows, so one bounded call covers every visible row. Per-row Combined
// calls would be an unbounded fan-out (the SPA's shape, deliberately not
// reproduced).
const commitsChecksBatch = 200

// repoCanWrite is the create-tag write gate: the viewer's identity role when
// the identity surface is wired, else the principal's write flag (the SPA's
// fail-open behaviour when useRole has no answer). Host admins always pass,
// matching tags.Service.requireRole.
func repoCanWrite(ctx context.Context, owner, repo string, p auth.Principal) bool {
	if p.Anonymous {
		return false
	}
	if p.Admin {
		return true
	}
	if repoDeps.RoleOf != nil {
		return formsRoleAtLeast(repoDeps.RoleOf(ctx, owner, repo, p), identity.RoleWrite)
	}
	return p.Write
}

// parityErrStatus maps the feature services behind the parity writes onto the
// page status. The sentinels come from three packages whose status tables are
// package-private (tags.statusFor, social.statusFor, notify.statusFor), so
// they are restated here via errors.Is — the wrapped-%w shapes the services
// actually return. Anything unrecognised falls through to formsErrStatus and
// finally to 503 (never a 500 page for a service hiccup).
func parityErrStatus(err error) int {
	switch {
	case errors.Is(err, tags.ErrInvalid), errors.Is(err, social.ErrInvalid),
		errors.Is(err, notify.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, tags.ErrConflict), errors.Is(err, social.ErrConflict),
		errors.Is(err, notify.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, tags.ErrNotFound), errors.Is(err, social.ErrNotFound),
		errors.Is(err, notify.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, tags.ErrUnauthorized), errors.Is(err, social.ErrUnauthorized),
		errors.Is(err, notify.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, tags.ErrForbidden), errors.Is(err, social.ErrForbidden),
		errors.Is(err, notify.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, tags.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, tags.ErrUnsupported):
		return http.StatusUnprocessableEntity
	}
	return formsErrStatus(err)
}

// parityErrMessage is the service text verbatim (07 §2's plain-text rule).
func parityErrMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// repoReturnTarget resolves a toggle form's `next` field: a local path only
// (no scheme, no //host), falling back to the repo page. The redirect is a
// GET, so the header re-reads the counters — the "refresh by redirect" rule.
func repoReturnTarget(r *http.Request, full string) string {
	next := strings.TrimSpace(r.PostFormValue("next"))
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/" + full
	}
	return next
}

// parityWritePreflight is the shared POST preflight for every parity write
// (the repo toggles, the tag form, the owner profile/avatar forms): same-origin
// FIRST, so a cross-site form can never reach a service call; then the
// anonymous-write redirect (a 303 to the interstitial, never a silent no-op);
// then a hard refusal for an anonymous principal on a config-less instance (no
// auth mode to route to). The caller runs the read gate first (visibility), so
// a cross-origin write to an invisible repo answers the same 404 as any other
// invisible read — no state changes either way.
func (s *Server) parityWritePreflight(w http.ResponseWriter, r *http.Request, p auth.Principal, action string) bool {
	if !formsOriginOK(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin request refused")
		return false
	}
	if s.formsAnonymousViewer(p) {
		s.formsRedirect(w, r, formsLoginRequired(r, action))
		return false
	}
	if p.Anonymous {
		s.errorPage(w, r, http.StatusUnauthorized, "authentication required")
		return false
	}
	return true
}

// --- star toggles ------------------------------------------------------------

func (s *Server) repoStar(w http.ResponseWriter, r *http.Request) {
	s.repoStarToggle(w, r, true)
}

func (s *Server) repoUnstar(w http.ResponseWriter, r *http.Request) {
	s.repoStarToggle(w, r, false)
}

// repoStarToggle serves POST …/star and …/unstar. Auth: the service's rules —
// Star needs the read gate + the star feature; Unstar always works for an
// authenticated principal (a retained star must stay removable, #522).
func (s *Server) repoStarToggle(w http.ResponseWriter, r *http.Request, on bool) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	action := "Star this repository"
	if !on {
		action = "Unstar this repository"
	}
	if !s.parityWritePreflight(w, r, rq.p, action) {
		return
	}
	fn := repoDeps.Star
	if !on {
		fn = repoDeps.Unstar
	}
	if fn == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "starring is not wired on this build")
		return
	}
	if _, err := fn(r.Context(), rq.p, rq.id.Owner, rq.id.Name); err != nil {
		s.errorPage(w, r, parityErrStatus(err), parityErrMessage(err))
		return
	}
	s.formsRedirect(w, r, repoReturnTarget(r, rq.full))
}

// --- watch toggles -----------------------------------------------------------

func (s *Server) repoWatch(w http.ResponseWriter, r *http.Request) {
	s.repoWatchToggle(w, r, true)
}

func (s *Server) repoUnwatch(w http.ResponseWriter, r *http.Request) {
	s.repoWatchToggle(w, r, false)
}

// repoWatchToggle serves POST …/watch and …/unwatch. notify.SetWatch takes the
// principal NAME (that is its wire shape), so the seam passes p.Name through.
func (s *Server) repoWatchToggle(w http.ResponseWriter, r *http.Request, on bool) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	action := "Watch this repository"
	if !on {
		action = "Unwatch this repository"
	}
	if !s.parityWritePreflight(w, r, rq.p, action) {
		return
	}
	if repoDeps.WatchSet == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "watching is not wired on this build")
		return
	}
	if _, err := repoDeps.WatchSet(r.Context(), rq.p.Name, rq.id.Owner, rq.id.Name, on); err != nil {
		s.errorPage(w, r, parityErrStatus(err), parityErrMessage(err))
		return
	}
	s.formsRedirect(w, r, repoReturnTarget(r, rq.full))
}

// --- create tag -------------------------------------------------------------

// repoCreateTag serves POST /{owner}/{repo}/tag from the commit page's form.
// On success it redirects to the commits list AT THE NEW TAG with a notice;
// on a rejection it re-renders the commit page with the message and the
// submitted values.
//
// The ref is the tag's SHORT name (refs/tags/ stripped): the commits page's
// ?ref= feeds api.RepoView.Resolve, which takes short names and prefers a
// branch of the same name (the §9.3 contract) — a repo with both a branch and
// a tag named "v1" lands on the branch's history. That is the resolve
// contract's own rule, not a page choice.
func (s *Server) repoCreateTag(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, rq.p, "Create a tag") {
		return
	}
	if repoDeps.Tags == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "tag creation is not wired on this build")
		return
	}
	if !repoCanWrite(r.Context(), rq.id.Owner, rq.id.Name, rq.p) {
		s.errorPage(w, r, http.StatusForbidden, "write access required")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	message := r.PostFormValue("message")
	sha := strings.TrimSpace(r.PostFormValue("sha"))

	data, ok := s.commitPageData(w, r, rq, sha)
	if !ok {
		return
	}
	data.TagName, data.TagMessage = name, message
	if data.Guide != nil {
		// The form does not render on the empty-repo guide; a POST there is a
		// stale page or a hand-rolled request.
		s.errorPage(w, r, http.StatusNotFound, "tag target not found in this repository")
		return
	}
	if name == "" {
		data.TagErr = "Give the tag a name."
		s.page(w, r, rq.p, http.StatusBadRequest, "repo/commit", shortSHA(data.SHA)+" · "+rq.full, data)
		return
	}
	tag, err := repoDeps.Tags(r.Context(), rq.id.Owner, rq.id.Name, rq.p, tags.CreateInput{
		Name:    name,
		SHA:     data.SHA,
		Message: message,
	})
	if err != nil {
		data.TagErr = parityErrMessage(err)
		s.page(w, r, rq.p, parityErrStatus(err), "repo/commit", shortSHA(data.SHA)+" · "+rq.full, data)
		return
	}
	target := "/" + rq.full + "/commits?ref=" + url.QueryEscape(shortRefName(tag.Ref))
	s.formsRedirectFlash(w, r, target, "Tag "+tag.Name+" created")
}
