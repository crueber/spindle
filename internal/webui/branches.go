package webui

// branches.go — the Branches page (`/{o}/{r}/branches`) and its create/delete
// writes (repo-browsing parity M2; argv in docs/go/04_git.md §16.2).
//
// Reads come from api.RepoBrowse.Branches: ONE for-each-ref spawn supplies
// every branch's tip, subject, age and ahead/behind against the default (the
// per-branch rev-list shape is deliberately not used). Writes are the
// branches.Service seams, which own the push gate, the default-branch refusal
// and the stored-policy protection check — the page never restates a rule.
//
// The page gate mirrors the API gate exactly: repoGate (visibility first) then
// repoOpen (existence + refs sync + summary). A write additionally runs the
// parity preflight (same-origin, anonymous → login interstitial) and the
// service's own CheckPush floor.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/branches"
)

type branchRow struct {
	Name         string
	SHA          string
	Short        string
	Subject      string
	Author       string
	Stamp        string
	Ahead        int
	Behind       int
	IsDefault    bool
	TreeHref     string
	CommitHref   string
	CompareHref  string
	CloneCommand string
	DeleteAction string
	CanDelete    bool
	Vs           string
}

type branchesPage struct {
	Repo       *RepoChrome
	Guide      *repoGuide
	Default    string
	AheadKnown bool
	Rows       []branchRow
	More       bool
	CanWrite   bool
	// WriteHref is the anonymous viewer's login interstitial (the blob page's
	// collabWriteHref rule): never a form that cannot submit.
	WriteHref   string
	FromDefault string
	FormName    string
	FormFrom    string
	Err         string
	Flash       string
}

func (s *Server) repoBranches(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	data := &branchesPage{Repo: c, Flash: formsFlash(r)}
	if c.Empty {
		data.Guide = s.repoGuide(c)
		s.page(w, r, rq.p, http.StatusOK, "repo/branches", "Branches · "+c.Full, data)
		return
	}
	browse, ok := s.env.API.Repo.(api.RepoBrowse)
	if !ok {
		s.errorPage(w, r, http.StatusServiceUnavailable, "branches are unavailable on this build")
		return
	}
	ov, err := browse.Branches(r.Context(), rq.id)
	if err != nil {
		s.repoViewError(w, r, err)
		return
	}
	s.applyBranches(r, rq, c, data, ov)
	s.page(w, r, rq.p, http.StatusOK, "repo/branches", "Branches · "+c.Full, data)
}

// applyBranches fills the render model from the overview and the viewer's role.
func (s *Server) applyBranches(r *http.Request, rq repoRequest, c *RepoChrome, d *branchesPage, ov api.BranchOverview) {
	d.Rows = nil
	d.Default = ov.Default
	d.AheadKnown = ov.AheadKnown
	d.More = ov.More
	d.FromDefault = ov.Default
	d.CanWrite = repoCanWrite(r.Context(), rq.id.Owner, rq.id.Name, rq.p) && repoDeps.BranchCreate != nil
	if !d.CanWrite && s.anonWrite(rq.p) && repoDeps.BranchCreate != nil {
		d.WriteHref = s.collabWriteHref(rq.p, "/"+c.Full+"/branches", "Create a branch")
	}
	for _, b := range ov.Branches {
		row := branchRow{
			Name:         b.Name,
			SHA:          b.SHA,
			Short:        shortSHA(b.SHA),
			Subject:      b.Subject,
			Author:       b.Author,
			Stamp:        rfc3339(b.Committed),
			Ahead:        b.Ahead,
			Behind:       b.Behind,
			IsDefault:    b.IsDefault,
			TreeHref:     "/" + c.Full + "/tree/" + escapePath(b.Name),
			CommitHref:   "/" + c.Full + "/commit/" + b.SHA,
			CloneCommand: "git clone --branch " + b.Name + " " + c.CloneURL,
		}
		if ov.Default != "" && b.Name != ov.Default {
			row.CompareHref = "/" + c.Full + "/compare/" + escapePath(ov.Default) + "..." + escapePath(b.Name)
		}
		row.DeleteAction = "/" + c.Full + "/branches/delete"
		row.CanDelete = d.CanWrite && !b.IsDefault
		switch {
		case b.IsDefault:
			row.Vs = "base"
		case ov.AheadKnown:
			row.Vs = fmt.Sprintf("%d ahead · %d behind", b.Ahead, b.Behind)
		default:
			row.Vs = "—"
		}
		d.Rows = append(d.Rows, row)
	}
}

// repoBranchCreate serves POST /{o}/{r}/branches.
func (s *Server) repoBranchCreate(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, rq.p, "Create a branch") {
		return
	}
	if repoDeps.BranchCreate == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "branch creation is not wired on this build")
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
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
	from := strings.TrimSpace(r.PostFormValue("from"))
	if name == "" {
		s.branchWriteError(w, r, rq, c, http.StatusBadRequest, "Give the branch a name.", "", from)
		return
	}
	if err := repoDeps.BranchCreate(r.Context(), rq.id.Owner, rq.id.Name, rq.p, name, from); err != nil {
		s.branchWriteError(w, r, rq, c, branchErrStatus(err), branchErrMessage(err), name, from)
		return
	}
	s.formsRedirectFlash(w, r, "/"+rq.full+"/branches", "Branch "+name+" created")
}

// repoBranchDelete serves POST /{o}/{r}/branches/delete (a form field carries
// the name: branch names contain slashes and must not fight the route shape).
func (s *Server) repoBranchDelete(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	if !s.parityWritePreflight(w, r, rq.p, "Delete a branch") {
		return
	}
	if repoDeps.BranchDelete == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "branch deletion is not wired on this build")
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
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
	if name == "" {
		s.branchWriteError(w, r, rq, c, http.StatusBadRequest, "Choose a branch to delete.", "", "")
		return
	}
	if err := repoDeps.BranchDelete(r.Context(), rq.id.Owner, rq.id.Name, rq.p, name); err != nil {
		s.branchWriteError(w, r, rq, c, branchErrStatus(err), branchErrMessage(err), "", "")
		return
	}
	s.formsRedirectFlash(w, r, "/"+rq.full+"/branches", "Branch "+name+" deleted")
}

// branchWriteError re-renders the page with the service's text (a rejected
// write lands on the page it was posted from, with the form's values kept).
func (s *Server) branchWriteError(w http.ResponseWriter, r *http.Request, rq repoRequest, c *RepoChrome, status int, msg, name, from string) {
	data := &branchesPage{Repo: c}
	if c.Empty {
		data.Guide = s.repoGuide(c)
		data.Err, data.FormName, data.FormFrom = msg, name, from
		s.page(w, r, rq.p, status, "repo/branches", "Branches · "+c.Full, data)
		return
	}
	if browse, ok := s.env.API.Repo.(api.RepoBrowse); ok {
		if ov, err := browse.Branches(r.Context(), rq.id); err == nil {
			s.applyBranches(r, rq, c, data, ov)
		}
	}
	data.Err, data.FormName, data.FormFrom = msg, name, from
	s.page(w, r, rq.p, status, "repo/branches", "Branches · "+c.Full, data)
}

// branchErrStatus maps the service's sentinel family onto the page status.
func branchErrStatus(err error) int {
	switch {
	case errors.Is(err, branches.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, branches.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, branches.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, branches.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, branches.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func branchErrMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
