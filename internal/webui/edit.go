// edit.go — Feature F (docs/PLAN-V2.md §4, docs/features/17_file_edit.md):
// the browser file editor's three pages and their form write. The templates
// compose only the shared vocabulary (docs/style-guideline.md): `.card`,
// `.btn`/`.primary`, `.link`, `.pill`, `.input`, `.err-line`, the code view's
// mono grid and the repo group's own chrome templates.
//
// Routes (docs/go/12_web_ui.md §2):
//
//	GET  /{owner}/{repo}/new[/{rest...}]      create file
//	GET  /{owner}/{repo}/edit/{rest...}       edit file
//	GET  /{owner}/{repo}/delete/{rest...}     delete file
//	POST /{owner}/{repo}/contents             the one form write
//
// The write path re-gates in internal/webedit (push floor + policy), so the
// page gate here is UX only — never the security boundary.
package webui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/webedit"
)

// WebeditDeps is the editor's feature seam (composition binds it once at
// boot; nil fields render 503, never a fabricated page).
type WebeditDeps struct {
	Save    func(ctx context.Context, owner, repo string, p auth.Principal, in webedit.Input) (*webedit.Result, error)
	Preview func(ctx context.Context, owner, repo string, p auth.Principal, in webedit.Input) (string, error)
	Load    func(ctx context.Context, owner, repo string, p auth.Principal, ref, path string) (*webedit.File, error)
	// CanPush is the same push gate the API consults (identity.CheckPush).
	// It decides whether the affordances render at all; nil means no.
	CanPush  func(ctx context.Context, owner, repo string, p auth.Principal) bool
	MaxBytes int64
}

var webeditDeps WebeditDeps

// BindWebedit installs the editor service (boot-time; never per request).
func BindWebedit(d WebeditDeps) { webeditDeps = d }

func init() { registerGroup(mountEdit) }

func mountEdit(s *Server) {
	s.mux.HandleFunc("GET /{owner}/{repo}/new", s.editPage)
	s.mux.HandleFunc("GET /{owner}/{repo}/new/{rest...}", s.editPage)
	s.mux.HandleFunc("GET /{owner}/{repo}/edit/{rest...}", s.editPage)
	s.mux.HandleFunc("GET /{owner}/{repo}/delete/{rest...}", s.editPage)
	s.mux.HandleFunc("POST /{owner}/{repo}/contents", s.editPost)
}

// editorPage is the shared view for the three modes.
type editorPage struct {
	Repo   *RepoChrome
	Guide  *repoGuide
	Mode   string // "new" | "edit" | "delete"
	Action string
	Branch string
	Path   string

	Message           string
	Description       string
	Content           string
	ExpectedBlobSHA   string
	ExpectedBranchSHA string
	NewBranch         string
	OpenPR            bool
	PRTitle           string
	PRBody            string

	Branches []repoBranchRef

	File     *webedit.File
	MaxBytes string
	Binary   bool
	LFS      bool
	TooLarge bool
	Size     string
	RawHref  string
	BackHref string

	Err       string
	Conflict  *conflictView
	Diff      []commitFile
	DiffShown bool
}

type conflictView struct {
	What     string
	Expected string
	Actual   string
}

// editMaxBytes resolves the editor cap for display.
func editorMaxBytes() int64 {
	if webeditDeps.MaxBytes > 0 {
		return webeditDeps.MaxBytes
	}
	return webedit.DefaultMaxBytes
}

