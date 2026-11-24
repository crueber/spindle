package webui

// blame.go — the Blame page (`/{o}/{r}/blame/{ref}/{path}`), the last missing
// read surface of the Forgejo/GitHub browse parity audit (docs/go/12_web_ui.md
// §2; argv pinned in docs/go/04_git.md §16).
//
// It is the blob page's sibling, deliberately: the same read gate (repoGate +
// repoOpen), the same resolve (`Repo.Resolve` splits ref from path), the same
// binary/too-large refusal language, the same per-line table, and the same
// breadcrumbs. Attribution comes from api.RepoBrowse.Blame; the page groups
// consecutive lines with one commit into a block and links each block to
// /{o}/{r}/commit/<sha>.
//
// Nothing durable is cached: attribution is a per-request read of the serving
// copy, exactly like every other object render (law 4 — a derived render is
// not state). The in-request memo is the single Blame call itself.

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/crueber/spindle/internal/api"
)

type blameLineView struct {
	No    int
	Code  template.HTML
	Blank bool
}

// blameBlock is one run of consecutive lines attributed to the same commit.
type blameBlock struct {
	Short  string
	Href   string
	Author string
	Stamp  string // RFC 3339; the template renders the relative age
	Lines  []blameLineView
}

type blamePage struct {
	Repo      *RepoChrome
	Guide     *repoGuide
	Crumbs    []repoCrumb
	Path      string
	Name      string
	Lang      string
	Size      string
	SHA       string
	Ref       string
	CodeHref  string
	BlameHref string
	RawHref   string
	TooLarge  bool
	Binary    bool
	Truncated bool
	Cap       int
	Blocks    []blameBlock
}

func (s *Server) repoBlame(w http.ResponseWriter, r *http.Request) {
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
		s.errorPage(w, r, http.StatusNotFound, "file not found")
		return
	}
	data := &blamePage{Repo: c, Cap: api.MaxBlameLines}
	if c.Empty {
		data.Guide = s.repoGuide(c)
		s.page(w, r, rq.p, http.StatusOK, "repo/blame", "Blame · "+c.Full, data)
		return
	}
	res, err := s.env.API.Repo.Resolve(ctx, rq.id, rest)
	if err != nil {
		if isEmptyRepoErr(err) {
			data.Guide = s.repoGuide(c)
			s.page(w, r, rq.p, http.StatusOK, "repo/blame", "Blame · "+c.Full, data)
			return
		}
		s.repoViewError(w, r, err)
		return
	}
	if res.Path == "" {
		s.errorPage(w, r, http.StatusNotFound, "file not found")
		return
	}
	browse, ok := s.env.API.Repo.(api.RepoBrowse)
	if !ok {
		s.errorPage(w, r, http.StatusServiceUnavailable, "blame is unavailable on this build")
		return
	}
	blame, err := browse.Blame(ctx, rq.id, res.SHA, res.Path)
	if err != nil {
		s.repoViewError(w, r, err)
		return
	}
	data.Path = res.Path
	data.Name = baseNameOf(res.Path)
	data.Lang = LanguageFor(data.Name)
	data.Size = fmtSize(blame.Size)
	data.SHA = shortSHA(res.SHA)
	data.TooLarge, data.Binary, data.Truncated = blame.TooLarge, blame.Binary, blame.Truncated
	ref := shortRefName(res.Ref)
	if ref == "" {
		ref = res.SHA
	}
	data.Ref = ref
	data.CodeHref = "/" + c.Full + "/blob/" + escapePath(ref+"/"+res.Path)
	data.BlameHref = r.URL.Path
	data.RawHref = "/" + c.Full + "/api/blob/" + escapePath(ref) + "/" + escapePath(res.Path) + "?raw"
	parts := strings.Split(res.Path, "/")
	for i, part := range parts {
		sub := strings.Join(parts[:i+1], "/")
		data.Crumbs = append(data.Crumbs, repoCrumb{
			Name: part,
			Href: "/" + c.Full + "/tree/" + escapePath(ref+"/"+sub),
			Last: i == len(parts)-1,
		})
	}
	if !blame.TooLarge && !blame.Binary {
		data.Blocks = buildBlameBlocks(c.Full, blame.Lines)
	}
	s.page(w, r, rq.p, http.StatusOK, "repo/blame", data.Name+" · blame · "+c.Full, data)
}

// buildBlameBlocks groups consecutive same-commit lines. Grouping happens on
// the SERVER: blocks are a render shape, not data, and the page ships no JS.
func buildBlameBlocks(full string, lines []api.BlameLine) []blameBlock {
	blocks := []blameBlock{}
	for _, l := range lines {
		view := blameLineView{
			No:    l.LineNo,
			Code:  template.HTML(template.HTMLEscapeString(l.Text)),
			Blank: l.Text == "",
		}
		if n := len(blocks); n > 0 && blocks[n-1].Short == shortSHA(l.SHA) {
			blocks[n-1].Lines = append(blocks[n-1].Lines, view)
			continue
		}
		blocks = append(blocks, blameBlock{
			Short:  shortSHA(l.SHA),
			Href:   "/" + full + "/commit/" + l.SHA,
			Author: l.Author,
			Stamp:  rfc3339(l.AuthorTime),
			Lines:  []blameLineView{view},
		})
	}
	return blocks
}

// blameLineNo is unused; the template prints the line number directly.

// baseNameOf returns the last path segment.
func baseNameOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