// editPage answers the three GET routes.
func (s *Server) editPage(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	// The editor is a write surface: an anonymous viewer in token/oidc mode
	// gets the login interstitial (the forms convention), a real read-only
	// principal gets a 403 page.
	if s.formsAnonymousViewer(rq.p) {
		s.formsRedirect(w, r, formsLoginRequired(r, "Edit a file"))
		return
	}
	if webeditDeps.Save == nil || webeditDeps.CanPush == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "file editing is not wired on this build")
		return
	}
	if !webeditDeps.CanPush(r.Context(), rq.id.Owner, rq.id.Name, rq.p) {
		s.errorPage(w, r, http.StatusForbidden, "write access required")
		return
	}
	c, ok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !ok {
		return
	}
	mode, rest := editRoute(r)
	data := &editorPage{Repo: c, Mode: mode, MaxBytes: fmtSize(editorMaxBytes()), OpenPR: true}
	data.Action = "/" + c.Full + "/contents"
	if c.Empty {
		data.Guide = s.repoGuide(c)
		data.BackHref = "/" + c.Full
		s.page(w, r, rq.p, http.StatusOK, "repo/editor", "Code · "+c.Full, data)
		return
	}
	data.Branches = s.editBranches(r, rq)
	res, err := s.env.API.Repo.Resolve(r.Context(), rq.id, rest)
	if err != nil {
		s.repoViewError(w, r, err)
		return
	}
	if res.Ref == "" {
		s.errorPage(w, r, http.StatusNotFound, "branch not found")
		return
	}
	data.Branch = shortRefName(res.Ref)
	data.ExpectedBranchSHA = res.SHA
	data.Path = res.Path
	data.File = nil
	if mode == "new" {
		if q := strings.TrimSpace(r.URL.Query().Get("path")); q != "" && data.Path == "" {
			data.Path = q
		}
	} else {
		if data.Path == "" {
			s.errorPage(w, r, http.StatusNotFound, "file not found")
			return
		}
	}
	switch mode {
	case "new":
		data.Message = "Create " + data.Path
		data.BackHref = "/" + c.Full + "/tree/" + escapePath(data.Branch)
	case "edit", "delete":
		f, lerr := webeditDeps.Load(r.Context(), rq.id.Owner, rq.id.Name, rq.p, data.Branch, data.Path)
		if lerr != nil {
			s.editError(w, r, rq, data, lerr)
			return
		}
		data.File = f
		data.ExpectedBlobSHA = f.SHA
		data.Size = fmtSize(f.Size)
		data.Content = strings.ReplaceAll(string(f.Content), "\r\n", "\n")
		data.Binary = f.Binary
		data.LFS = f.LFS
		data.TooLarge = f.Size > editorMaxBytes()
		if mode == "edit" {
			data.Message = "Update " + data.Path
		} else {
			data.Message = "Delete " + data.Path
		}
		ref := data.Branch
		data.RawHref = "/" + c.Full + "/api/blob/" + escapePath(ref) + "/" + escapePath(data.Path) + "?raw"
		data.BackHref = "/" + c.Full + "/blob/" + escapePath(ref+"/"+data.Path)
	}
	// The editor's submit is the page's one primary action; projectPrimary
	// demotes the masthead Clone from the rendered content (style guideline F3).
	s.editRender(w, r, rq, data, http.StatusOK)
}

// editPost handles the one form write. `action=preview` re-renders the page
// with the pending diff and writes nothing.
func (s *Server) editPost(w http.ResponseWriter, r *http.Request) {
	rq, ok := s.repoGate(w, r)
	if !ok {
		return
	}
	if !s.formsOriginOK(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin request refused")
		return
	}
	if s.formsAnonymousViewer(rq.p) {
		s.formsRedirect(w, r, formsLoginRequired(r, "Edit a file"))
		return
	}
	if webeditDeps.Save == nil || webeditDeps.CanPush == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "file editing is not wired on this build")
		return
	}
	if !webeditDeps.CanPush(r.Context(), rq.id.Owner, rq.id.Name, rq.p) {
		s.errorPage(w, r, http.StatusForbidden, "write access required")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "form parse failed")
		return
	}
	mode := strings.TrimSpace(r.PostFormValue("mode"))
	switch mode {
	case "new":
		mode = "create"
	case "edit":
	case "delete":
	default:
		s.errorPage(w, r, http.StatusBadRequest, "unknown editor mode")
		return
	}
	in := webedit.Input{
		Mode:              webedit.Mode(mode),
		Ref:               strings.TrimSpace(r.PostFormValue("ref")),
		Path:              strings.TrimSpace(r.PostFormValue("path")),
		Message:           r.PostFormValue("message"),
		Description:       r.PostFormValue("description"),
		ExpectedBlobSHA:   strings.TrimSpace(r.PostFormValue("expected_blob_sha")),
		ExpectedBranchSHA: strings.TrimSpace(r.PostFormValue("expected_branch_sha")),
		NewBranch:         strings.TrimSpace(r.PostFormValue("new_branch")),
		OpenPR:            r.PostFormValue("open_pr") != "",
		PRTitle:           r.PostFormValue("pr_title"),
		PRBody:            r.PostFormValue("pr_body"),
	}
	if in.NewBranch == "" {
		// The checkbox defaults on (GitHub parity) and is simply inert for a
		// direct edit: the API still rejects open_pr without new_branch, but
		// the page's default form must not 400.
		in.OpenPR = false
	}
	if mode != "delete" {
		// Browsers submit textarea values with CRLF line endings; git text
		// is LF. The JSON API carries exact bytes (base64), the form
		// normalizes — documented in docs/features/17_file_edit.md.
		in.Content = []byte(normalizeFormText(r.PostFormValue("content")))
	}
	c, cok := s.repoOpen(w, r, rq, api.SyncRefs)
	if !cok {
		return
	}
	data := s.editDataFromForm(c, in)
	if r.PostFormValue("action") == "preview" {
		if webeditDeps.Preview == nil {
			data.Err = "Preview is not available on this build."
			s.editRender(w, r, rq, data, http.StatusServiceUnavailable)
			return
		}
		patch, perr := webeditDeps.Preview(r.Context(), rq.id.Owner, rq.id.Name, rq.p, in)
		if perr != nil {
			s.editFail(w, r, rq, data, perr)
			return
		}
		data.Diff = diffFilesFromPatch(patch)
		data.DiffShown = true
		s.editRender(w, r, rq, data, http.StatusOK)
		return
	}
	res, serr := webeditDeps.Save(r.Context(), rq.id.Owner, rq.id.Name, rq.p, in)
	if serr != nil {
		s.editFail(w, r, rq, data, serr)
		return
	}
	target := "/" + rq.full + "/blob/" + escapePath(res.Branch+"/"+in.Path)
	flash := "Committed " + shortSHA(res.Commit)
	if res.PRNum > 0 {
		target = "/" + rq.full + "/pull/" + strconv.Itoa(res.PRNum)
		flash = "Opened pull request #" + strconv.Itoa(res.PRNum)
	} else if in.Mode == webedit.ModeDelete {
		target = "/" + rq.full + "/tree/" + escapePath(res.Branch)
		flash = "Deleted " + in.Path + " (" + shortSHA(res.Commit) + ")"
	}
	s.formsRedirectFlash(w, r, target, flash)
}

// editDataFromForm re-seeds the page from a submitted form (so a rejected
// write never loses the user's text).
func (s *Server) editDataFromForm(c *RepoChrome, in webedit.Input) *editorPage {
	mode := string(in.Mode)
	if in.Mode == webedit.ModeCreate {
		mode = "new"
	}
	data := &editorPage{
		Repo: c, Mode: mode, Action: "/" + c.Full + "/contents",
		Branch: in.Ref, Path: in.Path, Message: in.Message, Description: in.Description,
		Content: string(in.Content), ExpectedBlobSHA: in.ExpectedBlobSHA,
		ExpectedBranchSHA: in.ExpectedBranchSHA, NewBranch: in.NewBranch,
		OpenPR: in.OpenPR, PRTitle: in.PRTitle, PRBody: in.PRBody,
		MaxBytes: fmtSize(editorMaxBytes()),
	}
	data.BackHref = "/" + c.Full + "/tree/" + escapePath(in.Ref)
	if in.Mode != webedit.ModeDelete {
		data.BackHref = "/" + c.Full + "/blob/" + escapePath(in.Ref+"/"+in.Path)
	}
	return data
}

// editFail maps a service error onto the page: a conflict re-renders with the
// banner + the fresh shas, everything else with an inline message and the
// matrix status.
func (s *Server) editFail(w http.ResponseWriter, r *http.Request, rq repoRequest, data *editorPage, err error) {
	var ce *webedit.ConflictError
	if errors.As(err, &ce) {
		data.Conflict = &conflictView{What: ce.What, Expected: ce.Expected, Actual: ce.Actual}
		data.Err = ce.Error()
		s.editRender(w, r, rq, data, http.StatusConflict)
		return
	}
	data.Err = err.Error()
	status := webedit.HTTPStatus(err)
	if status == http.StatusInternalServerError {
		status = http.StatusServiceUnavailable
	}
	s.editRender(w, r, rq, data, status)
}

// editError renders a load-side refusal (a GET on binary/oversize/LFS still
// shows the explanation card, not a blank error page).
func (s *Server) editError(w http.ResponseWriter, r *http.Request, rq repoRequest, data *editorPage, err error) {
	status := webedit.HTTPStatus(err)
	if status == http.StatusInternalServerError {
		status = http.StatusServiceUnavailable
	}
	if status == http.StatusNotFound || status == http.StatusBadRequest {
		s.errorPage(w, r, status, err.Error())
		return
	}
	data.Err = err.Error()
	s.editRender(w, r, rq, data, status)
}

// editRender renders the editor page (one attach point).
func (s *Server) editRender(w http.ResponseWriter, r *http.Request, rq repoRequest, data *editorPage, status int) {
	title := "Edit " + data.Path
	switch data.Mode {
	case "new":
		title = "New file"
	case "delete":
		title = "Delete " + data.Path
	}
	s.page(w, r, rq.p, status, "repo/editor", title+" · "+rq.full, data)
}

// editRoute derives the mode and the rest-of-path from the request.
func editRoute(r *http.Request) (mode, rest string) {
	rest = strings.TrimSuffix(r.PathValue("rest"), "/")
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(segs) >= 3 {
		switch segs[2] {
		case "edit":
			return "edit", rest
		case "delete":
			return "delete", rest
		}
	}
	return "new", rest
}

// editBranches loads the branch selector rows (one bounded page; the editor
// is a human-scale surface, not a ref browser).
func (s *Server) editBranches(r *http.Request, rq repoRequest) []repoBranchRef {
	if s.env.API == nil || s.env.API.Repo == nil {
		return nil
	}
	refs, _, err := s.env.API.Repo.RefList(r.Context(), rq.id, "heads", api.RefQuery{N: 500})
	if err != nil {
		return nil
	}
	out := make([]repoBranchRef, 0, len(refs))
	for _, ref := range refs {
		name := shortRefName(ref.Name)
		out = append(out, repoBranchRef{Name: name})
	}
	return out
}

// normalizeFormText converts the browser's CRLF textarea value to git's LF.
func normalizeFormText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// diffFilesFromPatch converts the preview patch to the repo_diff view rows
// (the same parser the commit page uses).
func diffFilesFromPatch(patch string) []commitFile {
	var out []commitFile
	for _, f := range ParseUnifiedDiff(patch) {
		cf := commitFile{Path: f.Path, Anchor: fileAnchor(f.Path), Added: f.Added, Deleted: f.Deleted, Binary: f.Binary}
		switch f.Status {
		case "added":
			cf.Flags = "added"
		case "deleted":
			cf.Flags = "deleted"
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
		out = append(out, cf)
	}
	return out
}
